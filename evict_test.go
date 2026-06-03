package kioshun

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// evictRecorder is a concurrency-safe sink for OnRemove notifications.
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

// waitFor polls cond until it holds or the deadline passes; removal
// notifications are delivered asynchronously, so tests cannot assert immediately.
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

// Every distinct key inserted must end up either resident or reported exactly
// once to OnRemove; the two sets never overlap and never lose a key.
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
			// Set with NoExpiration and no Delete, so every notification is a
			// capacity removal — except SieveTinyLFU, which may also reject a
			// candidate that loses admission (RemovedRejected).
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
			// Spot-check that delivered values match their keys.
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

	// The background cleanup worker expires the entry and notifies.
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
	// CleanupInterval 0 disables the background sweeper, so the only way the
	// expired entry leaves is the lazy removal on Get.
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

// Replacing an existing key's value keeps the key resident, so it must not
// produce an eviction notification.
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
	// Allow any (erroneous) notification to be delivered before asserting.
	time.Sleep(50 * time.Millisecond)
	if n := rec.count(); n != 0 {
		t.Fatalf("overwrite produced %d eviction notifications, want 0", n)
	}
}

// Clear drops every entry in bulk and must not deliver per-key notifications.
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
