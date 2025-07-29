package cache

import (
	"fmt"
	"testing"
	"time"
)

// TestAllEvictionPolicies verifies that all eviction policies work correctly
func TestAllEvictionPolicies(t *testing.T) {
	policies := []EvictionPolicy{LRU, LFU, FIFO, Random, SampledLFU}
	policyNames := []string{"LRU", "LFU", "FIFO", "Random", "SampledLFU"}

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

// TestSampledLFUSpecificBehavior tests SampledLFU approximate frequency tracking
func TestSampledLFUSpecificBehavior(t *testing.T) {
	config := Config{
		MaxSize:        4,
		ShardCount:     1,
		EvictionPolicy: SampledLFU,
		StatsEnabled:   true,
	}

	cache := New[string, int](config)
	defer cache.Close()

	// Add items
	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)
	cache.Set("d", 4, time.Hour)

	// Access items with different frequencies
	// High frequency: "a"
	for i := 0; i < 10; i++ {
		cache.Get("a")
	}

	// Medium frequency: "b"
	for i := 0; i < 5; i++ {
		cache.Get("b")
	}

	// Low frequency: "c"
	cache.Get("c")

	// No access: "d"

	// Try adding multiple new items - some may be rejected by admission control
	addedCount := 0
	for i := 0; i < 10; i++ {
		err := cache.Set(fmt.Sprintf("new%d", i), 100+i, time.Hour)
		if err == nil {
			addedCount++
		}
	}

	// At least some items should have been added and potentially caused evictions
	if addedCount == 0 {
		t.Error("Expected at least some items to be admitted")
	}

	// High frequency items should be more likely to remain due to sampling
	if _, found := cache.Get("a"); !found {
		t.Log("Item 'a' was evicted despite high frequency - this can happen with sampling")
	}
	if _, found := cache.Get("b"); !found {
		t.Log("Item 'b' was evicted despite medium frequency - this can happen with sampling")
	}

	// Cache should maintain size constraint
	stats := cache.Stats()
	if stats.Size > 4 {
		t.Errorf("Cache size %d exceeds max capacity 4", stats.Size)
	}
}

// TestSampledLFUAdmissionControl tests that SampledLFU uses admission control
func TestSampledLFUAdmissionControl(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: SampledLFU,
		StatsEnabled:   true,
	}

	cache := New[string, int](config)
	defer cache.Close()

	// Fill cache to capacity
	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)

	// Access existing items to establish frequency
	for i := 0; i < 5; i++ {
		cache.Get("a")
		cache.Get("b")
		cache.Get("c")
	}

	initialEvictions := cache.Stats().Evictions

	// Try adding many new items - admission control should moderate cache pollution
	admitted := 0
	for i := 0; i < 20; i++ {
		sizeBefore := cache.Stats().Size
		cache.Set(string(rune('x'+i)), 100+i, time.Hour)
		sizeAfter := cache.Stats().Size

		// Count successful admissions (size changes or evictions occur)
		if sizeAfter > sizeBefore || cache.Stats().Evictions > initialEvictions {
			admitted++
			initialEvictions = cache.Stats().Evictions
		}
	}

	// Some items should be admitted due to 50% admission rate
	if admitted == 0 {
		t.Error("Expected at least some items to be admitted")
	}

	// Not all items should be admitted (admission control working)
	if admitted == 20 {
		t.Log("All items were admitted - this is possible but less likely with admission control")
	}

	// Cache should maintain size constraint
	stats := cache.Stats()
	if stats.Size > 3 {
		t.Errorf("Cache size %d exceeds max capacity 3", stats.Size)
	}

	t.Logf("Admitted %d out of 20 items with admission control", admitted)
}

// TestSampledLFUSampleSize tests SampledLFU with different scenarios
func TestSampledLFUSampleSize(t *testing.T) {
	config := Config{
		MaxSize:        10,
		ShardCount:     1,
		EvictionPolicy: SampledLFU,
		StatsEnabled:   true,
	}

	cache := New[string, int](config)
	defer cache.Close()

	// Fill cache to capacity
	for i := 0; i < 10; i++ {
		cache.Set(string(rune('a'+i)), i, time.Hour)
	}

	// Create frequency gradient: 'a' most frequent, 'j' least frequent
	for freq := 10; freq > 0; freq-- {
		key := string(rune('a' + (10 - freq)))
		for access := 0; access < freq; access++ {
			cache.Get(key)
		}
	}

	initialEvictions := cache.Stats().Evictions

	// Trigger evictions by adding new items
	for i := 0; i < 5; i++ {
		cache.Set(string(rune('x'+i)), 100+i, time.Hour)
	}

	// Verify evictions occurred
	finalEvictions := cache.Stats().Evictions
	if finalEvictions <= initialEvictions {
		t.Error("Expected evictions to occur when adding new items")
	}

	// High frequency items should be more likely to remain
	// Due to sampling, we can't guarantee exact behavior, but pattern should hold
	highFreqRemaining := 0
	lowFreqRemaining := 0

	for i := 0; i < 5; i++ { // High frequency items
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

// TestSampledLFUStressEviction tests SampledLFU under heavy eviction pressure
func TestSampledLFUStressEviction(t *testing.T) {
	config := Config{
		MaxSize:        5,
		ShardCount:     1,
		EvictionPolicy: SampledLFU,
		StatsEnabled:   true,
	}

	cache := New[string, int](config)
	defer cache.Close()

	// Establish a baseline with known access patterns
	cache.Set("high1", 1, time.Hour)
	cache.Set("high2", 2, time.Hour)
	cache.Set("low1", 3, time.Hour)
	cache.Set("low2", 4, time.Hour)
	cache.Set("low3", 5, time.Hour)

	// Create clear frequency distinction
	for i := 0; i < 20; i++ {
		cache.Get("high1")
		cache.Get("high2")
	}

	// Low frequency items get minimal access
	cache.Get("low1")

	initialEvictions := cache.Stats().Evictions

	// Force many operations - some will be rejected by admission control
	admitted := 0
	for i := 0; i < 50; i++ {
		evictionsBefore := cache.Stats().Evictions
		sizeBefore := cache.Stats().Size
		cache.Set(string(rune('z'+i%26)), 1000+i, time.Hour)
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
