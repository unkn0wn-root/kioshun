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
	q.init(mainQueue, 0)
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
	// The policy evicts the oldest entry as tail.prev + remove (see
	// evictProbation/findMainVictim); exercise that pattern directly.
	if oldest := q.tail.prev; oldest != a || !q.remove(oldest) {
		t.Fatalf("oldest=%v, want oldest item a", oldest)
	}
	if a.prev != nil || a.next != nil {
		t.Fatal("removed item links were not cleared")
	}

	if !q.remove(b) {
		t.Fatal("delete returned false for linked segment item")
	}
	if q.size != 1 {
		t.Fatalf("n=%d, want 1", q.size)
	}
	if last := q.tail.prev; last != c || !q.remove(last) {
		t.Fatalf("last=%v, want remaining item c", last)
	}
	if !q.empty() {
		t.Fatal("segment should be empty after final removal")
	}
}

func TestSieveQueueOwnerIsolation(t *testing.T) {
	var qa, qb sieveQueue[int, int]
	qa.init(probationQueue, 1) // shard 1 probation
	qb.init(probationQueue, 2) // shard 2 probation: same role, different owner

	it := &cacheItem[int, int]{value: 1}
	qa.pushFront(it)

	if qb.holds(it) {
		t.Fatal("queue B claims an item owned by queue A (same role, different owner)")
	}
	if qb.remove(it) {
		t.Fatal("queue B removed an item owned by queue A - corruption guard failed")
	}
	if qb.size != 0 {
		t.Fatalf("queue B size=%d after rejected remove, want 0", qb.size)
	}
	// Queue A still owns the item and can remove it.
	if !qa.holds(it) || qa.size != 1 {
		t.Fatal("item should remain linked in queue A")
	}
	if !qa.remove(it) {
		t.Fatal("queue A failed to remove its own item")
	}
}

func TestGhostQueueAddRemoveOverwrite(t *testing.T) {
	g := newGhostQueue(3)
	g.add(1)
	g.add(2)
	if !g.contains(1) || !g.contains(2) {
		t.Fatal("ghost queue missing inserted fingerprints")
	}

	if !g.remove(1) {
		t.Fatal("delete returned false for present ghost fingerprint")
	}
	if g.contains(1) {
		t.Fatal("deleted fingerprint still present")
	}

	g.add(3)
	g.add(4)
	g.add(5)
	if g.contains(2) {
		t.Fatal("oldest fingerprint should be overwritten")
	}
	for _, h := range []uint64{3, 4, 5} {
		if !g.contains(h) {
			t.Fatalf("fingerprint %d missing after overwrite", h)
		}
	}

	g.clear()
	if g.count() != 0 || g.contains(3) || g.contains(4) || g.contains(5) {
		t.Fatal("clear did not reset ghost queue")
	}
}

func TestGhostQueueZeroHashFingerprint(t *testing.T) {
	g := newGhostQueue(2)
	g.add(0)
	if !g.contains(0) {
		t.Fatal("zero-hash fingerprint not tracked")
	}

	g.add(9)
	if !g.contains(0) || !g.contains(9) {
		t.Fatal("zero-hash fingerprint lost after a second add")
	}

	// ring size 2: adding a third entry evicts the oldest (the zero hash).
	g.add(8)
	if g.contains(0) {
		t.Fatal("oldest (zero-hash) fingerprint should have been evicted")
	}
	if !g.contains(9) || !g.contains(8) {
		t.Fatal("recent fingerprints missing after eviction")
	}

	if !g.remove(8) || g.contains(8) {
		t.Fatal("removing fingerprint adjacent to a cleared slot failed")
	}
}

func TestGhostQueueDeleteMissingReturnsFalse(t *testing.T) {
	g := newGhostQueue(2)
	g.add(7)
	if g.remove(8) {
		t.Fatal("delete returned true for missing ghost fingerprint")
	}
	if !g.contains(7) {
		t.Fatal("missing delete removed existing ghost fingerprint")
	}
}

func TestGhostQueueDuplicateDoesNotGrow(t *testing.T) {
	g := newGhostQueue(2)
	g.add(7)
	g.add(7)
	if g.count() != 1 {
		t.Fatalf("n=%d, want 1", g.count())
	}
}

// increment is a test helper: it drives add + sample-aging directly, without the
// doorkeeper gate production applies in sieveTinyLFU.incrementFrequency. It
// avalanches like production callers so estimates through the policy path see
// the same cells.
func (s *countMinSketch) increment(h uint64) {
	s.add(keyhash.Avalanche(h))
	s.samples++
	if s.resetAt > 0 && s.samples >= s.resetAt {
		s.age()
	}
}

func TestCountMinSketchIncrementEstimateAgeClear(t *testing.T) {
	s := newCountMinSketch(32)
	h := uint64(12345)
	av := keyhash.Avalanche(h)
	if got := s.estimate(av); got != 0 {
		t.Fatalf("initial estimate=%d, want 0", got)
	}
	for i := 1; i <= 20; i++ {
		s.increment(h)
	}
	if got := s.estimate(av); got != sketchMaxCounter {
		t.Fatalf("saturated estimate=%d, want %d", got, sketchMaxCounter)
	}

	s.age()
	if got := s.estimate(av); got != sketchMaxCounter/2 {
		t.Fatalf("aged estimate=%d, want %d", got, sketchMaxCounter/2)
	}

	s.clear()
	if got := s.estimate(av); got != 0 {
		t.Fatalf("cleared estimate=%d, want 0", got)
	}
}

func TestDoorkeeperFiltersFirstFrequencyIncrement(t *testing.T) {
	a := newSieveTinyLFU[int, int](32, 0, 10, 100)
	h := uint64(12345)
	av := keyhash.Avalanche(h)

	// at the default shifting insert weight, recordAccess deposits both
	// observations (the unsampled Get miss plus the Set): the doorkeeper absorbs
	// the first, the sketch takes the second.
	a.recordAccess(h)
	if got := a.sketch.estimate(av); got != 1 {
		t.Fatalf("first insert sketch estimate=%d, want 1", got)
	}
	if got := a.estimate(h); got != 2 {
		t.Fatalf("first insert policy estimate=%d, want 2", got)
	}

	a.recordAccess(h)
	if got := a.sketch.estimate(av); got != 3 {
		t.Fatalf("second insert sketch estimate=%d, want 3", got)
	}
	if got := a.estimate(h); got != 4 {
		t.Fatalf("second insert policy estimate=%d, want 4", got)
	}

	a.sketch.age()
	a.door.clear()
	if got := a.estimate(h); got != 1 {
		t.Fatalf("aged and cleared estimate=%d, want 1", got)
	}
}

func TestNewSieveTinyLFUDefaults(t *testing.T) {
	a := newSieveTinyLFU[int, int](100, 0, 0, 0)
	if a.probationCap != 1 || a.mainCap != 99 || a.ghostCap != 74 {
		t.Fatalf("caps pc=%d mc=%d gc=%d, want 1/99/74", a.probationCap, a.mainCap, a.ghostCap)
	}
	if a.probation.size != 0 || a.main.size != 0 || !a.probation.empty() || !a.main.empty() {
		t.Fatal("new admission queues should be empty")
	}
	if len(a.ghost.entries) != 74 {
		t.Fatalf("ghost cap=%d, want 74", len(a.ghost.entries))
	}
	if len(a.sketch.counters) == 0 {
		t.Fatal("sketch was not initialized")
	}
	if len(a.door.bits) == 0 {
		t.Fatal("doorkeeper was not initialized")
	}
}

func TestNewSieveTinyLFUAdaptiveBounds(t *testing.T) {
	a := newSieveTinyLFU[int, int](100, 0, 10, 100)
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
	a := newSieveTinyLFU[int, int](16, 0, 10, 100)
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

func TestSieveProbationWindowKeepsNewResidentWhenBelowTarget(t *testing.T) {
	const capacity = 8
	cfg := Config{
		MaxSize:         capacity,
		ShardCount:      1,
		EvictionPolicy:  SieveTinyLFU,
		DefaultTTL:      NoExpiration,
		CleanupInterval: 0,
		ProbationRatio:  50,
		StatsEnabled:    true,
	}
	c, err := New[uint64, uint64](cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	for k := range uint64(capacity) {
		if err := c.Set(k, k, NoExpiration); err != nil {
			t.Fatalf("Set warm key %d: %v", k, err)
		}
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync warm: %v", err)
	}

	// Make resident main entries look strong to TinyLFU. A new item should still
	// get room in probation while that recency window is under target.
	for range 32 {
		for k := range uint64(capacity) {
			if _, ok := c.Get(k); !ok {
				t.Fatalf("warm key %d disappeared before admission test", k)
			}
		}
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync frequency samples: %v", err)
	}

	if err := c.Set(100, 100, NoExpiration); err != nil {
		t.Fatalf("Set new key: %v", err)
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync new key: %v", err)
	}
	if _, ok := c.Get(100); !ok {
		t.Fatal("new probation resident was rejected before the probation window filled")
	}
	if rejects := c.PolicyStats().Rejects; rejects != 0 {
		t.Fatalf("policy rejects=%d, want 0 while probation has room", rejects)
	}
	if size := c.Size(); size != capacity {
		t.Fatalf("size=%d, want capped size %d", size, capacity)
	}
}

func TestSieveTinyLFUAdaptiveResetCycleClearsCounters(t *testing.T) {
	a := newSieveTinyLFU[int, int](100, 0, 10, 100)

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
	a := newSieveTinyLFU[int, int](100, 0, 10, 100)
	start := a.probationCap

	// Probation churns (evictions > 2x promotions) and main is warm
	// (survivals > promotions): shrink probation to give main more room.
	a.controller.probationEvictions = 3
	a.controller.promotions = 1
	a.controller.mainSurvivals = 2
	a.controller.observationsInCycle = uint64(a.capacity*adaptiveCycleMultiplier - 1)

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
	a := newSieveTinyLFU[int, int](100, 0, 10, 100)
	start := a.probationCap

	// Probation churns, but main earns nothing (survivals <= promotions): the
	// guard must stop the controller from shrinking probation to feed a cold main.
	a.controller.probationEvictions = 3
	a.controller.promotions = 1
	a.controller.mainSurvivals = 1
	a.controller.observationsInCycle = uint64(a.capacity*adaptiveCycleMultiplier - 1)

	a.tick()

	if a.probationCap != start {
		t.Fatalf("probationCap=%d, want unchanged %d when main is cold", a.probationCap, start)
	}
}

func TestSieveTinyLFUAdaptiveGrowBlockedByResurrection(t *testing.T) {
	a := newSieveTinyLFU[int, int](100, 0, 10, 100)
	start := a.probationCap

	// B1 ghost hits alone would grow probation, but B2 resurrection means main
	// victims are still needed. The fallback grow path must honor the loop guard.
	a.controller.ghostHits = 10
	a.controller.probationEvictions = 4
	a.controller.cycleMainEvicts = 10
	a.controller.cycleB2Hits = 5
	a.controller.observationsInCycle = uint64(a.capacity*adaptiveCycleMultiplier - 1)

	a.tick()

	if a.probationCap != start {
		t.Fatalf("probationCap=%d, want unchanged %d when main victims resurrect", a.probationCap, start)
	}
}

func TestSieveTinyLFUAdaptiveWindowFloor(t *testing.T) {
	a := newSieveTinyLFU[int, int](2000, 0, 1, 100)
	start := a.probationCap

	a.controller.ghostHits = 10
	a.controller.cycleMainEvicts = 10
	a.controller.observationsInCycle = uint64(a.capacity*adaptiveCycleMultiplier - 1)

	a.tick()
	if a.probationCap != start {
		t.Fatalf("probationCap=%d, want unchanged before min cycle %d", a.probationCap, start)
	}

	a.controller.observationsInCycle = adaptiveMinCycle - 1
	a.tick()
	if a.probationCap <= start {
		t.Fatalf("probationCap=%d, want growth after min cycle from %d", a.probationCap, start)
	}
}

func TestSieveTinyLFUMainSweepCountsSurvivals(t *testing.T) {
	a := newSieveTinyLFU[int, int](16, 0, 10, 100)
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
	now := c.nowNano()
	expiredHash := c.hasher.Sum(1)
	otherHash := c.hasher.Sum(2)

	expired := &cacheItem[int, int]{
		key:        1,
		value:      1,
		expireTime: max(int64(1), now-int64(time.Second)),
		hash:       expiredHash,
	}
	other := &cacheItem[int, int]{
		key:        2,
		value:      2,
		expireTime: now + int64(time.Hour),
		hash:       otherHash,
	}

	shard.mu.Lock()
	shard.tab.store(expired)
	shard.tab.store(other)
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
	a := newSieveTinyLFU[int, int](8, 0, 25, 100)
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
	if !s.sieve.ghost.contains(h) {
		t.Fatal("expected cold eviction to leave a ghost entry")
	}

	c.Set(1, 10, time.Hour)
	waitForWrites(t, c)

	s.mu.RLock()
	it, ok := s.tab.lookup(c.hasher.Sum(1), 1)
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
	a := newSieveTinyLFU[int, int](100, 0, 10, 100)
	start := a.probationCap
	it := &cacheItem[int, int]{hash: 1}
	a.ghost.add(it.hash)

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
	s := &shard[int, int]{tab: newHtable[int, int](3), cap: 3, stats: newStats(1)}
	s.sieve = newSieveTinyLFU[int, int](3, 0, 25, 100)

	var in *cacheItem[int, int]
	for k := 1; k <= 4; k++ {
		it := &cacheItem[int, int]{
			value:   k,
			key:     k,
			hash:    uint64(k),
			reuse:   1,
			visited: sieveVisited,
		}
		s.tab.store(it)
		s.sieve.insertMain(it)
		atomic.AddInt64(&s.size, 1)
		if k == 4 {
			in = it
		}
	}

	s.enforceSieveCapacity(true, in, true)

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
	it, _ := s.tab.lookup(c.hasher.Sum(1), 1)
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
	_, ok := s.tab.lookup(c.hasher.Sum(1), 1)
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
	if s.sieve.probation.size != 0 || s.sieve.main.size != 0 || s.sieve.ghost.count() != 0 {
		t.Fatalf("policy state not cleared: p=%d m=%d g=%d", s.sieve.probation.size, s.sieve.main.size, s.sieve.ghost.count())
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
	s := &shard[int, int]{tab: newHtable[int, int](4), stats: newStats(1)}
	s.sieve = newSieveTinyLFU[int, int](4, 0, 25, 100)

	for k := 1; k <= 3; k++ {
		it := &cacheItem[int, int]{
			value:   k,
			key:     k,
			hash:    uint64(k),
			reuse:   1,
			visited: sieveVisited,
		}
		s.tab.store(it)
		s.sieve.insertMain(it)
		atomic.AddInt64(&s.size, 1)
	}

	if ok := s.evictMain(true, nil, false, s.sieve.main.size+1, true); !ok {
		t.Fatal("main eviction returned false")
	}
	if _, ok := s.tab.lookup(1, 1); ok {
		t.Fatal("expected oldest hand item to be evicted after visited-bit sweep")
	}
	for _, k := range []int{2, 3} {
		it, _ := s.tab.lookup(uint64(k), k)
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
	if _, _, ev, _ := s.stats.aggregate(); ev != 1 {
		t.Fatalf("evictions=%d, want 1", ev)
	}
	if s.sieve.stats.MainEvictions != 1 {
		t.Fatalf("main evictions=%d, want 1", s.sieve.stats.MainEvictions)
	}
}

func TestSieveTinyLFUEqualFrequencyTieRejection(t *testing.T) {
	a := newSieveTinyLFU[int, int](4, 0, 25, 100)
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

// TestSieveDropUnpublishedCandidateIsTableFreeAndIdempotent locks down the
// late-publication drop contract. A rejected candidate is live in policy but was
// never stored, so the drop must unlink it policy-only without touching the table,
// and a redundant second drop must be a no-op - the cleared unpublished flag falls
// through to removeExact, which fails because nothing was ever stored, so there is
// no double policy-unlink and no size underflow.
func TestSieveDropUnpublishedCandidateIsTableFreeAndIdempotent(t *testing.T) {
	s := &shard[int, int]{tab: newHtable[int, int](4), cap: 4, stats: newStats(1)}
	s.sieve = newSieveTinyLFU[int, int](4, 0, 25, 100)

	// A published resident the candidate drop must leave untouched.
	resident := &cacheItem[int, int]{key: 1, value: 1, hash: 1}
	s.tab.store(resident)
	s.sieve.insertMain(resident)
	atomic.AddInt64(&s.size, 1)

	// An unpublished candidate: linked into policy, deliberately absent from the
	// table, exactly as applySieve holds a candidate during admission.
	cand := &cacheItem[int, int]{key: 2, value: 2, hash: 2, unpublished: true}
	s.sieve.insert(cand, false)
	atomic.AddInt64(&s.size, 1)
	if _, ok := s.tab.lookup(cand.hash, cand.key); ok {
		t.Fatal("candidate must not be in the table before any drop")
	}

	if !s.dropSieveItem(cand, false, RemovedRejected) {
		t.Fatal("dropping an unpublished candidate returned false")
	}
	if cand.unpublished {
		t.Fatal("drop did not clear the unpublished mark")
	}
	if s.sieve.owns(cand) {
		t.Fatal("drop left the candidate linked in a policy queue")
	}
	if _, ok := s.tab.lookup(cand.hash, cand.key); ok {
		t.Fatal("drop published a candidate that was never stored")
	}
	if sz := atomic.LoadInt64(&s.size); sz != 1 {
		t.Fatalf("size=%d after reject, want 1 (only the resident)", sz)
	}

	// Self-healing: re-dropping the same pointer is a no-op via removeExact.
	if s.dropSieveItem(cand, false, RemovedRejected) {
		t.Fatal("re-dropping a rejected candidate must be a no-op")
	}
	if sz := atomic.LoadInt64(&s.size); sz != 1 {
		t.Fatalf("size=%d after redundant drop, want 1", sz)
	}

	// The resident is untouched: still in the table and still policy-owned.
	if _, ok := s.tab.lookup(resident.hash, resident.key); !ok {
		t.Fatal("resident vanished after dropping the candidate")
	}
	if !s.sieve.owns(resident) {
		t.Fatal("resident lost its policy linkage")
	}
	if n := s.tab.length(); n != 1 {
		t.Fatalf("table length=%d, want 1", n)
	}
}

// TestSieveRejectedCandidateNeverPublished drives the real applySieve insert path
// and forces a reject (a cold candidate under frequency admission cannot beat hot
// incumbents). The rejected candidate must never reach the table - invisible to a
// lock-free Get - and leave size and the policy queues consistent.
func TestSieveRejectedCandidateNeverPublished(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         2,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
		StatsEnabled:    true,
	})
	defer c.Close()

	s := c.shards[0]
	p := s.sieve

	// Seed two hot, evictable main residents and pin frequency admission so a
	// never-seen candidate is rejected deterministically.
	s.mu.Lock()
	for _, k := range []int{1, 2} {
		it := &cacheItem[int, int]{key: k, value: k, hash: c.hasher.Sum(k)}
		s.tab.store(it)
		p.insertMain(it)
		clearSieveItemVisited(it) // no second chance: the hand evicts on first pass
		it.reuse = 0
		atomic.AddInt64(&s.size, 1)
		for range 8 {
			p.sketch.increment(it.hash) // make the incumbents look hot
		}
	}
	p.tuner.mode = admitFrequency
	s.mu.Unlock()

	_, cmd, err := c.setCommand(99, 99, time.Hour, nil)
	if err != nil {
		t.Fatalf("setCommand: %v", err)
	}
	s.mu.Lock()
	committed := c.applySieve(s, &cmd)
	s.mu.Unlock()

	if committed {
		t.Fatal("cold candidate was admitted under frequency mode, want reject")
	}
	if _, ok := c.Get(99); ok {
		t.Fatal("rejected candidate is visible to Get; it must never be published")
	}

	s.mu.RLock()
	_, inTable := s.tab.lookup(cmd.hash, 99)
	sz := atomic.LoadInt64(&s.size)
	s.mu.RUnlock()

	if inTable {
		t.Fatal("rejected candidate left a slot in the table")
	}
	if sz != 2 {
		t.Fatalf("size=%d after reject, want 2 (insert increment was undone)", sz)
	}
	if rejects := c.PolicyStats().Rejects; rejects != 1 {
		t.Fatalf("policy rejects=%d, want 1", rejects)
	}
	assertSieveShardConsistent(t, s)
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
	n := s.tab.length()
	if int64(n) != atomic.LoadInt64(&s.size) {
		t.Fatalf("table size=%d, shard size=%d", n, atomic.LoadInt64(&s.size))
	}

	seen := make(map[*cacheItem[K, V]]struct{}, n)
	pn := assertSieveQueueConsistent(t, &s.sieve.probation, probationQueue, n, seen)
	mn := assertSieveQueueConsistent(t, &s.sieve.main, mainQueue, n, seen)
	total := pn + mn
	if total != atomic.LoadInt64(&s.size) {
		t.Fatalf("queue size=%d, shard size=%d", total, atomic.LoadInt64(&s.size))
	}

	s.tab.forEach(func(item *cacheItem[K, V]) bool {
		if _, ok := seen[item]; !ok {
			t.Fatalf("resident key %v is not linked in any Sieve queue", item.key)
		}
		return true
	})
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
		if it.queue != id || it.queue != q.id {
			t.Fatalf("item queue id=%d, want %d (q.id=%d)", it.queue, id, q.id)
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

func TestInsertWeightDecouplesEvidenceFromTimebase(t *testing.T) {
	h := uint64(12345)
	av := keyhash.Avalanche(h)

	shifting := newSieveTinyLFU[int, int](32, 0, 10, 100)
	if shifting.insertWeight != insertWeightShifting {
		t.Fatalf("default insertWeight=%d, want shifting (%d)", shifting.insertWeight, insertWeightShifting)
	}
	shifting.recordAccess(h)
	if got := shifting.sketch.samples; got != 2 {
		t.Fatalf("shifting samples=%d after one insert, want 2", got)
	}
	if got := shifting.sketch.estimate(av); got != 1 {
		t.Fatalf("shifting sketch estimate=%d, want 1 (second observation deposits)", got)
	}

	stationary := newSieveTinyLFU[int, int](32, 0, 10, 100)
	stationary.insertWeight = insertWeightStationary
	stationary.recordAccess(h)
	if got := stationary.sketch.samples; got != 2 {
		t.Fatalf("stationary samples=%d after one insert, want 2 (timebase is weight-independent)", got)
	}
	if got := stationary.sketch.estimate(av); got != 0 {
		t.Fatalf("stationary sketch estimate=%d, want 0 (second observation banks no deposit)", got)
	}
	if got := stationary.estimate(h); got != 1 {
		t.Fatalf("stationary policy estimate=%d, want 1 (doorkeeper only)", got)
	}
}
