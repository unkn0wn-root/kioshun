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
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	const total = 10_000
	cfg := Config{MaxSize: 16, ShardCount: 4, EvictionPolicy: LRU}

	t.Run("worker_blocked", func(t *testing.T) {
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
			t.Fatalf("delivered %d removals when Close returned, want %d", got, want)
		}
	})
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
	delivered := make(chan struct{})
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
