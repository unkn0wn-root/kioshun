package kioshun

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"
)

type cbPayload struct{ _ [1 << 10]byte }

func newCallbackCache(t *testing.T, shards int) *Cache[int, *cbPayload] {
	t.Helper()
	return newTestCache[int, *cbPayload](t, Config{MaxSize: 1 << 16, ShardCount: shards, EvictionPolicy: LRU})
}

func noCallback(int, *cbPayload) {}

// heapEntry wraps a task for tests that build a shard's heap directly.
func heapEntry[K comparable, V any](task callbackTask[K, V]) callbackEntry[K, V] {
	return callbackEntry[K, V]{expireTime: task.expireTime, task: &task}
}

// tickCache is a one-shard cache whose entries and callbacks are set straight on
// the shard with exact deadlines. It counts how often each callback runs and how
// many got a value other than their key.
type tickCache struct {
	*Cache[int, int]
	calls      []atomic.Int32
	ran        atomic.Int32
	wrongValue atomic.Int32
	hook       func(k int)
}

// newTickCache returns a tickCache for keys below n. Each callback calls hook
// with its key, unless hook is nil.
func newTickCache(t *testing.T, n int, hook func(k int)) *tickCache {
	t.Helper()
	c := newTestCache[int, int](t, Config{MaxSize: 2 * int64(n), ShardCount: 1, EvictionPolicy: LRU})
	t.Cleanup(func() { c.Close() })
	return &tickCache{Cache: c, calls: make([]atomic.Int32, n), hook: hook}
}

func (tc *tickCache) put(k int, deadline int64) {
	s := tc.shards[0]
	cmd := writeCommand[int, int]{op: writeSet, key: k, value: k, hash: tc.hasher.Sum(k), expireTime: deadline}
	s.mu.Lock()
	tc.applySet(s, &cmd)
	s.mu.Unlock()
	tc.scheduleCallback(s, callbackTask[int, int]{key: k, value: k, expireTime: deadline, callback: func(k, v int) {
		if v != k {
			tc.wrongValue.Add(1)
		}
		tc.calls[k].Add(1)
		tc.ran.Add(1)
		if tc.hook != nil {
			tc.hook(k)
		}
	}})
}

// checkOnce waits for the callbacks, then fails unless each ran exactly once
// with its own value.
func (tc *tickCache) checkOnce(t *testing.T) {
	t.Helper()
	n := int32(len(tc.calls))
	for deadline := time.Now().Add(5 * time.Second); tc.ran.Load() < n && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	for k := range tc.calls {
		if got := tc.calls[k].Load(); got != 1 {
			t.Fatalf("callback %d ran %d times, want 1 (%d of %d ran)", k, got, tc.ran.Load(), n)
		}
	}
	if w := tc.wrongValue.Load(); w != 0 {
		t.Fatalf("%d of %d callbacks got a value other than the one set with their key", w, n)
	}
}

// A TTL this short has passed before the timer starts, so the timer fires at once.
func TestSetWithCallbackElapsedTTLRunsOnce(t *testing.T) {
	const n = 10000
	c := newCallbackCache(t, 4)
	defer c.Close()

	weaks := make([]weak.Pointer[cbPayload], n)
	calls := make([]atomic.Int32, n)
	var ran atomic.Int32
	done := make(chan struct{})
	for k := range n {
		v := new(cbPayload)
		weaks[k] = weak.Make(v)
		err := c.SetWithCallback(k, v, time.Nanosecond, func(k int, _ *cbPayload) {
			calls[k].Add(1)
			if ran.Add(1) == n {
				close(done)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%d of %d callbacks ran within 5s", ran.Load(), n)
	}
	time.Sleep(10 * time.Millisecond)
	for k := range calls {
		if got := calls[k].Load(); got != 1 {
			t.Fatalf("callback for %d ran %d times, want 1", k, got)
		}
	}

	// A fired timer must not keep its value while the cache stays open.
	c.Cleanup()
	runtime.GC()
	for k, w := range weaks {
		if w.Value() != nil {
			t.Fatalf("value %d still reachable after its callback ran and Cleanup removed it", k)
		}
	}
}

// A 1ms ttl makes timers fire while keys are still being set, so many deadlines
// fall right at the instant a timer fires.
func TestSetWithCallbackRunsEachOnceAfterTTL(t *testing.T) {
	const (
		n   = 100_000
		ttl = time.Millisecond
	)
	c, err := New[int, int](Config{MaxSize: 2 * n, ShardCount: 256, EvictionPolicy: LRU})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	calls := make([]atomic.Int32, n)
	set := make([]time.Time, n)
	var ran, early atomic.Int64
	done := make(chan struct{})
	for k := range n {
		set[k] = time.Now()
		err := c.SetWithCallback(k, k, ttl, func(k, _ int) {
			if time.Since(set[k]) < ttl {
				early.Add(1)
			}
			calls[k].Add(1)
			if ran.Add(1) == n {
				close(done)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
		time.Sleep(20 * time.Millisecond)
	case <-time.After(5 * time.Second):
	}
	var missing, dup int
	for k := range calls {
		switch calls[k].Load() {
		case 0:
			missing++
		case 1:
		default:
			dup++
		}
	}
	if missing != 0 || dup != 0 || early.Load() != 0 {
		t.Fatalf("of %d callbacks, %d never ran, %d ran more than once and %d ran before their ttl", n, missing, dup, early.Load())
	}
}

func TestCallbacksRunAfterTheirTTL(t *testing.T) {
	const n = 200
	c := newCallbackCache(t, 4)
	defer c.Close()
	var wg sync.WaitGroup
	wg.Add(n)
	for k := range n {
		ttl := time.Duration(20-k%20) * time.Millisecond
		start := time.Now()
		err := c.SetWithCallback(k, new(cbPayload), ttl, func(int, *cbPayload) {
			if d := time.Since(start); d < ttl {
				t.Errorf("callback ran %v after Set, before its ttl of %v", d, ttl)
			}
			wg.Done()
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	runWithin(t, "callbacks with mixed ttls", wg.Wait)
}

func TestShortCallbackNotHeldByLongerOne(t *testing.T) {
	c := newCallbackCache(t, 1)
	defer c.Close()
	if err := c.SetWithCallback(1, new(cbPayload), time.Hour, noCallback); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	if err := c.SetWithCallback(2, new(cbPayload), time.Millisecond, func(int, *cbPayload) { close(done) }); err != nil {
		t.Fatal(err)
	}
	if err := c.SetWithCallback(3, new(cbPayload), time.Hour, noCallback); err != nil {
		t.Fatal(err)
	}
	runWithin(t, "a 1ms callback scheduled between two 1h ones", func() { <-done })
}

// A callback runs only if its entry is still present and expired. Without
// spacing the four keys share one deadline, so one wake takes them as a batch.
// Spaced deadlines give each key a wake of its own.
func TestCallbackSkippedForDeletedOrRenewedKey(t *testing.T) {
	for _, spacing := range []time.Duration{0, 15 * time.Millisecond} {
		t.Run(fmt.Sprintf("spacing=%v", spacing), func(t *testing.T) {
			done := make(chan struct{})
			tc := newTickCache(t, 4, func(k int) {
				if k == 3 {
					close(done)
				}
			})
			d := tc.nowNano() + int64(20*time.Millisecond)
			for k := range 4 {
				tc.put(k, d+int64(k)*int64(spacing))
			}
			tc.Delete(0)
			if err := tc.Set(1, 1, time.Hour); err != nil {
				t.Fatal(err)
			}
			if err := tc.Set(2, 2, NoExpiration); err != nil {
				t.Fatal(err)
			}
			runWithin(t, "the callback of a key left alone", func() { <-done })
			// Other callbacks of a batch may run just after key 3's, so give them a moment.
			time.Sleep(10 * time.Millisecond)
			if a, b, c := tc.calls[0].Load(), tc.calls[1].Load(), tc.calls[2].Load(); a+b+c != 0 {
				t.Fatalf("callbacks ran for a deleted key (%d), a key renewed for 1h (%d) or a key renewed without expiry (%d)", a, b, c)
			}
		})
	}
}

// A ttl at the int64 limit saturates instead of wrapping into the past. Behind
// a short callback it arms the timer for the far future. Alone it starts no
// timer, and Close must still drop it.
func TestSetWithCallbackMaxTTL(t *testing.T) {
	c := newCallbackCache(t, 1)
	defer c.Close()
	var ran atomic.Bool
	if err := c.SetWithCallback(1, new(cbPayload), time.Duration(math.MaxInt64), func(int, *cbPayload) { ran.Store(true) }); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	if err := c.SetWithCallback(2, new(cbPayload), time.Millisecond, func(int, *cbPayload) { close(done) }); err != nil {
		t.Fatal(err)
	}
	runWithin(t, "a 1ms callback ahead of one at the int64 limit", func() { <-done })
	ct := c.shards[0].callbacks
	ct.mu.Lock()
	pending, when := len(ct.heap), ct.when
	ct.mu.Unlock()
	if pending != 1 {
		t.Fatalf("after the 1ms callback, %d callbacks pending, want 1", pending)
	}
	if now := c.nowNano(); when-now < int64(100*365*24*time.Hour) {
		t.Fatalf("after the 1ms callback, the timer is set %v from now, want over 100 years", time.Duration(when-now))
	}
	if _, ok := c.Get(1); !ok {
		t.Fatal("entry with a ttl at the int64 limit is already expired")
	}
	time.Sleep(20 * time.Millisecond)
	if ran.Load() {
		t.Fatal("callback with a ttl at the int64 limit ran")
	}

	alone := newCallbackCache(t, 1)
	if err := alone.SetWithCallback(1, new(cbPayload), time.Duration(math.MaxInt64), noCallback); err != nil {
		t.Fatal(err)
	}
	ct = alone.shards[0].callbacks
	ct.mu.Lock()
	pending, started := len(ct.heap), ct.timer != nil
	ct.mu.Unlock()
	if pending != 1 || started {
		t.Fatalf("alone, %d pending and timer started %v, want 1 and false", pending, started)
	}
	if err := alone.Close(); err != nil {
		t.Fatal(err)
	}
	ct.mu.Lock()
	pending = len(ct.heap)
	ct.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d callbacks pending after Close with no timer started", pending)
	}
}

// Each due callback waits for all the others, so they must all run at once.
// There are more of them than one fire takes.
func TestBlockedCallbackDoesNotDelayOthers(t *testing.T) {
	const n = 40
	c := newCallbackCache(t, 1)
	defer c.Close()
	release := make(chan struct{})
	defer close(release)
	var entered sync.WaitGroup
	entered.Add(n)
	all := make(chan struct{})
	go func() { entered.Wait(); close(all) }()
	// Shrinking ttls put every deadline at about the same instant, so one wake finds them all due.
	start := time.Now()
	for k := range n {
		err := c.SetWithCallback(k, new(cbPayload), 20*time.Millisecond-time.Since(start), func(int, *cbPayload) {
			entered.Done()
			select {
			case <-all:
			case <-release:
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	runWithin(t, "due callbacks that each wait for all the others", func() { <-all })
}

// A lone callback that never returns must not hold back later ones.
func TestBlockedLoneCallbackDoesNotDelayLaterOnes(t *testing.T) {
	c := newCallbackCache(t, 1)
	defer c.Close()
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{})
	if err := c.SetWithCallback(1, new(cbPayload), time.Millisecond, func(int, *cbPayload) {
		close(entered)
		<-release
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	if err := c.SetWithCallback(2, new(cbPayload), 50*time.Millisecond, func(int, *cbPayload) { close(done) }); err != nil {
		t.Fatal(err)
	}
	runWithin(t, "the blocking callback", func() { <-entered })
	runWithin(t, "a callback due after one that blocks", func() { <-done })
}

// Two callbacks share one deadline, so they run as a batch, and neither returns.
// They must not keep a later callback from running or a Set on their shard
// from finishing.
func TestBlockedBatchDoesNotDelayLaterCallbacksOrWriters(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 2)
	done := make(chan struct{})
	tc := newTickCache(t, 3, func(k int) {
		if k == 2 {
			close(done)
			return
		}
		entered <- struct{}{}
		<-release
	})
	d := tc.nowNano() + int64(20*time.Millisecond)
	tc.put(0, d)
	tc.put(1, d)
	tc.put(2, d+int64(50*time.Millisecond))
	runWithin(t, "the blocking batch", func() {
		<-entered
		<-entered
	})
	runWithin(t, "a callback due after a batch that blocks", func() { <-done })
	var err error
	runWithin(t, "a Set while a batch blocks", func() { err = tc.Set(3, 3, time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
}

func TestCallbackCanCallSetWithCallbackAndClose(t *testing.T) {
	c := newCallbackCache(t, 1)
	done := make(chan struct{})
	err := c.SetWithCallback(1, new(cbPayload), time.Nanosecond, func(int, *cbPayload) {
		defer close(done)
		if err := c.SetWithCallback(2, new(cbPayload), time.Hour, noCallback); err != nil {
			t.Errorf("SetWithCallback inside a callback: %v", err)
		}
		c.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	runWithin(t, "a callback that calls SetWithCallback and Close", func() { <-done })
	if err := c.Set(3, new(cbPayload), time.Hour); !errors.Is(err, ErrCacheClosed) {
		t.Fatalf("Set after a callback closed the cache: %v, want ErrCacheClosed", err)
	}
}

func TestClosePendingCallbackKeepsNothing(t *testing.T) {
	c := newCallbackCache(t, 4)
	v := new(cbPayload)
	wv, wc := weak.Make(v), weak.Make(c)
	if err := c.SetWithCallback(1, v, time.Hour, noCallback); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c = nil
	runtime.GC()
	if wv.Value() != nil {
		t.Error("value of a pending callback still reachable after Close")
	}
	// Close uses the cache's sync.Pool, which stays reachable until the next cycle starts.
	runtime.GC()
	if wc.Value() != nil {
		t.Error("cache still reachable after Close with a pending callback")
	}
}

// Live timers before and after keep the stopped timer inside the runtime's heap.
func TestCloseStoppedTimerKeepsNothing(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	early := time.AfterFunc(30*time.Minute, func() {})
	defer early.Stop()

	c := newCallbackCache(t, 1)
	v := new(cbPayload)
	wv, wc := weak.Make(v), weak.Make(c)
	if err := c.SetWithCallback(1, v, time.Hour, noCallback); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		late := time.AfterFunc(2*time.Hour, func() {})
		defer late.Stop()
	}
	c.Close()
	c = nil
	runtime.GC()
	if wv.Value() != nil {
		t.Error("value reachable through a stopped timer after Close")
	}
	runtime.GC()
	if wc.Value() != nil {
		t.Error("cache reachable through a stopped timer after Close")
	}
}

// Close keeps nothing of the pending callbacks.
// With one P the runtime drops stopped timers at its next check, so only live ones stay.
func TestCloseReleasesPendingCallbacks(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	const n = 100_000
	heap := func() int64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return int64(m.HeapAlloc)
	}
	before := heap()
	c, err := New[int, int](Config{MaxSize: 1 << 18, ShardCount: 4, EvictionPolicy: LRU})
	if err != nil {
		t.Fatal(err)
	}
	for k := range n {
		if err := c.SetWithCallback(k, k, time.Hour, func(int, int) {}); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	c = nil
	if held := (heap() - before) / n; held > 64 {
		t.Fatalf("%d bytes per callback still held after Close, want at most 64", held)
	}
}

// Close must stop the shard's timer, also after it was re-armed for an earlier deadline.
func TestCloseStopsShardTimer(t *testing.T) {
	c := newCallbackCache(t, 1)
	if err := c.SetWithCallback(1, new(cbPayload), time.Hour, noCallback); err != nil {
		t.Fatal(err)
	}
	ct := c.shards[0].callbacks
	ct.mu.Lock()
	timer := ct.timer
	ct.mu.Unlock()
	if err := c.SetWithCallback(2, new(cbPayload), time.Minute, noCallback); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if timer.Stop() {
		t.Fatal("shard timer still scheduled after Close")
	}
}

func TestCloseReturnsWhileCallbackRuns(t *testing.T) {
	c := newCallbackCache(t, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	err := c.SetWithCallback(1, new(cbPayload), time.Nanosecond, func(int, *cbPayload) {
		close(entered)
		<-release
	})
	if err != nil {
		t.Fatal(err)
	}
	runWithin(t, "the callback start", func() { <-entered })
	runWithin(t, "Close while a callback runs", func() { c.Close() })
}

func TestCloseTwiceWithPendingCallbacks(t *testing.T) {
	for round := range 20 {
		c := newCallbackCache(t, 4)
		v := new(cbPayload)
		wv := weak.Make(v)
		var called atomic.Bool
		if err := c.SetWithCallback(1, v, 50*time.Millisecond, func(int, *cbPayload) { called.Store(true) }); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := c.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
				runtime.GC()
				if wv.Value() != nil {
					t.Errorf("round %d: value reachable after a concurrent Close returned", round)
				}
			}()
		}
		wg.Wait()
		if err := c.Close(); err != nil {
			t.Fatalf("third Close: %v", err)
		}
		if round == 0 {
			time.Sleep(100 * time.Millisecond)
			if called.Load() {
				t.Fatal("callback ran after Close")
			}
		}
	}
}

// Odd keys expire at once, so their timers fire while Close runs. With one
// shard and several Ps, writers queue on its callback lock while Close runs.
func TestSetWithCallbackRacingCloseKeepsNothing(t *testing.T) {
	const writers = 8
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(max(runtime.GOMAXPROCS(0), 4)))
	short := new(cbPayload)
	for round := range 50 {
		c := newCallbackCache(t, 1)
		var mu sync.Mutex
		var weaks []weak.Pointer[cbPayload]
		var wg sync.WaitGroup
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := w; ; k += writers {
					v, ttl := short, time.Microsecond
					if k%2 == 0 {
						v, ttl = new(cbPayload), time.Hour
						mu.Lock()
						weaks = append(weaks, weak.Make(v))
						mu.Unlock()
					}
					if err := c.SetWithCallback(k, v, ttl, noCallback); err != nil {
						if !errors.Is(err, ErrCacheClosed) {
							t.Errorf("SetWithCallback: %v", err)
						}
						return
					}
				}
			}()
		}
		time.Sleep(time.Millisecond)
		c.Close()
		wg.Wait()
		// No task may reach the heap once Close has dropped it.
		ct := c.shards[0].callbacks
		ct.mu.Lock()
		pending := len(ct.heap)
		ct.mu.Unlock()
		if pending > 0 {
			t.Fatalf("round %d: %d callbacks pending after Close", round, pending)
		}
		c = nil
		// Retry the GC briefly, so that a value held for a moment by work that is
		// still finishing is not taken for a leak.
		left := len(weaks)
		for try := 0; try < 3 && left > 0; try++ {
			if try > 0 {
				time.Sleep(time.Millisecond)
			}
			runtime.GC()
			left = 0
			for _, w := range weaks {
				if w.Value() != nil {
					left++
				}
			}
		}
		if left > 0 {
			t.Fatalf("round %d: %d of %d values still reachable after Close and 3 GCs", round, left, len(weaks))
		}
	}
}

// A SetWithCallback that commits before Close can schedule after Close returns.
func TestScheduleCallbackAfterCloseKeepsNothing(t *testing.T) {
	c := newCallbackCache(t, 1)
	c.Close()
	v := new(cbPayload)
	wv := weak.Make(v)
	c.scheduleCallback(c.shards[0], callbackTask[int, *cbPayload]{
		key:        1,
		value:      v,
		expireTime: c.nowNano() + int64(time.Hour),
		callback:   noCallback,
	})
	runtime.GC()
	if wv.Value() != nil {
		t.Fatal("callback scheduled after Close keeps its value")
	}
}

// The flag is set without Close, so the timer stays scheduled and fires.
func TestCallbackNotRunAfterClosedFlag(t *testing.T) {
	c := newCallbackCache(t, 1)
	defer c.Close()
	var calls atomic.Int32
	cb := func(int, *cbPayload) { calls.Add(1) }
	if err := c.SetWithCallback(1, new(cbPayload), time.Millisecond, cb); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); calls.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("callback did not run within 5s of its ttl")
		}
		time.Sleep(time.Millisecond)
	}
	// Keys 2 and 3 share a wake, and key 4 has one of its own.
	start := time.Now()
	for i, ttl := range []time.Duration{200 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond} {
		if err := c.SetWithCallback(2+i, new(cbPayload), ttl-time.Since(start), cb); err != nil {
			t.Fatal(err)
		}
	}
	c.closed.Store(true)
	time.Sleep(500 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("a callback ran after the closed flag was set (%d calls)", got)
	}
}

// A wake before the earliest deadline must re-arm the timer, not take the task early.
func TestEarlyWakeRearmsTimer(t *testing.T) {
	c := newCallbackCache(t, 1)
	defer c.Close()
	done := make(chan struct{})
	if err := c.SetWithCallback(1, new(cbPayload), 50*time.Millisecond, func(int, *cbPayload) { close(done) }); err != nil {
		t.Fatal(err)
	}
	ct := c.shards[0].callbacks
	ct.mu.Lock()
	ct.timer.Stop()
	deadline := ct.heap[0].expireTime
	ct.mu.Unlock()
	ct.fire()
	ct.mu.Lock()
	left := len(ct.heap)
	ct.mu.Unlock()
	if c.nowNano() <= deadline && left != 1 {
		t.Fatal("a wake before the deadline took the task")
	}
	runWithin(t, "a callback after an early wake", func() { <-done })
}

// A far deadline is armed a little early. A later task due after the armed time
// needs no Reset, and one due before it re-arms the timer.
func TestFarDeadlineArmsTimerEarly(t *testing.T) {
	c := newCallbackCache(t, 1)
	defer c.Close()
	ct := c.shards[0].callbacks
	set := func(k int, ttl time.Duration) (when, deadline int64) {
		t.Helper()
		if err := c.SetWithCallback(k, new(cbPayload), ttl, noCallback); err != nil {
			t.Fatal(err)
		}
		ct.mu.Lock()
		defer ct.mu.Unlock()
		return ct.when, ct.heap[0].expireTime
	}
	start := c.nowNano()
	when, deadline := set(1, time.Hour)
	if when >= deadline || when < start+int64(time.Hour)*15/16 {
		t.Fatalf("a 1h deadline armed the timer %v from now, want from 15/16 h up to 1h", time.Duration(when-start))
	}
	if w, _ := set(2, time.Hour-time.Second); w != when {
		t.Fatal("a deadline after the armed time re-armed the timer")
	}
	if w, d := set(3, time.Minute); w > d || w >= when {
		t.Fatalf("a 1 min deadline left the timer %v from now", time.Duration(w-start))
	}
	// A task due within 1 ms is armed for its first due instant, 1 ns past its deadline.
	ms := int64(time.Millisecond)
	ct.mu.Lock()
	now := c.nowNano()
	ct.arm(now+ms-1, now)
	exact := ct.when
	ct.arm(now+ms, now)
	early := ct.when
	ct.mu.Unlock()
	if exact != now+ms || early >= now+ms+1 {
		t.Fatalf("tasks due in 1ms and 1ms+1ns armed the timer %v and %v from now, want exactly 1ms and less than 1ms+1ns",
			time.Duration(exact-now), time.Duration(early-now))
	}
}

// A task is due once the clock is past its deadline, the rule cache entries expire by.
func TestTakeUsesExpiryRule(t *testing.T) {
	const now = 1000
	for _, tt := range []struct {
		deadlines []int64
		n         int
		more      bool
	}{
		{[]int64{now}, 0, false},
		{[]int64{now - 1, now}, 1, false},
		{[]int64{now - 2, now - 1}, 1, true},
	} {
		var ct callbackTimer[int, int]
		for _, d := range tt.deadlines {
			ct.heap = append(ct.heap, heapEntry(callbackTask[int, int]{expireTime: d}))
			ct.up(len(ct.heap) - 1)
		}
		n, more := ct.take(make([]*callbackTask[int, int], 1), now)
		if n != tt.n || more != tt.more {
			t.Errorf("take of one task at %d from deadlines %v = %d, %v, want %d, %v",
				now, tt.deadlines, n, more, tt.n, tt.more)
		}
	}
}

// The second earliest task can be any child of the root. One fire must see two
// due tasks and take both, wherever the second one is.
func TestFireTakesSecondEarliestAtEachChild(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for second := 1; second <= callbackHeapArity; second++ {
		c := newCallbackCache(t, 1)
		ct := c.shards[0].callbacks
		now, hour := c.nowNano(), int64(time.Hour)
		deadlines := slices.Repeat([]int64{now + hour}, callbackHeapArity+1)
		deadlines[0], deadlines[second] = now-4, now-3
		ct.mu.Lock()
		for k, d := range deadlines {
			ct.heap = append(ct.heap, heapEntry(callbackTask[int, *cbPayload]{key: k, expireTime: d, callback: noCallback}))
		}
		ct.mu.Unlock()
		ct.fire()
		ct.mu.Lock()
		left := len(ct.heap)
		ct.mu.Unlock()
		c.Close()
		if want := len(deadlines) - 2; left != want {
			t.Fatalf("second earliest at child %d: one fire left %d of %d tasks, want %d", second, left, len(deadlines), want)
		}
	}
}

// One fire takes several due tasks, and hands any beyond dueBatch to another fire.
func TestFireTakesDueTasksInBatches(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	c := newCallbackCache(t, 1)
	defer c.Close()
	ct := c.shards[0].callbacks
	left := func() int {
		ct.mu.Lock()
		defer ct.mu.Unlock()
		return len(ct.heap)
	}
	push := func(n int) {
		ct.mu.Lock()
		defer ct.mu.Unlock()
		for k := range n {
			ct.heap = append(ct.heap, heapEntry(callbackTask[int, *cbPayload]{key: k, expireTime: 1, callback: noCallback}))
		}
	}
	// With one P, a hand-off cannot run before fire returns.
	push(3)
	ct.fire()
	if n := left(); n != 0 {
		t.Fatalf("one fire left %d of 3 due tasks", n)
	}
	push(dueBatch + 1)
	ct.fire()
	for deadline := time.Now().Add(5 * time.Second); left() > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d due tasks beyond one batch were never taken", left())
		}
	}
}

// Each trial puts two tasks one clock tick before a third and wakes the timer in
// a tight loop across those deadlines, so many wakes run on the very tick of the
// third. Every callback must still run exactly once.
func TestWakeOnTheTickLosesNothing(t *testing.T) {
	const trials = 4000
	tc := newTickCache(t, 3*trials, nil)
	ct := tc.shards[0].callbacks
	for trial := range trials {
		d := tc.nowNano() + 20_000
		tc.put(3*trial, d-42)
		tc.put(3*trial+1, d-42)
		tc.put(3*trial+2, d)
		for tc.nowNano() <= d+2000 {
			ct.fire()
		}
	}
	tc.checkOnce(t)
}

// The batch path's version of TestWakeOnTheTickLosesNothing. Each trial puts
// dueBatch tasks 25ns apart and calls fireBatch in a tight loop across their
// deadlines, so most calls find a deadline within a clock tick of now. Every
// callback must still run exactly once.
func TestBatchOnTheTickLosesNothing(t *testing.T) {
	const trials = 400
	tc := newTickCache(t, dueBatch*trials, nil)
	ct := tc.shards[0].callbacks
	for trial := range trials {
		d := tc.nowNano() + 500_000
		for i := range dueBatch {
			tc.put(dueBatch*trial+i, d+int64(25*i))
		}
		for end := d + 25*dueBatch + 2000; tc.nowNano() <= end; {
			ct.fireBatch()
		}
	}
	tc.checkOnce(t)
}

func TestCallbackHeapOrder(t *testing.T) {
	var ct callbackTimer[int, int]
	want := make([]int64, 1000)
	for i := range want {
		want[i] = 1 + rand.Int64N(100)
		ct.heap = append(ct.heap, heapEntry(callbackTask[int, int]{expireTime: want[i]}))
		ct.up(len(ct.heap) - 1)
	}
	slices.Sort(want)
	for i, w := range want {
		if got := ct.pop().expireTime; got != w {
			t.Fatalf("pop %d returned deadline %d, want %d", i, got, w)
		}
		if h := ct.heap; len(h) > 0 && h[:cap(h)][len(h)] != (callbackEntry[int, int]{}) {
			t.Fatalf("pop %d left its vacated slot filled", i)
		}
	}
}

// The heap is dropped once nothing is pending, and shrinks as a burst fires,
// down to a capacity of 64. A 200ms burst is all in the heap before it fires.
func TestFiredBurstReleasesHeap(t *testing.T) {
	const n = 10000
	for _, tt := range []struct {
		pending int
		ttl     time.Duration
	}{
		{0, time.Nanosecond},
		{10, 200 * time.Millisecond},
	} {
		t.Run(fmt.Sprintf("%d pending", tt.pending), func(t *testing.T) {
			c := newCallbackCache(t, 1)
			defer c.Close()
			for k := range tt.pending {
				if err := c.SetWithCallback(k, new(cbPayload), time.Hour, noCallback); err != nil {
					t.Fatal(err)
				}
			}
			for k := tt.pending; k < tt.pending+n; k++ {
				if err := c.SetWithCallback(k, new(cbPayload), tt.ttl, noCallback); err != nil {
					t.Fatal(err)
				}
			}
			ct := c.shards[0].callbacks
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
				ct.mu.Lock()
				h := ct.heap
				ct.mu.Unlock()
				if len(h) == tt.pending && (h == nil) == (tt.pending == 0) && cap(h) <= 64 {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("heap still holds %d tasks with capacity %d after its burst fired, want %d tasks", len(h), cap(h), tt.pending)
				}
			}
		})
	}
}

// expiredEntry stores k as an entry that expired at deadline, so that a callback
// for k runs.
func expiredEntry[V any](c *Cache[int, V], k int, deadline int64) {
	var v V
	s := c.shards[0]
	cmd := writeCommand[int, V]{op: writeSet, key: k, value: v, hash: c.hasher.Sum(k), expireTime: deadline}
	s.mu.Lock()
	c.applySet(s, &cmd)
	s.mu.Unlock()
}

// pushDue adds a task that is already due without arming the timer, so that only
// the benchmark calls fire. The caller holds the callback lock.
func pushDue[V any](c *Cache[int, V], task *callbackTask[int, V]) {
	ct := c.shards[0].callbacks
	ct.heap = append(ct.heap, callbackEntry[int, V]{expireTime: task.expireTime, task: task})
	ct.up(len(ct.heap) - 1)
}

func BenchmarkSetWithCallback(b *testing.B) {
	b.Run("8B", benchSetWithCallback[int])
	b.Run("4KB", benchSetWithCallback[[4096]byte])
}

// The heap is dropped every 1024 sets, so it stays small and its growth is measured.
func benchSetWithCallback[V any](b *testing.B) {
	const pending = 1024
	c := newTestCache[int, V](b, Config{MaxSize: 4 * pending, ShardCount: 1, EvictionPolicy: LRU})
	defer c.Close()
	ct := c.shards[0].callbacks
	var v V
	callback := func(int, V) {}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		if i%pending == 0 {
			ct.mu.Lock()
			ct.heap = nil
			ct.when = math.MaxInt64
			ct.mu.Unlock()
		}
		ttl := time.Hour + time.Duration(i*7919%pending)*time.Millisecond
		if err := c.SetWithCallback(i%pending, v, ttl, callback); err != nil {
			b.Fatal(err)
		}
		i++
	}
}

func BenchmarkCallbackFire(b *testing.B) {
	b.Run("one/8B", benchCallbackFireOne[int])
	b.Run("one/4KB", benchCallbackFireOne[[4096]byte])
	b.Run("burst/8B", benchCallbackFireBurst[int])
	b.Run("burst/4KB", benchCallbackFireBurst[[4096]byte])
}

// One callback is due per fire, ahead of 1024 pending ones.
func benchCallbackFireOne[V any](b *testing.B) {
	const pending = 1024
	c := newTestCache[int, V](b, Config{MaxSize: 4 * pending, ShardCount: 1, EvictionPolicy: LRU})
	defer c.Close()
	var v V
	callback := func(int, V) {}
	for k := 1; k <= pending; k++ {
		ttl := time.Hour + time.Duration(k*7919%pending)*time.Millisecond
		if err := c.SetWithCallback(k, v, ttl, callback); err != nil {
			b.Fatal(err)
		}
	}
	expiredEntry(c, 0, 1)
	task := &callbackTask[int, V]{key: 0, expireTime: 1, callback: callback}
	ct := c.shards[0].callbacks
	b.ReportAllocs()
	for b.Loop() {
		ct.mu.Lock()
		pushDue(c, task)
		ct.mu.Unlock()
		ct.fire()
	}
}

// 1024 callbacks are due at one fire.
func benchCallbackFireBurst[V any](b *testing.B) {
	const burst = 1024
	c := newTestCache[int, V](b, Config{MaxSize: 4 * burst, ShardCount: 1, EvictionPolicy: LRU})
	defer c.Close()
	var wg sync.WaitGroup
	tasks := make([]*callbackTask[int, V], burst)
	for k := range tasks {
		deadline := int64(1 + k%7)
		expiredEntry(c, k, deadline)
		tasks[k] = &callbackTask[int, V]{key: k, expireTime: deadline, callback: func(int, V) { wg.Done() }}
	}
	ct := c.shards[0].callbacks
	b.ReportAllocs()
	for b.Loop() {
		wg.Add(burst)
		ct.mu.Lock()
		for _, task := range tasks {
			pushDue(c, task)
		}
		ct.mu.Unlock()
		ct.fire()
		wg.Wait()
	}
}
