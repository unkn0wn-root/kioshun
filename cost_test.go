package kioshun

import (
	"errors"
	"strings"
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

	for key := range 4 {
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
		var badKey, badCost int64
		bad := false
		shard.tab.forEach(func(item *cacheItem[int, int]) bool {
			if item.cost != 0 {
				badKey, badCost, bad = int64(item.key), item.cost, true
				return false
			}
			return true
		})
		shard.mu.RUnlock()
		if bad {
			t.Fatalf("key %d carries cost metadata cost=%d in default cache", badKey, badCost)
		}
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

	// An unbounded cache does not allocate eviction policy state.
	if cache.shards[0].sieve != nil {
		t.Fatal("unbounded Sieve shard allocated policy state")
	}
	for key := range 50 {
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

	// Policies other than SieveTinyLFU support a cost-only limit.
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
	cfg.ShardCount = 8
	cfg.CleanupInterval = 0
	cfg.EvictionPolicy = LRU

	cache, err := New[int, int](cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer cache.Close()

	if len(cache.shards) != 2 {
		t.Fatalf("shards=%d, want 2: MaxCost=%d must cap ShardCount=%d to avoid zero-budget shards", len(cache.shards), cfg.MaxCost, cfg.ShardCount)
	}
	for i, shard := range cache.shards {
		if shard.costCap <= 0 {
			t.Fatalf("shard %d costCap=%d, want positive cost budget", i, shard.costCap)
		}
	}
}

func newShardBudgetTestCache(t *testing.T) *Cache[string, []byte] {
	t.Helper()
	cfg := Config{MaxSize: 10000, MaxCost: 1 << 20, ShardCount: 64}
	c, err := New[string, []byte](cfg, WithWeigher(func(_ string, v []byte) int64 {
		return int64(len(v))
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

func TestItemTooLargeReportsShardBudget(t *testing.T) {
	c := newShardBudgetTestCache(t)
	defer c.Close()

	err := c.Set("big", make([]byte, 64<<10), NoExpiration)
	if !errors.Is(err, ErrItemTooLarge) {
		t.Fatalf("Set error = %v, want ErrItemTooLarge", err)
	}
	if !strings.Contains(err.Error(), "budget 16384") {
		t.Fatalf("Set error = %q, want it to report budget 16384", err)
	}
}

func TestItemCostLimitIsPerShard(t *testing.T) {
	c := newShardBudgetTestCache(t)
	defer c.Close()

	if err := c.Set("fits", make([]byte, 16384), NoExpiration); err != nil {
		t.Fatalf("Set cost 16384 error = %v, want nil", err)
	}
	if err := c.Set("over", make([]byte, 16385), NoExpiration); !errors.Is(err, ErrItemTooLarge) {
		t.Fatalf("Set cost 16385 error = %v, want ErrItemTooLarge", err)
	}
}

func TestSetNegativeTTLSkipsCostChecks(t *testing.T) {
	// A nil value has an invalid cost.
	c, err := New[string, []byte](Config{MaxSize: 64, MaxCost: 100, ShardCount: 1}, WithWeigher(func(_ string, v []byte) int64 {
		if v == nil {
			return -1
		}
		return int64(len(v))
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.Close()

	tests := []struct {
		name  string
		value []byte
	}{
		{"too large", make([]byte, 101)},
		{"negative cost", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := c.Set("k", []byte("old"), time.Hour); err != nil {
				t.Fatal(err)
			}
			if err := c.Set("k", tt.value, -time.Second); err != nil {
				t.Fatalf("Set with negative TTL error = %v, want nil", err)
			}
			if got := c.Size(); got != 0 {
				t.Fatalf("Size()=%d, want 0", got)
			}
			if _, ok := c.Get("k"); ok {
				t.Fatal("old entry survived Set with a negative TTL")
			}
		})
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
	// Weighted modes should prefer the smaller candidate; frequency-only mode
	// should keep the more frequently accessed victim.
	in := &cacheItem[int, int]{key: 1, hash: 1, cost: 1}
	victim := &cacheItem[int, int]{key: 2, hash: 2, cost: 64}

	frequency := newSieveTinyLFU[int, int](8, 0, 25, 100, CostAdmissionFrequency)
	incrementSieveFrequency(frequency, in.hash, 2)
	incrementSieveFrequency(frequency, victim.hash, 3)
	if frequency.shouldAdmit(in, victim, false) {
		t.Fatal("frequency-only admission should keep the higher-frequency victim")
	}

	density := newSieveTinyLFU[int, int](8, 0, 25, 100, CostAdmissionDensity)
	incrementSieveFrequency(density, in.hash, 2)
	incrementSieveFrequency(density, victim.hash, 3)
	if !density.shouldAdmit(in, victim, false) {
		t.Fatal("density admission should admit the smaller dense candidate")
	}

	balanced := newSieveTinyLFU[int, int](8, 0, 25, 100, CostAdmissionBalanced)
	incrementSieveFrequency(balanced, in.hash, 2)
	incrementSieveFrequency(balanced, victim.hash, 3)
	if !balanced.shouldAdmit(in, victim, false) {
		t.Fatal("balanced admission should admit the smaller candidate in this score range")
	}
}

func incrementSieveFrequency[K comparable, V any](p *sieveTinyLFU[K, V], h uint64, n int) {
	for range n {
		p.incrementFrequency(h)
	}
}
