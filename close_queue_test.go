package kioshun

import (
	"errors"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
	"weak"
)

const inSeal = `\.seal\(`

func ringHoldsNothing(t *testing.T, s *shard[string, string]) {
	t.Helper()
	for i := range s.queue.buffer {
		if cmd := s.queue.buffer[i].cmd; cmd != (writeCommand[string, string]{}) {
			t.Errorf("after Close, ring cell %d holds the write to %q", i, cmd.key)
		}
	}
}

// lateEnqueue resumes a SetAsync that already passed its closed check.
func lateEnqueue(t *testing.T, c *Cache[string, string], key string) error {
	t.Helper()
	s, cmd, err := c.setCommand(key, "v", NoExpiration, nil)
	if err != nil {
		t.Fatal(err)
	}
	done, _ := call(func() error { return s.queue.enqueue(cmd) })
	return wait(t, done, "enqueue after Close")
}

type weakValue [64]byte

func queueWeakValues(t *testing.T, c *Cache[string, *weakValue], n int) []weak.Pointer[weakValue] {
	t.Helper()
	ptrs := make([]weak.Pointer[weakValue], n)
	for i := range ptrs {
		v := new(weakValue)
		ptrs[i] = weak.Make(v)
		s, cmd, err := c.setCommand("k"+strconv.Itoa(i), v, NoExpiration, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.queue.enqueue(cmd); err != nil {
			t.Fatal(err)
		}
	}
	return ptrs
}

func TestCloseSealsQueue(t *testing.T) {
	for _, ring := range []int{2, 4} {
		// Start Close with head at each position in the ring.
		for queued := range ring + 1 {
			t.Run("ring="+strconv.Itoa(ring)+"/queued="+strconv.Itoa(queued), func(t *testing.T) {
				c := newTestCache[string, string](t, Config{
					MaxSize: 1024, ShardCount: 1, EvictionPolicy: LRU, WriteBufferSize: ring, WriteBatchSize: 1,
				})
				s := c.shards[0]
				for i := range queued {
					queueSet(t, c, "k"+strconv.Itoa(i))
				}
				if err := c.Sync(); err != nil {
					t.Fatal(err)
				}
				c.Close()
				if err := lateEnqueue(t, c, "late"); !errors.Is(err, ErrCacheClosed) {
					t.Errorf("a write that reached the ring after Close returned %v, want ErrCacheClosed", err)
				}
				ringHoldsNothing(t, s)
			})
		}
	}
	t.Run("full", func(t *testing.T) {
		c, s, _ := newFullRingCache(t, LRU)
		queueSet(t, c, "a")
		queueSet(t, c, "b")
		c.Close()
		if err := lateEnqueue(t, c, "late"); !errors.Is(err, ErrCacheClosed) {
			t.Errorf("a write that reached the ring after Close returned %v, want ErrCacheClosed", err)
		}
		ringHoldsNothing(t, s)
	})
}

func TestCloseDropsWriteClaimedBeforeSeal(t *testing.T) {
	entered := make(chan struct{}, 1)
	unblock := make(chan struct{})
	c, err := New[string, string](Config{
		MaxSize: 1, ShardCount: 1, EvictionPolicy: LRU, WriteBufferSize: 2, WriteBatchSize: 1,
	}, WithOnEvict(func(string, string) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-unblock
	}))
	if err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(func() { close(unblock) })
	t.Cleanup(release)
	s := c.shards[0]
	for _, k := range []string{"a", "b"} { // b evicts a and blocks the notify worker
		if err := c.Set(k, "v", NoExpiration); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the removal listener was not called")
	}

	closed, returned := call(func() error { return c.Close() })
	if stackWith(t, returned, `\.\(\*WaitGroup\)\.Wait\(`) == "" {
		t.Fatal("Close returned while the removal listener was blocked")
	}
	// Claim after the flush and hold publication so only seal can pick up the command.
	_, claimed, err := c.setCommand("claimed", "v", NoExpiration, nil)
	if err != nil {
		t.Fatal(err)
	}
	publishSlot := claimSlot(t, s.queue)
	publish := sync.OnceFunc(func() { publishSlot(claimed) })
	t.Cleanup(publish)
	release()
	if stackWith(t, returned, inSeal) == "" {
		t.Fatal("Close returned while a write claimed before it finished was unpublished")
	}
	publish()
	if err := wait(t, closed, "Close"); err != nil {
		t.Fatal(err)
	}
	ringHoldsNothing(t, s)

	// Go straight to the queue to check what a late drain could see.
	s.drainMu.Lock()
	c.drainShardQueue(s, s.queue.head.Load())
	s.drainMu.Unlock()
	if n := s.tab.length(); n != 0 {
		t.Errorf("a drain after Close applied %d writes", n)
	}
	if err := lateEnqueue(t, c, "late"); !errors.Is(err, ErrCacheClosed) {
		t.Errorf("a write that reached the ring after Close returned %v, want ErrCacheClosed", err)
	}
}

func TestCloseReleasesQueuedValues(t *testing.T) {
	c := newTestCache[string, *weakValue](t, Config{
		MaxSize: 1024, ShardCount: 1, EvictionPolicy: LRU, WriteBufferSize: 4, WriteBatchSize: 4,
	})
	t.Cleanup(func() { c.Close() }) // registered first so cleanup releases the worker before closing
	s := c.shards[0]
	release := parkWorker(t, s)
	// Close's one-command flush would miss stale references left by a longer batch.
	ptrs := queueWeakValues(t, c, 3)
	release()
	if err := c.Sync(); err != nil {
		t.Fatal(err)
	}
	c.Close()
	for i, cmd := range s.writeBatch {
		if cmd != (writeCommand[string, *weakValue]{}) {
			t.Errorf("after Close, batch entry %d holds the write to %q", i, cmd.key)
		}
	}
	runtime.GC()
	for i, p := range ptrs {
		if p.Value() != nil {
			t.Errorf("after Close, the value of write %d is still reachable", i)
		}
	}
	runtime.KeepAlive(c)
}

func TestCloseSealsUnderDrainLock(t *testing.T) {
	c := newTestCache[string, string](t, Config{MaxSize: 1024, ShardCount: 1, EvictionPolicy: LRU, WriteBufferSize: 2})
	closeIfPassed(t, c)
	s := c.shards[0]
	publishSlot := claimSlot(t, s.queue)
	publish := sync.OnceFunc(func() { publishSlot(writeCommand[string, string]{op: writeBarrier}) })
	t.Cleanup(publish)

	done, returned := call(func() error { c.clearDirect(); return nil })
	if stackWith(t, returned, inSeal) == "" {
		t.Fatal("Close's clear returned while a claimed slot was unpublished")
	}
	if s.drainMu.TryLock() {
		s.drainMu.Unlock()
		t.Error("Close's clear sealed the ring without holding drainMu")
	}
	publish()
	if err := wait(t, done, "clearDirect"); err != nil {
		t.Fatal(err)
	}
}

func TestClearKeepsQueueOpen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		queued int
	}{
		{"direct", 2},
		{"queued", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, s, release := newFullRingCache(t, LRU)
			for i := range tc.queued {
				queueSet(t, c, "k"+strconv.Itoa(i))
			}
			c.Clear()
			if n := s.tab.length(); n != 0 {
				t.Fatalf("Clear left %d entries", n)
			}

			s.drainMu.Lock() // force SetAsync to use the ring
			unlockDrain := sync.OnceFunc(s.drainMu.Unlock)
			t.Cleanup(unlockDrain)
			head := s.queue.head.Load()
			done, _ := call(func() error { return c.SetAsync("late", "v", NoExpiration) })
			if err := wait(t, done, "SetAsync after Clear"); err != nil {
				t.Fatalf("SetAsync after Clear returned %v", err)
			}
			if got := s.queue.head.Load(); got != head+1 {
				t.Fatalf("SetAsync after Clear moved head from %d to %d, want %d", head, got, head+1)
			}
			unlockDrain()
			release()
			if err := c.Sync(); err != nil {
				t.Fatal(err)
			}
			if _, ok := c.Get("late"); !ok {
				t.Error("a write queued after Clear was not applied")
			}
		})
	}
}

func TestCloseSealsEveryShard(t *testing.T) {
	c := newTestCache[string, string](t, Config{MaxSize: 1024, ShardCount: 4, EvictionPolicy: LRU, WriteBufferSize: 2})
	c.Close()
	for i, s := range c.shards {
		done, _ := call(func() error { return s.queue.enqueue(writeCommand[string, string]{key: "late", value: "v"}) })
		if err := wait(t, done, "enqueue after Close"); !errors.Is(err, ErrCacheClosed) {
			t.Errorf("shard %d: a write that reached the ring after Close returned %v, want ErrCacheClosed", i, err)
		}
		ringHoldsNothing(t, s)
	}
}

func TestCloseAppliesAcceptedWrites(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ring, queued int
	}{{"room", 8, 3}, {"full", 4, 4}} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCache[string, string](t, Config{
				MaxSize: 1, ShardCount: 1, EvictionPolicy: LRU, WriteBufferSize: tc.ring, WriteBatchSize: 1,
				StatsEnabled: true,
			})
			release := parkWorker(t, c.shards[0])
			for i := range tc.queued {
				queueSet(t, c, "k"+strconv.Itoa(i))
			}
			done, returned := call(func() error { return c.Close() })
			if tc.queued < tc.ring {
				if stackWith(t, returned, inAwaitResult) == "" {
					t.Fatal("Close returned while the worker that holds its flush was asleep")
				}
				release()
			}
			if err := wait(t, done, "Close"); err != nil {
				t.Fatal(err)
			}
			// Close clears the entries, but every write after the first leaves an eviction in the stats.
			if got, want := c.Stats().Evictions, int64(tc.queued-1); got != want {
				t.Errorf("Close applied writes that evicted %d entries, want %d", got, want)
			}
		})
	}
}

func TestAwaitResultReturnsOnClose(t *testing.T) {
	c := newTestCache[string, string](t, Config{MaxSize: 1024, ShardCount: 1, EvictionPolicy: LRU})
	c.Close()
	// A barrier dropped by seal leaves its result channel open.
	done, _ := call(func() error { return c.awaitResult(make(chan struct{})) })
	if err := wait(t, done, "awaitResult"); !errors.Is(err, ErrCacheClosed) {
		t.Errorf("waiting for a dropped barrier returned %v, want ErrCacheClosed", err)
	}
}
