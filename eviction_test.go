package cache

import (
	"testing"
	"time"
)

// TestAllEvictionPolicies verifies that all eviction policies work correctly
func TestAllEvictionPolicies(t *testing.T) {
	policies := []EvictionPolicy{LRU, LFU, FIFO, Random}
	policyNames := []string{"LRU", "LFU", "FIFO", "Random"}

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

			cache := New[string, int](config)
			defer cache.Close()

			// Fill cache beyond capacity to trigger eviction
			for j := 0; j < 10; j++ {
				cache.Set(string(rune('a'+j)), j, time.Hour)
			}

			// Verify cache doesn't exceed capacity
			stats := cache.Stats()
			if stats.Size > 5 {
				t.Errorf("Cache size %d exceeds max capacity 5 for policy %s", stats.Size, policyNames[i])
			}

			// Verify evictions occurred
			if stats.Evictions == 0 {
				t.Errorf("Expected evictions for policy %s, got 0", policyNames[i])
			}

			// Test that cache still works after evictions
			cache.Set("test", 999, time.Hour)
			if val, found := cache.Get("test"); !found || val != 999 {
				t.Errorf("Cache not working after evictions for policy %s", policyNames[i])
			}
		})
	}
}

// TestLFUSpecificBehavior tests LFU frequency tracking
func TestLFUSpecificBehavior(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: LFU,
		StatsEnabled:   true,
	}

	cache := New[string, int](config)
	defer cache.Close()

	// Add items
	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)

	// Access "a" multiple times to increase frequency
	for i := 0; i < 5; i++ {
		cache.Get("a")
	}

	// Access "b" fewer times
	cache.Get("b")
	cache.Get("b")

	// Don't access "c" at all after insertion

	// Add new item to trigger eviction - "c" should be evicted (lowest frequency)
	cache.Set("d", 4, time.Hour)

	// "c" should be gone, "a" and "b" should remain
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

// TestLRUSpecificBehavior tests LRU access order tracking
func TestLRUSpecificBehavior(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: LRU,
		StatsEnabled:   true,
	}

	cache := New[string, int](config)
	defer cache.Close()

	// Add items in order
	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)

	// Access "a" to make it most recently used
	cache.Get("a")

	// Add new item to trigger eviction - "b" should be evicted (least recently used)
	cache.Set("d", 4, time.Hour)

	// "b" should be gone, others should remain
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
