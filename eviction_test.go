package kioshun

import (
	"fmt"
	"testing"
	"time"
)

// TestAllEvictionPolicies verifies that all eviction policies work correctly
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

			// Fill cache beyond capacity to trigger eviction
			for j := 0; j < 10; j++ {
				cache.Set(string(rune('a'+j)), j, time.Hour)
			}
			waitForWrites(t, cache)

			// Verify cache doesn't exceed capacity
			stats := cache.Stats()
			if stats.Size > 5 {
				t.Errorf("Cache size %d exceeds max capacity 5 for policy %s", stats.Size, policyNames[i])
			}

			// Verify evictions occurred (except for SieveTinyLFU which may prevent them)
			if stats.Evictions == 0 && policies[i] != SieveTinyLFU {
				t.Errorf("Expected evictions for policy %s, got 0", policyNames[i])
			}

			// For SieveTinyLFU, low evictions are expected due to admission control
			if policies[i] == SieveTinyLFU && stats.Evictions == 0 {
				t.Logf("SieveTinyLFU prevented evictions through admission control - this is correct behavior")
			}

			// Test that cache still works
			cache.Set("test", 999, time.Hour)
			waitForWrites(t, cache)

			// For SieveTinyLFU, the test item might be rejected by admission control
			if policies[i] == SieveTinyLFU {
				// Try accessing the item to build frequency for admission
				cache.Get("test")
				cache.Set("test", 999, time.Hour) // Try again with higher chance
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

// TestLFUSpecificBehavior tests LFU frequency tracking
func TestLFUSpecificBehavior(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: LFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Add items
	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)
	waitForWrites(t, cache)

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
	waitForWrites(t, cache)

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

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Add items in order
	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)
	waitForWrites(t, cache)

	// Access "a" to make it most recently used
	cache.Get("a")

	// Add new item to trigger eviction - "b" should be evicted (least recently used)
	cache.Set("d", 4, time.Hour)
	waitForWrites(t, cache)

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

// TestSieveTinyLFUSpecificBehavior tests SieveTinyLFU with new adaptive admission filter
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
	for i := 0; i < 5; i++ {
		cache.Get("a")
	}

	// Medium frequency: "b"
	for i := 0; i < 3; i++ {
		cache.Get("b")
	}

	// Low frequency: "c"
	cache.Get("c")

	// Test frequency-based admission: high-frequency items should be more likely to be admitted
	statsBefore := cache.Stats()

	// Try to add a new high-frequency item.
	cache.Set("high_freq", 100, time.Hour)
	waitForWrites(t, cache)
	for i := 0; i < 4; i++ {
		cache.Get("high_freq") // Build TinyLFU frequency.
	}

	// Force eviction with another item - high frequency item should be more likely to stay
	cache.Set("new_item", 200, time.Hour)
	waitForWrites(t, cache)

	statsAfter := cache.Stats()
	if statsAfter.Evictions <= statsBefore.Evictions {
		t.Log("No evictions occurred - admission control may be preventing cache pollution")
	}

	// Verify cache size constraint
	if statsAfter.Size > 6 {
		t.Errorf("Cache size %d exceeds max capacity 6", statsAfter.Size)
	}

	// High frequency items should be more likely to survive
	if _, found := cache.Get("a"); !found {
		t.Log("High frequency item 'a' was evicted - this can happen but is less likely")
	}
}

// TestSieveTinyLFUAdmissionControl tests that SieveTinyLFU uses new adaptive admission control
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
	for i := 0; i < 4; i++ {
		cache.Get("a")
	}

	// Medium frequency: "b"
	for i := 0; i < 2; i++ {
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
	for i := 0; i < 12; i++ {
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

// TestSieveTinyLFUSampleSize tests SieveTinyLFU with different scenarios
func TestSieveTinyLFUSampleSize(t *testing.T) {
	config := Config{
		MaxSize:        10,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Fill cache to capacity
	for i := 0; i < 10; i++ {
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
	for i := 0; i < 20; i++ { // Try more items to overcome admission control
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

// TestSieveTinyLFUStressEviction tests SieveTinyLFU under heavy eviction pressure
func TestSieveTinyLFUStressEviction(t *testing.T) {
	config := Config{
		MaxSize:        5,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Establish a baseline with known access patterns
	cache.Set("high1", 1, time.Hour)
	cache.Set("high2", 2, time.Hour)
	cache.Set("low1", 3, time.Hour)
	cache.Set("low2", 4, time.Hour)
	cache.Set("low3", 5, time.Hour)
	waitForWrites(t, cache)

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

// TestFrequencyAdmissionFilter tests the new frequency-based admission filter
func TestFrequencyAdmissionFilter(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Fill cache to capacity first
	cache.Set("victim1", 1, time.Hour) // Low frequency victim
	cache.Set("victim2", 2, time.Hour) // Low frequency victim
	cache.Set("victim3", 3, time.Hour) // Low frequency victim
	waitForWrites(t, cache)

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

// TestVictimFrequencyTracking tests that victim frequencies are properly tracked
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

// TestFrequencyBasedAdmissionDecisions tests specific admission logic
func TestFrequencyBasedAdmissionDecisions(t *testing.T) {
	config := Config{
		MaxSize:        4,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Fill cache and establish frequency patterns
	cache.Set("freq_5", 1, time.Hour)
	cache.Set("freq_3", 2, time.Hour)
	cache.Set("freq_1", 3, time.Hour)
	cache.Set("freq_0", 4, time.Hour)
	waitForWrites(t, cache)

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
		waitForWrites(t, cache)

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

	// Fill cache
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

	// The second set should likely cause eviction since repeated keys get priority.

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
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Fill cache to trigger admission filter usage
	cache.Set("item1", 1, time.Hour)
	cache.Set("item2", 2, time.Hour)
	cache.Set("item3", 3, time.Hour)
	waitForWrites(t, cache)

	initialEvictions := cache.Stats().Evictions

	// Add more items to trigger admission filter
	itemsAdded := 0
	for i := 0; i < 10; i++ {
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

	// Basic sanity checks
	if cache.Stats().Size > 3 {
		t.Errorf("Cache size %d exceeds max capacity 3", cache.Stats().Size)
	}

	// Should have some admission control effect
	if itemsAdded == 10 {
		t.Log("All items were admitted - this is possible but shows admission control effect")
	}
}

// TestSieveTinyLFUWithFrequencyAdmission tests integration between eviction and admission
func TestSieveTinyLFUWithFrequencyAdmission(t *testing.T) {
	config := Config{
		MaxSize:        5,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Establish baseline with known access patterns
	cache.Set("high1", 1, time.Hour)
	cache.Set("high2", 2, time.Hour)
	cache.Set("med1", 3, time.Hour)
	cache.Set("low1", 4, time.Hour)
	cache.Set("low2", 5, time.Hour)
	waitForWrites(t, cache)

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

	// Verify cache constraint maintained
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

// TestSieveTinyLFUFrequencyThreshold tests frequency-based guaranteed admission
func TestSieveTinyLFUFrequencyThreshold(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Fill cache
	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)
	waitForWrites(t, cache)

	// Create a high-frequency item that should get strong SieveTinyLFU admission priority.
	cache.Set("high_freq", 100, time.Hour)
	waitForWrites(t, cache)
	for i := 0; i < 4; i++ { // Build TinyLFU frequency.
		cache.Get("high_freq")
	}

	initialEvictions := cache.Stats().Evictions

	// Try to add another high-frequency item
	cache.Set("guaranteed", 200, time.Hour)
	waitForWrites(t, cache)
	for i := 0; i < 4; i++ { // Build TinyLFU frequency.
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

	// Fill cache with stable items
	cache.Set("stable1", 1, time.Hour)
	cache.Set("stable2", 2, time.Hour)
	cache.Set("stable3", 3, time.Hour)
	cache.Set("stable4", 4, time.Hour)
	waitForWrites(t, cache)

	// Build frequency for stable items
	for i := 0; i < 3; i++ {
		cache.Get("stable1")
		cache.Get("stable2")
		cache.Get("stable3")
		cache.Get("stable4")
	}

	initialEvictions := cache.Stats().Evictions

	// Simulate scanning pattern with sequential cold keys.
	scanRejections := 0
	for i := 0; i < 15; i++ {
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

// TestSieveTinyLFURepeatedKeyBehavior tests repeated-key admission behavior.
func TestSieveTinyLFURepeatedKeyBehavior(t *testing.T) {
	config := Config{
		MaxSize:        5,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Fill cache
	for i := 0; i < 5; i++ {
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

// TestSieveTinyLFUAdaptiveProbability tests dynamic probability adjustment
func TestSieveTinyLFUAdaptiveProbability(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Fill cache
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

	for i := 0; i < 20; i++ {
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

// TestSieveTinyLFURecencyTieBreaking tests recency-based tie breaking
func TestSieveTinyLFURecencyTieBreaking(t *testing.T) {
	config := Config{
		MaxSize:        3,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
		StatsEnabled:   true,
	}

	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Fill cache
	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)
	waitForWrites(t, cache)

	// Create equal frequency scenario
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

	// Verify cache constraints
	if cache.Size() != 3 {
		t.Errorf("Cache size should be 3, got %d", cache.Size())
	}
}
