package kioshun

import (
	"errors"
	"testing"
	"time"
)

func TestWeightedLRUTracksCostAndEvictsToMaxCost(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSize = 100
	cfg.MaxCost = 10
	cfg.ShardCount = 1
	cfg.CleanupInterval = 0
	cfg.EvictionPolicy = LRU

	cache, err := New[int, int](cfg, WithWeigher(func(_ int, value int) int64 {
		return int64(value)
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer cache.Close()

	for _, key := range []int{1, 2, 3} {
		if err := cache.Set(key, 4, time.Hour); err != nil {
			t.Fatalf("Set(%d) error = %v", key, err)
		}
	}

	if cost := cache.Cost(); cost > cfg.MaxCost {
		t.Fatalf("cost=%d exceeds max cost=%d", cost, cfg.MaxCost)
	}
	if _, ok := cache.Get(1); ok {
		t.Fatal("oldest key survived weighted LRU eviction")
	}
	if stats := cache.Stats(); stats.Cost != cache.Cost() || stats.MaxCost != cfg.MaxCost {
		t.Fatalf("stats cost/max=%d/%d want %d/%d", stats.Cost, stats.MaxCost, cache.Cost(), cfg.MaxCost)
	}
}

func TestDefaultCacheDoesNotTrackCostMetadata(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSize = 10
	cfg.ShardCount = 1
	cfg.CleanupInterval = 0

	cache, err := New[int, int](cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer cache.Close()

	for key := 0; key < 4; key++ {
		if err := cache.Set(key, key, time.Hour); err != nil {
			t.Fatalf("Set(%d) error = %v", key, err)
		}
	}

	if cache.trackCost {
		t.Fatal("default cache enabled cost tracking")
	}
	if got := cache.Cost(); got != cache.Size() {
		t.Fatalf("Cost()=%d, want Size()=%d when MaxCost and WithWeigher are unset", got, cache.Size())
	}
	for _, shard := range cache.shards {
		shard.mu.RLock()
		for key, item := range shard.data {
			if item.cost != 0 {
				shard.mu.RUnlock()
				t.Fatalf("key %d carries cost metadata cost=%d in default cache", key, item.cost)
			}
		}
		shard.mu.RUnlock()
	}
}

func TestWeightedSetRejectsInvalidAndTooLargeCosts(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSize = 10
	cfg.MaxCost = 10
	cfg.ShardCount = 1
	cfg.CleanupInterval = 0

	tooLarge, err := New[int, int](cfg, WithWeigher(func(_ int, value int) int64 {
		return int64(value)
	}))
	if err != nil {
		t.Fatalf("New() tooLarge error = %v", err)
	}
	defer tooLarge.Close()
	if err := tooLarge.Set(1, 11, time.Hour); !errors.Is(err, ErrItemTooLarge) {
		t.Fatalf("Set too large error=%v, want ErrItemTooLarge", err)
	}

	negative, err := New[int, int](cfg, WithWeigher(func(int, int) int64 {
		return -1
	}))
	if err != nil {
		t.Fatalf("New() negative error = %v", err)
	}
	defer negative.Close()
	if err := negative.Set(1, 1, time.Hour); !errors.Is(err, ErrInvalidCost) {
		t.Fatalf("Set negative cost error=%v, want ErrInvalidCost", err)
	}
}

func TestUnboundedSieveAllowsNoEviction(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSize = 0
	cfg.MaxCost = 0
	cfg.ShardCount = 1
	cfg.CleanupInterval = 0
	cfg.EvictionPolicy = SieveTinyLFU

	cache, err := New[int, int](cfg)
	if err != nil {
		t.Fatalf("New() unbounded Sieve error = %v", err)
	}
	defer cache.Close()

	// No capacity bound means nothing to size or evict, so the policy state is
	// never allocated and the cache grows without dropping entries.
	if cache.shards[0].sieve != nil {
		t.Fatal("unbounded Sieve shard allocated policy state")
	}
	for key := 0; key < 50; key++ {
		if err := cache.Set(key, key, time.Hour); err != nil {
			t.Fatalf("Set(%d) error = %v", key, err)
		}
	}
	if got := cache.Size(); got != 50 {
		t.Fatalf("unbounded cache size=%d, want 50 (no eviction)", got)
	}
}

func TestCostOnlyLRUEnforcesBudget(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSize = 0
	cfg.MaxCost = 10
	cfg.ShardCount = 1
	cfg.CleanupInterval = 0
	cfg.EvictionPolicy = LRU

	// Non-Sieve policies size from a tail-eviction list, not a frequency sketch,
	// so a cost-only budget without MaxSize is valid for them.
	cache, err := New[int, int](cfg, WithWeigher(func(_ int, value int) int64 {
		return int64(value)
	}))
	if err != nil {
		t.Fatalf("New() cost-only LRU error = %v", err)
	}
	defer cache.Close()

	for _, key := range []int{1, 2, 3} {
		if err := cache.Set(key, 4, time.Hour); err != nil {
			t.Fatalf("Set(%d) error = %v", key, err)
		}
	}
	if cost := cache.Cost(); cost > cfg.MaxCost {
		t.Fatalf("cost-only LRU cost=%d exceeds max cost=%d", cost, cfg.MaxCost)
	}
}

func TestWeightedMaxCostNormalizesShardCount(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSize = 100
	cfg.MaxCost = 3
	cfg.CleanupInterval = 0
	cfg.EvictionPolicy = LRU

	cache, err := New[int, int](cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer cache.Close()

	if len(cache.shards) > int(cfg.MaxCost) {
		t.Fatalf("shards=%d exceeds MaxCost=%d and would create zero-budget shards", len(cache.shards), cfg.MaxCost)
	}
	for i, shard := range cache.shards {
		if shard.costCap <= 0 {
			t.Fatalf("shard %d costCap=%d, want positive cost budget", i, shard.costCap)
		}
	}
}

func TestWeightedSieveEnforcesCostDuringWarmup(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSize = 100
	cfg.MaxCost = 10
	cfg.ShardCount = 1
	cfg.CleanupInterval = 0
	cfg.EvictionPolicy = SieveTinyLFU

	cache, err := New[int, int](cfg, WithWeigher(func(_ int, value int) int64 {
		return int64(value)
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer cache.Close()

	for _, key := range []int{1, 2, 3} {
		if err := cache.Set(key, 4, time.Hour); err != nil {
			t.Fatalf("Set(%d) error = %v", key, err)
		}
	}
	if cost := cache.Cost(); cost > cfg.MaxCost {
		t.Fatalf("Sieve cost=%d exceeds max cost=%d", cost, cfg.MaxCost)
	}
}

func TestSieveCostAdmissionScores(t *testing.T) {
	// Cost-aware modes read the denominator from item.cost; the smaller, denser
	// candidate (cost 1) should beat the larger victim (cost 64) once cost weights
	// the frequencies, while frequency-only admission keeps the hotter victim.
	in := &cacheItem[int, int]{key: 1, hash: 1, cost: 1}
	victim := &cacheItem[int, int]{key: 2, hash: 2, cost: 64}

	frequency := newSieveTinyLFU[int, int](8, 25, 100, CostAdmissionFrequency)
	incrementSieveFrequency(frequency, in.hash, 2)
	incrementSieveFrequency(frequency, victim.hash, 3)
	if frequency.shouldAdmit(in, victim, false) {
		t.Fatal("frequency-only admission should keep the higher-frequency victim")
	}

	density := newSieveTinyLFU[int, int](8, 25, 100, CostAdmissionDensity)
	incrementSieveFrequency(density, in.hash, 2)
	incrementSieveFrequency(density, victim.hash, 3)
	if !density.shouldAdmit(in, victim, false) {
		t.Fatal("density admission should admit the smaller dense candidate")
	}

	balanced := newSieveTinyLFU[int, int](8, 25, 100, CostAdmissionBalanced)
	incrementSieveFrequency(balanced, in.hash, 2)
	incrementSieveFrequency(balanced, victim.hash, 3)
	if !balanced.shouldAdmit(in, victim, false) {
		t.Fatal("balanced admission should admit the smaller candidate in this score range")
	}
}

func incrementSieveFrequency[K comparable, V any](p *sieveTinyLFU[K, V], h uint64, n int) {
	for i := 0; i < n; i++ {
		p.incrementFrequency(h)
	}
}
