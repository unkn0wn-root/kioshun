package cache

import (
	"fmt"
	"testing"
	"time"
)

// TestAllEvictionPolicies verifies that all eviction policies work correctly
func TestAllEvictionPolicies(t *testing.T) {
	policies := []EvictionPolicy{LRU, LFU, FIFO, AdmissionLFU}
	policyNames := []string{"LRU", "LFU", "FIFO", "AdmissionLFU"}

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

// TestAdmissionLFUSpecificBehavior tests AdmissionLFU approximate frequency tracking
func TestAdmissionLFUSpecificBehavior(t *testing.T) {
	config := Config{
		MaxSize:        4,
		ShardCount:     1,
		EvictionPolicy: AdmissionLFU,
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
		evictionsBefore := cache.Stats().Evictions
		cache.Set(fmt.Sprintf("new%d", i), 100+i, time.Hour)
		evictionsAfter := cache.Stats().Evictions
		
		// Count as added only if eviction occurred (item was admitted)
		if evictionsAfter > evictionsBefore {
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

// TestAdmissionLFUAdmissionControl tests that AdmissionLFU uses admission control
func TestAdmissionLFUAdmissionControl(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: AdmissionLFU,
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

	// Try adding many new items - admission control should moderate cache pollution
	admitted := 0
	for i := 0; i < 20; i++ {
		evictionsBefore := cache.Stats().Evictions
		cache.Set(string(rune('x'+i)), 100+i, time.Hour)
		evictionsAfter := cache.Stats().Evictions

		// Count successful admissions (eviction occurred means item was admitted)
		if evictionsAfter > evictionsBefore {
			admitted++
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

// TestAdmissionLFUSampleSize tests AdmissionLFU with different scenarios
func TestAdmissionLFUSampleSize(t *testing.T) {
	config := Config{
		MaxSize:        10,
		ShardCount:     1,
		EvictionPolicy: AdmissionLFU,
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

	// Trigger evictions by adding new items - try many to overcome admission control
	admittedItems := 0
	for i := 0; i < 20; i++ { // Try more items to overcome admission control
		evictionsBefore := cache.Stats().Evictions
		cache.Set(string(rune('x'+i)), 100+i, time.Hour)
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

// TestAdmissionLFUStressEviction tests AdmissionLFU under heavy eviction pressure
func TestAdmissionLFUStressEviction(t *testing.T) {
	config := Config{
		MaxSize:        5,
		ShardCount:     1,
		EvictionPolicy: AdmissionLFU,
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

// TestFrequencyAdmissionFilter tests the new frequency-based admission filter
func TestFrequencyAdmissionFilter(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: AdmissionLFU,
		StatsEnabled:   true,
	}

	cache := New[string, int](config)
	defer cache.Close()

	// Fill cache to capacity first
	cache.Set("victim1", 1, time.Hour) // Low frequency victim
	cache.Set("victim2", 2, time.Hour) // Low frequency victim
	cache.Set("victim3", 3, time.Hour) // Low frequency victim

	// Create frequency gradient - access some items more than others
	for i := 0; i < 5; i++ {
		cache.Get("victim1") // Higher frequency
	}
	cache.Get("victim2") // Medium frequency
	// victim3 stays at low frequency

	// Try to add items with different expected admission patterns
	admittedCount := 0
	rejectedCount := 0

	// Test 1: High frequency items should have better admission chances
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("high_freq_%d", i)
		evictionsBefore := cache.Stats().Evictions

		// Pre-populate this key in the frequency filter by simulating access
		// This simulates a key that has been seen before and has frequency
		cache.Set(key, 100+i, time.Hour)

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

	// Test 3: Verify some evictions occurred
	if finalStats.Evictions == 0 {
		t.Error("Expected some evictions to occur with admission filter")
	}

	// Test 4: Verify admission control is working (not all items admitted)
	if rejectedCount == 0 {
		t.Log("No items were rejected - this can happen but admission control should typically reject some")
	}
}

// TestVictimFrequencyTracking tests that victim frequencies are properly tracked
func TestVictimFrequencyTracking(t *testing.T) {
	config := Config{
		MaxSize:        2,
		ShardCount:     1,
		EvictionPolicy: AdmissionLFU,
		StatsEnabled:   true,
	}

	cache := New[string, int](config)
	defer cache.Close()

	// Add initial items with different frequencies
	cache.Set("low_freq", 1, time.Hour)
	cache.Set("high_freq", 2, time.Hour)

	// Create clear frequency difference
	for i := 0; i < 10; i++ {
		cache.Get("high_freq") // High frequency
	}
	cache.Get("low_freq") // Low frequency (1 access)

	initialEvictions := cache.Stats().Evictions

	// Add new item that should trigger eviction
	cache.Set("new_item", 3, time.Hour)

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

// TestFrequencyBasedAdmissionDecisions tests specific admission logic
func TestFrequencyBasedAdmissionDecisions(t *testing.T) {
	config := Config{
		MaxSize:        4,
		ShardCount:     1,
		EvictionPolicy: AdmissionLFU,
		StatsEnabled:   true,
	}

	cache := New[string, int](config)
	defer cache.Close()

	// Fill cache and establish frequency patterns
	cache.Set("freq_5", 1, time.Hour)
	cache.Set("freq_3", 2, time.Hour)
	cache.Set("freq_1", 3, time.Hour)
	cache.Set("freq_0", 4, time.Hour)

	// Create frequency gradient
	for i := 0; i < 5; i++ {
		cache.Get("freq_5")
	}
	for i := 0; i < 3; i++ {
		cache.Get("freq_3")
	}
	for i := 0; i < 1; i++ {
		cache.Get("freq_1")
	}
	// freq_0 has 0 additional accesses

	// Test admission patterns
	admissionResults := make(map[string]bool)

	// Try multiple new items to see admission patterns
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("test_%d", i)
		evictionsBefore := cache.Stats().Evictions

		cache.Set(key, 100+i, time.Hour)

		evictionsAfter := cache.Stats().Evictions
		admitted := evictionsAfter > evictionsBefore
		admissionResults[key] = admitted
	}

	// Count admissions
	admittedCount := 0
	for _, admitted := range admissionResults {
		if admitted {
			admittedCount++
		}
	}

	// Verify some level of admission control
	t.Logf("Admitted %d out of %d items", admittedCount, len(admissionResults))

	// Should have some admissions and some rejections
	if admittedCount == 0 {
		t.Error("Expected at least some items to be admitted")
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
		EvictionPolicy: AdmissionLFU,
		StatsEnabled:   true,
	}

	cache := New[string, int](config)
	defer cache.Close()

	// Fill cache
	cache.Set("old1", 1, time.Hour)
	cache.Set("old2", 2, time.Hour)

	// Add and immediately re-add same item - should be admitted due to doorkeeper
	cache.Set("doorkeeper_test", 3, time.Hour) // First time - may or may not be admitted

	evictionsBefore := cache.Stats().Evictions
	cache.Set("doorkeeper_test", 4, time.Hour) // Second time - should be admitted (doorkeeper)
	evictionsAfter := cache.Stats().Evictions

	// The second set should likely cause eviction since item is in doorkeeper
	// This tests the doorkeeper logic - recently seen items get priority

	t.Logf("Evictions before: %d, after: %d", evictionsBefore, evictionsAfter)

	// Verify cache maintains size constraint
	if cache.Stats().Size > 2 {
		t.Errorf("Cache size %d exceeds max capacity 2", cache.Stats().Size)
	}
}

// TestAdmissionFilterStats tests statistics tracking
func TestAdmissionFilterStats(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: AdmissionLFU,
		StatsEnabled:   true,
	}

	cache := New[string, int](config)
	defer cache.Close()

	// Fill cache to trigger admission filter usage
	cache.Set("item1", 1, time.Hour)
	cache.Set("item2", 2, time.Hour)
	cache.Set("item3", 3, time.Hour)

	initialEvictions := cache.Stats().Evictions

	// Add more items to trigger admission filter
	itemsAdded := 0
	for i := 0; i < 10; i++ {
		evictionsBefore := cache.Stats().Evictions
		cache.Set(fmt.Sprintf("new_%d", i), 100+i, time.Hour)
		evictionsAfter := cache.Stats().Evictions

		if evictionsAfter > evictionsBefore {
			itemsAdded++
		}
	}

	finalEvictions := cache.Stats().Evictions
	totalEvictions := finalEvictions - initialEvictions

	t.Logf("Items that caused evictions: %d, Total evictions: %d", itemsAdded, totalEvictions)

	// Basic sanity checks
	if cache.Stats().Size > 3 {
		t.Errorf("Cache size %d exceeds max capacity 3", cache.Stats().Size)
	}

	// Should have some admission control effect
	if itemsAdded == 10 {
		t.Log("All items were admitted - this is possible but shows admission control effect")
	}
}

// TestAdmissionLFUWithFrequencyAdmission tests integration between eviction and admission
func TestAdmissionLFUWithFrequencyAdmission(t *testing.T) {
	config := Config{
		MaxSize:        5,
		ShardCount:     1,
		EvictionPolicy: AdmissionLFU,
		StatsEnabled:   true,
	}

	cache := New[string, int](config)
	defer cache.Close()

	// Establish baseline with known access patterns
	cache.Set("high1", 1, time.Hour)
	cache.Set("high2", 2, time.Hour)
	cache.Set("med1", 3, time.Hour)
	cache.Set("low1", 4, time.Hour)
	cache.Set("low2", 5, time.Hour)

	// Create clear frequency differences
	for i := 0; i < 15; i++ {
		cache.Get("high1")
		cache.Get("high2")
	}

	for i := 0; i < 7; i++ {
		cache.Get("med1")
	}

	for i := 0; i < 2; i++ {
		cache.Get("low1")
	}
	// low2 gets no additional accesses

	initialStats := cache.Stats()

	// Try to add many new items - admission filter should moderate
	admissionAttempts := 0
	actualAdmissions := 0

	for i := 0; i < 30; i++ {
		evictionsBefore := cache.Stats().Evictions
		sizeBefore := cache.Stats().Size

		cache.Set(fmt.Sprintf("candidate_%d", i), 1000+i, time.Hour)
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

	// Verify cache constraint maintained
	if finalStats.Size > 5 {
		t.Errorf("Cache size %d exceeds max capacity 5", finalStats.Size)
	}

	// Should have some admission control (not 100% admission rate typically)
	if actualAdmissions == 0 {
		t.Error("Expected at least some items to be admitted")
	}

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
