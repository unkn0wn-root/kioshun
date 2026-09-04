package kioshun

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

type evictRecorder struct {
	mu      sync.Mutex
	seen    map[int]string
	reasons map[int]RemovalReason
	repeats int // notifications for a key already recorded ('should' never happen)
}

func newEvictRecorder() *evictRecorder {
	return &evictRecorder{seen: make(map[int]string), reasons: make(map[int]RemovalReason)}
}

func (r *evictRecorder) record(k int, v string, reason RemovalReason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.seen[k]; ok {
		r.repeats++
	}
	r.seen[k] = v
	r.reasons[k] = reason
}

func (r *evictRecorder) reason(k int) (RemovalReason, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rsn, ok := r.reasons[k]
	return rsn, ok
}

func (r *evictRecorder) allReasons() []RemovalReason {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RemovalReason, 0, len(r.reasons))
	for _, rsn := range r.reasons {
		out = append(out, rsn)
	}
	return out
}

func (r *evictRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func (r *evictRecorder) value(k int) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.seen[k]
	return v, ok
}

type capacityEvictRecorder struct {
	mu   sync.Mutex
	seen map[int]string
}

func newCapacityEvictRecorder() *capacityEvictRecorder {
	return &capacityEvictRecorder{seen: make(map[int]string)}
}

func (r *capacityEvictRecorder) record(k int, v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen[k] = v
}

func (r *capacityEvictRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func (r *capacityEvictRecorder) value(k int) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.seen[k]
	return v, ok
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within deadline")
}

func valFor(i int) string { return fmt.Sprintf("v%d", i) }

func TestOnEvict_CapacityOnly(t *testing.T) {
	rec := newCapacityEvictRecorder()
	c, err := New(
		Config{MaxSize: 1, ShardCount: 1, CleanupInterval: 0, EvictionPolicy: LRU, StatsEnabled: true},
		WithOnEvict(rec.record),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if mask := c.shards[0].removeNotifyMask; mask != notifyRemovedCapacity {
		t.Fatalf("notify mask = %b, want capacity only", mask)
	}

	if err := c.Set(1, "one", NoExpiration); err != nil {
		t.Fatalf("Set(1): %v", err)
	}
	if err := c.Set(2, "two", NoExpiration); err != nil {
		t.Fatalf("Set(2): %v", err)
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	waitFor(t, func() bool {
		v, ok := rec.value(1)
		return ok && v == "one"
	})

	if !c.Delete(2) {
		t.Fatal("Delete(2) = false, want true")
	}
	if err := c.Set(3, "three", 10*time.Millisecond); err != nil {
		t.Fatalf("Set(3): %v", err)
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync after Set(3): %v", err)
	}
	time.Sleep(25 * time.Millisecond)
	if _, ok := c.Get(3); ok {
		t.Fatal("Get(3) returned a value after expiry")
	}

	time.Sleep(50 * time.Millisecond)
	if n := rec.count(); n != 1 {
		t.Fatalf("OnEvict delivered %d notifications, want only one capacity eviction", n)
	}
}

func TestOnRemoveAndOnEvict_BothFireForCapacity(t *testing.T) {
	removals := newEvictRecorder()
	evictions := newCapacityEvictRecorder()
	c, err := New(
		Config{MaxSize: 1, ShardCount: 1, CleanupInterval: 0, EvictionPolicy: LRU, StatsEnabled: true},
		WithOnRemove(removals.record),
		WithOnEvict(evictions.record),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if mask := c.shards[0].removeNotifyMask; mask != notifyAllRemovals {
		t.Fatalf("notify mask = %b, want all removals", mask)
	}

	if err := c.Set(1, "one", NoExpiration); err != nil {
		t.Fatalf("Set(1): %v", err)
	}
	if err := c.Set(2, "two", NoExpiration); err != nil {
		t.Fatalf("Set(2): %v", err)
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	waitFor(t, func() bool {
		_, removed := removals.value(1)
		_, evicted := evictions.value(1)
		return removed && evicted
	})
	if rsn, _ := removals.reason(1); rsn != RemovedCapacity {
		t.Fatalf("removal reason = %v, want capacity", rsn)
	}
}

func TestOnRemove_CapacityEvictionAccounting(t *testing.T) {
	for _, policy := range []EvictionPolicy{LRU, LFU, FIFO, SieveTinyLFU} {
		t.Run(policyName(policy), func(t *testing.T) {
			rec := newEvictRecorder()
			c, err := New(
				Config{MaxSize: 200, ShardCount: 4, EvictionPolicy: policy, StatsEnabled: true},
				WithOnRemove(rec.record),
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer c.Close()

			const total = 5000
			for i := range total {
				if err := c.Set(i, valFor(i), NoExpiration); err != nil {
					t.Fatalf("Set: %v", err)
				}
			}
			if err := c.Sync(); err != nil {
				t.Fatalf("Sync: %v", err)
			}

			waitFor(t, func() bool {
				return int64(rec.count())+c.Size() == total
			})

			if rec.repeats != 0 {
				t.Fatalf("got %d duplicate eviction notifications", rec.repeats)
			}
			// Only SieveTinyLFU can reject a candidate instead of evicting a resident.
			for _, rsn := range rec.allReasons() {
				switch policy {
				case SieveTinyLFU:
					if rsn != RemovedCapacity && rsn != RemovedRejected {
						t.Fatalf("Sieve eviction reason = %v, want capacity or rejected", rsn)
					}
				default:
					if rsn != RemovedCapacity {
						t.Fatalf("%s eviction reason = %v, want capacity", policyName(policy), rsn)
					}
				}
			}
			for k := 0; k < total; k += 137 {
				if v, ok := rec.value(k); ok && v != valFor(k) {
					t.Fatalf("evicted key %d carried value %q, want %q", k, v, valFor(k))
				}
			}
		})
	}
}

func TestOnRemove_Delete(t *testing.T) {
	rec := newEvictRecorder()
	c, err := New(
		Config{MaxSize: 100, ShardCount: 2, StatsEnabled: true},
		WithOnRemove(rec.record),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if err := c.Set(7, "seven", NoExpiration); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !c.Delete(7) {
		t.Fatal("Delete(7) = false, want true")
	}

	waitFor(t, func() bool {
		v, ok := rec.value(7)
		return ok && v == "seven"
	})
	if rsn, _ := rec.reason(7); rsn != RemovedDeleted {
		t.Fatalf("Delete reason = %v, want deleted", rsn)
	}
}

func TestOnRemove_TTLExpiration(t *testing.T) {
	rec := newEvictRecorder()
	c, err := New(
		Config{MaxSize: 100, ShardCount: 2, CleanupInterval: 5 * time.Millisecond, StatsEnabled: true},
		WithOnRemove(rec.record),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if err := c.Set(3, "three", 15*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	waitFor(t, func() bool {
		v, ok := rec.value(3)
		return ok && v == "three"
	})
	if rsn, _ := rec.reason(3); rsn != RemovedExpired {
		t.Fatalf("background expiration reason = %v, want expired", rsn)
	}
}

func TestOnRemove_ExpirationOnAccess(t *testing.T) {
	rec := newEvictRecorder()
	// With cleanup disabled, Get must remove the expired entry.
	c, err := New(
		Config{MaxSize: 100, ShardCount: 2, CleanupInterval: 0, EvictionPolicy: LRU, StatsEnabled: true},
		WithOnRemove(rec.record),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if err := c.Set(9, "nine", 10*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	time.Sleep(25 * time.Millisecond)
	if _, ok := c.Get(9); ok {
		t.Fatal("Get(9) returned a value after expiry")
	}

	waitFor(t, func() bool {
		v, ok := rec.value(9)
		return ok && v == "nine"
	})
	if rsn, _ := rec.reason(9); rsn != RemovedExpired {
		t.Fatalf("lazy expiration reason = %v, want expired", rsn)
	}
}

func TestOnRemove_NotFiredOnOverwrite(t *testing.T) {
	rec := newEvictRecorder()
	c, err := New(
		Config{MaxSize: 100, ShardCount: 2, StatsEnabled: true},
		WithOnRemove(rec.record),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	for i := range 50 {
		if err := c.Set(1, valFor(i), NoExpiration); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Wait long enough to catch an incorrect asynchronous notification.
	time.Sleep(50 * time.Millisecond)
	if n := rec.count(); n != 0 {
		t.Fatalf("overwrite produced %d eviction notifications, want 0", n)
	}
}

func TestOnRemove_NotFiredOnClear(t *testing.T) {
	rec := newEvictRecorder()
	c, err := New(
		Config{MaxSize: 100, ShardCount: 2, StatsEnabled: true},
		WithOnRemove(rec.record),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	for i := range 30 {
		if err := c.Set(i, valFor(i), NoExpiration); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	c.Clear()

	time.Sleep(50 * time.Millisecond)
	if n := rec.count(); n != 0 {
		t.Fatalf("Clear produced %d eviction notifications, want 0", n)
	}
}

func policyName(p EvictionPolicy) string {
	switch p {
	case LRU:
		return "LRU"
	case LFU:
		return "LFU"
	case FIFO:
		return "FIFO"
	case SieveTinyLFU:
		return "SieveTinyLFU"
	default:
		return fmt.Sprintf("policy(%d)", p)
	}
}

func TestAllEvictionPolicies(t *testing.T) {
	policies := []EvictionPolicy{LRU, LFU, FIFO, SieveTinyLFU}
	policyNames := []string{"LRU", "LFU", "FIFO", "SieveTinyLFU"}

	for i, policy := range policies {
		t.Run(policyNames[i], func(t *testing.T) {
			config := Config{
				MaxSize:         5,
				ShardCount:      2,
				CleanupInterval: 0,
				DefaultTTL:      0,
				EvictionPolicy:  policy,
				StatsEnabled:    true,
			}

			cache := newTestCache[string, int](t, config)
			defer cache.Close()

			for j := range 10 {
				cache.Set(string(rune('a'+j)), j, time.Hour)
			}
			waitForWrites(t, cache)

			stats := cache.Stats()
			if stats.Size > 5 {
				t.Errorf("Cache size %d exceeds max capacity 5 for policy %s", stats.Size, policyNames[i])
			}

			if stats.Evictions == 0 && policies[i] != SieveTinyLFU {
				t.Errorf("Expected evictions for policy %s, got 0", policyNames[i])
			}

			if policies[i] == SieveTinyLFU && stats.Evictions == 0 {
				t.Logf("SieveTinyLFU prevented evictions through admission control - this is correct behavior")
			}

			cache.Set("test", 999, time.Hour)
			waitForWrites(t, cache)

			if policies[i] == SieveTinyLFU {
				cache.Get("test")
				cache.Set("test", 999, time.Hour)
				waitForWrites(t, cache)
			}

			if val, found := cache.Get("test"); !found || val != 999 {
				if policies[i] == SieveTinyLFU {
					t.Logf("Test item was rejected by SieveTinyLFU admission control - expected behavior")
				} else {
					t.Errorf("Cache not working for policy %s", policyNames[i])
				}
			}
		})
	}
}

func TestLFUSpecificBehavior(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: LFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)
	waitForWrites(t, cache)

	for i := 0; i < 5; i++ {
		cache.Get("a")
	}

	cache.Get("b")
	cache.Get("b")

	cache.Set("d", 4, time.Hour)
	waitForWrites(t, cache)

	if _, found := cache.Get("c"); found {
		t.Error("Item 'c' should have been evicted (LFU)")
	}
	if _, found := cache.Get("a"); !found {
		t.Error("Item 'a' should not have been evicted (high frequency)")
	}
	if _, found := cache.Get("b"); !found {
		t.Error("Item 'b' should not have been evicted (medium frequency)")
	}
}

func TestLRUSpecificBehavior(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: LRU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)
	waitForWrites(t, cache)

	cache.Get("a")

	cache.Set("d", 4, time.Hour)
	waitForWrites(t, cache)

	if _, found := cache.Get("b"); found {
		t.Error("Item 'b' should have been evicted (LRU)")
	}
	if _, found := cache.Get("a"); !found {
		t.Error("Item 'a' should not have been evicted (recently accessed)")
	}
	if _, found := cache.Get("c"); !found {
		t.Error("Item 'c' should not have been evicted")
	}
}
