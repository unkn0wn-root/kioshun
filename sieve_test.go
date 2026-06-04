package kioshun

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unkn0wn-root/kioshun/internal/keyhash"
)

func TestSieveQueuePushPopRemove(t *testing.T) {
	var q sieveQueue[int, int]
	q.init()
	if !q.empty() {
		t.Fatal("new segment should be empty")
	}

	a := &cacheItem[int, int]{value: 1}
	b := &cacheItem[int, int]{value: 2}
	c := &cacheItem[int, int]{value: 3}
	q.pushFront(a)
	q.pushFront(b)
	q.pushFront(c)

	if q.size != 3 {
		t.Fatalf("n=%d, want 3", q.size)
	}
	if got := q.popBack(); got != a {
		t.Fatalf("pop got=%v, want oldest item", got)
	}
	if a.prev != nil || a.next != nil {
		t.Fatal("popped item links were not cleared")
	}

	if !q.remove(b) {
		t.Fatal("delete returned false for linked segment item")
	}
	if q.size != 1 {
		t.Fatalf("n=%d, want 1", q.size)
	}
	if got := q.popBack(); got != c {
		t.Fatalf("pop got=%v, want remaining item", got)
	}
	if !q.empty() {
		t.Fatal("segment should be empty after final pop")
	}
}

func TestGhostQueueAddRemoveOverwrite(t *testing.T) {
	g := newGhostQueue(3)
	g.add(1, 11)
	g.add(2, 22)
	if !g.contains(1, 11) || !g.contains(2, 22) {
		t.Fatal("ghost queue missing inserted fingerprints")
	}

	if g.contains(1, 99) {
		t.Fatal("hash match with wrong tag should not hit ghost queue")
	}

	if !g.remove(1, 11) {
		t.Fatal("delete returned false for present ghost fingerprint")
	}
	if g.contains(1, 11) {
		t.Fatal("deleted fingerprint still present")
	}

	g.add(3, 33)
	g.add(4, 44)
	g.add(5, 55)
	if g.contains(2, 22) {
		t.Fatal("oldest fingerprint should be overwritten")
	}
	for _, x := range []ghostEntry{{3, 33}, {4, 44}, {5, 55}} {
		if !g.contains(x.hash, x.tag) {
			t.Fatalf("fingerprint %v missing after overwrite", x)
		}
	}

	g.clear()
	if len(g.index) != 0 || g.contains(3, 33) || g.contains(4, 44) || g.contains(5, 55) {
		t.Fatal("clear did not reset ghost queue")
	}
}

func TestGhostQueueDeleteMissingReturnsFalse(t *testing.T) {
	g := newGhostQueue(2)
	g.add(7, 77)
	if g.remove(8, 88) {
		t.Fatal("delete returned true for missing ghost fingerprint")
	}
	if !g.contains(7, 77) {
		t.Fatal("missing delete removed existing ghost fingerprint")
	}
}

func TestGhostQueueDuplicateDoesNotGrow(t *testing.T) {
	g := newGhostQueue(2)
	g.add(7, 77)
	g.add(7, 77)
	if len(g.index) != 1 {
		t.Fatalf("n=%d, want 1", len(g.index))
	}
}

// increment is a test helper: it drives add + sample-aging directly, without the
// doorkeeper gate production applies in sieveTinyLFU.incrementFrequency.
func (s *countMinSketch) increment(h uint64) {
	s.add(h)
	s.samples++
	if s.resetAt > 0 && s.samples >= s.resetAt {
		s.age()
	}
}

func TestCountMinSketchIncrementEstimateAgeClear(t *testing.T) {
	s := newCountMinSketch(32)
	h := uint64(12345)
	if got := s.estimate(h); got != 0 {
		t.Fatalf("initial estimate=%d, want 0", got)
	}
	for i := 1; i <= 20; i++ {
		s.increment(h)
	}
	if got := s.estimate(h); got != sketchMaxCounter {
		t.Fatalf("saturated estimate=%d, want %d", got, sketchMaxCounter)
	}

	s.age()
	if got := s.estimate(h); got != sketchMaxCounter/2 {
		t.Fatalf("aged estimate=%d, want %d", got, sketchMaxCounter/2)
	}

	s.clear()
	if got := s.estimate(h); got != 0 {
		t.Fatalf("cleared estimate=%d, want 0", got)
	}
}

func TestDoorkeeperFiltersFirstFrequencyIncrement(t *testing.T) {
	a := newSieveTinyLFU[int, int](32, 10, 100)
	h := uint64(12345)

	a.recordAccess(h)
	if got := a.sketch.estimate(h); got != 0 {
		t.Fatalf("first access sketch estimate=%d, want 0", got)
	}
	if got := a.estimate(h); got != 1 {
		t.Fatalf("first access policy estimate=%d, want 1", got)
	}

	a.recordAccess(h)
	if got := a.sketch.estimate(h); got != 1 {
		t.Fatalf("second access sketch estimate=%d, want 1", got)
	}
	if got := a.estimate(h); got != 2 {
		t.Fatalf("second access policy estimate=%d, want 2", got)
	}

	a.sketch.age()
	a.door.clear()
	if got := a.estimate(h); got != 0 {
		t.Fatalf("aged and cleared estimate=%d, want 0", got)
	}
}

func TestNewSieveTinyLFUDefaults(t *testing.T) {
	a := newSieveTinyLFU[int, int](100, 0, 0)
	if a.probationCap != 10 || a.mainCap != 90 || a.ghostCap != 90 {
		t.Fatalf("caps pc=%d mc=%d gc=%d, want 10/90/90", a.probationCap, a.mainCap, a.ghostCap)
	}
	if a.probation.size != 0 || a.main.size != 0 || !a.probation.empty() || !a.main.empty() {
		t.Fatal("new admission queues should be empty")
	}
	if len(a.ghost.entries) != 90 {
		t.Fatalf("ghost cap=%d, want 90", len(a.ghost.entries))
	}
	if len(a.sketch.counters) == 0 {
		t.Fatal("sketch was not initialized")
	}
	if len(a.door.bits) == 0 {
		t.Fatal("doorkeeper was not initialized")
	}
}

func TestNewSieveTinyLFUAdaptiveBounds(t *testing.T) {
	a := newSieveTinyLFU[int, int](100, 10, 100)
	if a.minProbationCap != 1 || a.maxProbationCap != 60 || a.adaptStep != 1 {
		t.Fatalf("bounds lo=%d hi=%d st=%d, want 1/60/1", a.minProbationCap, a.maxProbationCap, a.adaptStep)
	}

	a.setProbationCap(1000)
	if a.probationCap != 60 || a.mainCap != 40 {
		t.Fatalf("upper clamp pc=%d mc=%d, want 60/40", a.probationCap, a.mainCap)
	}

	a.setProbationCap(-1)
	if a.probationCap != 1 || a.mainCap != 99 {
		t.Fatalf("lower clamp pc=%d mc=%d, want 1/99", a.probationCap, a.mainCap)
	}
}

func TestSieveTinyLFURecordReadHitMarksVisitedOnly(t *testing.T) {
	a := newSieveTinyLFU[int, int](16, 10, 100)
	probationItem := &cacheItem[int, int]{key: 1}
	mainItem := &cacheItem[int, int]{key: 2}

	a.insert(probationItem, false)
	a.insertMain(mainItem)

	// A read hit only sets the visited bit for items the policy owns; it must
	// never touch the adaptive controller. The "main is useful" signal is
	// gathered on the eviction sweep (findMainVictim), not on the read path.
	clearSieveItemVisited(probationItem)
	a.recordReadHit(probationItem)
	if !sieveItemVisited(probationItem) {
		t.Fatal("probation read did not mark item visited")
	}

	clearSieveItemVisited(mainItem)
	a.recordReadHit(mainItem)
	if !sieveItemVisited(mainItem) {
		t.Fatal("main read did not mark item visited")
	}

	if got := a.controller.mainSurvivals; got != 0 {
		t.Fatalf("recordReadHit touched mainSurvivals=%d, want 0", got)
	}
}

func TestSieveTinyLFUAdaptiveResetCycleClearsCounters(t *testing.T) {
	a := newSieveTinyLFU[int, int](100, 10, 100)

	// Every adaptive signal lives on the single-consumer maintenance path now,
	// so resetCycle clears them all with plain assignment, mainSurvivals included.
	a.controller.ghostHits = 3
	a.controller.probationEvictions = 5
	a.controller.promotions = 2
	a.controller.mainSurvivals = 7
	a.controller.observationsInCycle = 11

	a.controller.resetCycle()

	if c := a.controller; c.ghostHits != 0 || c.probationEvictions != 0 ||
		c.promotions != 0 || c.mainSurvivals != 0 || c.observationsInCycle != 0 {
		t.Fatalf("resetCycle left non-zero counters: %+v", c)
	}
}

func TestSieveTinyLFUAdaptiveShrinkUsesMainSurvivals(t *testing.T) {
	a := newSieveTinyLFU[int, int](100, 10, 100)
	start := a.probationCap

	// Probation churns (evictions > 2x promotions) and main is warm
	// (survivals > promotions): shrink probation to give main more room.
	a.controller.probationEvictions = 3
	a.controller.promotions = 1
	a.controller.mainSurvivals = 2
	a.controller.observationsInCycle = uint64(a.capacity*10 - 1)

	a.tick()

	if want := start - a.adaptStep; a.probationCap != want {
		t.Fatalf("probationCap=%d, want %d", a.probationCap, want)
	}
	if a.mainCap != a.capacity-a.probationCap {
		t.Fatalf("mainCap=%d, want %d", a.mainCap, a.capacity-a.probationCap)
	}
	if got := a.controller.mainSurvivals; got != 0 {
		t.Fatalf("mainSurvivals after tick=%d, want 0", got)
	}
	if a.controller.observationsInCycle != 0 {
		t.Fatalf("observationsInCycle=%d, want 0", a.controller.observationsInCycle)
	}
}

func TestSieveTinyLFUAdaptiveShrinkBlockedWhenMainCold(t *testing.T) {
	a := newSieveTinyLFU[int, int](100, 10, 100)
	start := a.probationCap

	// Probation churns, but main earns nothing (survivals <= promotions): the
	// guard must stop the controller from shrinking probation to feed a cold main.
	a.controller.probationEvictions = 3
	a.controller.promotions = 1
	a.controller.mainSurvivals = 1
	a.controller.observationsInCycle = uint64(a.capacity*10 - 1)

	a.tick()

	if a.probationCap != start {
		t.Fatalf("probationCap=%d, want unchanged %d when main is cold", a.probationCap, start)
	}
}

func TestSieveTinyLFUMainSweepCountsSurvivals(t *testing.T) {
	a := newSieveTinyLFU[int, int](16, 10, 100)
	spared := &cacheItem[int, int]{key: 1}
	victim := &cacheItem[int, int]{key: 2}
	a.insertMain(spared)
	a.insertMain(victim) // queue: head -> victim -> spared -> tail; both visited

	// Leave spared visited (it earns a second chance and should be counted), and
	// make victim the unvisited tail-ward node the hand evicts.
	clearSieveItemVisited(victim)
	a.hand = spared

	got := a.findMainVictim(defaultMainVictimScan, false)
	if got != victim {
		gotKey := -1
		if got != nil {
			gotKey = got.key
		}
		t.Fatalf("findMainVictim returned key=%d, want victim key=2", gotKey)
	}
	if a.controller.mainSurvivals != 1 {
		t.Fatalf("mainSurvivals=%d, want 1 (one visited resident spared)", a.controller.mainSurvivals)
	}
	if sieveItemVisited(spared) {
		t.Fatal("sweep should have cleared the spared resident's visited bit")
	}
}

func TestSieveTinyLFUExpiredFastPathDoesNotCountMainSurvival(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         2,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
	})
	defer c.Close()

	shard := c.shards[0]
	now := time.Now().UnixNano()
	expiredHash := c.hasher.Sum(1)
	otherHash := c.hasher.Sum(2)

	expired := &cacheItem[int, int]{
		key:        1,
		value:      1,
		expireTime: now - int64(time.Second),
		hash:       expiredHash,
		tag:        keyhash.TagFromHash(expiredHash),
	}
	other := &cacheItem[int, int]{
		key:        2,
		value:      2,
		expireTime: now + int64(time.Hour),
		hash:       otherHash,
		tag:        keyhash.TagFromHash(otherHash),
	}

	shard.mu.Lock()
	shard.data[1] = expired
	shard.data[2] = other
	atomic.StoreInt64(&shard.size, 2)
	shard.sieve.insertMain(expired)
	shard.sieve.insert(other, false)
	shard.mu.Unlock()

	if _, ok := c.Get(1); ok {
		t.Fatal("expired item was returned as a hit")
	}
	if got := shard.sieve.controller.mainSurvivals; got != 0 {
		t.Fatalf("mainSurvivals after expired read=%d, want 0", got)
	}
}

func TestSieveTinyLFUInitializesState(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         16,
		ShardCount:      4,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
		ProbationRatio:  25,
		GhostRatio:      50,
	})
	defer c.Close()

	for i, s := range c.shards {
		if s.sieve == nil {
			t.Fatalf("shard %d missing admission state", i)
		}
		if s.sieve.probationCap != 1 || s.sieve.mainCap != 3 || s.sieve.ghostCap != 1 {
			t.Fatalf("shard %d caps pc=%d mc=%d gc=%d, want 1/3/1", i, s.sieve.probationCap, s.sieve.mainCap, s.sieve.ghostCap)
		}
		if s.sieve.probation.size != 0 || s.sieve.main.size != 0 || !s.sieve.probation.empty() || !s.sieve.main.empty() {
			t.Fatalf("shard %d policy queues should start empty", i)
		}
	}
}

func TestShardCapacityRemainderDistributed(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         10,
		ShardCount:      4,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
	})
	defer c.Close()

	var sum int64
	seen := make(map[int64]int)
	for _, s := range c.shards {
		sum += s.cap
		seen[s.cap]++
	}

	if sum != 10 {
		t.Fatalf("capacity sum=%d, want 10", sum)
	}
	if seen[3] != 2 || seen[2] != 2 {
		t.Fatalf("capacity distribution=%v, want two 3-cap and two 2-cap shards", seen)
	}
}

func TestNonAdmissionPolicyDoesNotInitializeAdmissionState(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         16,
		ShardCount:      4,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  LRU,
	})
	defer c.Close()

	for i, s := range c.shards {
		if s.sieve != nil {
			t.Fatalf("shard %d unexpectedly initialized admission state", i)
		}
	}
}

func TestSieveTinyLFUBoundedVictimNeedsForce(t *testing.T) {
	a := newSieveTinyLFU[int, int](8, 25, 100)
	for k := 1; k <= 4; k++ {
		a.insertMain(&cacheItem[int, int]{
			key:     k,
			hash:    uint64(k),
			visited: sieveVisited,
			reuse:   1,
		})
	}

	if v := a.findMainVictim(2, false); v != nil {
		t.Fatalf("non-forced bounded scan returned victim=%v, want nil", v.key)
	}

	cleared := 0
	it := a.main.head.next
	for it != &a.main.tail {
		if !sieveItemVisited(it) {
			cleared++
		}
		it = it.next
	}
	if cleared != 2 {
		t.Fatalf("cleared visited bits=%d, want 2", cleared)
	}

	if v := a.findMainVictim(2, true); v == nil {
		t.Fatal("forced bounded scan did not return a victim")
	}
}

func TestSieveTinyLFUGhostHitEntersMain(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         2,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
		StatsEnabled:    true,
	})
	defer c.Close()

	c.Set(1, 1, time.Hour)
	c.Set(2, 2, time.Hour)
	c.Set(3, 3, time.Hour)
	waitForWrites(t, c)

	s := c.shards[0]
	h := c.hasher.Sum(1)
	tag := c.hasher.Tag(1)
	if !s.sieve.ghost.contains(h, tag) {
		t.Fatal("expected cold eviction to leave a ghost entry")
	}

	c.Set(1, 10, time.Hour)
	waitForWrites(t, c)

	s.mu.RLock()
	it, ok := s.data[1]
	if !ok {
		s.mu.RUnlock()
		t.Fatal("ghost hit candidate was not resident")
	}
	q := it.queue
	sz := atomic.LoadInt64(&s.size)
	s.mu.RUnlock()

	if q != mainQueue {
		t.Fatalf("ghost hit queue=%d, want main", q)
	}
	if sz != 2 {
		t.Fatalf("size=%d, want 2", sz)
	}
}

func TestSieveTinyLFUGhostHitDoesNotResizeImmediately(t *testing.T) {
	a := newSieveTinyLFU[int, int](100, 10, 100)
	start := a.probationCap
	it := &cacheItem[int, int]{hash: 1}
	a.ghost.add(it.hash, 0)

	a.insert(it, true)

	if a.probationCap != start {
		t.Fatalf("probationCap=%d, want unchanged %d", a.probationCap, start)
	}
	if a.main.size != 1 || it.queue != mainQueue {
		t.Fatalf("ghost hit main size=%d queue=%d, want main resident", a.main.size, it.queue)
	}
	if a.controller.ghostHits != 1 || a.stats.GhostHits != 1 {
		t.Fatalf("ghost hits controller/stats=%d/%d, want 1/1", a.controller.ghostHits, a.stats.GhostHits)
	}
}

func TestSieveTinyLFUMaintainForcesCapacityAfterBoundedScan(t *testing.T) {
	s := &shard[int, int]{data: make(map[int]*cacheItem[int, int]), cap: 3}
	s.initLRU()
	s.sieve = newSieveTinyLFU[int, int](3, 25, 100)
	var p sync.Pool
	p.New = func() any { return &cacheItem[int, int]{} }

	var in *cacheItem[int, int]
	for k := 1; k <= 4; k++ {
		it := &cacheItem[int, int]{
			value:   k,
			key:     k,
			hash:    uint64(k),
			reuse:   1,
			visited: sieveVisited,
		}
		s.data[k] = it
		s.sieve.insertMain(it)
		atomic.AddInt64(&s.size, 1)
		if k == 4 {
			in = it
		}
	}

	s.enforceSieveCapacity(&p, true, in, true)

	if sz := atomic.LoadInt64(&s.size); sz > s.cap {
		t.Fatalf("size=%d exceeds cap=%d", sz, s.cap)
	}
	if qn := s.sieve.probation.size + s.sieve.main.size; qn != atomic.LoadInt64(&s.size) {
		t.Fatalf("queue size=%d, shard size=%d", qn, atomic.LoadInt64(&s.size))
	}
}

func TestSieveTinyLFUSetKeepsShardWithinCapacity(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         4,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
	})
	defer c.Close()

	for k := range 64 {
		for hot := range k {
			c.Get(hot)
		}
		if err := c.Set(k, k, time.Hour); err != nil {
			t.Fatal(err)
		}
		waitForWrites(t, c)

		s := c.shards[0]
		s.mu.RLock()
		sz := atomic.LoadInt64(&s.size)
		qn := s.sieve.probation.size + s.sieve.main.size
		capacity := s.cap
		s.mu.RUnlock()

		if sz > capacity {
			t.Fatalf("after Set(%d), size=%d exceeds cap=%d", k, sz, capacity)
		}
		if qn != sz {
			t.Fatalf("after Set(%d), queue size=%d, shard size=%d", k, qn, sz)
		}
	}
}

func TestSieveTinyLFUPolicyStats(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         2,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
	})
	defer c.Close()

	c.Set(1, 1, time.Hour)
	c.Set(2, 2, time.Hour)
	c.Set(3, 3, time.Hour)
	waitForWrites(t, c)
	c.Set(1, 10, time.Hour)
	waitForWrites(t, c)

	ps := c.PolicyStats()
	if ps.Admits != 4 {
		t.Fatalf("admits=%d, want 4", ps.Admits)
	}
	if ps.GhostHits != 1 {
		t.Fatalf("ghost hits=%d, want 1", ps.GhostHits)
	}
	if ps.ProbationEvictions != 2 {
		t.Fatalf("probation evictions=%d, want 2", ps.ProbationEvictions)
	}
	if ps.Rejects != 0 {
		t.Fatalf("rejects=%d, want 0", ps.Rejects)
	}

	c.Clear()
	if got := c.PolicyStats(); got != (PolicyStats{}) {
		t.Fatalf("policy stats after clear=%+v, want zero", got)
	}
}

func TestSieveTinyLFUExistingUpdatePromotesProbation(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         4,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
	})
	defer c.Close()

	c.Set(1, 1, time.Hour)
	c.Set(2, 2, time.Hour)
	c.Set(3, 3, time.Hour)
	waitForWrites(t, c)
	c.Get(1)
	c.Get(1)
	c.Set(1, 11, time.Hour)
	waitForWrites(t, c)

	s := c.shards[0]
	s.mu.RLock()
	it := s.data[1]
	q := it.queue
	v := it.value
	s.mu.RUnlock()

	if v != 11 {
		t.Fatalf("value=%d, want 11", v)
	}
	if q != mainQueue {
		t.Fatalf("updated hot item queue=%d, want main", q)
	}
	if ps := c.PolicyStats(); ps.Promotions == 0 {
		t.Fatal("expected update to record a promotion")
	}
}

func TestSieveTinyLFUDeleteUnlinksQueue(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         4,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
	})
	defer c.Close()

	c.Set(1, 1, time.Hour)
	c.Set(2, 2, time.Hour)
	waitForWrites(t, c)
	if !c.Delete(1) {
		t.Fatal("delete returned false")
	}

	s := c.shards[0]
	s.mu.RLock()
	n := s.sieve.probation.size + s.sieve.main.size
	sz := atomic.LoadInt64(&s.size)
	_, ok := s.data[1]
	s.mu.RUnlock()

	if ok {
		t.Fatal("deleted key still present")
	}
	if n != sz {
		t.Fatalf("queue size=%d, shard size=%d", n, sz)
	}
}

func TestSieveTinyLFUClearResetsPolicyState(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         4,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
	})
	defer c.Close()

	c.Set(1, 1, time.Hour)
	c.Set(2, 2, time.Hour)
	c.Set(3, 3, time.Hour)
	waitForWrites(t, c)
	c.Clear()

	s := c.shards[0]
	if atomic.LoadInt64(&s.size) != 0 {
		t.Fatalf("size=%d, want 0", atomic.LoadInt64(&s.size))
	}
	if s.sieve.probation.size != 0 || s.sieve.main.size != 0 || len(s.sieve.ghost.index) != 0 {
		t.Fatalf("policy state not cleared: p=%d m=%d g=%d", s.sieve.probation.size, s.sieve.main.size, len(s.sieve.ghost.index))
	}
}

func TestSieveTinyLFUCleanupRemovesExpiredQueueItem(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         4,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
		StatsEnabled:    true,
	})
	defer c.Close()

	c.Set(1, 1, time.Millisecond)
	waitForWrites(t, c)
	time.Sleep(2 * time.Millisecond)
	c.Cleanup()

	s := c.shards[0]
	s.mu.RLock()
	n := s.sieve.probation.size + s.sieve.main.size
	sz := atomic.LoadInt64(&s.size)
	s.mu.RUnlock()

	if sz != 0 {
		t.Fatalf("size=%d, want 0", sz)
	}
	if n != 0 {
		t.Fatalf("queue size=%d, want 0", n)
	}
	if c.Stats().Expirations == 0 {
		t.Fatal("expected expiration stat to increment")
	}
}

func TestSieveTinyLFUMainSieveEvictionClearsVisited(t *testing.T) {
	s := &shard[int, int]{data: make(map[int]*cacheItem[int, int])}
	s.initLRU()
	s.sieve = newSieveTinyLFU[int, int](4, 25, 100)
	var p sync.Pool
	p.New = func() any { return &cacheItem[int, int]{} }

	for k := 1; k <= 3; k++ {
		it := &cacheItem[int, int]{
			value:   k,
			key:     k,
			hash:    uint64(k),
			reuse:   1,
			visited: sieveVisited,
		}
		s.data[k] = it
		s.sieve.insertMain(it)
		atomic.AddInt64(&s.size, 1)
	}

	if ok := s.evictMain(&p, true, nil, false, s.sieve.main.size+1, true); !ok {
		t.Fatal("main eviction returned false")
	}
	if _, ok := s.data[1]; ok {
		t.Fatal("expected oldest hand item to be evicted after visited-bit sweep")
	}
	for _, k := range []int{2, 3} {
		it := s.data[k]
		if it == nil {
			t.Fatalf("item %d missing", k)
		}
		if sieveItemVisited(it) {
			t.Fatalf("item %d visited bit still set after sweep", k)
		}
	}
	if s.sieve.main.size != 2 {
		t.Fatalf("main size=%d, want 2", s.sieve.main.size)
	}
	if atomic.LoadInt64(&s.evictions) != 1 {
		t.Fatalf("evictions=%d, want 1", atomic.LoadInt64(&s.evictions))
	}
	if s.sieve.stats.MainEvictions != 1 {
		t.Fatalf("main evictions=%d, want 1", s.sieve.stats.MainEvictions)
	}
}

func TestSieveTinyLFUEqualFrequencyTieRejection(t *testing.T) {
	a := newSieveTinyLFU[int, int](4, 25, 100)
	in := &cacheItem[int, int]{hash: 11}
	v := &cacheItem[int, int]{hash: 22, queue: mainQueue, reuse: 1}

	for range 3 {
		a.sketch.increment(in.hash)
		a.sketch.increment(v.hash)
	}

	if a.shouldAdmit(in, v, false) {
		t.Fatal("equal-frequency candidate should not replace a retained main victim without a tie signal")
	}
	if !a.shouldAdmit(in, v, true) {
		t.Fatal("ghost/tie signal should admit an equal-frequency candidate")
	}

	v.reuse = 0
	clearSieveItemVisited(v)
	if !a.shouldAdmit(in, v, false) {
		t.Fatal("unvisited zero-reuse victim should lose equal-frequency tie")
	}
}

func TestSieveTinyLFUQueueSizeMatchesShardSizeAfterMixedOps(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         8,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
		StatsEnabled:    true,
	})
	defer c.Close()

	for i := range 32 {
		c.Set(i%12, i, time.Hour)
		if i%3 == 0 {
			c.Get(i % 6)
		}
		if i%7 == 0 {
			c.Delete((i + 3) % 12)
		}
	}
	waitForWrites(t, c)

	s := c.shards[0]
	s.mu.RLock()
	qn := s.sieve.probation.size + s.sieve.main.size
	sz := atomic.LoadInt64(&s.size)
	s.mu.RUnlock()

	if sz > c.perShardCap {
		t.Fatalf("size=%d exceeds cap=%d", sz, c.perShardCap)
	}
	if qn != sz {
		t.Fatalf("queue size=%d, shard size=%d", qn, sz)
	}
	assertSieveShardConsistent(t, s)
}

func TestSieveTinyLFUParallelRepeatedWritesKeepQueuesConsistent(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         32,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
		StatsEnabled:    true,
	})
	defer c.Close()

	keys := make([]int, 96)
	for i := range keys {
		keys[i] = i
	}

	var failed int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			for round := 0; round < 250 && atomic.LoadInt32(&failed) == 0; round++ {
				for _, k := range keys {
					if err := c.Set(k, id<<20|round, time.Hour); err != nil {
						atomic.StoreInt32(&failed, 1)
						return
					}
					if k%8 == 0 {
						c.Get(k / 2)
					}
				}
			}
		}(g)
	}
	close(start)
	wg.Wait()
	waitForWrites(t, c)

	if atomic.LoadInt32(&failed) != 0 {
		t.Fatal("parallel Set returned an error")
	}
	assertSieveShardConsistent(t, c.shards[0])
	if sz := c.Size(); sz > 32 {
		t.Fatalf("size=%d exceeds capacity=32", sz)
	}
}

func TestSieveTinyLFUParallelRepeatedStringWritesAcrossShards(t *testing.T) {
	c := newTestCache[string, []byte](t, Config{
		MaxSize:         1024,
		ShardCount:      32,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
		StatsEnabled:    true,
	})
	defer c.Close()

	keys := make([]string, 4096)
	for i := range keys {
		keys[i] = "key_" + strconv.Itoa(i)
	}
	value := make([]byte, 1024)

	var failed int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for round := 0; round < 64 && atomic.LoadInt32(&failed) == 0; round++ {
				for _, k := range keys {
					if err := c.Set(k, value, time.Hour); err != nil {
						atomic.StoreInt32(&failed, 1)
						return
					}
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	waitForWrites(t, c)

	if atomic.LoadInt32(&failed) != 0 {
		t.Fatal("parallel Set returned an error")
	}
	if sz := c.Size(); sz > c.config.MaxSize {
		t.Fatalf("size=%d exceeds capacity=%d", sz, c.config.MaxSize)
	}
	for _, s := range c.shards {
		assertSieveShardConsistent(t, s)
	}
}

func TestSieveTinyLFUPooledItemsResetAfterClear(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         16,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
	})
	defer c.Close()

	for round := range 4 {
		for k := range 128 {
			if err := c.Set(k, round, time.Hour); err != nil {
				t.Fatal(err)
			}
			if k%3 == 0 {
				c.Get(k)
			}
		}
		c.Clear()
	}

	for k := range 128 {
		if err := c.Set(k, k, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	waitForWrites(t, c)
	assertSieveShardConsistent(t, c.shards[0])
}

func assertSieveShardConsistent[K comparable, V any](t *testing.T, s *shard[K, V]) {
	t.Helper()

	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.sieve == nil {
		t.Fatal("missing sieve policy state")
	}
	if int64(len(s.data)) != atomic.LoadInt64(&s.size) {
		t.Fatalf("map size=%d, shard size=%d", len(s.data), atomic.LoadInt64(&s.size))
	}

	seen := make(map[*cacheItem[K, V]]struct{}, len(s.data))
	pn := assertSieveQueueConsistent(t, &s.sieve.probation, probationQueue, len(s.data), seen)
	mn := assertSieveQueueConsistent(t, &s.sieve.main, mainQueue, len(s.data), seen)
	total := pn + mn
	if total != atomic.LoadInt64(&s.size) {
		t.Fatalf("queue size=%d, shard size=%d", total, atomic.LoadInt64(&s.size))
	}

	for key, item := range s.data {
		if _, ok := seen[item]; !ok {
			t.Fatalf("resident key %v is not linked in any Sieve queue", key)
		}
		if item.key != key {
			t.Fatalf("resident key %v stored item key=%v", key, item.key)
		}
	}
}

func assertSieveQueueConsistent[K comparable, V any](
	t *testing.T,
	q *sieveQueue[K, V],
	id sieveQueueID,
	residents int,
	seen map[*cacheItem[K, V]]struct{},
) int64 {
	t.Helper()

	if q.head.next == nil || q.tail.prev == nil {
		t.Fatal("queue sentinel link is nil")
	}
	if q.head.prev != nil || q.tail.next != nil {
		t.Fatal("queue sentinel outer link is not nil")
	}

	var n int64
	limit := residents + 2
	for it := q.head.next; it != &q.tail; it = it.next {
		if it == nil {
			t.Fatal("queue contains nil next link")
		}
		if n > int64(limit) {
			t.Fatal("queue traversal exceeded resident count; possible cycle")
		}
		if it.prev == nil || it.prev.next != it {
			t.Fatal("queue prev/next linkage is inconsistent")
		}
		if it.next == nil || it.next.prev != it {
			t.Fatal("queue next/prev linkage is inconsistent")
		}
		if it.sieveQ != q {
			t.Fatalf("item queue owner mismatch: got %p want %p", it.sieveQ, q)
		}
		if it.queue != id {
			t.Fatalf("item queue id=%d, want %d", it.queue, id)
		}
		if _, ok := seen[it]; ok {
			t.Fatal("item is linked in multiple Sieve queues")
		}
		seen[it] = struct{}{}
		n++
	}

	if n != q.size {
		t.Fatalf("walked queue size=%d, stored size=%d", n, q.size)
	}
	return n
}
