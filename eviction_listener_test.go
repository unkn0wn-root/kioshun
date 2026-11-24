package kioshun

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"
)

// removalEvent is one listener call. evict marks an OnEvict call.
type removalEvent struct {
	key    int
	value  string
	reason RemovalReason
	evict  bool
}

// removalLog records listener calls in delivery order.
type removalLog struct {
	mu     sync.Mutex
	events []removalEvent
}

func (l *removalLog) onRemove(k int, v string, reason RemovalReason) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, removalEvent{key: k, value: v, reason: reason})
}

func (l *removalLog) onEvict(k int, v string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, removalEvent{key: k, value: v, reason: RemovedCapacity, evict: true})
}

func (l *removalLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

func (l *removalLog) snapshot() []removalEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// listenerGate holds the notify worker inside a listener until open is closed.
type listenerGate struct {
	entered chan struct{}
	open    chan struct{}
}

func newListenerGate() *listenerGate {
	return &listenerGate{entered: make(chan struct{}, 1), open: make(chan struct{})}
}

// wait blocks a listener until the gate opens.
func (g *listenerGate) wait() {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.open
}

// awaitEntered fails the test unless a listener reaches the gate within 2s.
func (g *listenerGate) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no listener reached the gate")
	}
}

// runWithin fails the test if f does not return within 5s. f must not call
// t.Fatal.
func runWithin(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s still blocked after 5s", what)
	}
}

// mustSet stores valFor(k) under k without expiry.
func mustSet(t *testing.T, c *Cache[int, string], k int) {
	t.Helper()
	if err := c.Set(k, valFor(k), NoExpiration); err != nil {
		t.Fatalf("Set(%d): %v", k, err)
	}
}

func TestOnRemove_EveryReasonEveryPath(t *testing.T) {
	expire := func(t *testing.T, c *Cache[int, string]) {
		t.Helper()
		if err := c.Set(1, valFor(1), time.Millisecond); err != nil {
			t.Fatalf("Set: %v", err)
		}
		// Keys skips expired entries without removing them.
		waitFor(t, func() bool { return len(c.Keys()) == 0 })
	}
	expired := removalEvent{key: 1, value: valFor(1), reason: RemovedExpired}
	getExpired := func(t *testing.T, c *Cache[int, string]) removalEvent {
		expire(t, c)
		if _, ok := c.Get(1); ok {
			t.Fatal("Get returned an expired entry")
		}
		return expired
	}

	paths := []struct {
		name     string
		cfg      Config
		opts     []Option[int, string]
		policies []EvictionPolicy // nil means every policy
		run      func(t *testing.T, c *Cache[int, string]) removalEvent
	}{
		{
			name: "capacity/set",
			cfg:  Config{MaxSize: 1, ShardCount: 1},
			run: func(t *testing.T, c *Cache[int, string]) removalEvent {
				mustSet(t, c, 1)
				mustSet(t, c, 2)
				return removalEvent{key: 1, value: valFor(1), reason: RemovedCapacity}
			},
		},
		{
			name: "capacity/set-async",
			cfg:  Config{MaxSize: 1, ShardCount: 1},
			run: func(t *testing.T, c *Cache[int, string]) removalEvent {
				for k := 1; k <= 2; k++ {
					if err := c.SetAsync(k, valFor(k), NoExpiration); err != nil {
						t.Fatalf("SetAsync(%d): %v", k, err)
					}
				}
				if err := c.Sync(); err != nil {
					t.Fatalf("Sync: %v", err)
				}
				return removalEvent{key: 1, value: valFor(1), reason: RemovedCapacity}
			},
		},
		{
			name: "capacity/cost-update",
			cfg:  Config{MaxSize: 2, MaxCost: 2, ShardCount: 1},
			opts: []Option[int, string]{WithWeigher(func(_ int, v string) int64 {
				if v == "heavy" {
					return 2
				}
				return 1
			})},
			run: func(t *testing.T, c *Cache[int, string]) removalEvent {
				mustSet(t, c, 1)
				mustSet(t, c, 2)
				if err := c.Set(2, "heavy", NoExpiration); err != nil {
					t.Fatalf("Set(2, heavy): %v", err)
				}
				// Policies differ in which entry pays for the update.
				if slices.Contains(c.Keys(), 1) {
					return removalEvent{key: 2, value: "heavy", reason: RemovedCapacity}
				}
				return removalEvent{key: 1, value: valFor(1), reason: RemovedCapacity}
			},
		},
		{
			name:     "rejected",
			cfg:      Config{MaxSize: 2, ShardCount: 1},
			policies: []EvictionPolicy{SieveTinyLFU},
			run: func(t *testing.T, c *Cache[int, string]) removalEvent {
				// Hot residents in frequency mode turn away a cold candidate, as in
				// TestSieveRejectedCandidateNeverPublished.
				s := c.shards[0]
				s.mu.Lock()
				for _, k := range []int{1, 2} {
					it := &cacheItem[int, string]{key: k, value: valFor(k), hash: c.hasher.Sum(k)}
					s.tab.store(it)
					s.sieve.insertMain(it)
					clearItemVisited(it)
					it.reuse = 0
					atomic.AddInt64(&s.size, 1)
					for range 8 {
						s.sieve.sketch.increment(it.hash)
					}
				}
				s.sieve.tuner.mode = admitFrequency
				s.mu.Unlock()

				mustSet(t, c, 99)
				return removalEvent{key: 99, value: valFor(99), reason: RemovedRejected}
			},
		},
		{
			name: "expired/cleanup",
			cfg:  Config{MaxSize: 100, ShardCount: 1},
			run: func(t *testing.T, c *Cache[int, string]) removalEvent {
				expire(t, c)
				c.Cleanup()
				return expired
			},
		},
		{
			// An unbounded cache reads under the shard lock for every policy.
			name: "expired/get-locked",
			cfg:  Config{ShardCount: 1},
			run:  getExpired,
		},
		{
			name:     "expired/get-lock-free",
			cfg:      Config{MaxSize: 100, ShardCount: 1},
			policies: []EvictionPolicy{SieveTinyLFU},
			run:      getExpired,
		},
		{
			name: "expired/exists",
			cfg:  Config{MaxSize: 100, ShardCount: 1},
			run: func(t *testing.T, c *Cache[int, string]) removalEvent {
				expire(t, c)
				if c.Exists(1) {
					t.Fatal("Exists reported an expired entry")
				}
				return expired
			},
		},
		{
			name: "deleted",
			cfg:  Config{MaxSize: 100, ShardCount: 1},
			run: func(t *testing.T, c *Cache[int, string]) removalEvent {
				mustSet(t, c, 1)
				if !c.Delete(1) {
					t.Fatal("Delete(1) = false, want true")
				}
				return removalEvent{key: 1, value: valFor(1), reason: RemovedDeleted}
			},
		},
	}

	for _, policy := range []EvictionPolicy{LRU, LFU, FIFO, SieveTinyLFU} {
		for _, p := range paths {
			if p.policies != nil && !slices.Contains(p.policies, policy) {
				continue
			}
			for _, listener := range []string{"OnRemove", "OnEvict"} {
				t.Run(policyName(policy)+"/"+p.name+"/"+listener, func(t *testing.T) {
					var log removalLog
					opt := WithOnRemove(log.onRemove)
					if listener == "OnEvict" {
						opt = WithOnEvict(log.onEvict)
					}
					cfg := p.cfg
					cfg.EvictionPolicy = policy
					c, err := New(cfg, append([]Option[int, string]{opt}, p.opts...)...)
					if err != nil {
						t.Fatalf("New: %v", err)
					}
					defer c.Close()

					removed := p.run(t, c)
					var want []removalEvent
					switch {
					case listener == "OnRemove":
						want = []removalEvent{removed}
					case removed.reason == RemovedCapacity:
						removed.evict = true
						want = []removalEvent{removed}
					}
					if len(want) > 0 {
						waitFor(t, func() bool { return log.count() > 0 })
					}
					// Close delivers everything staged, so nothing can arrive later.
					c.Close()
					if got := log.snapshot(); !slices.Equal(got, want) {
						t.Fatalf("listener calls = %+v, want %+v", got, want)
					}
				})
			}
		}
	}
}

func TestOnRemove_CloseDeliversPending(t *testing.T) {
	const rounds, total = 32, 2000 // about 500 staged per shard, above the reuse limit
	cfg := Config{MaxSize: 16, ShardCount: 4, EvictionPolicy: LRU}

	// select picks the wake-up or close at random, hence the rounds.
	t.Run("worker_blocked", func(t *testing.T) {
		for round := range rounds {
			gate := newListenerGate()
			var delivered atomic.Int64
			c, err := New(cfg, WithOnRemove(func(int, string, RemovalReason) {
				gate.wait()
				delivered.Add(1)
			}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			runWithin(t, "Set", func() {
				for k := range total {
					if err := c.Set(k, valFor(k), NoExpiration); err != nil {
						t.Errorf("Set(%d): %v", k, err)
						return
					}
				}
			})
			gate.awaitEntered(t)
			size := c.Size()
			// Keep the worker inside the listener until Close has started.
			go func() {
				<-c.closeCh
				close(gate.open)
			}()
			runWithin(t, "Close", func() { c.Close() })
			if got, want := delivered.Load(), total-size; got != want {
				t.Fatalf("round %d: delivered %d removals when Close returned, want %d", round, got, want)
			}
		}
	})
}

func TestOnRemove_RemovalAfterCloseDoesNotBlock(t *testing.T) {
	for round := range 32 {
		gate := newListenerGate()
		c, err := New(Config{MaxSize: 1, ShardCount: 1, EvictionPolicy: LRU},
			WithOnRemove(func(k int, _ string, _ RemovalReason) {
				if k == 0 {
					gate.wait()
				}
			}))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mustSet(t, c, 0)
		mustSet(t, c, 1) // evicts 0 and holds the worker
		gate.awaitEntered(t)
		runWithin(t, "Set", func() {
			// evicts 1 while the worker is held
			if err := c.Set(2, valFor(2), NoExpiration); err != nil {
				t.Errorf("Set(2): %v", err)
			}
		})
		go func() {
			<-c.closeCh
			close(gate.open)
		}()
		runWithin(t, "Close", func() { c.Close() })
		s := c.shards[0]
		runWithin(t, fmt.Sprintf("round %d: a removal after Close", round), func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			// Call dropItem directly to exercise staging after Close has emptied the shard.
			s.dropItem(&cacheItem[int, string]{key: 3, value: valFor(3), unpublished: true}, false, RemovedDeleted, dropLRU)
		})
	}
}

const (
	inWaitWorkers   = `\.\(\*WaitGroup\)\.Wait\(`
	inCloseListener = `(?s)\.\(\*listenerGate\)\.wait\(.*\.Close\(`
)

// stageAtClear mimics an expiry check holding the shard lock when Close reaches its final clear.
// Close holds drainMu by then, so tests inject other removal reasons to exercise the listeners.
func stageAtClear[V any](t *testing.T, c *Cache[int, V], release func(), returned func() bool, key int, reason RemovalReason) {
	t.Helper()
	if stackWith(t, returned, inWaitWorkers) == "" {
		t.Fatal("Close returned while the notify worker was held in a listener call")
	}
	s := c.getShard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Let the notify worker stop before staging the removal.
	release()
	if stackWith(t, returned, shardLockedIn("clearDirect")) == "" {
		t.Fatal("Close returned without taking the shard lock to clear the shard")
	}
	item, ok := s.tab.lookup(c.hasher.Sum(key), key)
	if !ok {
		t.Fatalf("key %d is not resident when Close clears its shard", key)
	}
	c.removeItem(s, item, reason)
}

var closeListeners = []struct {
	name          string
	remove, evict bool
}{
	{"OnRemove", true, false},
	{"OnEvict", false, true},
	{"Both", true, true},
}

// listenerOptions calls first once per delivered removal, even when both listeners are registered.
func listenerOptions(remove, evict bool, log *removalLog, first func(k int)) []Option[int, string] {
	var opts []Option[int, string]
	if remove {
		opts = append(opts, WithOnRemove(func(k int, v string, reason RemovalReason) {
			first(k)
			log.onRemove(k, v, reason)
		}))
	}
	if evict {
		opts = append(opts, WithOnEvict(func(k int, v string) {
			if !remove {
				first(k)
			}
			log.onEvict(k, v)
		}))
	}
	return opts
}

func capacityRemovals(remove, evict bool, keys ...int) []removalEvent {
	var want []removalEvent
	for _, k := range keys {
		if remove {
			want = append(want, removalEvent{key: k, value: valFor(k), reason: RemovedCapacity})
		}
		if evict {
			want = append(want, removalEvent{key: k, value: valFor(k), reason: RemovedCapacity, evict: true})
		}
	}
	return want
}

func TestOnRemove_CloseDeliversRemovalStagedBeforeClear(t *testing.T) {
	for _, tc := range closeListeners {
		t.Run(tc.name, func(t *testing.T) {
			worker, last := newListenerGate(), newListenerGate()
			var log removalLog
			var c *Cache[int, string]
			var keys []int
			c, err := New(Config{MaxSize: 4, ShardCount: 2, EvictionPolicy: LRU},
				listenerOptions(tc.remove, tc.evict, &log, func(k int) {
					switch k {
					case keys[0]:
						worker.wait()
					case keys[1]:
						if s := c.shards[1]; s.mu.TryLock() {
							if n := s.tab.length(); n != 0 {
								t.Errorf("Close called the listener with %d entries left in the shard", n)
							}
							s.mu.Unlock()
						} else {
							t.Error("Close held the shard lock while it called the listener")
						}
						last.wait()
					}
				})...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			release := sync.OnceFunc(func() { close(worker.open) })
			t.Cleanup(release)
			releaseLast := sync.OnceFunc(func() { close(last.open) })
			t.Cleanup(releaseLast)
			// Use the last shard so the removal can be staged as late as possible.
			for k := 0; len(keys) < 3; k++ {
				if c.getShard(k) != c.shards[1] {
					continue
				}
				keys = append(keys, k)
				ttl := NoExpiration
				if len(keys) == 3 {
					// Leave one expired entry for Close to clear silently.
					ttl = time.Nanosecond
				}
				if err := c.Set(k, valFor(k), ttl); err != nil {
					t.Fatalf("Set(%d): %v", k, err)
				}
			}
			worker.awaitEntered(t)

			done, returned := call(func() error { return c.Close() })
			stageAtClear(t, c, release, returned, keys[1], RemovedCapacity)
			select {
			case <-last.entered:
			case <-done:
				t.Fatal("Close returned without delivering the removal staged before it cleared the shard")
			case <-time.After(5 * time.Second):
				t.Fatal("the removal staged before Close cleared the shard was not delivered")
			}
			if stackWith(t, returned, inCloseListener) == "" {
				t.Fatal("Close did not make the listener call for the removal staged before it cleared the shard")
			}
			again, againReturned := call(func() error { return c.Close() })
			if stackWith(t, againReturned, lockedIn(`\(\*Once\)\.doSlow`)) == "" {
				t.Fatal("a second Close returned while the first was in a listener call")
			}
			releaseLast()
			if err := wait(t, done, "Close"); err != nil {
				t.Fatal(err)
			}
			if err := wait(t, again, "the second Close"); err != nil {
				t.Fatal(err)
			}
			if got, want := log.snapshot(), capacityRemovals(tc.remove, tc.evict, keys[0], keys[1]); !slices.Equal(got, want) {
				t.Fatalf("listener calls = %+v, want %+v", got, want)
			}
			s := c.shards[1]
			for j, e := range s.removeBuf[:cap(s.removeBuf)] {
				if e != (removedEntry[int, string]{}) {
					t.Errorf("after Close, staging slot %d holds the removal of key %d", j, e.key)
				}
			}
		})
	}
}

func TestOnRemove_CloseDeliversEvictionByLastDrain(t *testing.T) {
	for _, tc := range closeListeners {
		t.Run(tc.name, func(t *testing.T) {
			var log removalLog
			c, err := New(Config{MaxSize: 2, ShardCount: 2, EvictionPolicy: LRU, WriteBufferSize: 2, WriteBatchSize: 1},
				listenerOptions(tc.remove, tc.evict, &log, func(int) {})...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			// Match this cache's notify worker by the goroutine that called New.
			buf := make([]byte, 1<<20)
			createdHere := "in goroutine " + strings.Fields(string(buf[:runtime.Stack(buf, false)]))[1] + "\n"
			notifyStopped := func() bool {
				for g := range strings.SplitSeq(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
					if strings.Contains(g, ".removeNotifyWorker(") && strings.Contains(g, createdHere) {
						return false
					}
				}
				return true
			}
			s0, s1 := c.shards[0], c.shards[1]
			var keys []int
			for k := 0; len(keys) < 2; k++ {
				if c.getShard(k) == s0 {
					keys = append(keys, k)
				}
			}
			a, b := keys[0], keys[1]
			// Queue the write as if SetAsync had already passed its closed check.
			queue := func(k int) {
				t.Helper()
				_, cmd, err := c.setCommand(k, valFor(k), NoExpiration, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := s0.queue.enqueue(cmd); err != nil {
					t.Fatal(err)
				}
			}
			parkWorker(t, s0)
			queue(a)
			queue(a) // fill shard 0 so Close flushes it directly
			tail := s1.queue.tail.Load()
			held := gateWrite(t, s1)
			waitFor(t, func() bool { return s1.queue.tail.Load() == tail+1 })

			done, returned := call(func() error { return c.Close() })
			if stackWith(t, returned, inAwaitResult) == "" {
				t.Fatal("Close returned while shard 1's worker held its flush")
			}
			last := gateWrite(t, s0)
			queue(b) // evicts a once applied
			if notifyStopped() {
				t.Fatal("the notify worker was not found")
			}
			<-held // let shard 1 finish the flush
			waitFor(t, notifyStopped)
			<-last // let shard 0 evict a after the notify worker stops
			if err := wait(t, done, "Close"); err != nil {
				t.Fatal(err)
			}
			if got, want := log.snapshot(), capacityRemovals(tc.remove, tc.evict, a); !slices.Equal(got, want) {
				t.Fatalf("listener calls = %+v, want %+v", got, want)
			}
		})
	}
}

func TestOnRemove_CloseReleasesStagedRemovals(t *testing.T) {
	worker := newListenerGate()
	c, err := New(Config{MaxSize: 2, ShardCount: 1, EvictionPolicy: LRU},
		WithOnRemove(func(k int, _ *weakValue, _ RemovalReason) {
			if k == 0 {
				worker.wait()
			}
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	release := sync.OnceFunc(func() { close(worker.open) })
	t.Cleanup(release)
	var staged weak.Pointer[weakValue]
	for k := range 3 { // 2 evicts 0 and holds the worker
		v := new(weakValue)
		if k == 1 {
			staged = weak.Make(v)
		}
		if err := c.Set(k, v, NoExpiration); err != nil {
			t.Fatalf("Set(%d): %v", k, err)
		}
	}
	worker.awaitEntered(t)

	done, returned := call(func() error { return c.Close() })
	stageAtClear(t, c, release, returned, 1, RemovedExpired)
	if err := wait(t, done, "Close"); err != nil {
		t.Fatal(err)
	}
	for i, s := range c.shards {
		for j, e := range s.removeBuf[:cap(s.removeBuf)] {
			if e != (removedEntry[int, *weakValue]{}) {
				t.Errorf("after Close, shard %d staging slot %d holds the removal of key %d", i, j, e.key)
			}
		}
	}
	runtime.GC()
	if staged.Value() != nil {
		t.Error("after Close, the value of the removal staged before the clear is still reachable")
	}
	runtime.KeepAlive(c)
}

func TestOnRemove_CloseListenerUsesClosedCache(t *testing.T) {
	worker := newListenerGate()
	var c *Cache[int, string]
	var reentered atomic.Bool
	c, err := New(Config{MaxSize: 2, ShardCount: 1, EvictionPolicy: LRU},
		WithOnRemove(func(k int, _ string, _ RemovalReason) {
			if k == 0 {
				worker.wait()
				return
			}
			s := c.shards[0]
			if s.mu.TryLock() {
				s.mu.Unlock()
			} else {
				t.Error("Close held the shard lock while it called the listener")
			}
			if s.drainMu.TryLock() {
				s.drainMu.Unlock()
			} else {
				t.Error("Close held drainMu while it called the listener")
			}
			c.Get(2)
			c.GetWithTTL(2)
			_ = c.Set(5, valFor(5), NoExpiration)
			_ = c.SetAsync(6, valFor(6), NoExpiration)
			_ = c.SetWithCallback(7, valFor(7), time.Minute, func(int, string) {})
			c.Delete(2)
			c.Exists(2)
			c.Keys()
			_ = c.Sync()
			c.Clear()
			c.Cleanup()
			c.Stats()
			c.PolicyStats()
			reentered.Store(true)
		}),
		WithOnEvict(func(k int, _ string) {
			if k != 0 {
				t.Errorf("OnEvict was called for key %d, which was not removed for capacity", k)
			}
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	release := sync.OnceFunc(func() { close(worker.open) })
	t.Cleanup(release)
	for k := range 3 {
		mustSet(t, c, k) // 2 evicts 0 and holds the worker
	}
	worker.awaitEntered(t)

	done, returned := call(func() error { return c.Close() })
	stageAtClear(t, c, release, returned, 1, RemovedDeleted)
	if err := wait(t, done, "Close"); err != nil {
		t.Fatal(err)
	}
	if !reentered.Load() {
		t.Fatal("Close did not call the listener for the removal staged before it cleared the shard")
	}
	if n := c.shards[0].tab.length(); n != 0 {
		t.Errorf("the listener's writes left %d entries in the closed cache", n)
	}
}

func TestOnRemove_CloseDoesNotOverlapWorker(t *testing.T) {
	for _, tc := range closeListeners {
		t.Run(tc.name, func(t *testing.T) {
			worker := newListenerGate()
			var log removalLog
			var active atomic.Int32
			var overlapped atomic.Bool
			c, err := New(Config{MaxSize: 1, ShardCount: 1, EvictionPolicy: LRU},
				listenerOptions(tc.remove, tc.evict, &log, func(k int) {
					if active.Add(1) > 1 {
						overlapped.Store(true)
					}
					defer active.Add(-1)
					if k == 0 {
						worker.wait()
					}
				})...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			release := sync.OnceFunc(func() { close(worker.open) })
			t.Cleanup(release)
			mustSet(t, c, 0)
			mustSet(t, c, 1)
			worker.awaitEntered(t)
			mustSet(t, c, 2) // stage a second removal while the first listener call is blocked

			done, returned := call(func() error { return c.Close() })
			if stackWith(t, returned, inWaitWorkers) == "" {
				t.Fatal("Close returned while the notify worker was held in a listener call")
			}
			if overlapped.Load() {
				t.Error("Close called the listener while the notify worker was in a listener call")
			}
			release()
			if err := wait(t, done, "Close"); err != nil {
				t.Fatal(err)
			}
			if got, want := log.snapshot(), capacityRemovals(tc.remove, tc.evict, 0, 1); !slices.Equal(got, want) {
				t.Fatalf("listener calls = %+v, want %+v", got, want)
			}
		})
	}
}

func TestOnRemove_RemovalsRacingClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rounds int
		cfg    Config
		race   func(c *Cache[int, string], wg *sync.WaitGroup)
	}{
		{
			name:   "set-async",
			rounds: 1000,
			cfg:    Config{MaxSize: 64, ShardCount: 2, EvictionPolicy: LRU, StatsEnabled: true},
			race: func(c *Cache[int, string], wg *sync.WaitGroup) {
				for g := range 4 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for i := range 256 {
							_ = c.SetAsync(g<<24|i, "v", NoExpiration)
						}
					}()
				}
			},
		},
		{
			name:   "expiry",
			rounds: 100,
			cfg:    Config{MaxSize: 4096, ShardCount: 8, EvictionPolicy: LRU, StatsEnabled: true},
			race: func(c *Cache[int, string], wg *sync.WaitGroup) {
				const n = 2000
				for k := range n {
					_ = c.Set(k, "v", time.Millisecond)
				}
				time.Sleep(2 * time.Millisecond)
				wg.Add(2)
				go func() { defer wg.Done(); c.Cleanup() }()
				go func() {
					defer wg.Done()
					for k := n - 1; k >= 0; k-- {
						c.Get(k)
					}
				}()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rounds := tc.rounds
			if testing.Short() {
				rounds /= 10
			}
			for round := range rounds {
				var capacity, expired atomic.Int64
				var active atomic.Int32
				var overlapped, returned, late atomic.Bool
				c, err := New(tc.cfg, WithOnRemove(func(_ int, _ string, reason RemovalReason) {
					if active.Add(1) > 1 {
						overlapped.Store(true)
					}
					defer active.Add(-1)
					if returned.Load() {
						late.Store(true)
					}
					switch reason {
					case RemovedCapacity:
						capacity.Add(1)
					case RemovedExpired:
						expired.Add(1)
					}
				}))
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				var wg sync.WaitGroup
				tc.race(c, &wg)
				time.Sleep(time.Duration(round%100) * time.Microsecond)
				c.Close()
				returned.Store(true)
				wg.Wait()

				// Stats count removals independently of listener delivery.
				st := c.Stats()
				if got := capacity.Load(); got != st.Evictions {
					t.Fatalf("round %d: %d capacity removals reached the listener, want %d", round, got, st.Evictions)
				}
				if got := expired.Load(); got != st.Expirations {
					t.Fatalf("round %d: %d expirations reached the listener, want %d", round, got, st.Expirations)
				}
				if overlapped.Load() {
					t.Fatalf("round %d: two listener calls overlapped", round)
				}
				if late.Load() {
					t.Fatalf("round %d: a listener call came after Close returned", round)
				}
			}
		})
	}
}

func TestOnRemove_ListenerReentersCache(t *testing.T) {
	const n = 200
	rec := newEvictRecorder()
	var c *Cache[int, string]
	var planned atomic.Int64 // keys the listener has decided to insert
	listener := func(k int, v string, reason RemovalReason) {
		// Keys from n up were inserted by the listener, which leaves them alone.
		if k < n {
			planned.Add(2)
			c.Get(k)
			_ = c.Set(n+k, valFor(n+k), NoExpiration)
			_ = c.SetAsync(2*n+k, valFor(2*n+k), NoExpiration)
			c.Delete(n + k)
			_ = c.Sync()
		}
		rec.record(k, v, reason)
	}
	c, err := New(Config{MaxSize: 8, ShardCount: 2, EvictionPolicy: LRU}, WithOnRemove(listener))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runWithin(t, "Set", func() {
		for k := range n {
			if err := c.Set(k, valFor(k), NoExpiration); err != nil {
				t.Errorf("Set(%d): %v", k, err)
				return
			}
		}
	})
	waitFor(t, func() bool {
		return int64(rec.count())+c.Size() == n+planned.Load()
	})
	runWithin(t, "Close", func() { c.Close() })

	if rec.repeats != 0 {
		t.Fatalf("got %d duplicate notifications", rec.repeats)
	}
	for k := range 3 * n {
		if v, ok := rec.value(k); ok && v != valFor(k) {
			t.Fatalf("key %d carried value %q, want %q", k, v, valFor(k))
		}
	}
}

func TestOnRemove_ListenerMayTakeCallerLock(t *testing.T) {
	const total = 200
	var mu sync.Mutex
	rec := newEvictRecorder()
	c, err := New(
		Config{MaxSize: 8, ShardCount: 2, EvictionPolicy: LRU},
		WithOnRemove(func(k int, v string, reason RemovalReason) {
			mu.Lock()
			defer mu.Unlock()
			rec.record(k, v, reason)
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runWithin(t, "Set while holding the listener's lock", func() {
		mu.Lock()
		defer mu.Unlock()
		for k := range total {
			if err := c.Set(k, valFor(k), NoExpiration); err != nil {
				t.Errorf("Set(%d): %v", k, err)
				return
			}
		}
	})
	waitFor(t, func() bool { return int64(rec.count())+c.Size() == total })
	runWithin(t, "Close", func() { c.Close() })
	if rec.repeats != 0 {
		t.Fatalf("got %d duplicate notifications", rec.repeats)
	}
}

// A panicking listener must crash the process, so this runs in a child process.
func TestOnRemove_ListenerPanicCrashes(t *testing.T) {
	const marker = "listener called after the panic"
	if os.Getenv("KIOSHUN_LISTENER_PANIC_CHILD") == "1" {
		staged := make(chan struct{})
		first := true
		c, err := New(Config{MaxSize: 4, ShardCount: 4, EvictionPolicy: LRU},
			WithOnRemove(func(int, string, RemovalReason) {
				if first {
					first = false
					<-staged // more removals are staged while this call runs
					panic("listener bug")
				}
				fmt.Println(marker)
			}))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		for k := range 200 {
			mustSet(t, c, k)
		}
		close(staged)
		time.Sleep(5 * time.Second) // the panic ends the process long before this
		return
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOnRemove_ListenerPanicCrashes$")
	cmd.Env = append(os.Environ(), "KIOSHUN_LISTENER_PANIC_CHILD=1")
	out, err := cmd.CombinedOutput()
	switch {
	case ctx.Err() != nil:
		t.Fatalf("child still running after 10s:\n%s", out)
	case err == nil:
		t.Fatalf("child exited cleanly after the listener panicked:\n%s", out)
	case bytes.Contains(out, []byte(marker)):
		t.Fatalf("the worker made %d listener calls after a listener panicked", bytes.Count(out, []byte(marker)))
	case !bytes.Contains(out, []byte("panic: listener bug")):
		t.Fatalf("child failed without the listener panic: %v\n%s", err, out)
	}
}

func TestOnRemove_ClearKeepsEarlierRemovals(t *testing.T) {
	gate := newListenerGate()
	var log removalLog
	c, err := New(
		Config{MaxSize: 10, ShardCount: 1, EvictionPolicy: LRU},
		WithOnRemove(func(k int, v string, reason RemovalReason) {
			gate.wait()
			log.onRemove(k, v, reason)
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	// Set(10) evicts key 0 and holds the worker.
	for k := range 11 {
		mustSet(t, c, k)
	}
	gate.awaitEntered(t)
	// Keys 1 to 19 stay staged while the worker is held.
	for k := 11; k < 30; k++ {
		mustSet(t, c, k)
	}
	c.Clear()
	close(gate.open)
	c.Close()

	var want []removalEvent
	for k := range 20 {
		want = append(want, removalEvent{key: k, value: valFor(k), reason: RemovedCapacity})
	}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("listener calls = %+v, want %+v", got, want)
	}
}

func TestOnRemove_StagingMemoryAfterBurst(t *testing.T) {
	const burst, total = 100_000, 101_000
	gate := newListenerGate()
	delivered := make([]atomic.Bool, total)
	cleaned := make([]atomic.Bool, total)
	var n atomic.Int64
	c, err := New(
		Config{MaxSize: 64, ShardCount: 4, EvictionPolicy: LRU},
		WithOnRemove(func(k int, _ *[64]byte, _ RemovalReason) {
			gate.wait()
			delivered[k].Store(true)
			n.Add(1)
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	set := func(k int) {
		v := new([64]byte)
		runtime.AddCleanup(v, func(k int) { cleaned[k].Store(true) }, k)
		if err := c.Set(k, v, NoExpiration); err != nil {
			t.Fatalf("Set(%d): %v", k, err)
		}
	}
	// The burst piles up while the worker is held.
	for k := range burst {
		set(k)
	}
	close(gate.open)
	waitFor(t, func() bool { return n.Load()+c.Size() == burst })
	// Then a trickle of removals is delivered in small passes.
	for k := burst; k < total; k++ {
		set(k)
	}
	waitFor(t, func() bool { return n.Load()+c.Size() == total })

	for i, s := range c.shards {
		s.mu.Lock()
		got := cap(s.removeBuf)
		s.mu.Unlock()
		if got > 256 {
			t.Errorf("shard %d keeps a staging buffer with capacity %d, want <= 256", i, got)
		}
	}

	// Delivered values must not stay reachable from a reused buffer.
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		reachable := 0
		for k := range total {
			if delivered[k].Load() && !cleaned[k].Load() {
				reachable++
			}
		}
		if reachable == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d delivered values are still reachable after GC", reachable)
		}
		time.Sleep(time.Millisecond)
	}
}

// Like TestOnRemove_ChurnAllocs, but it also runs under -race.
func TestOnRemove_ReusesStagingBuffer(t *testing.T) {
	delivered := make(chan struct{}, 3)
	c, err := New(Config{MaxSize: 1, ShardCount: 1, EvictionPolicy: LRU},
		WithOnRemove(func(int, string, RemovalReason) { delivered <- struct{}{} }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	deadline := time.After(10 * time.Second)
	mustSet(t, c, 0)
	for k := 1; k <= 3; k++ {
		mustSet(t, c, k) // evicts k-1
		select {
		case <-delivered:
		case <-deadline:
			t.Fatalf("removal of key %d not delivered within 10s", k-1)
		}
	}
	// From the second pass on, the shard gets the previous pass's buffer.
	s := c.shards[0]
	s.mu.Lock()
	got := cap(s.removeBuf)
	s.mu.Unlock()
	if got == 0 {
		t.Fatal("the shard has no staging buffer after three passes, want a reused one")
	}
}

func TestOnRemove_KeepsStagingBufferUpToLimit(t *testing.T) {
	const burst = 100 // grows the buffer to between 100 and 256
	gate := newListenerGate()
	delivered := make(chan int, burst+2)
	c, err := New(Config{MaxSize: 1, ShardCount: 1, EvictionPolicy: LRU},
		WithOnRemove(func(k int, _ string, _ RemovalReason) {
			if k == 0 {
				gate.wait()
			}
			delivered <- k
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	deadline := time.After(10 * time.Second)
	await := func(key int) {
		t.Helper()
		for {
			select {
			case k := <-delivered:
				if k == key {
					return
				}
			case <-deadline:
				t.Fatalf("removal of key %d not delivered within 10s", key)
			}
		}
	}

	mustSet(t, c, 0)
	mustSet(t, c, 1) // evicts 0 and holds the worker
	gate.awaitEntered(t)
	for k := 2; k <= burst+1; k++ {
		mustSet(t, c, k) // evicts k-1
	}
	close(gate.open)
	await(burst)
	// The next pass hands the burst's buffer back to the shard.
	mustSet(t, c, burst+2) // evicts burst+1
	await(burst + 1)

	s := c.shards[0]
	s.mu.Lock()
	got := cap(s.removeBuf)
	s.mu.Unlock()
	if got < burst || got > 256 {
		t.Fatalf("the shard's staging buffer has capacity %d, want the burst's buffer of %d to 256 entries", got, burst)
	}
}

func TestOnRemove_ConcurrentExactlyOnce(t *testing.T) {
	const writers, perWriter = 8, 2000
	const inserted = writers * perWriter
	for _, policy := range []EvictionPolicy{LRU, LFU, FIFO, SieveTinyLFU} {
		t.Run(policyName(policy), func(t *testing.T) {
			rec := newEvictRecorder()
			c, err := New(
				Config{MaxSize: 256, ShardCount: 8, EvictionPolicy: policy},
				WithOnRemove(rec.record),
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer c.Close()

			var wg sync.WaitGroup
			for w := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range perWriter {
						k := w*perWriter + i
						var err error
						switch i % 3 {
						case 0:
							err = c.Set(k, valFor(k), NoExpiration)
						case 1:
							err = c.SetAsync(k, valFor(k), NoExpiration)
						case 2:
							err = c.Set(k, valFor(k), NoExpiration)
							c.Delete(k)
						}
						if err != nil {
							t.Errorf("write %d: %v", k, err)
							return
						}
					}
				}()
			}
			wg.Wait()
			if err := c.Sync(); err != nil {
				t.Fatalf("Sync: %v", err)
			}

			// Close delivers every staged removal, so no repeat can arrive later.
			size := c.Size()
			c.Close()
			if got := int64(rec.count()) + size; got != inserted {
				t.Fatalf("delivered %d + resident %d = %d, want %d inserted", rec.count(), size, got, inserted)
			}
			if rec.repeats != 0 {
				t.Fatalf("got %d duplicate notifications", rec.repeats)
			}
			for k := range inserted {
				if v, ok := rec.value(k); ok && v != valFor(k) {
					t.Fatalf("key %d carried value %q, want %q", k, v, valFor(k))
				}
			}
		})
	}
}

// Every removal must be delivered with no later write or Close to flush it.
func TestOnRemove_ConcurrentDeliveryWithoutClose(t *testing.T) {
	const producers, perRound, rounds, shards = 6, 16, 1500, 16
	v := &churnValue{}
	var calls atomic.Int64
	var c *Cache[int, *churnValue]
	c, err := New(Config{MaxSize: shards, ShardCount: shards, EvictionPolicy: LRU},
		WithOnRemove(func(k int, _ *churnValue, _ RemovalReason) {
			calls.Add(1)
			if k%61 == 0 {
				c.Delete(k + 1)
			}
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	var inserted int64
	for r := range rounds {
		var wg sync.WaitGroup
		for p := range producers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range perRound {
					k := (r*producers+p)*perRound + i
					if err := c.Set(k, v, NoExpiration); err != nil {
						t.Errorf("Set(%d): %v", k, err)
						return
					}
				}
			}()
		}
		wg.Wait()
		inserted += producers * perRound
		deadline := time.Now().Add(2 * time.Second)
		for calls.Load()+c.Size() != inserted {
			if time.Now().After(deadline) {
				t.Fatalf("round %d: %d listener calls and %d resident after 2s, want %d inserted",
					r, calls.Load(), c.Size(), inserted)
			}
			runtime.Gosched()
		}
	}
}

func TestOnRemove_DeliveryOrder(t *testing.T) {
	const n = 1000
	gate := newListenerGate()
	var log removalLog
	c, err := New(
		Config{MaxSize: 1, ShardCount: 1, EvictionPolicy: LRU},
		WithOnRemove(func(k int, v string, reason RemovalReason) {
			gate.wait()
			log.onRemove(k, v, reason)
		}),
		WithOnEvict(log.onEvict),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	for k := range n {
		mustSet(t, c, k)
	}
	close(gate.open)
	c.Close()

	got := log.snapshot()
	if len(got) != 2*(n-1) {
		t.Fatalf("got %d listener calls, want %d", len(got), 2*(n-1))
	}
	for k := range n - 1 {
		remove := removalEvent{key: k, value: valFor(k), reason: RemovedCapacity}
		evict := remove
		evict.evict = true
		if got[2*k] != remove || got[2*k+1] != evict {
			t.Fatalf("calls %d and %d = %+v, %+v, want %+v, %+v", 2*k, 2*k+1, got[2*k], got[2*k+1], remove, evict)
		}
	}
}

// churnValue stands in for a cached response in the churn benchmarks.
type churnValue struct{ _ [64]byte }

// newChurnCache returns a SieveTinyLFU cache with 1024 entries, so once it is
// full every Set of a new key removes one entry.
func newChurnCache(tb testing.TB, opts ...Option[string, *churnValue]) *Cache[string, *churnValue] {
	tb.Helper()
	cfg := DefaultConfig()
	cfg.MaxSize = 1024
	c, err := New(cfg, opts...)
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	tb.Cleanup(func() { c.Close() })
	return c
}

var churnListeners = []struct {
	name string
	opts []Option[string, *churnValue]
}{
	{"listener=none", nil},
	{"listener=noop", []Option[string, *churnValue]{
		WithOnRemove(func(string, *churnValue, RemovalReason) {}),
	}},
}

func BenchmarkRemovalListenerChurn(b *testing.B) {
	for _, tc := range churnListeners {
		b.Run(tc.name, func(b *testing.B) {
			c := newChurnCache(b, tc.opts...)
			v := &churnValue{}
			i := 0
			for b.Loop() {
				i++
				if err := c.Set(strconv.Itoa(i), v, time.Minute); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkRemovalListenerChurnParallel(b *testing.B) {
	for _, tc := range churnListeners {
		b.Run(tc.name, func(b *testing.B) {
			c := newChurnCache(b, tc.opts...)
			v := &churnValue{}
			var worker atomic.Int64
			b.RunParallel(func(pb *testing.PB) {
				// Each goroutine writes its own key range.
				for i := worker.Add(1) << 32; pb.Next(); i++ {
					if err := c.Set(strconv.FormatInt(i, 10), v, time.Minute); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}
