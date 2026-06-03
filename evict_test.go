package kioshun

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// evictRecorder is a concurrency-safe sink for OnEvict notifications.
type evictRecorder struct {
	mu      sync.Mutex
	seen    map[int]string
	repeats int // notifications for a key already recorded ('should' never happen)
}

func newEvictRecorder() *evictRecorder {
	return &evictRecorder{seen: make(map[int]string)}
}

func (r *evictRecorder) record(k int, v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.seen[k]; ok {
		r.repeats++
	}
	r.seen[k] = v
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

// waitFor polls cond until it holds or the deadline passes; eviction
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

// Every distinct key inserted must end up either resident or reported exactly
// once to OnEvict; the two sets never overlap and never lose a key.
func TestOnEvict_CapacityEvictionAccounting(t *testing.T) {
	for _, policy := range []EvictionPolicy{LRU, LFU, FIFO, SieveTinyLFU} {
		t.Run(policyName(policy), func(t *testing.T) {
			rec := newEvictRecorder()
			c, err := New(
				Config{MaxSize: 200, ShardCount: 4, EvictionPolicy: policy, StatsEnabled: true},
				WithOnEvict(rec.record),
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
			// Spot-check that delivered values match their keys.
			for k := 0; k < total; k += 137 {
				if v, ok := rec.value(k); ok && v != valFor(k) {
					t.Fatalf("evicted key %d carried value %q, want %q", k, v, valFor(k))
				}
			}
		})
	}
}

func TestOnEvict_Delete(t *testing.T) {
	rec := newEvictRecorder()
	c, err := New(
		Config{MaxSize: 100, ShardCount: 2, StatsEnabled: true},
		WithOnEvict(rec.record),
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
}

func TestOnEvict_TTLExpiration(t *testing.T) {
	rec := newEvictRecorder()
	c, err := New(
		Config{MaxSize: 100, ShardCount: 2, CleanupInterval: 5 * time.Millisecond, StatsEnabled: true},
		WithOnEvict(rec.record),
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
}

func TestOnEvict_ExpirationOnAccess(t *testing.T) {
	rec := newEvictRecorder()
	// CleanupInterval 0 disables the background sweeper, so the only way the
	// expired entry leaves is the lazy removal on Get.
	c, err := New(
		Config{MaxSize: 100, ShardCount: 2, CleanupInterval: 0, EvictionPolicy: LRU, StatsEnabled: true},
		WithOnEvict(rec.record),
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
}

// Replacing an existing key's value keeps the key resident, so it must not
// produce an eviction notification.
func TestOnEvict_NotFiredOnOverwrite(t *testing.T) {
	rec := newEvictRecorder()
	c, err := New(
		Config{MaxSize: 100, ShardCount: 2, StatsEnabled: true},
		WithOnEvict(rec.record),
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
func TestOnEvict_NotFiredOnClear(t *testing.T) {
	rec := newEvictRecorder()
	c, err := New(
		Config{MaxSize: 100, ShardCount: 2, StatsEnabled: true},
		WithOnEvict(rec.record),
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
