// cache_test.go - Comprehensive unit tests
package kioshun

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestCache[K comparable, V any](t testing.TB, config Config) *Cache[K, V] {
	t.Helper()
	cache, err := New[K, V](config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return cache
}

func newDefaultTestCache[K comparable, V any](t testing.TB) *Cache[K, V] {
	t.Helper()
	return NewDefault[K, V]()
}

func TestDefaultConfigValid(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig() is invalid: %v", err)
	}
}

func TestCacheBasicOperations(t *testing.T) {
	cache := newDefaultTestCache[string, string](t)
	defer cache.Close()

	// Test Set and Get
	cache.Set("key1", "value1", 1*time.Minute)
	waitForWrites(t, cache)

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
	cache := newDefaultTestCache[string, string](t)
	defer cache.Close()

	// Set value with short TTL
	cache.Set("expiring", "value", 100*time.Millisecond)
	waitForWrites(t, cache)

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
	cache := newDefaultTestCache[string, string](t)
	defer cache.Close()

	cache.Set("ttl_key", "value", 1*time.Minute)
	waitForWrites(t, cache)

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
	cache := newTestCache[string, string](t, config)
	defer cache.Close()

	// Fill cache to capacity
	cache.Set("key1", "value1", 1*time.Hour)
	cache.Set("key2", "value2", 1*time.Hour)
	cache.Set("key3", "value3", 1*time.Hour)
	waitForWrites(t, cache)

	// Access key1 to make it more recently used
	cache.Get("key1")

	// Add one more item, should evict key2 (least recently used)
	cache.Set("key4", "value4", 1*time.Hour)
	waitForWrites(t, cache)

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
	config := DefaultConfig()
	config.StatsEnabled = true
	cache := newTestCache[string, string](t, config)
	defer cache.Close()

	// Initial stats
	stats := cache.Stats()
	if stats.Hits != 0 || stats.Misses != 0 {
		t.Error("Expected initial stats to be zero")
	}

	// Set a value
	cache.Set("key1", "value1", 1*time.Minute)
	waitForWrites(t, cache)

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
	cache := newDefaultTestCache[string, string](t)
	defer cache.Close()

	cache.Set("key1", "value1", 1*time.Minute)
	waitForWrites(t, cache)

	if !cache.Exists("key1") {
		t.Error("Expected key1 to exist")
	}

	if cache.Exists("nonexistent") {
		t.Error("Expected nonexistent key to not exist")
	}
}

func TestCacheKeys(t *testing.T) {
	cache := newDefaultTestCache[string, string](t)
	defer cache.Close()

	cache.Set("key1", "value1", 1*time.Minute)
	cache.Set("key2", "value2", 1*time.Minute)
	cache.Set("key3", "value3", 1*time.Minute)
	waitForWrites(t, cache)

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
	cache := newDefaultTestCache[string, int](t)
	defer cache.Close()

	var wg sync.WaitGroup
	numGoroutines := 100
	numOperations := 100

	// Concurrent writes
	for i := range numGoroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := range numOperations {
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
			for j := range numOperations {
				key := fmt.Sprintf("key:%d:%d", id, j)
				cache.Get(key)
			}
		}(i)
	}

	wg.Wait()
	waitForWrites(t, cache)

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
	cache := newTestCache[string, string](t, config)
	defer cache.Close()

	// Add items that will expire
	cache.Set("key1", "value1", 100*time.Millisecond)
	cache.Set("key2", "value2", 100*time.Millisecond)
	waitForWrites(t, cache)

	// Wait for items to expire
	time.Sleep(150 * time.Millisecond)

	// Force cleanup
	cache.Cleanup()
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
	cache := newDefaultTestCache[string, string](t)
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
	waitForWrites(t, cache)

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
	err := manager.Register("test", config)
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
	waitForWrites(t, cache)
	if value, found := cache.Get("key1"); !found || value != "value1" {
		t.Errorf("Expected 'value1', got '%s', found: %v", value, found)
	}

	// Get stats
	stats := manager.Stats()
	if len(stats) != 1 {
		t.Errorf("Expected 1 cache in stats, got %d", len(stats))
	}
}

func TestCacheGenerics(t *testing.T) {
	// Test with different types
	stringCache := newDefaultTestCache[string, string](t)
	defer stringCache.Close()

	intCache := newDefaultTestCache[int, string](t)
	defer intCache.Close()

	structCache := newDefaultTestCache[string, User](t)
	defer structCache.Close()

	// Test string cache
	stringCache.Set("key", "value", 1*time.Minute)
	waitForWrites(t, stringCache)
	if value, found := stringCache.Get("key"); !found || value != "value" {
		t.Error("String cache failed")
	}

	// Test int cache
	intCache.Set(123, "int_value", 1*time.Minute)
	waitForWrites(t, intCache)
	if value, found := intCache.Get(123); !found || value != "int_value" {
		t.Error("Int cache failed")
	}

	// Test struct cache
	user := User{ID: "123", Name: "Test User", Email: "test@example.com"}
	structCache.Set("user:123", user, 1*time.Minute)
	waitForWrites(t, structCache)
	if value, found := structCache.Get("user:123"); !found || value.ID != "123" {
		t.Error("Struct cache failed")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		field  string
		value  any
		reason string
	}{
		{
			name: "negative max size",
			config: Config{
				MaxSize: -1,
			},
			field:  "MaxSize",
			value:  int64(-1),
			reason: "must be >= 0",
		},
		{
			name: "negative shard count",
			config: Config{
				ShardCount: -1,
			},
			field:  "ShardCount",
			value:  -1,
			reason: "must be >= 0",
		},
		{
			name: "negative cleanup interval",
			config: Config{
				CleanupInterval: -time.Second,
			},
			field:  "CleanupInterval",
			value:  -time.Second,
			reason: "must be >= 0",
		},
		{
			name: "invalid default ttl",
			config: Config{
				DefaultTTL: -2,
			},
			field:  "DefaultTTL",
			value:  time.Duration(-2),
			reason: "must be >= 0 or NoExpiration",
		},
		{
			name: "unknown policy",
			config: Config{
				EvictionPolicy: SieveTinyLFU + 1,
			},
			field:  "EvictionPolicy",
			value:  SieveTinyLFU + 1,
			reason: "must be a known eviction policy",
		},
		{
			name: "invalid probation ratio",
			config: Config{
				ProbationRatio: 101,
			},
			field:  "ProbationRatio",
			value:  uint8(101),
			reason: "must be <= 100",
		},
		{
			name: "invalid ghost ratio",
			config: Config{
				GhostRatio: 101,
			},
			field:  "GhostRatio",
			value:  uint8(101),
			reason: "must be <= 100",
		},
		{
			name: "negative write buffer",
			config: Config{
				WriteBufferSize: -1,
			},
			field:  "WriteBufferSize",
			value:  -1,
			reason: "must be >= 0",
		},
		{
			name: "negative write batch",
			config: Config{
				WriteBatchSize: -1,
			},
			field:  "WriteBatchSize",
			value:  -1,
			reason: "must be >= 0",
		},
		{
			name: "sieve cost budget without max size",
			config: Config{
				EvictionPolicy: SieveTinyLFU,
				MaxCost:        5,
			},
			field:  "MaxSize",
			value:  int64(0),
			reason: "must be > 0 for SieveTinyLFU when MaxCost is set",
		},
		{
			name: "default policy cost budget without max size",
			config: Config{
				MaxCost: 5, // EvictionPolicy unset resolves to SieveTinyLFU
			},
			field:  "MaxSize",
			value:  int64(0),
			reason: "must be > 0 for SieveTinyLFU when MaxCost is set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New[string, string](tt.config)
			if c != nil {
				t.Fatal("expected nil cache for invalid config")
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() error = %v, want ErrInvalidConfig", err)
			}
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("New() error = %T, want *ConfigError", err)
			}
			if configErr.Field != tt.field {
				t.Fatalf("ConfigError.Field = %q, want %q", configErr.Field, tt.field)
			}
			if !reflect.DeepEqual(configErr.Value, tt.value) {
				t.Fatalf("ConfigError.Value = %#v, want %#v", configErr.Value, tt.value)
			}
			if configErr.Reason != tt.reason {
				t.Fatalf("ConfigError.Reason = %q, want %q", configErr.Reason, tt.reason)
			}
		})
	}
}

func TestManagerRegisterValidatesConfig(t *testing.T) {
	manager := NewManager()
	err := manager.Register("bad", Config{MaxSize: -1})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Register() error = %v, want ErrInvalidConfig", err)
	}
	var configErr *ConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("Register() error = %T, want wrapped *ConfigError", err)
	}
	if configErr.Field != "MaxSize" {
		t.Fatalf("ConfigError.Field = %q, want MaxSize", configErr.Field)
	}
}

func TestRegisterCacheAppliesTypedOptions(t *testing.T) {
	manager := NewManager()
	t.Cleanup(func() { manager.CloseAll() })

	cfg := DefaultConfig()
	cfg.MaxSize = 100
	cfg.MaxCost = 10
	cfg.ShardCount = 1
	cfg.CleanupInterval = 0
	cfg.EvictionPolicy = LRU

	err := RegisterCache[string, []byte](manager, "weighted", cfg, WithWeigher(func(_ string, value []byte) int64 {
		return int64(len(value))
	}))
	if err != nil {
		t.Fatalf("RegisterCache() error = %v", err)
	}

	cache, err := GetCache[string, []byte](manager, "weighted")
	if err != nil {
		t.Fatalf("GetCache() error = %v", err)
	}
	if err := cache.Set("small", []byte("1234"), NoExpiration); err != nil {
		t.Fatalf("Set small error = %v", err)
	}
	if got := cache.Cost(); got != 4 {
		t.Fatalf("Cost() = %d, want weighted cost 4", got)
	}
	if err := cache.Set("large", make([]byte, 11), NoExpiration); !errors.Is(err, ErrItemTooLarge) {
		t.Fatalf("Set large error = %v, want ErrItemTooLarge", err)
	}
}

func TestRegisterCacheTypeMismatchDoesNotCreateInstance(t *testing.T) {
	manager := NewManager()
	t.Cleanup(func() { manager.CloseAll() })

	cfg := DefaultConfig()
	cfg.CleanupInterval = 0
	if err := RegisterCache[int, int](manager, "typed", cfg); err != nil {
		t.Fatalf("RegisterCache() error = %v", err)
	}

	if _, err := GetCache[string, int](manager, "typed"); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("GetCache() mismatch error = %v, want ErrTypeMismatch", err)
	}
	if stats := manager.Stats(); len(stats) != 0 {
		t.Fatalf("type mismatch created a cache instance; stats=%v", stats)
	}

	if _, err := GetCache[int, int](manager, "typed"); err != nil {
		t.Fatalf("GetCache() with registered type error = %v", err)
	}
}

func TestManagerRemoveDropsConfig(t *testing.T) {
	manager := NewManager()
	t.Cleanup(func() { manager.CloseAll() })

	const customMaxSize = 12345
	cfg := DefaultConfig()
	cfg.MaxSize = customMaxSize
	if err := manager.Register("c", cfg); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	first, err := GetCache[string, int](manager, "c")
	if err != nil {
		t.Fatalf("GetCache() error = %v", err)
	}
	if got := first.Stats().Capacity; got != customMaxSize {
		t.Fatalf("Capacity before Remove = %d, want %d", got, customMaxSize)
	}

	if err := manager.Remove("c"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	// Remove drops the registration, so a fresh GetCache rebuilds from
	// DefaultConfig rather than resurrecting the removed config.
	second, err := GetCache[string, int](manager, "c")
	if err != nil {
		t.Fatalf("GetCache() after Remove error = %v", err)
	}
	if second == first {
		t.Fatal("expected a new cache instance after Remove")
	}
	if got, want := second.Stats().Capacity, DefaultConfig().MaxSize; got != want {
		t.Fatalf("Capacity after Remove = %d, want default %d", got, want)
	}
}

func TestManagerCloseAllPreservesConfigs(t *testing.T) {
	manager := NewManager()
	t.Cleanup(func() { manager.CloseAll() })

	const customMaxSize = 4096
	cfg := DefaultConfig()
	cfg.MaxSize = customMaxSize
	if err := manager.Register("c", cfg); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, err := GetCache[string, int](manager, "c"); err != nil {
		t.Fatalf("GetCache() error = %v", err)
	}

	if err := manager.CloseAll(); err != nil {
		t.Fatalf("CloseAll() error = %v", err)
	}

	// CloseAll closes instances but keeps registrations, so the name rebuilds
	// from its registered config.
	cache, err := GetCache[string, int](manager, "c")
	if err != nil {
		t.Fatalf("GetCache() after CloseAll error = %v", err)
	}
	if got := cache.Stats().Capacity; got != customMaxSize {
		t.Fatalf("Capacity after CloseAll = %d, want registered %d", got, customMaxSize)
	}
}

func TestManagerCloseAllPreservesTypedRegistration(t *testing.T) {
	manager := NewManager()
	t.Cleanup(func() { manager.CloseAll() })

	cfg := DefaultConfig()
	cfg.MaxSize = 100
	cfg.MaxCost = 5
	cfg.ShardCount = 1
	cfg.CleanupInterval = 0
	cfg.EvictionPolicy = LRU

	if err := RegisterCache[string, []byte](manager, "weighted", cfg, WithWeigher(func(_ string, value []byte) int64 {
		return int64(len(value))
	})); err != nil {
		t.Fatalf("RegisterCache() error = %v", err)
	}
	if _, err := GetCache[string, []byte](manager, "weighted"); err != nil {
		t.Fatalf("GetCache() error = %v", err)
	}
	if err := manager.CloseAll(); err != nil {
		t.Fatalf("CloseAll() error = %v", err)
	}

	cache, err := GetCache[string, []byte](manager, "weighted")
	if err != nil {
		t.Fatalf("GetCache() after CloseAll error = %v", err)
	}
	if err := cache.Set("large", make([]byte, 6), NoExpiration); !errors.Is(err, ErrItemTooLarge) {
		t.Fatalf("Set large error = %v, want ErrItemTooLarge after CloseAll rebuild", err)
	}
}

func TestGetCacheWithConfig(t *testing.T) {
	manager := NewManager()
	t.Cleanup(func() { manager.CloseAll() })

	const firstMaxSize = 2048
	cfg := DefaultConfig()
	cfg.MaxSize = firstMaxSize

	// No prior Register: the cache is created from the passed config.
	first, err := GetCacheWithConfig[string, int](manager, "c", cfg)
	if err != nil {
		t.Fatalf("GetCacheWithConfig() error = %v", err)
	}
	if got := first.Stats().Capacity; got != firstMaxSize {
		t.Fatalf("Capacity = %d, want %d", got, firstMaxSize)
	}

	// Existing entry: the passed config is ignored (get-or-create) and the same
	// instance is returned, never reconfigured.
	other := DefaultConfig()
	other.MaxSize = 9999
	second, err := GetCacheWithConfig[string, int](manager, "c", other)
	if err != nil {
		t.Fatalf("GetCacheWithConfig() error = %v", err)
	}
	if second != first {
		t.Fatal("expected the existing cache instance to be returned")
	}
	if got := second.Stats().Capacity; got != firstMaxSize {
		t.Fatalf("Capacity after second call = %d, want unchanged %d", got, firstMaxSize)
	}

	// Type mismatch is reported, mirroring GetCache.
	if _, err := GetCacheWithConfig[string, string](manager, "c", cfg); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("GetCacheWithConfig() type mismatch error = %v, want ErrTypeMismatch", err)
	}
}

func TestGetCacheWithConfigAppliesTypedOptions(t *testing.T) {
	manager := NewManager()
	t.Cleanup(func() { manager.CloseAll() })

	cfg := DefaultConfig()
	cfg.MaxSize = 100
	cfg.MaxCost = 8
	cfg.ShardCount = 1
	cfg.CleanupInterval = 0
	cfg.EvictionPolicy = LRU

	cache, err := GetCacheWithConfig[string, []byte](
		manager,
		"weighted",
		cfg,
		WithWeigher(func(_ string, value []byte) int64 {
			return int64(len(value))
		}),
	)
	if err != nil {
		t.Fatalf("GetCacheWithConfig() error = %v", err)
	}
	if err := cache.Set("small", []byte("12345"), NoExpiration); err != nil {
		t.Fatalf("Set small error = %v", err)
	}
	if got := cache.Cost(); got != 5 {
		t.Fatalf("Cost() = %d, want weighted cost 5", got)
	}
	if err := cache.Set("large", make([]byte, 9), NoExpiration); !errors.Is(err, ErrItemTooLarge) {
		t.Fatalf("Set large error = %v, want ErrItemTooLarge", err)
	}
}

func TestCacheCloseBehavior(t *testing.T) {
	cache := newDefaultTestCache[string, string](t)

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

func TestSetAsyncReturnsAfterEnqueueBeforeCommit(t *testing.T) {
	cache := newTestCache[string, string](t, Config{
		MaxSize:         10,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  LRU,
		WriteBufferSize: 1,
		WriteBatchSize:  1,
	})
	defer cache.Close()

	shard := cache.shards[0]
	shard.mu.Lock()
	locked := true
	defer func() {
		if locked {
			shard.mu.Unlock()
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- cache.SetAsync("key", "value", time.Hour)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SetAsync returned error: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("set blocked until commit; expected enqueue-only return")
	}

	if _, ok := shard.tab.lookup(cache.hasher.Sum("key"), "key"); ok {
		t.Fatal("set committed while shard lock was held")
	}

	shard.mu.Unlock()
	locked = false
	waitForWrites(t, cache)

	if value, found := cache.Get("key"); !found || value != "value" {
		t.Fatalf("expected value after Sync, got %q found=%v", value, found)
	}
}

func TestSetWaitsForCommit(t *testing.T) {
	cache := newTestCache[string, string](t, Config{
		MaxSize:         10,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  LRU,
		WriteBufferSize: 1,
		WriteBatchSize:  1,
	})
	defer cache.Close()

	shard := cache.shards[0]
	shard.mu.Lock()
	locked := true
	defer func() {
		if locked {
			shard.mu.Unlock()
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- cache.Set("key", "value", time.Hour)
	}()

	select {
	case err := <-done:
		t.Fatalf("Set returned before commit; err=%v", err)
	case <-time.After(20 * time.Millisecond):
	}

	if _, ok := shard.tab.lookup(cache.hasher.Sum("key"), "key"); ok {
		t.Fatal("set committed while shard lock was held")
	}

	shard.mu.Unlock()
	locked = false

	if err := <-done; err != nil {
		t.Fatalf("Set returned error: %v", err)
	}
	if value, found := cache.Get("key"); !found || value != "value" {
		t.Fatalf("expected value after Set, got %q found=%v", value, found)
	}
}

func TestDeleteOrdersAfterPendingSet(t *testing.T) {
	cache := newTestCache[string, string](t, Config{
		MaxSize:         10,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  LRU,
		WriteBufferSize: 8,
		WriteBatchSize:  8,
	})
	defer cache.Close()

	shard := cache.shards[0]
	shard.mu.Lock()
	locked := true
	defer func() {
		if locked {
			shard.mu.Unlock()
		}
	}()

	if err := cache.SetAsync("key", "value", time.Hour); err != nil {
		t.Fatal(err)
	}

	deleted := make(chan bool, 1)
	go func() {
		deleted <- cache.Delete("key")
	}()

	select {
	case <-deleted:
		t.Fatal("delete committed while shard lock was held")
	case <-time.After(20 * time.Millisecond):
	}

	shard.mu.Unlock()
	locked = false

	if ok := <-deleted; !ok {
		t.Fatal("delete returned false for pending set")
	}
	waitForWrites(t, cache)

	if _, found := cache.Get("key"); found {
		t.Fatal("delete after pending set resurrected key")
	}
}

func TestSetAsyncAppliesInlineWhenUncontended(t *testing.T) {
	cache := newTestCache[int, int](t, Config{
		MaxSize:         1024,
		ShardCount:      8,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  LRU,
		WriteBufferSize: 16,
		WriteBatchSize:  8,
	})
	defer cache.Close()

	for i := range 32 {
		if err := cache.SetAsync(i, i*7, time.Hour); err != nil {
			t.Fatalf("SetAsync(%d): %v", i, err)
		}
		// No Sync between the write and the read: an uncontended SetAsync must
		// have applied inline for the key to be visible here.
		if v, ok := cache.Get(i); !ok || v != i*7 {
			t.Fatalf("key %d not visible after uncontended SetAsync; got %d ok=%v", i, v, ok)
		}
	}
}

type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

func TestSetInPlaceUpdate(t *testing.T) {
	config := Config{
		MaxSize:         100,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      0,
		EvictionPolicy:  LRU,
		StatsEnabled:    true,
	}
	cache := newTestCache[string, string](t, config)
	defer cache.Close()

	// Set initial value
	cache.Set("key1", "value1", time.Hour)
	waitForWrites(t, cache)

	// Verify initial state
	if value, found := cache.Get("key1"); !found || value != "value1" {
		t.Errorf("Expected initial value 'value1', got '%s', found: %v", value, found)
	}

	// Get the item pointer before update (if we could access it)
	shard := cache.getShard("key1")
	shard.mu.RLock()
	originalItem, _ := shard.tab.lookup(cache.hasher.Sum("key1"), "key1")
	shard.mu.RUnlock()

	// Update the same key - should reuse the item
	cache.Set("key1", "value2", time.Hour)
	waitForWrites(t, cache)

	// Verify update worked
	if value, found := cache.Get("key1"); !found || value != "value2" {
		t.Errorf("Expected updated value 'value2', got '%s', found: %v", value, found)
	}

	// Check that item was reused (same pointer)
	shard.mu.RLock()
	updatedItem, _ := shard.tab.lookup(cache.hasher.Sum("key1"), "key1")
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

func TestSetLFUFrequencyReset(t *testing.T) {
	config := Config{
		MaxSize:         3,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      0,
		EvictionPolicy:  LFU,
		StatsEnabled:    true,
	}
	cache := newTestCache[string, int](t, config)
	defer cache.Close()

	// Add items
	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)
	waitForWrites(t, cache)

	// Access "a" multiple times to increase frequency
	for range 10 {
		cache.Get("a")
	}

	// Access other items less
	cache.Get("b")
	cache.Get("c")

	// Update "a" with new value - frequency should reset to 1
	cache.Set("a", 999, time.Hour)
	waitForWrites(t, cache)

	// Now add a new item to trigger eviction
	// "a" should now be evicted since its frequency was reset
	cache.Set("d", 4, time.Hour)
	waitForWrites(t, cache)

	// Verify that "a" was evicted (due to frequency reset)
	if _, found := cache.Get("a"); found {
		t.Error("Item 'a' should have been evicted after frequency reset")
	}
}
