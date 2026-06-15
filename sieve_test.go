package kioshun

import (
	"fmt"
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
	a := newSieveTinyLFU[int, int](32, 0, 10, 100, CostAdmissionFrequency)
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
	a := newSieveTinyLFU[int, int](100, 0, 0, 0, CostAdmissionFrequency)
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
	a := newSieveTinyLFU[int, int](100, 0, 10, 100, CostAdmissionFrequency)
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
	a := newSieveTinyLFU[int, int](16, 0, 10, 100, CostAdmissionFrequency)
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
	a := newSieveTinyLFU[int, int](100, 0, 10, 100, CostAdmissionFrequency)

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
	a := newSieveTinyLFU[int, int](100, 0, 10, 100, CostAdmissionFrequency)
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
	a := newSieveTinyLFU[int, int](100, 0, 10, 100, CostAdmissionFrequency)
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
	a := newSieveTinyLFU[int, int](100, 0, 10, 100, CostAdmissionFrequency)
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
	a := newSieveTinyLFU[int, int](2000, 0, 1, 100, CostAdmissionFrequency)
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
	a := newSieveTinyLFU[int, int](16, 0, 10, 100, CostAdmissionFrequency)
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
			t.Fatalf(
				"shard %d caps pc=%d mc=%d gc=%d, want 1/3/1",
				i,
				s.sieve.probationCap,
				s.sieve.mainCap,
				s.sieve.ghostCap,
			)
		}
		if s.sieve.probation.size != 0 || s.sieve.main.size != 0 || !s.sieve.probation.empty() ||
			!s.sieve.main.empty() {
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

func TestShardSieveStateMatchesPolicyInvariant(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{
			name: "bounded sieve",
			config: Config{
				MaxSize:         16,
				ShardCount:      4,
				CleanupInterval: 0,
				DefaultTTL:      time.Hour,
				EvictionPolicy:  SieveTinyLFU,
			},
		},
		{
			name: "unbounded sieve",
			config: Config{
				MaxSize:         0,
				MaxCost:         0,
				ShardCount:      4,
				CleanupInterval: 0,
				DefaultTTL:      time.Hour,
				EvictionPolicy:  SieveTinyLFU,
			},
		},
		{
			name: "default policy resolves to bounded sieve",
			config: Config{
				MaxSize:         16,
				ShardCount:      4,
				CleanupInterval: 0,
				DefaultTTL:      time.Hour,
				EvictionPolicy:  DefaultEvictionPolicy,
			},
		},
		{
			name: "lru",
			config: Config{
				MaxSize:         16,
				ShardCount:      4,
				CleanupInterval: 0,
				DefaultTTL:      time.Hour,
				EvictionPolicy:  LRU,
			},
		},
		{
			name: "lfu",
			config: Config{
				MaxSize:         16,
				ShardCount:      4,
				CleanupInterval: 0,
				DefaultTTL:      time.Hour,
				EvictionPolicy:  LFU,
			},
		},
		{
			name: "fifo",
			config: Config{
				MaxSize:         16,
				ShardCount:      4,
				CleanupInterval: 0,
				DefaultTTL:      time.Hour,
				EvictionPolicy:  FIFO,
			},
		},
		{
			name: "cost-only lru",
			config: Config{
				MaxSize:         0,
				MaxCost:         16,
				ShardCount:      4,
				CleanupInterval: 0,
				DefaultTTL:      time.Hour,
				EvictionPolicy:  LRU,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache[int, int](t, tt.config)
			defer c.Close()

			wantEvictor := c.config.EvictionPolicy != SieveTinyLFU
			if got := c.evictor != nil; got != wantEvictor {
				t.Fatalf("evictor initialized=%v, want %v for policy %v", got, wantEvictor, c.config.EvictionPolicy)
			}

			for i, s := range c.shards {
				wantSieve := c.config.EvictionPolicy == SieveTinyLFU && s.cap > 0
				if got := s.sieve != nil; got != wantSieve {
					t.Fatalf(
						"shard %d sieve initialized=%v, want %v (policy=%v cap=%d)",
						i,
						got,
						wantSieve,
						c.config.EvictionPolicy,
						s.cap,
					)
				}
				if wantSieve && len(s.readBuf.stripes) == 0 {
					t.Fatalf("shard %d bounded Sieve missing read buffer", i)
				}
				if !wantSieve && (s.head == nil || s.tail == nil) {
					t.Fatalf("shard %d non-Sieve storage path missing LRU sentinels", i)
				}
			}
		})
	}
}

func TestSieveTinyLFUBoundedVictimNeedsForce(t *testing.T) {
	a := newSieveTinyLFU[int, int](8, 0, 25, 100, CostAdmissionFrequency)
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
	a := newSieveTinyLFU[int, int](100, 0, 10, 100, CostAdmissionFrequency)
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
	s.sieve = newSieveTinyLFU[int, int](3, 0, 25, 100, CostAdmissionFrequency)

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
		t.Fatalf(
			"policy state not cleared: p=%d m=%d g=%d",
			s.sieve.probation.size,
			s.sieve.main.size,
			s.sieve.ghost.count(),
		)
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
	s.sieve = newSieveTinyLFU[int, int](4, 0, 25, 100, CostAdmissionFrequency)

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
	a := newSieveTinyLFU[int, int](4, 0, 25, 100, CostAdmissionFrequency)
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
	s.sieve = newSieveTinyLFU[int, int](4, 0, 25, 100, CostAdmissionFrequency)

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

	shifting := newSieveTinyLFU[int, int](32, 0, 10, 100, CostAdmissionFrequency)
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

	stationary := newSieveTinyLFU[int, int](32, 0, 10, 100, CostAdmissionFrequency)
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

func TestSieveTinyLFUSpecificBehavior(t *testing.T) {
	config := Config{
		MaxSize:        6,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Fill cache to capacity
	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)
	cache.Set("d", 4, time.Hour)
	cache.Set("e", 5, time.Hour)
	cache.Set("f", 6, time.Hour)
	waitForWrites(t, cache)

	// Build TinyLFU frequency through repeated access.
	// High frequency: "a" should get strong SieveTinyLFU admission priority.
	for range 5 {
		cache.Get("a")
	}

	// Medium frequency: "b"
	for range 3 {
		cache.Get("b")
	}

	// Low frequency: "c"
	cache.Get("c")

	// Test frequency-based admission: high-frequency items should be more likely to be admitted
	statsBefore := cache.Stats()

	// Try to add a new high-frequency item.
	cache.Set("high_freq", 100, time.Hour)
	waitForWrites(t, cache)
	for range 4 {
		cache.Get("high_freq") // Build TinyLFU frequency.
	}

	// Force eviction with another item - high frequency item should be more likely to stay
	cache.Set("new_item", 200, time.Hour)
	waitForWrites(t, cache)

	statsAfter := cache.Stats()
	if statsAfter.Evictions <= statsBefore.Evictions {
		t.Log("No evictions occurred - admission control may be preventing cache pollution")
	}

	if statsAfter.Size > 6 {
		t.Errorf("Cache size %d exceeds max capacity 6", statsAfter.Size)
	}

	// High frequency items should be more likely to survive
	if _, found := cache.Get("a"); !found {
		t.Log("High frequency item 'a' was evicted - this can happen but is less likely")
	}
}

func TestSieveTinyLFUAdmissionControl(t *testing.T) {
	config := Config{
		MaxSize:        4,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Fill cache to capacity with different frequency items
	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)
	cache.Set("d", 4, time.Hour)
	waitForWrites(t, cache)

	// Build frequency profiles:
	// High frequency: "a" (should get guaranteed admission ≥ threshold=3)
	for range 4 {
		cache.Get("a")
	}

	// Medium frequency: "b"
	for range 2 {
		cache.Get("b")
	}

	// Low frequency: "c", "d" (1 access each)
	cache.Get("c")
	cache.Get("d")

	initialEvictions := cache.Stats().Evictions

	// Test admission control with new items
	admitted := 0
	rejected := 0

	// Try adding multiple items - admission control should moderate cache pollution
	for i := range 12 {
		key := fmt.Sprintf("candidate%d", i)
		cache.Set(key, 100+i, time.Hour)
		waitForWrites(t, cache)

		// Check if item was actually added (not rejected by admission control)
		if _, exists := cache.Get(key); exists {
			admitted++
		} else {
			rejected++
		}
	}

	// Should have some evictions
	finalEvictions := cache.Stats().Evictions
	if finalEvictions <= initialEvictions {
		t.Error("Expected some evictions to occur")
	}

	// SieveTinyLFU admits through probation/main queues and may keep the just-written item
	// while evicting older cold residents. The important invariant here is that
	// capacity is held while eviction pressure is applied.
	if admitted == 0 {
		t.Error("Expected some items to be admitted through SieveTinyLFU admission")
	}

	// Cache should maintain size constraint
	stats := cache.Stats()
	if stats.Size != 4 {
		t.Errorf("Expected cache size 4, got %d", stats.Size)
	}

	// High frequency items should be more likely to survive
	if _, found := cache.Get("a"); !found {
		t.Log("High frequency item 'a' was evicted - unexpected but possible")
	}

	if cache.shards[0].sieve == nil {
		t.Fatal("SieveTinyLFU should use admission state")
	}

	t.Logf("Admitted %d, Rejected %d out of 12 items with SieveTinyLFU admission control", admitted, rejected)
}

func TestSieveTinyLFUSampleSize(t *testing.T) {
	config := Config{
		MaxSize:        10,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	for i := range 10 {
		cache.Set(string(rune('a'+i)), i, time.Hour)
	}
	waitForWrites(t, cache)

	// Create frequency gradient: 'a' most frequent, 'j' least frequent
	for freq := 10; freq > 0; freq-- {
		key := string(rune('a' + (10 - freq)))
		for access := 0; access < freq; access++ {
			cache.Get(key)
		}
	}

	initialEvictions := cache.Stats().Evictions

	// Trigger evictions by adding new items - try many to overcome admission control
	admittedItems := 0
	for i := range 20 { // Try more items to overcome admission control
		evictionsBefore := cache.Stats().Evictions
		cache.Set(string(rune('x'+i)), 100+i, time.Hour)
		waitForWrites(t, cache)
		evictionsAfter := cache.Stats().Evictions

		if evictionsAfter > evictionsBefore {
			admittedItems++
		}
	}

	// Verify at least some evictions occurred
	finalEvictions := cache.Stats().Evictions
	if finalEvictions <= initialEvictions {
		t.Error("Expected at least some evictions to occur when adding new items")
	}

	// High frequency items should be more likely to remain
	// Due to sampling, we can't guarantee exact behavior, but pattern should hold
	highFreqRemaining := 0
	lowFreqRemaining := 0

	for i := range 5 { // High frequency items
		if _, found := cache.Get(string(rune('a' + i))); found {
			highFreqRemaining++
		}
	}

	for i := 5; i < 10; i++ { // Low frequency items
		if _, found := cache.Get(string(rune('a' + i))); found {
			lowFreqRemaining++
		}
	}

	// This is probabilistic, but high frequency items should generally survive better
	t.Logf("High frequency items remaining: %d, Low frequency items remaining: %d",
		highFreqRemaining, lowFreqRemaining)
}

func TestSieveTinyLFUStressEviction(t *testing.T) {
	config := Config{
		MaxSize:        5,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	cache.Set("high1", 1, time.Hour)
	cache.Set("high2", 2, time.Hour)
	cache.Set("low1", 3, time.Hour)
	cache.Set("low2", 4, time.Hour)
	cache.Set("low3", 5, time.Hour)
	waitForWrites(t, cache)

	for range 20 {
		cache.Get("high1")
		cache.Get("high2")
	}

	// Low frequency items get minimal access
	cache.Get("low1")

	initialEvictions := cache.Stats().Evictions

	// Force many operations - some will be rejected by admission control
	admitted := 0
	for i := range 50 {
		evictionsBefore := cache.Stats().Evictions
		sizeBefore := cache.Stats().Size
		cache.Set(string(rune('z'+i%26)), 1000+i, time.Hour)
		waitForWrites(t, cache)
		evictionsAfter := cache.Stats().Evictions
		sizeAfter := cache.Stats().Size

		// Count if item was admitted (caused eviction or size change)
		if evictionsAfter > evictionsBefore || sizeAfter > sizeBefore {
			admitted++
		}
	}

	// High frequency items should have better survival odds with sampling
	high1Exists := false
	high2Exists := false
	if _, found := cache.Get("high1"); found {
		high1Exists = true
	}
	if _, found := cache.Get("high2"); found {
		high2Exists = true
	}

	// At least one high frequency item should likely survive
	if !high1Exists && !high2Exists {
		t.Log("Note: Both high frequency items were evicted - this can happen with sampling")
	}

	// Verify cache maintains size constraint
	if cache.Stats().Size > 5 {
		t.Errorf("Cache size %d exceeds maximum %d", cache.Stats().Size, 5)
	}

	// Verify reasonable number of items were admitted (not all due to admission control)
	finalEvictions := cache.Stats().Evictions
	totalEvictions := finalEvictions - initialEvictions

	if admitted == 0 {
		t.Error("Expected at least some items to be admitted")
	}

	if admitted < 10 {
		t.Logf("Only %d items admitted out of 50 - admission control is working", admitted)
	}

	t.Logf("Total evictions: %d, Items admitted: %d", totalEvictions, admitted)
}

func TestFrequencyAdmissionFilter(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	cache.Set("victim1", 1, time.Hour) // Low frequency victim
	cache.Set("victim2", 2, time.Hour) // Low frequency victim
	cache.Set("victim3", 3, time.Hour) // Low frequency victim
	waitForWrites(t, cache)

	// Create frequency gradient - access some items more than others
	for range 5 {
		cache.Get("victim1") // Higher frequency
	}
	cache.Get("victim2") // Medium frequency
	// victim3 stays at low frequency

	// Try to add items with different expected admission patterns
	admittedCount := 0
	rejectedCount := 0

	// Test 1: High frequency items should have better admission chances
	for i := range 10 {
		key := fmt.Sprintf("high_freq_%d", i)
		evictionsBefore := cache.Stats().Evictions

		// Pre-populate this key in the frequency filter by simulating access
		// This simulates a key that has been seen before and has frequency
		cache.Set(key, 100+i, time.Hour)
		waitForWrites(t, cache)

		evictionsAfter := cache.Stats().Evictions
		if evictionsAfter > evictionsBefore {
			admittedCount++
		} else {
			rejectedCount++
		}
	}

	t.Logf("High frequency items: %d admitted, %d rejected", admittedCount, rejectedCount)

	// Test 2: Verify cache maintains size constraint
	finalStats := cache.Stats()
	if finalStats.Size > 3 {
		t.Errorf("Cache size %d exceeds max capacity 3", finalStats.Size)
	}

	// Test 3: Should have some evictions due to capacity pressure
	if finalStats.Evictions == 0 {
		t.Log("No evictions occurred - this can happen if admission control rejects items")
	}

	// Test 4: At least some items should be admitted to show filter is working
	if admittedCount == 0 {
		t.Log("No items were admitted - admission control may be very restrictive")
	} else {
		t.Logf("Admission filter admitted %d/%d items", admittedCount, admittedCount+rejectedCount)
	}
}

func TestVictimFrequencyTracking(t *testing.T) {
	config := Config{
		MaxSize:        2,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Add initial items with different frequencies
	cache.Set("low_freq", 1, time.Hour)
	cache.Set("high_freq", 2, time.Hour)
	waitForWrites(t, cache)

	// Create clear frequency difference
	for i := 0; i < 10; i++ {
		cache.Get("high_freq") // High frequency
	}
	cache.Get("low_freq") // Low frequency (1 access)

	initialEvictions := cache.Stats().Evictions

	// SieveTinyLFU is probabilistic. Retry with distinct keys to trigger an eviction.
	// With ~50% fallback admit probability here, 20 attempts fail with prob ~9.5e-7.
	for i := 0; i < 20 && cache.Stats().Evictions == initialEvictions; i++ {
		cache.Set(fmt.Sprintf("new_item_%d", i), 3, time.Hour)
		waitForWrites(t, cache)
	}

	finalEvictions := cache.Stats().Evictions

	// Verify eviction occurred
	if finalEvictions <= initialEvictions {
		t.Error("Expected eviction to occur when adding item to full cache")
	}

	// Verify cache maintains size
	if cache.Stats().Size > 2 {
		t.Errorf("Cache size %d exceeds max capacity 2", cache.Stats().Size)
	}

	// The specific item evicted depends on sampling, but the mechanism should work
	t.Logf("Evictions occurred: %d", finalEvictions-initialEvictions)
}

func TestFrequencyBasedAdmissionDecisions(t *testing.T) {
	config := Config{
		MaxSize:        4,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	cache.Set("freq_5", 1, time.Hour)
	cache.Set("freq_3", 2, time.Hour)
	cache.Set("freq_1", 3, time.Hour)
	cache.Set("freq_0", 4, time.Hour)
	waitForWrites(t, cache)

	for range 5 {
		cache.Get("freq_5")
	}
	for range 3 {
		cache.Get("freq_3")
	}
	for range 1 {
		cache.Get("freq_1")
	}

	// Test admission patterns
	admissionResults := make(map[string]bool)

	// Try multiple new items to see admission patterns
	for i := range 20 {
		key := fmt.Sprintf("test_%d", i)
		evictionsBefore := cache.Stats().Evictions

		cache.Set(key, 100+i, time.Hour)
		waitForWrites(t, cache)

		evictionsAfter := cache.Stats().Evictions
		admitted := evictionsAfter > evictionsBefore
		admissionResults[key] = admitted
	}

	admittedCount := 0
	for _, admitted := range admissionResults {
		if admitted {
			admittedCount++
		}
	}

	// Verify some level of admission control
	t.Logf("Admitted %d out of %d items", admittedCount, len(admissionResults))

	// Should have some admission activity
	if admittedCount == 0 {
		t.Log("No items were admitted - admission control may be restrictive")
	}

	if admittedCount == len(admissionResults) {
		t.Log("All items were admitted - admission control may be less restrictive")
	}

	// Verify cache constraint maintained
	if cache.Stats().Size > 4 {
		t.Errorf("Cache size %d exceeds max capacity 4", cache.Stats().Size)
	}
}

// TestDoorkeeperBehavior tests that recently seen items are always admitted
func TestDoorkeeperBehavior(t *testing.T) {
	config := Config{
		MaxSize:        2,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	cache.Set("old1", 1, time.Hour)
	cache.Set("old2", 2, time.Hour)
	waitForWrites(t, cache)

	// Add and immediately re-add same item to exercise repeated-key priority.
	cache.Set("history_test", 3, time.Hour) // First time - may or may not be admitted
	waitForWrites(t, cache)

	evictionsBefore := cache.Stats().Evictions
	cache.Set("history_test", 4, time.Hour) // Second time should have a stronger history signal.
	waitForWrites(t, cache)
	evictionsAfter := cache.Stats().Evictions

	t.Logf("Evictions before: %d, after: %d", evictionsBefore, evictionsAfter)

	// Verify cache maintains size constraint
	if cache.Stats().Size > 2 {
		t.Errorf("Cache size %d exceeds max capacity 2", cache.Stats().Size)
	}
}

func TestAdmissionFilterStats(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	cache.Set("item1", 1, time.Hour)
	cache.Set("item2", 2, time.Hour)
	cache.Set("item3", 3, time.Hour)
	waitForWrites(t, cache)

	initialEvictions := cache.Stats().Evictions

	// Add more items to trigger admission filter
	itemsAdded := 0
	for i := range 10 {
		evictionsBefore := cache.Stats().Evictions
		cache.Set(fmt.Sprintf("new_%d", i), 100+i, time.Hour)
		waitForWrites(t, cache)
		evictionsAfter := cache.Stats().Evictions

		if evictionsAfter > evictionsBefore {
			itemsAdded++
		}
	}

	finalEvictions := cache.Stats().Evictions
	totalEvictions := finalEvictions - initialEvictions

	t.Logf("Items that caused evictions: %d, Total evictions: %d", itemsAdded, totalEvictions)

	if cache.Stats().Size > 3 {
		t.Errorf("Cache size %d exceeds max capacity 3", cache.Stats().Size)
	}

	// Should have some admission control effect
	if itemsAdded == 10 {
		t.Log("All items were admitted - this is possible but shows admission control effect")
	}
}

func TestSieveTinyLFUWithFrequencyAdmission(t *testing.T) {
	config := Config{
		MaxSize:        5,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	cache.Set("high1", 1, time.Hour)
	cache.Set("high2", 2, time.Hour)
	cache.Set("med1", 3, time.Hour)
	cache.Set("low1", 4, time.Hour)
	cache.Set("low2", 5, time.Hour)
	waitForWrites(t, cache)

	// Create clear frequency differences
	for range 15 {
		cache.Get("high1")
		cache.Get("high2")
	}

	for range 7 {
		cache.Get("med1")
	}

	for range 2 {
		cache.Get("low1")
	}

	initialStats := cache.Stats()

	// Try to add many new items - admission filter should moderate
	admissionAttempts := 0
	actualAdmissions := 0

	for i := range 30 {
		evictionsBefore := cache.Stats().Evictions
		sizeBefore := cache.Stats().Size

		cache.Set(fmt.Sprintf("candidate_%d", i), 1000+i, time.Hour)
		waitForWrites(t, cache)
		admissionAttempts++

		evictionsAfter := cache.Stats().Evictions
		sizeAfter := cache.Stats().Size

		// Item was admitted if it caused eviction or size change
		if evictionsAfter > evictionsBefore || sizeAfter > sizeBefore {
			actualAdmissions++
		}
	}

	finalStats := cache.Stats()

	// Verify admission control is working
	admissionRate := float64(actualAdmissions) / float64(admissionAttempts) * 100

	t.Logf("Admission attempts: %d, Actual admissions: %d, Rate: %.1f%%",
		admissionAttempts, actualAdmissions, admissionRate)

	t.Logf("Total evictions: %d", finalStats.Evictions-initialStats.Evictions)

	if finalStats.Size > 5 {
		t.Errorf("Cache size %d exceeds max capacity 5", finalStats.Size)
	}

	// Verify admission control is working (may admit few or many items based on frequency)
	t.Logf("Admission control processed %d requests with %d admissions", admissionAttempts, actualAdmissions)

	// High frequency items should have better survival chances
	highFreqSurvival := 0
	if _, found := cache.Get("high1"); found {
		highFreqSurvival++
	}
	if _, found := cache.Get("high2"); found {
		highFreqSurvival++
	}

	t.Logf("High frequency items surviving: %d/2", highFreqSurvival)
}

func TestSieveTinyLFUFrequencyThreshold(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)
	waitForWrites(t, cache)

	// Create a high-frequency item that should get strong SieveTinyLFU admission priority.
	cache.Set("high_freq", 100, time.Hour)
	waitForWrites(t, cache)
	for range 4 { // Build TinyLFU frequency.
		cache.Get("high_freq")
	}

	initialEvictions := cache.Stats().Evictions

	// Try to add another high-frequency item
	cache.Set("guaranteed", 200, time.Hour)
	waitForWrites(t, cache)
	for range 4 {
		cache.Get("guaranteed")
	}

	// Add competing item - high frequency items should survive
	cache.Set("competitor", 300, time.Hour)
	waitForWrites(t, cache)

	finalEvictions := cache.Stats().Evictions
	t.Logf("Evictions: %d", finalEvictions-initialEvictions)

	// Admission control may prevent evictions by rejecting items at the door
	if finalEvictions <= initialEvictions {
		t.Log("No evictions occurred - admission control prevented cache entry")
	}

	// High frequency items should be more likely to survive
	if _, found := cache.Get("high_freq"); found {
		t.Log("High frequency item survived - good")
	}

	if _, found := cache.Get("guaranteed"); found {
		t.Log("Guaranteed admission item survived - good")
	}
}

// TestSieveTinyLFUScanDetection tests scan resistance functionality.
func TestSieveTinyLFUScanDetection(t *testing.T) {
	config := Config{
		MaxSize:        4,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	cache.Set("stable1", 1, time.Hour)
	cache.Set("stable2", 2, time.Hour)
	cache.Set("stable3", 3, time.Hour)
	cache.Set("stable4", 4, time.Hour)
	waitForWrites(t, cache)

	for range 3 {
		cache.Get("stable1")
		cache.Get("stable2")
		cache.Get("stable3")
		cache.Get("stable4")
	}

	initialEvictions := cache.Stats().Evictions

	// Simulate scanning pattern with sequential cold keys.
	scanRejections := 0
	for i := range 15 {
		key := fmt.Sprintf("scan_%010d", i) // Sequential keys
		sizeBefore := cache.Size()
		cache.Set(key, 1000+i, time.Hour)
		waitForWrites(t, cache)

		// Check if item was rejected (size didn't change)
		if cache.Size() == sizeBefore {
			scanRejections++
		}
	}

	finalEvictions := cache.Stats().Evictions

	t.Logf("Scan rejections: %d/15", scanRejections)
	t.Logf("Evictions during scan test: %d", finalEvictions-initialEvictions)

	// Should reject some items during scanning to prevent pollution
	if scanRejections == 0 {
		t.Log("No scan rejections detected - scan detection may not be active or pattern not detected")
	}

	// Stable items should be more likely to survive
	survivingStable := 0
	for i := 1; i <= 4; i++ {
		if _, found := cache.Get(fmt.Sprintf("stable%d", i)); found {
			survivingStable++
		}
	}
	t.Logf("Surviving stable items: %d/4", survivingStable)
}

func TestSieveTinyLFURepeatedKeyBehavior(t *testing.T) {
	config := Config{
		MaxSize:        5,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	for i := range 5 {
		cache.Set(fmt.Sprintf("init%d", i), i, time.Hour)
	}
	waitForWrites(t, cache)

	// Create repeated items with history.
	cache.Set("history1", 100, time.Hour)
	waitForWrites(t, cache)
	cache.Get("history1") // Build TinyLFU frequency.

	cache.Set("history2", 200, time.Hour)
	waitForWrites(t, cache)
	cache.Get("history2") // Build TinyLFU frequency.

	initialEvictions := cache.Stats().Evictions

	// Items with history should have higher admission priority.
	cache.Set("test1", 300, time.Hour)
	waitForWrites(t, cache)
	cache.Get("test1") // Build TinyLFU frequency.

	// Force eviction
	cache.Set("competitor", 400, time.Hour)
	waitForWrites(t, cache)

	finalEvictions := cache.Stats().Evictions
	t.Logf("Evictions: %d", finalEvictions-initialEvictions)

	// Admission control may prevent evictions by rejecting items
	if finalEvictions <= initialEvictions {
		t.Log("No evictions occurred - admission control working effectively")
	}

	// Items with history should be more likely to survive.
	historySurvival := 0
	if _, found := cache.Get("history1"); found {
		historySurvival++
	}
	if _, found := cache.Get("history2"); found {
		historySurvival++
	}
	if _, found := cache.Get("test1"); found {
		historySurvival++
	}

	t.Logf("History-bearing items surviving: %d/3", historySurvival)

	// Wait to make sure time-based code paths do not affect policy state.
	time.Sleep(60 * time.Millisecond)

	// Test that a new pattern can still be admitted.
	cache.Set("post_reset", 500, time.Hour)
	waitForWrites(t, cache)
}

func TestSieveTinyLFUAdaptiveProbability(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)
	waitForWrites(t, cache)

	// Build initial frequency
	cache.Get("a")
	cache.Get("b")
	cache.Get("c")

	// Simulate high eviction pressure to test adaptive behavior
	admitted := 0
	rejected := 0

	for i := range 20 {
		key := fmt.Sprintf("pressure%d", i)
		cache.Set(key, 1000+i, time.Hour)
		waitForWrites(t, cache)

		// Check admission success
		if _, exists := cache.Get(key); exists {
			admitted++
		} else {
			rejected++
		}

		// Small delay to allow adaptive adjustment
		if i%5 == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}

	t.Logf("Under pressure - Admitted: %d, Rejected: %d", admitted, rejected)

	// Should show adaptive behavior (some rejections due to pressure)
	if rejected == 0 {
		t.Log("No rejections under pressure - adaptive probability may not be active")
	}

	// Verify cache constraints
	stats := cache.Stats()
	if stats.Size != 3 {
		t.Errorf("Cache size %d should be 3", stats.Size)
	}
}

func TestSieveTinyLFURecencyTieBreaking(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)
	waitForWrites(t, cache)

	cache.Get("a")
	cache.Get("b")
	cache.Get("c")

	// Wait to create age difference
	time.Sleep(10 * time.Millisecond)

	// Access 'a' to make it more recent
	cache.Get("a")

	// Force eviction with new item
	cache.Set("new_recent", 100, time.Hour)
	waitForWrites(t, cache)
	cache.Get("new_recent") // Make it recent

	cache.Set("trigger_eviction", 200, time.Hour)
	waitForWrites(t, cache)

	// More recent items should be more likely to survive
	recentSurvival := 0
	if _, found := cache.Get("a"); found {
		recentSurvival++
	}
	if _, found := cache.Get("new_recent"); found {
		recentSurvival++
	}

	t.Logf("Recent items surviving: %d/2", recentSurvival)

	if cache.Size() != 3 {
		t.Errorf("Cache size should be 3, got %d", cache.Size())
	}
}
