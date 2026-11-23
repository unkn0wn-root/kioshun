package kioshun

import (
	"errors"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// parkWorker makes producers see a pending wake-up while the worker sleeps.
// release sends the actual wake-up.
func parkWorker(t *testing.T, s *shard[string, string]) (release func()) {
	s.queue.wakeState.Store(1)
	release = sync.OnceFunc(func() {
		s.queue.wakeState.Store(0)
		s.queue.wakeConsumer()
	})
	t.Cleanup(release)
	return release
}

// holdGate parks the worker inside a drain with drainMu held until open is called.
func holdGate(t *testing.T, s *shard[string, string]) (open func()) {
	t.Helper()
	tail := s.queue.tail.Load()
	gate := gateWrite(t, s)
	open = sync.OnceFunc(func() { <-gate })
	t.Cleanup(open)
	waitFor(t, func() bool { return s.queue.tail.Load() == tail+1 })
	return open
}

func keyOn(c *Cache[string, string], s *shard[string, string], prefix string) string {
	for i := 0; ; i++ {
		k := prefix + strconv.Itoa(i)
		if c.shardByHash(c.hasher.Sum(k)) == s {
			return k
		}
	}
}

// Pass call a closure from the test so stackWith can pick out its goroutine.
func call(f func() error) (done chan error, returned func() bool) {
	var r atomic.Bool
	done = make(chan error, 1)
	go func() {
		err := f()
		r.Store(true)
		done <- err
	}()
	return done, r.Load
}

// stackWith returns the first pattern it sees in this test's goroutine stacks, or "" if the call finishes first.
func stackWith(t *testing.T, returned func() bool, patterns ...string) string {
	t.Helper()
	owner, _, _ := strings.Cut(t.Name(), "/")
	owner = "." + owner + ".func"
	res := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		res[i] = regexp.MustCompile(p)
	}
	buf := make([]byte, 1<<20)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); runtime.Gosched() {
		for g := range strings.SplitSeq(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
			if !strings.Contains(g, owner) {
				continue
			}
			for i, re := range res {
				if re.MatchString(g) {
					return patterns[i]
				}
			}
		}
		if returned() {
			return ""
		}
	}
	t.Fatalf("no goroutine of the test matched any of %q", patterns)
	return ""
}

func wait(t *testing.T, done chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
		return nil
	}
}

// lockedIn matches a goroutine blocked in a Mutex.Lock called directly by fn.
func lockedIn(fn string) string {
	return `^goroutine \d+ \[sync\.Mutex\.Lock(?s:.*)\nsync\.\(\*Mutex\)\.Lock\(.*\n.*\n.*\.` + fn + `\(`
}

// shardLockedIn allows the wait reason to name the inner Mutex used by RWMutex.Lock.
func shardLockedIn(fn string) string {
	return `^goroutine \d+ \[sync\.(RW)?Mutex\.Lock(?s:.*)\nsync\.\(\*RWMutex\)\.Lock\(.*\n.*\n.*\.` + fn + `\(`
}

var (
	lockedInDirect = lockedIn("applyBarrierDirect")
	clearingDirect = shardLockedIn("applyBarrierDirect")
)

const (
	inWaitForSpace  = `\.waitForSpace\(`
	inWaitClaimed   = `\.waitClaimed\(`
	inAwaitResult   = `\.awaitResult\(`
	inBarrierDirect = `\.applyBarrierDirect\(`
	inApplyBatch    = `\.applyWriteBatch\(`
)

var barrierCalls = []struct {
	name    string
	call    func(c *Cache[string, string]) error
	present bool // whether writes queued before the barrier are visible afterward
	closes  bool
}{
	{name: "Sync", call: func(c *Cache[string, string]) error { return c.Sync() }, present: true},
	{name: "Clear", call: func(c *Cache[string, string]) error { c.Clear(); return nil }},
	{name: "Close", call: func(c *Cache[string, string]) error { return c.Close() }, closes: true},
}

// newFullRingCache parks the worker on a two-slot ring so two writes force a direct barrier.
func newFullRingCache(t *testing.T, policy EvictionPolicy) (*Cache[string, string], *shard[string, string], func()) {
	t.Helper()
	c := newTestCache[string, string](t, Config{
		MaxSize: 1024, ShardCount: 1, EvictionPolicy: policy, WriteBufferSize: 2, WriteBatchSize: 1,
	})
	closeIfPassed(t, c)
	s := c.shards[0]
	return c, s, parkWorker(t, s)
}

func TestBarrierOnFullRingDoesNotWaitForWorker(t *testing.T) {
	for _, tc := range barrierCalls {
		t.Run(tc.name, func(t *testing.T) {
			c, s, release := newFullRingCache(t, LRU)
			queueSet(t, c, "a")
			queueSet(t, c, "b")

			done, _ := call(func() error { return tc.call(c) })
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
			case <-time.After(2 * time.Second):
				release()
				t.Fatalf("%s waited for a free ring slot while nothing else drained the ring", tc.name)
			}
			if n := s.queue.head.Load() - s.queue.tail.Load(); n != 0 {
				t.Errorf("%s left %d writes in the ring", tc.name, n)
			}
			if tc.closes {
				return
			}
			for _, k := range []string{"a", "b"} {
				if _, ok := c.Get(k); ok != tc.present {
					t.Errorf("after %s, Get(%q) found %v, want %v", tc.name, k, ok, tc.present)
				}
			}
		})
	}
}

func TestBarrierOnFullRingWaitsForClaimedWrite(t *testing.T) {
	for _, tc := range barrierCalls {
		t.Run(tc.name, func(t *testing.T) {
			c, s, _ := newFullRingCache(t, LRU)
			_, claimed, err := c.setCommand("claimed", "v", NoExpiration, nil)
			if err != nil {
				t.Fatal(err)
			}
			publishSlot := claimSlot(t, s.queue)
			publish := sync.OnceFunc(func() { publishSlot(claimed) })
			t.Cleanup(publish)
			queueSet(t, c, "b")

			done, returned := call(func() error { return tc.call(c) })
			switch stackWith(t, returned, inWaitClaimed, inWaitForSpace) {
			case inWaitForSpace:
				t.Fatalf("%s waited for a free ring slot instead of draining the ring", tc.name)
			case "":
				t.Fatalf("%s returned while a write claimed before it was unpublished", tc.name)
			}
			publish()
			if err := wait(t, done, tc.name); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got := s.queue.tail.Load(); got != 2 {
				t.Fatalf("%s returned with tail %d, want 2", tc.name, got)
			}
			if tc.closes {
				return
			}
			for _, k := range []string{"claimed", "b"} {
				if _, ok := c.Get(k); ok != tc.present {
					t.Errorf("after %s, Get(%q) found %v, want %v", tc.name, k, ok, tc.present)
				}
			}
		})
	}
}

func TestBarrierOnFullRingTakesDrainLock(t *testing.T) {
	c := newTestCache[string, string](t, Config{
		MaxSize: 1024, ShardCount: 1, EvictionPolicy: LRU, WriteBufferSize: 2, WriteBatchSize: 1,
	})
	closeIfPassed(t, c)
	s := c.shards[0]
	open := holdGate(t, s)
	_, claimed, err := c.setCommand("claimed", "v", NoExpiration, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Claim a slot beyond the worker's drain limit so Sync has to wait for it itself.
	publishSlot := claimSlot(t, s.queue)
	publish := sync.OnceFunc(func() { publishSlot(claimed) })
	t.Cleanup(publish)
	queueSet(t, c, "b")
	tail := s.queue.tail.Load()

	done, returned := call(func() error { return c.Sync() })
	switch stackWith(t, returned, lockedInDirect, inWaitClaimed, inWaitForSpace) {
	case inWaitForSpace:
		t.Fatal("Sync waited for a free ring slot instead of draining the ring")
	case inWaitClaimed:
		t.Fatal("Sync drained the ring while the worker held drainMu")
	case "":
		t.Fatal("Sync returned while the worker held drainMu")
	}
	if got := s.queue.tail.Load(); got != tail {
		t.Fatalf("tail moved from %d to %d while the worker held drainMu", tail, got)
	}
	open()
	if stackWith(t, returned, inWaitClaimed) == "" {
		t.Fatal("Sync returned while a write claimed before it was unpublished")
	}
	publish()
	if err := wait(t, done, "Sync"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"claimed", "b"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("Sync returned before the write to %q was applied", k)
		}
	}
}

func fill(t *testing.T, c *Cache[string, string], n int) {
	t.Helper()
	for i := range n {
		if err := c.Set("f"+strconv.Itoa(i), "v", NoExpiration); err != nil {
			t.Fatal(err)
		}
	}
}

// resetBeforeDrainLock polls drainMu to catch a clear that unlocks before resetting size.
// A lower size is enough because a drainer could apply new writes as soon as the clear releases drainMu.
func resetBeforeDrainLock(t *testing.T, s *shard[string, string], unlockShard func()) bool {
	t.Helper()
	before := atomic.LoadInt64(&s.size)
	unlockShard()
	deadline := time.Now().Add(5 * time.Second)
	for i := 1; !s.drainMu.TryLock(); i++ {
		if i%(1<<16) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("drainMu was still held 5s after the clear got the shard lock")
			}
			runtime.Gosched()
		}
	}
	reset := atomic.LoadInt64(&s.size) < before
	s.drainMu.Unlock()
	return reset
}

// Repeat the lock check to catch an early unlock when the clear runs on another P.
const clearRounds = 20

// A drainer can update the frequency sketch under drainMu alone, so Clear has to hold it through the reset.
func TestDirectClearHoldsDrainLockUntilCleared(t *testing.T) {
	for range clearRounds {
		c, s, _ := newFullRingCache(t, SieveTinyLFU)
		fill(t, c, 512)
		queueSet(t, c, "a")
		queueSet(t, c, "b")

		s.drainMu.Lock()
		unlockDrain := sync.OnceFunc(s.drainMu.Unlock)
		t.Cleanup(unlockDrain)
		done, returned := call(func() error { c.Clear(); return nil })
		if got := stackWith(t, returned, lockedInDirect, inWaitForSpace); got != lockedInDirect {
			t.Fatalf("Clear was seen in %q, want it waiting for drainMu", got)
		}
		// Empty the ring before Clear gets drainMu so it goes straight to the reset.
		c.drainShardQueue(s, s.queue.head.Load())
		s.mu.Lock()
		unlockShard := sync.OnceFunc(s.mu.Unlock)
		t.Cleanup(unlockShard)
		unlockDrain()
		if stackWith(t, returned, clearingDirect) == "" {
			t.Fatal("Clear returned without waiting for the shard lock")
		}

		queueSet(t, c, "late")
		got, gotReturned := call(func() error { c.Get("late"); return nil })
		if stackWith(t, gotReturned, inApplyBatch) != "" {
			t.Fatal("a Get drained the ring while Clear was clearing the shard")
		}
		if !resetBeforeDrainLock(t, s, unlockShard) {
			t.Fatal("Clear released drainMu before it reset the shard")
		}
		if err := wait(t, got, "Get"); err != nil {
			t.Fatal(err)
		}
		if err := wait(t, done, "Clear"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSetAsyncOnFullRingWaitsForSlot(t *testing.T) {
	c, s, release := newFullRingCache(t, LRU)
	queueSet(t, c, "a")
	queueSet(t, c, "b")
	tail := s.queue.tail.Load()

	done, returned := call(func() error { return c.SetAsync("c", "v", NoExpiration) })
	switch stackWith(t, returned, inWaitForSpace, inBarrierDirect) {
	case inBarrierDirect:
		t.Fatal("SetAsync on a full ring drained the ring itself")
	case "":
		t.Fatal("SetAsync returned while the ring was full")
	}
	if got := s.queue.tail.Load(); got != tail {
		t.Fatalf("tail moved from %d to %d while SetAsync waited for a slot", tail, got)
	}
	release()
	if err := wait(t, done, "SetAsync"); err != nil {
		t.Fatal(err)
	}
	if err := c.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("c"); !ok {
		t.Error("the write SetAsync queued after waiting for a slot is missing")
	}
}

func TestBarrierWithRoomIsQueued(t *testing.T) {
	c := newTestCache[string, string](t, Config{
		MaxSize: 1024, ShardCount: 1, EvictionPolicy: LRU, WriteBufferSize: 4, WriteBatchSize: 1,
	})
	closeIfPassed(t, c)
	s := c.shards[0]
	open := holdGate(t, s)
	head := s.queue.head.Load()

	done, returned := call(func() error { return c.Sync() })
	switch stackWith(t, returned, inAwaitResult, inBarrierDirect) {
	case inBarrierDirect:
		t.Fatal("Sync drained a ring that had room instead of queuing its barrier")
	case "":
		t.Fatal("Sync returned while the worker held drainMu")
	}
	if got := s.queue.head.Load(); got != head+1 {
		t.Fatalf("head moved from %d to %d while Sync waited, want one queued barrier", head, got)
	}
	open()
	if err := wait(t, done, "Sync"); err != nil {
		t.Fatal(err)
	}
}

func TestClearWithFullRingClearsEveryShard(t *testing.T) {
	c := newTestCache[string, string](t, Config{
		MaxSize: 1024, ShardCount: 2, EvictionPolicy: LRU, WriteBufferSize: 2, WriteBatchSize: 1,
	})
	closeIfPassed(t, c)
	full, other := c.shards[0], c.shards[1]
	kept := keyOn(c, other, "kept")
	if err := c.Set(kept, "v", NoExpiration); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get(kept); !ok {
		t.Fatalf("Get(%q) missed before Clear", kept)
	}
	release := parkWorker(t, full)
	queueSet(t, c, keyOn(c, full, "a"))
	queueSet(t, c, keyOn(c, full, "b"))

	done, _ := call(func() error { c.Clear(); return nil })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("Clear waited for a free ring slot while nothing else drained the ring")
	}
	if _, ok := c.Get(kept); ok {
		t.Errorf("Clear left %q on shard 1 after finding shard 0's ring full", kept)
	}
}

func TestSyncWithFullRingSyncsEveryShard(t *testing.T) {
	c := newTestCache[string, string](t, Config{
		MaxSize: 1024, ShardCount: 2, EvictionPolicy: LRU, WriteBufferSize: 4, WriteBatchSize: 1,
	})
	closeIfPassed(t, c)
	full, other := c.shards[0], c.shards[1]
	parkWorker(t, full)
	for i := range 4 {
		queueSet(t, c, keyOn(c, full, "k"+strconv.Itoa(i)+"-"))
	}
	if n := full.queue.head.Load() - full.queue.tail.Load(); n != 4 {
		t.Fatalf("shard 0 holds %d queued writes, want 4", n)
	}
	open := holdGate(t, other)
	later := keyOn(c, other, "later")
	queueSet(t, c, later)
	head := other.queue.head.Load()

	done, returned := call(func() error { return c.Sync() })
	switch stackWith(t, returned, inAwaitResult, inWaitForSpace) {
	case inWaitForSpace:
		t.Fatal("Sync waited for a free ring slot on shard 0 instead of draining it")
	case "":
		t.Fatalf("Sync returned before the write to %q queued on shard 1 was applied", later)
	}
	if got := other.queue.head.Load(); got != head+1 {
		t.Fatalf("shard 1 head moved from %d to %d, want one queued barrier", head, got)
	}
	open()
	if err := wait(t, done, "Sync"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get(later); !ok {
		t.Errorf("Sync returned before the write to %q on shard 1 was applied", later)
	}
}

func TestBarrierAfterCloseAppliesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   writeOp
	}{{"Sync", writeBarrier}, {"Clear", writeClear}} {
		for _, queued := range []int{1, 2} { // 1 leaves room in the ring of two, 2 fills it
			t.Run(tc.name+"/queued="+strconv.Itoa(queued), func(t *testing.T) {
				c := newTestCache[string, string](t, Config{
					MaxSize: 1024, ShardCount: 1, EvictionPolicy: LRU, WriteBufferSize: 2, WriteBatchSize: 1,
				})
				s := c.shards[0]
				c.Close()
				// These writes stand in for SetAsync calls that passed the closed check.
				for i := range queued {
					queueSet(t, c, "k"+strconv.Itoa(i))
				}
				tail := s.queue.tail.Load()
				done, _ := call(func() error { return c.enqueueAllShardsAndWait(tc.op) })
				if err := wait(t, done, tc.name); !errors.Is(err, ErrCacheClosed) {
					t.Errorf("%s after Close returned %v, want ErrCacheClosed", tc.name, err)
				}
				if got := s.queue.tail.Load(); got != tail {
					t.Errorf("%s after Close drained the ring: tail moved from %d to %d", tc.name, tail, got)
				}
				if n := s.tab.length(); n != 0 {
					t.Errorf("%s after Close left %d entries in the closed cache", tc.name, n)
				}
			})
		}
	}
}

func TestMissAfterCloseAppliesNothing(t *testing.T) {
	c := newTestCache[string, string](t, Config{MaxSize: 1024, ShardCount: 1, EvictionPolicy: SieveTinyLFU})
	s := c.shards[0]
	c.Close()
	queueSet(t, c, "k")
	if _, ok := c.drainMissAndLookup(s, c.hasher.Sum("k"), "k"); ok {
		t.Error("a miss after Close applied a queued write")
	}
	if n := s.tab.length(); n != 0 {
		t.Errorf("a miss after Close left %d entries in the closed cache", n)
	}
}

func TestMissDuringCloseReleasesDrainLock(t *testing.T) {
	c := newTestCache[string, string](t, Config{MaxSize: 1024, ShardCount: 1, EvictionPolicy: SieveTinyLFU})
	s := c.shards[0]
	release := parkWorker(t, s)
	queueSet(t, c, "k")
	c.closed.Store(true) // simulate the start of Close before the flush

	if _, ok := c.drainMissAndLookup(s, c.hasher.Sum("k"), "k"); ok {
		t.Error("a miss on a closed cache applied a queued write")
	}
	if s.drainMu.TryLock() {
		s.drainMu.Unlock()
	} else {
		t.Error("a miss on a closed cache kept drainMu")
		s.drainMu.Unlock() // so that Close can finish
	}
	release()
	done, _ := call(func() error { return c.Close() })
	if err := wait(t, done, "Close"); err != nil {
		t.Fatal(err)
	}
}

func TestCloseClearHoldsDrainLock(t *testing.T) {
	c := newTestCache[string, string](t, Config{MaxSize: 1024, ShardCount: 1, EvictionPolicy: SieveTinyLFU})
	closeIfPassed(t, c)
	for range clearRounds {
		fill(t, c, 512)
		s := c.shards[0]
		s.drainMu.Lock()
		unlockDrain := sync.OnceFunc(s.drainMu.Unlock)
		t.Cleanup(unlockDrain)

		done, returned := call(func() error { c.clearDirect(); return nil })
		if stackWith(t, returned, lockedIn("clearDirect")) == "" {
			t.Fatal("Close's clear reset the shard while another drainer held drainMu")
		}
		if !s.mu.TryLock() {
			t.Fatal("Close's clear held the shard lock while it waited for drainMu")
		}
		unlockShard := sync.OnceFunc(s.mu.Unlock)
		t.Cleanup(unlockShard)
		unlockDrain()
		if stackWith(t, returned, shardLockedIn("clearDirect")) == "" {
			t.Fatal("Close's clear reset the shard without waiting for the shard lock")
		}
		if s.drainMu.TryLock() {
			s.drainMu.Unlock()
			t.Fatal("Close's clear released drainMu while it waited for the shard lock")
		}
		if !resetBeforeDrainLock(t, s, unlockShard) {
			t.Fatal("Close's clear released drainMu before it reset the shard")
		}
		if err := wait(t, done, "clearDirect"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBarrierWaitingThroughCloseAppliesNothing(t *testing.T) {
	c := newTestCache[string, string](t, Config{
		MaxSize: 1024, ShardCount: 1, EvictionPolicy: LRU, WriteBufferSize: 2, WriteBatchSize: 1,
	})
	closeIfPassed(t, c)
	s := c.shards[0]
	open := holdGate(t, s)
	queueSet(t, c, "a")
	queueSet(t, c, "b")

	synced, returned := call(func() error { return c.Sync() })
	if got := stackWith(t, returned, lockedInDirect, inWaitForSpace); got != lockedInDirect {
		t.Fatalf("Sync was seen in %q, want it waiting for drainMu", got)
	}
	closed, _ := call(func() error { return c.Close() })
	waitFor(t, c.isClosed)
	open()
	if err := wait(t, synced, "Sync"); !errors.Is(err, ErrCacheClosed) {
		t.Errorf("Sync that got drainMu after Close began returned %v, want ErrCacheClosed", err)
	}
	if err := wait(t, closed, "Close"); err != nil {
		t.Fatal(err)
	}
	if n := s.tab.length(); n != 0 {
		t.Errorf("Close left %d entries in the closed cache", n)
	}
}
