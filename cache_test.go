// cache_test.go - Comprehensive unit tests
package cache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheBasicOperations(t *testing.T) {
	cache := NewWithDefaults[string, string]()
	defer cache.Close()

	// Test Set and Get
	cache.Set("key1", "value1", 1*time.Minute)

	if value, found := cache.Get("key1"); !found || value != "value1" {
		t.Errorf("Expected 'value1', got '%s', found: %v", value, found)
	}

	// Test non-existent key
	if _, found := cache.Get("nonexistent"); found {
		t.Error("Expected key to not exist")
	}

	// Test Delete
	if !cache.Delete("key1") {
		t.Error("Expected delete to return true")
	}

	if _, found := cache.Get("key1"); found {
		t.Error("Expected key to be deleted")
	}
}

func TestCacheExpiration(t *testing.T) {
	cache := NewWithDefaults[string, string]()
	defer cache.Close()

	// Set value with short TTL
	cache.Set("expiring", "value", 100*time.Millisecond)

	// Should be available immediately
	if _, found := cache.Get("expiring"); !found {
		t.Error("Expected key to exist immediately after set")
	}

	// Wait for expiration
	time.Sleep(200 * time.Millisecond)

	// Should be expired
	if _, found := cache.Get("expiring"); found {
		t.Error("Expected key to be expired")
	}
}

func TestCacheTTL(t *testing.T) {
	cache := NewWithDefaults[string, string]()
	defer cache.Close()

	cache.Set("ttl_key", "value", 1*time.Minute)

	value, ttl, found := cache.GetWithTTL("ttl_key")
	if !found {
		t.Error("Expected key to exist")
	}
	if value != "value" {
		t.Errorf("Expected 'value', got '%s'", value)
	}
	if ttl <= 0 || ttl > 1*time.Minute {
		t.Errorf("Expected TTL to be positive and <= 1 minute, got %v", ttl)
	}
}

func TestCacheLRUEviction(t *testing.T) {
	config := Config{
		MaxSize:         3,
		ShardCount:      1, // Use single shard for predictable eviction
		CleanupInterval: 0, // Disable cleanup for this test
		DefaultTTL:      1 * time.Hour,
		EvictionPolicy:  LRU,
	}
	cache := New[string, string](config)
	defer cache.Close()

	// Fill cache to capacity
	cache.Set("key1", "value1", 1*time.Hour)
	cache.Set("key2", "value2", 1*time.Hour)
	cache.Set("key3", "value3", 1*time.Hour)

	// Access key1 to make it more recently used
	cache.Get("key1")

	// Add one more item, should evict key2 (least recently used)
	cache.Set("key4", "value4", 1*time.Hour)

	// key2 should be evicted
	if _, found := cache.Get("key2"); found {
		t.Error("Expected key2 to be evicted")
	}

	// key1 should still exist (was accessed recently)
	if _, found := cache.Get("key1"); !found {
		t.Error("Expected key1 to still exist")
	}
}

func TestCacheStats(t *testing.T) {
	cache := NewWithDefaults[string, string]()
	defer cache.Close()

	// Initial stats
	stats := cache.Stats()
	if stats.Hits != 0 || stats.Misses != 0 {
		t.Error("Expected initial stats to be zero")
	}

	// Set a value
	cache.Set("key1", "value1", 1*time.Minute)

	// Hit
	cache.Get("key1")
	stats = cache.Stats()
	if stats.Hits != 1 {
		t.Errorf("Expected 1 hit, got %d", stats.Hits)
	}

	// Miss
	cache.Get("nonexistent")
	stats = cache.Stats()
	if stats.Misses != 1 {
		t.Errorf("Expected 1 miss, got %d", stats.Misses)
	}
}

func TestCacheExists(t *testing.T) {
	cache := NewWithDefaults[string, string]()
	defer cache.Close()

	cache.Set("key1", "value1", 1*time.Minute)

	if !cache.Exists("key1") {
		t.Error("Expected key1 to exist")
	}

	if cache.Exists("nonexistent") {
		t.Error("Expected nonexistent key to not exist")
	}
}

func TestCacheKeys(t *testing.T) {
	cache := NewWithDefaults[string, string]()
	defer cache.Close()

	cache.Set("key1", "value1", 1*time.Minute)
	cache.Set("key2", "value2", 1*time.Minute)
	cache.Set("key3", "value3", 1*time.Minute)

	keys := cache.Keys()
	if len(keys) != 3 {
		t.Errorf("Expected 3 keys, got %d", len(keys))
	}

	expectedKeys := map[string]bool{"key1": true, "key2": true, "key3": true}
	for _, key := range keys {
		if !expectedKeys[key] {
			t.Errorf("Unexpected key: %s", key)
		}
	}
}

func TestCacheConcurrency(t *testing.T) {
	cache := NewWithDefaults[string, int]()
	defer cache.Close()

	var wg sync.WaitGroup
	numGoroutines := 100
	numOperations := 100

	// Concurrent writes
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				key := fmt.Sprintf("key:%d:%d", id, j)
				cache.Set(key, id*numOperations+j, 1*time.Minute)
			}
		}(i)
	}

	// Concurrent reads
	for i := 0; i < numGoroutines/2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				key := fmt.Sprintf("key:%d:%d", id, j)
				cache.Get(key)
			}
		}(i)
	}

	wg.Wait()

	// Verify no data races occurred
	stats := cache.Stats()
	if stats.Size < 0 {
		t.Error("Invalid cache size after concurrent operations")
	}
}

func TestCacheCleanup(t *testing.T) {
	config := Config{
		MaxSize:         100,
		CleanupInterval: 50 * time.Millisecond,
		DefaultTTL:      100 * time.Millisecond,
		StatsEnabled:    true,
	}
	cache := New[string, string](config)
	defer cache.Close()

	// Add items that will expire
	cache.Set("key1", "value1", 100*time.Millisecond)
	cache.Set("key2", "value2", 100*time.Millisecond)

	// Wait for items to expire
	time.Sleep(150 * time.Millisecond)

	// Force cleanup
	cache.TriggerCleanup()
	time.Sleep(50 * time.Millisecond)

	// Items should be cleaned up
	if cache.Exists("key1") || cache.Exists("key2") {
		t.Error("Expected expired items to be cleaned up")
	}

	stats := cache.Stats()
	if stats.Expirations == 0 {
		t.Error("Expected expiration count to be > 0")
	}
}

func TestCacheCallback(t *testing.T) {
	cache := NewWithDefaults[string, string]()
	defer cache.Close()

	var callbackCalled int32
	var callbackKey string
	var callbackValue string
	var mu sync.Mutex

	cache.SetWithCallback("key1", "value1", 100*time.Millisecond, func(key string, value string) {
		mu.Lock()
		defer mu.Unlock()
		atomic.StoreInt32(&callbackCalled, 1)
		callbackKey = key
		callbackValue = value
	})

	// Wait for expiration
	time.Sleep(200 * time.Millisecond)

	if atomic.LoadInt32(&callbackCalled) != 1 {
		t.Error("Expected callback to be called")
	}

	mu.Lock()
	defer mu.Unlock()
	if callbackKey != "key1" || callbackValue != "value1" {
		t.Errorf("Expected callback with key1/value1, got %s/%s", callbackKey, callbackValue)
	}
}

func TestCacheManager(t *testing.T) {
	manager := NewManager()
	defer manager.CloseAll()

	// Register cache
	config := DefaultConfig()
	err := manager.RegisterCache("test", config)
	if err != nil {
		t.Errorf("Expected no error registering cache, got %v", err)
	}

	// Get cache
	cache, err := GetCache[string, string](manager, "test")
	if err != nil {
		t.Errorf("Expected no error getting cache, got %v", err)
	}

	// Use cache
	cache.Set("key1", "value1", 1*time.Minute)
	if value, found := cache.Get("key1"); !found || value != "value1" {
		t.Errorf("Expected 'value1', got '%s', found: %v", value, found)
	}

	// Get stats
	stats := manager.GetCacheStats()
	if len(stats) != 1 {
		t.Errorf("Expected 1 cache in stats, got %d", len(stats))
	}
}

func TestCacheGenerics(t *testing.T) {
	// Test with different types
	stringCache := NewWithDefaults[string, string]()
	defer stringCache.Close()

	intCache := NewWithDefaults[int, string]()
	defer intCache.Close()

	structCache := NewWithDefaults[string, User]()
	defer structCache.Close()

	// Test string cache
	stringCache.Set("key", "value", 1*time.Minute)
	if value, found := stringCache.Get("key"); !found || value != "value" {
		t.Error("String cache failed")
	}

	// Test int cache
	intCache.Set(123, "int_value", 1*time.Minute)
	if value, found := intCache.Get(123); !found || value != "int_value" {
		t.Error("Int cache failed")
	}

	// Test struct cache
	user := User{ID: "123", Name: "Test User", Email: "test@example.com"}
	structCache.Set("user:123", user, 1*time.Minute)
	if value, found := structCache.Get("user:123"); !found || value.ID != "123" {
		t.Error("Struct cache failed")
	}
}

func TestCacheCloseBehavior(t *testing.T) {
	cache := NewWithDefaults[string, string]()

	// Set a value
	cache.Set("key1", "value1", 1*time.Minute)

	// Close the cache
	err := cache.Close()
	if err != nil {
		t.Errorf("Expected no error closing cache, got %v", err)
	}

	// Operations after close should fail gracefully
	err = cache.Set("key2", "value2", 1*time.Minute)
	if err == nil {
		t.Error("Expected error when setting after close")
	}

	if _, found := cache.Get("key1"); found {
		t.Error("Expected get to fail after close")
	}

	// Double close should be safe
	err = cache.Close()
	if err != nil {
		t.Errorf("Expected no error on double close, got %v", err)
	}
}

// Basic benchmarks are in cache_bench_test.go

type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// Test that verifies the new Set method's in-place update behavior
func TestSetInPlaceUpdate(t *testing.T) {
	config := Config{
		MaxSize:         100,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      0,
		EvictionPolicy:  LRU,
		StatsEnabled:    true,
	}
	cache := New[string, string](config)
	defer cache.Close()

	// Set initial value
	cache.Set("key1", "value1", time.Hour)

	// Verify initial state
	if value, found := cache.Get("key1"); !found || value != "value1" {
		t.Errorf("Expected initial value 'value1', got '%s', found: %v", value, found)
	}

	// Get the item pointer before update (if we could access it)
	shard := cache.getShard("key1")
	shard.mu.RLock()
	originalItem := shard.data["key1"]
	shard.mu.RUnlock()

	// Update the same key - should reuse the item
	cache.Set("key1", "value2", time.Hour)

	// Verify update worked
	if value, found := cache.Get("key1"); !found || value != "value2" {
		t.Errorf("Expected updated value 'value2', got '%s', found: %v", value, found)
	}

	// Check that item was reused (same pointer)
	shard.mu.RLock()
	updatedItem := shard.data["key1"]
	shard.mu.RUnlock()

	if originalItem != updatedItem {
		t.Error("Expected in-place update to reuse the same cacheItem, but got different pointers")
	}

	// Verify size didn't change (no new allocation)
	stats := cache.Stats()
	if stats.Size != 1 {
		t.Errorf("Expected size 1 after update, got %d", stats.Size)
	}
}

// Test LFU frequency reset behavior on Set
func TestSetLFUFrequencyReset(t *testing.T) {
	config := Config{
		MaxSize:         3,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      0,
		EvictionPolicy:  LFU,
		StatsEnabled:    true,
	}
	cache := New[string, int](config)
	defer cache.Close()

	// Add items
	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)

	// Access "a" multiple times to increase frequency
	for i := 0; i < 10; i++ {
		cache.Get("a")
	}

	// Access other items less
	cache.Get("b")
	cache.Get("c")

	// Update "a" with new value - frequency should reset to 1
	cache.Set("a", 999, time.Hour)

	// Now add a new item to trigger eviction
	// "a" should now be evicted since its frequency was reset
	cache.Set("d", 4, time.Hour)

	// Verify that "a" was evicted (due to frequency reset)
	if _, found := cache.Get("a"); found {
		t.Error("Item 'a' should have been evicted after frequency reset")
	}
}
