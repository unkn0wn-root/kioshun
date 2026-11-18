package kioshun

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/unkn0wn-root/kioshun/internal/mathx"
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

func waitForWrites[K comparable, V any](t testing.TB, c *Cache[K, V]) {
	t.Helper()
	if err := c.Sync(); err != nil {
		t.Fatalf("wait for async writes: %v", err)
	}
}

func TestDefaultConfigValid(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig() is invalid: %v", err)
	}
}

func TestCacheBasicOperations(t *testing.T) {
	cache := newDefaultTestCache[string, string](t)
	defer cache.Close()

	cache.Set("key1", "value1", 1*time.Minute)
	waitForWrites(t, cache)

	if value, found := cache.Get("key1"); !found || value != "value1" {
		t.Errorf("Expected 'value1', got '%s', found: %v", value, found)
	}

	if _, found := cache.Get("nonexistent"); found {
		t.Error("Expected key to not exist")
	}

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

	cache.Set("expiring", "value", 100*time.Millisecond)
	waitForWrites(t, cache)

	if _, found := cache.Get("expiring"); !found {
		t.Error("Expected key to exist immediately after set")
	}

	time.Sleep(200 * time.Millisecond)

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
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      1 * time.Hour,
		EvictionPolicy:  LRU,
	}
	cache := newTestCache[string, string](t, config)
	defer cache.Close()

	cache.Set("key1", "value1", 1*time.Hour)
	cache.Set("key2", "value2", 1*time.Hour)
	cache.Set("key3", "value3", 1*time.Hour)
	waitForWrites(t, cache)

	cache.Get("key1")

	cache.Set("key4", "value4", 1*time.Hour)
	waitForWrites(t, cache)

	if _, found := cache.Get("key2"); found {
		t.Error("Expected key2 to be evicted")
	}

	if _, found := cache.Get("key1"); !found {
		t.Error("Expected key1 to still exist")
	}
}

func TestCacheStats(t *testing.T) {
	config := DefaultConfig()
	config.StatsEnabled = true
	cache := newTestCache[string, string](t, config)
	defer cache.Close()

	stats := cache.Stats()
	if stats.Hits != 0 || stats.Misses != 0 {
		t.Error("Expected initial stats to be zero")
	}

	cache.Set("key1", "value1", 1*time.Minute)
	waitForWrites(t, cache)

	cache.Get("key1")
	stats = cache.Stats()
	if stats.Hits != 1 {
		t.Errorf("Expected 1 hit, got %d", stats.Hits)
	}

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

	cache.Set("key1", "value1", 100*time.Millisecond)
	cache.Set("key2", "value2", 100*time.Millisecond)
	waitForWrites(t, cache)

	time.Sleep(150 * time.Millisecond)

	cache.Cleanup()
	time.Sleep(50 * time.Millisecond)

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

	config := DefaultConfig()
	err := manager.Register("test", config)
	if err != nil {
		t.Errorf("Expected no error registering cache, got %v", err)
	}

	cache, err := GetCache[string, string](manager, "test")
	if err != nil {
		t.Errorf("Expected no error getting cache, got %v", err)
	}

	cache.Set("key1", "value1", 1*time.Minute)
	waitForWrites(t, cache)
	if value, found := cache.Get("key1"); !found || value != "value1" {
		t.Errorf("Expected 'value1', got '%s', found: %v", value, found)
	}

	stats := manager.Stats()
	if len(stats) != 1 {
		t.Errorf("Expected 1 cache in stats, got %d", len(stats))
	}
}

func TestCacheGenerics(t *testing.T) {
	stringCache := newDefaultTestCache[string, string](t)
	defer stringCache.Close()

	intCache := newDefaultTestCache[int, string](t)
	defer intCache.Close()

	structCache := newDefaultTestCache[string, User](t)
	defer structCache.Close()

	stringCache.Set("key", "value", 1*time.Minute)
	waitForWrites(t, stringCache)
	if value, found := stringCache.Get("key"); !found || value != "value" {
		t.Error("String cache failed")
	}

	intCache.Set(123, "int_value", 1*time.Minute)
	waitForWrites(t, intCache)
	if value, found := intCache.Get(123); !found || value != "int_value" {
		t.Error("Int cache failed")
	}

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

	// Remove also drops the registration.
	if _, err := GetCache[string, int](manager, "c"); !errors.Is(err, ErrCacheNotRegistered) {
		t.Fatalf("GetCache() after Remove error = %v, want ErrCacheNotRegistered", err)
	}

	second, err := GetCacheWithConfig[string, int](manager, "c", DefaultConfig())
	if err != nil {
		t.Fatalf("GetCacheWithConfig() after Remove error = %v", err)
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

	// CloseAll keeps registrations.
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

	if err := RegisterCache(manager, "weighted", cfg, WithWeigher(func(_ string, value []byte) int64 {
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

	first, err := GetCacheWithConfig[string, int](manager, "c", cfg)
	if err != nil {
		t.Fatalf("GetCacheWithConfig() error = %v", err)
	}
	if got := first.Stats().Capacity; got != firstMaxSize {
		t.Fatalf("Capacity = %d, want %d", got, firstMaxSize)
	}

	// An existing cache ignores the new configuration.
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

	cache, err := GetCacheWithConfig(
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

	cache.Set("key1", "value1", 1*time.Minute)

	err := cache.Close()
	if err != nil {
		t.Errorf("Expected no error closing cache, got %v", err)
	}

	err = cache.Set("key2", "value2", 1*time.Minute)
	if err == nil {
		t.Error("Expected error when setting after close")
	}

	if _, found := cache.Get("key1"); found {
		t.Error("Expected get to fail after close")
	}

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
		// No Sync: an idle shard must apply SetAsync before returning.
		if v, ok := cache.Get(i); !ok || v != i*7 {
			t.Fatalf("key %d not visible after uncontended SetAsync; got %d ok=%v", i, v, ok)
		}
	}
}

func TestSetAsyncCommitsWithoutAnotherCall(t *testing.T) {
	c := newTestCache[string, string](t, Config{MaxSize: 10, ShardCount: 1, EvictionPolicy: LRU})
	defer c.Close()
	s := c.shards[0]
	s.mu.Lock() // makes SetAsync queue the write
	err := c.SetAsync("key", "value", NoExpiration)
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("SetAsync: %v", err)
	}
	kh := c.hasher.Sum("key")
	waitFor(t, func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		_, ok := s.tab.lookup(kh, "key")
		return ok
	})
}

// A producer or reader that raced Close can still ask for a wake-up.
func TestWriteWakeAfterCloseDoesNotBlock(t *testing.T) {
	for round := range 200 {
		c := newTestCache[int, int](t, Config{MaxSize: 10, ShardCount: 1, EvictionPolicy: LRU})
		c.Close()
		runWithin(t, fmt.Sprintf("round %d: a wake-up after Close", round), c.shards[0].queue.wakeConsumer)
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

	cache.Set("key1", "value1", time.Hour)
	waitForWrites(t, cache)

	if value, found := cache.Get("key1"); !found || value != "value1" {
		t.Errorf("Expected initial value 'value1', got '%s', found: %v", value, found)
	}

	shard := cache.getShard("key1")
	shard.mu.RLock()
	originalItem, _ := shard.tab.lookup(cache.hasher.Sum("key1"), "key1")
	shard.mu.RUnlock()

	cache.Set("key1", "value2", time.Hour)
	waitForWrites(t, cache)

	if value, found := cache.Get("key1"); !found || value != "value2" {
		t.Errorf("Expected updated value 'value2', got '%s', found: %v", value, found)
	}

	shard.mu.RLock()
	updatedItem, _ := shard.tab.lookup(cache.hasher.Sum("key1"), "key1")
	shard.mu.RUnlock()

	if originalItem != updatedItem {
		t.Error("Expected in-place update to reuse the same cacheItem, but got different pointers")
	}

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

	cache.Set("a", 1, time.Hour)
	cache.Set("b", 2, time.Hour)
	cache.Set("c", 3, time.Hour)
	waitForWrites(t, cache)

	for range 10 {
		cache.Get("a")
	}

	cache.Get("b")
	cache.Get("c")

	cache.Set("a", 999, time.Hour)
	waitForWrites(t, cache)

	cache.Set("d", 4, time.Hour)
	waitForWrites(t, cache)

	if _, found := cache.Get("a"); found {
		t.Error("Item 'a' should have been evicted after frequency reset")
	}
}

var allPolicies = []EvictionPolicy{LRU, LFU, FIFO, SieveTinyLFU}

func TestAutoShardCountKeepsMinShardEntries(t *testing.T) {
	cpuShards := mathx.NextPowerOf2(min(runtime.NumCPU()*shardMultiplier, maxShardCount))
	for _, policy := range allPolicies {
		for _, maxSize := range []int64{0, 1, 16, 64, 100, 1000, 10000, 100000} {
			c := newTestCache[int, int](t, Config{MaxSize: maxSize, EvictionPolicy: policy})
			shards := len(c.shards)
			c.Close()

			switch {
			case maxSize == 0 || maxSize == 100000:
				if shards != cpuShards {
					t.Errorf("%s MaxSize=%d: shards=%d, want %d", policyName(policy), maxSize, shards, cpuShards)
				}
			case maxSize <= 100:
				if shards != 1 {
					t.Errorf("%s MaxSize=%d: shards=%d, want 1", policyName(policy), maxSize, shards)
				}
			}
			if maxSize > 0 && shards != 1 && maxSize/int64(shards) < 64 {
				t.Errorf("%s MaxSize=%d: shards=%d leaves %d entries per shard, want at least 64",
					policyName(policy), maxSize, shards, maxSize/int64(shards))
			}
		}
	}
}

func TestSmallCacheHoldsMaxSize(t *testing.T) {
	for _, policy := range allPolicies {
		for _, maxSize := range []int{16, 64, 100} {
			t.Run(fmt.Sprintf("%s/%d", policyName(policy), maxSize), func(t *testing.T) {
				c := newTestCache[int, int](t, Config{MaxSize: int64(maxSize), EvictionPolicy: policy})
				defer c.Close()

				for k := range maxSize {
					if err := c.Set(k, k, NoExpiration); err != nil {
						t.Fatalf("Set(%d): %v", k, err)
					}
				}
				if got := c.Size(); got != int64(maxSize) {
					t.Fatalf("Size()=%d after %d distinct keys, want %d (shards=%d)", got, maxSize, maxSize, len(c.shards))
				}
				for round := range 5 {
					for k := range maxSize {
						if _, ok := c.Get(k); !ok {
							t.Fatalf("round %d: Get(%d) missed", round, k)
						}
					}
				}
			})
		}
	}
}

func TestExplicitShardCountIsKept(t *testing.T) {
	tests := []struct {
		maxSize    int64
		shardCount int
		want       int
	}{
		{maxSize: 16, shardCount: 4, want: 4},
		{maxSize: 3, shardCount: 8, want: 2},
		{maxSize: 100, shardCount: 3, want: 4},
	}
	for _, tt := range tests {
		c := newTestCache[int, int](t, Config{MaxSize: tt.maxSize, ShardCount: tt.shardCount, EvictionPolicy: LRU})
		if got := len(c.shards); got != tt.want {
			t.Errorf("MaxSize=%d ShardCount=%d: shards=%d, want %d", tt.maxSize, tt.shardCount, got, tt.want)
		}
		for i, s := range c.shards {
			if s.cap <= 0 {
				t.Errorf("MaxSize=%d ShardCount=%d: shard %d cap=%d, want > 0", tt.maxSize, tt.shardCount, i, s.cap)
			}
		}
		c.Close()
	}
}

func TestExistsDoesNotWaitForShardLock(t *testing.T) {
	c := newTestCache[string, int](t, Config{MaxSize: 64, EvictionPolicy: SieveTinyLFU})
	defer c.Close()
	if err := c.Set("k", 1, time.Hour); err != nil {
		t.Fatal(err)
	}

	s := c.getShard("k")
	s.mu.Lock()
	defer s.mu.Unlock()

	done := make(chan bool, 1)
	go func() { done <- c.Exists("k") }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("Exists(k) = false, want true")
		}
	case <-time.After(time.Second):
		t.Fatal("Exists waited for the shard write lock")
	}
}

func TestExistsSharesReadLock(t *testing.T) {
	for _, policy := range []EvictionPolicy{LRU, LFU, FIFO} {
		t.Run(policyName(policy), func(t *testing.T) {
			c := newTestCache[string, int](t, Config{MaxSize: 64, EvictionPolicy: policy})
			defer c.Close()
			if err := c.Set("k", 1, time.Hour); err != nil {
				t.Fatal(err)
			}

			s := c.getShard("k")
			s.mu.RLock()
			defer s.mu.RUnlock()

			done := make(chan bool, 1)
			go func() { done <- c.Exists("k") }()
			select {
			case ok := <-done:
				if !ok {
					t.Fatal("Exists(k) = false, want true")
				}
			case <-time.After(time.Second):
				t.Fatal("Exists waited for the shard write lock while a reader held it")
			}
		})
	}
}

func TestExistsSeesQueuedSetAsync(t *testing.T) {
	c := newTestCache[int, int](t, Config{MaxSize: 64, ShardCount: 1})
	defer c.Close()

	s := c.shards[0]
	// A pending wake state stops producers from waking the worker, and holding
	// the shard lock keeps SetAsync off the inline path.
	s.queue.wakeState.Store(1)
	defer s.queue.wakeState.Store(0)
	s.mu.Lock()
	err := c.SetAsync(1, 1, NoExpiration)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if s.queue.quiescent() {
		t.Fatal("SetAsync was not queued")
	}

	if !c.Exists(1) {
		t.Fatal("Exists(1) = false for a queued SetAsync")
	}
}

func TestExistsRemovesExpiredEntry(t *testing.T) {
	for _, policy := range allPolicies {
		t.Run(policyName(policy), func(t *testing.T) {
			c := newTestCache[string, int](t, Config{MaxSize: 64, EvictionPolicy: policy, StatsEnabled: true})
			defer c.Close()
			if err := c.Set("k", 1, time.Millisecond); err != nil {
				t.Fatal(err)
			}
			time.Sleep(5 * time.Millisecond)

			if c.Exists("k") {
				t.Fatal("Exists(k) = true for an expired entry")
			}
			if got := c.Size(); got != 0 {
				t.Fatalf("Size()=%d, want 0 after Exists found the entry expired", got)
			}
			st := c.Stats()
			if st.Expirations != 1 || st.Hits != 0 || st.Misses != 0 {
				t.Fatalf("Expirations/Hits/Misses = %d/%d/%d, want 1/0/0", st.Expirations, st.Hits, st.Misses)
			}
		})
	}
}

func TestExistsKeepsEntryRefreshedWhileWaitingForLock(t *testing.T) {
	c := newTestCache[string, int](t, Config{MaxSize: 64, ShardCount: 1, EvictionPolicy: SieveTinyLFU})
	defer c.Close()
	if err := c.Set("k", 1, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)

	s := c.shards[0]
	s.mu.Lock()
	done := make(chan bool, 1)
	go func() { done <- c.Exists("k") }()
	time.Sleep(20 * time.Millisecond) // let Exists see the expired item and block on the lock

	cmd := writeCommand[string, int]{op: writeSet, key: "k", value: 2, hash: c.hasher.Sum("k"), expireTime: int64(time.Hour)}
	c.stampExpireTime(&cmd, c.nowNano())
	c.applySet(s, &cmd)
	s.mu.Unlock()

	if !<-done {
		t.Fatal("Exists(k) = false for an entry refreshed before Exists took the lock")
	}
	if _, ok := c.Get("k"); !ok {
		t.Fatal("Exists removed an entry that was refreshed before it took the lock")
	}
}

func TestExistsConcurrentWithWrites(t *testing.T) {
	configs := []struct {
		name string
		cfg  Config
	}{
		{"LRU", Config{MaxSize: 256, EvictionPolicy: LRU}},
		{"LFU", Config{MaxSize: 256, EvictionPolicy: LFU}},
		{"FIFO", Config{MaxSize: 256, EvictionPolicy: FIFO}},
		{"SieveTinyLFU", Config{MaxSize: 256, EvictionPolicy: SieveTinyLFU}},
		{"UnboundedSieveTinyLFU", Config{EvictionPolicy: SieveTinyLFU}},
	}
	for _, tc := range configs {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCache[int, int](t, tc.cfg)
			defer c.Close()

			var wg sync.WaitGroup
			for g := range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range 2000 {
						k := (g*7 + i) % 64
						switch i % 6 {
						case 0:
							if err := c.Set(k, i, time.Duration(i%4)*time.Millisecond); err != nil {
								t.Error(err)
								return
							}
						case 1:
							c.Delete(k)
						case 2, 3:
							c.Exists(k)
						case 4:
							c.Get(k)
						case 5:
							if i%60 == 5 {
								c.Cleanup()
							}
						}
					}
				}()
			}
			wg.Wait()
		})
	}
}

func BenchmarkExistsParallel(b *testing.B) {
	c := newTestCache[int, int](b, Config{MaxSize: 100000})
	defer c.Close()
	for k := range 1000 {
		if err := c.Set(k, k, NoExpiration); err != nil {
			b.Fatal(err)
		}
	}
	const hotKeys = 8

	b.Run("Get", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for i := 0; pb.Next(); i++ {
				c.Get(i % hotKeys)
			}
		})
	})
	b.Run("Exists", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for i := 0; pb.Next(); i++ {
				c.Exists(i % hotKeys)
			}
		})
	})
}

func TestSetWithCallbackStartsNoGoroutinePerKey(t *testing.T) {
	c := newTestCache[int, int](t, Config{MaxSize: 1000, EvictionPolicy: LRU})
	defer c.Close()

	before := runtime.NumGoroutine()
	for k := range 1000 {
		if err := c.SetWithCallback(k, k, time.Hour, func(int, int) {}); err != nil {
			t.Fatalf("SetWithCallback(%d): %v", k, err)
		}
	}
	if grew := runtime.NumGoroutine() - before; grew > 10 {
		t.Fatalf("NumGoroutine grew by %d after 1000 pending callbacks, want at most 10", grew)
	}
}

func TestSetWithCallbackNotFiredAfterClose(t *testing.T) {
	c := newTestCache[string, int](t, Config{MaxSize: 64, EvictionPolicy: LRU})
	var called atomic.Bool
	if err := c.SetWithCallback("k", 1, 20*time.Millisecond, func(string, int) { called.Store(true) }); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if called.Load() {
		t.Fatal("callback ran after Close")
	}
}

type cbPayload struct{ _ [1 << 10]byte }

func newCallbackCache(t *testing.T, shards int) *Cache[int, *cbPayload] {
	t.Helper()
	c, err := New[int, *cbPayload](Config{MaxSize: 1 << 16, ShardCount: shards, EvictionPolicy: LRU})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func noCallback(int, *cbPayload) {}

func TestClosePendingCallbackKeepsNothing(t *testing.T) {
	c := newCallbackCache(t, 4)
	v := new(cbPayload)
	wv, wc := weak.Make(v), weak.Make(c)
	if err := c.SetWithCallback(1, v, time.Hour, noCallback); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c = nil
	runtime.GC()
	if wv.Value() != nil {
		t.Error("value of a pending callback still reachable after Close")
	}
	// Close uses the cache's sync.Pool, which stays reachable until the next cycle starts.
	runtime.GC()
	if wc.Value() != nil {
		t.Error("cache still reachable after Close with a pending callback")
	}
}

// Live timers before and after keep the stopped timer inside the runtime's heap.
func TestCloseReleasesCallbackRuntimeStillHolds(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	early := time.AfterFunc(30*time.Minute, func() {})
	defer early.Stop()

	c := newCallbackCache(t, 1)
	v := new(cbPayload)
	wv, wc := weak.Make(v), weak.Make(c)
	if err := c.SetWithCallback(1, v, time.Hour, noCallback); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		late := time.AfterFunc(2*time.Hour, func() {})
		defer late.Stop()
	}
	c.Close()
	c = nil
	runtime.GC()
	if wv.Value() != nil {
		t.Error("value reachable through a stopped timer after Close")
	}
	runtime.GC()
	if wc.Value() != nil {
		t.Error("cache reachable through a stopped timer after Close")
	}
}

// A TTL this short has passed before the timer starts, so the timer fires at once.
func TestSetWithCallbackElapsedTTLRunsOnce(t *testing.T) {
	const n = 10000
	c := newCallbackCache(t, 4)
	defer c.Close()

	weaks := make([]weak.Pointer[cbPayload], n)
	calls := make([]atomic.Int32, n)
	var ran atomic.Int32
	done := make(chan struct{})
	for k := range n {
		v := new(cbPayload)
		weaks[k] = weak.Make(v)
		err := c.SetWithCallback(k, v, time.Nanosecond, func(k int, _ *cbPayload) {
			calls[k].Add(1)
			if ran.Add(1) == n {
				close(done)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%d of %d callbacks ran within 5s", ran.Load(), n)
	}
	time.Sleep(10 * time.Millisecond)
	for k := range calls {
		if got := calls[k].Load(); got != 1 {
			t.Fatalf("callback for %d ran %d times, want 1", k, got)
		}
	}

	// A fired timer must not keep its value while the cache stays open.
	c.Cleanup()
	runtime.GC()
	for k, w := range weaks {
		if w.Value() != nil {
			t.Fatalf("value %d still reachable after its callback ran and Cleanup removed it", k)
		}
	}
}

// A 1ms ttl makes timers fire while keys are still being set, so many deadlines
// fall right at the instant a timer fires.
func TestSetWithCallbackRunsEachOnceAfterTTL(t *testing.T) {
	const (
		n   = 100_000
		ttl = time.Millisecond
	)
	c, err := New[int, int](Config{MaxSize: 2 * n, ShardCount: 256, EvictionPolicy: LRU})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	calls := make([]atomic.Int32, n)
	set := make([]time.Time, n)
	var ran, early atomic.Int64
	done := make(chan struct{})
	for k := range n {
		set[k] = time.Now()
		err := c.SetWithCallback(k, k, ttl, func(k, _ int) {
			if time.Since(set[k]) < ttl {
				early.Add(1)
			}
			calls[k].Add(1)
			if ran.Add(1) == n {
				close(done)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
		time.Sleep(20 * time.Millisecond)
	case <-time.After(5 * time.Second):
	}
	var missing, dup int
	for k := range calls {
		switch calls[k].Load() {
		case 0:
			missing++
		case 1:
		default:
			dup++
		}
	}
	if missing != 0 || dup != 0 || early.Load() != 0 {
		t.Fatalf("of %d callbacks, %d never ran, %d ran more than once and %d ran before their ttl", n, missing, dup, early.Load())
	}
}

// Odd keys expire at once, so their timers fire while Close runs. With one
// shard and several Ps, writers queue on its callback lock while Close runs.
func TestSetWithCallbackRacingCloseKeepsNothing(t *testing.T) {
	const writers = 8
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(max(runtime.GOMAXPROCS(0), 4)))
	short := new(cbPayload)
	for round := range 50 {
		c := newCallbackCache(t, 1)
		var mu sync.Mutex
		var weaks []weak.Pointer[cbPayload]
		var wg sync.WaitGroup
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := w; ; k += writers {
					v, ttl := short, time.Microsecond
					if k%2 == 0 {
						v, ttl = new(cbPayload), time.Hour
						mu.Lock()
						weaks = append(weaks, weak.Make(v))
						mu.Unlock()
					}
					if err := c.SetWithCallback(k, v, ttl, noCallback); err != nil {
						if !errors.Is(err, ErrCacheClosed) {
							t.Errorf("SetWithCallback: %v", err)
						}
						return
					}
				}
			}()
		}
		time.Sleep(time.Millisecond)
		c.Close()
		wg.Wait()
		// No task may reach the heap once Close has swept it, whether or not a
		// stopped timer still holds the callback timers for a moment.
		ct := c.shards[0].callbacks
		ct.mu.Lock()
		pending := len(ct.heap)
		ct.mu.Unlock()
		if pending > 0 {
			t.Fatalf("round %d: %d callbacks pending after Close", round, pending)
		}
		c = nil
		// Retry the GC briefly, so that a value held for a moment by work that is
		// still finishing is not taken for a leak.
		left := len(weaks)
		for try := 0; try < 3 && left > 0; try++ {
			if try > 0 {
				time.Sleep(time.Millisecond)
			}
			runtime.GC()
			left = 0
			for _, w := range weaks {
				if w.Value() != nil {
					left++
				}
			}
		}
		if left > 0 {
			t.Fatalf("round %d: %d of %d values still reachable after Close and 3 GCs", round, left, len(weaks))
		}
	}
}

// The flag is set without Close, so the timer stays scheduled and fires.
func TestCallbackNotRunAfterClosedFlag(t *testing.T) {
	c := newCallbackCache(t, 1)
	defer c.Close()
	var calls atomic.Int32
	cb := func(int, *cbPayload) { calls.Add(1) }
	if err := c.SetWithCallback(1, new(cbPayload), time.Millisecond, cb); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); calls.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("callback did not run within 5s of its ttl")
		}
		time.Sleep(time.Millisecond)
	}
	// Keys 2 and 3 share a wake, and key 4 has one of its own.
	start := time.Now()
	for i, ttl := range []time.Duration{200 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond} {
		if err := c.SetWithCallback(2+i, new(cbPayload), ttl-time.Since(start), cb); err != nil {
			t.Fatal(err)
		}
	}
	c.closed.Store(true)
	time.Sleep(500 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("a callback ran after the closed flag was set (%d calls)", got)
	}
}

// Close keeps nothing of the pending callbacks.
// With one P the runtime drops stopped timers at its next check, so only live ones stay.
func TestCloseReleasesPendingCallbacks(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	const n = 100_000
	heap := func() int64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return int64(m.HeapAlloc)
	}
	before := heap()
	c, err := New[int, int](Config{MaxSize: 1 << 18, ShardCount: 4, EvictionPolicy: LRU})
	if err != nil {
		t.Fatal(err)
	}
	for k := range n {
		if err := c.SetWithCallback(k, k, time.Hour, func(int, int) {}); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	c = nil
	if held := (heap() - before) / n; held > 64 {
		t.Fatalf("%d bytes per callback still held after Close, want at most 64", held)
	}
}

func TestCloseReturnsWhileCallbackRuns(t *testing.T) {
	c := newCallbackCache(t, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	err := c.SetWithCallback(1, new(cbPayload), time.Nanosecond, func(int, *cbPayload) {
		close(entered)
		<-release
	})
	if err != nil {
		t.Fatal(err)
	}
	runWithin(t, "the callback start", func() { <-entered })
	runWithin(t, "Close while a callback runs", func() { c.Close() })
}

func TestCallbackCanCallSetWithCallbackAndClose(t *testing.T) {
	c := newCallbackCache(t, 1)
	done := make(chan struct{})
	err := c.SetWithCallback(1, new(cbPayload), time.Nanosecond, func(int, *cbPayload) {
		defer close(done)
		if err := c.SetWithCallback(2, new(cbPayload), time.Hour, noCallback); err != nil {
			t.Errorf("SetWithCallback inside a callback: %v", err)
		}
		c.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	runWithin(t, "a callback that calls SetWithCallback and Close", func() { <-done })
	if err := c.Set(3, new(cbPayload), time.Hour); !errors.Is(err, ErrCacheClosed) {
		t.Fatalf("Set after a callback closed the cache: %v, want ErrCacheClosed", err)
	}
}

func TestCloseTwiceWithPendingCallbacks(t *testing.T) {
	for round := range 20 {
		c := newCallbackCache(t, 4)
		v := new(cbPayload)
		wv := weak.Make(v)
		var called atomic.Bool
		if err := c.SetWithCallback(1, v, 50*time.Millisecond, func(int, *cbPayload) { called.Store(true) }); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := c.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
				runtime.GC()
				if wv.Value() != nil {
					t.Errorf("round %d: value reachable after a concurrent Close returned", round)
				}
			}()
		}
		wg.Wait()
		if err := c.Close(); err != nil {
			t.Fatalf("third Close: %v", err)
		}
		if round == 0 {
			time.Sleep(100 * time.Millisecond)
			if called.Load() {
				t.Fatal("callback ran after Close")
			}
		}
	}
}

func TestSetNegativeTTLStoresNothing(t *testing.T) {
	for _, policy := range allPolicies {
		t.Run(policyName(policy), func(t *testing.T) {
			c := newTestCache[int, int](t, Config{MaxSize: 64, EvictionPolicy: policy})
			defer c.Close()

			if err := c.Set(1, 1, -time.Second); err != nil {
				t.Fatalf("Set: %v", err)
			}
			if err := c.SetAsync(2, 2, -2*time.Nanosecond); err != nil {
				t.Fatalf("SetAsync: %v", err)
			}
			if err := c.SetWithCallback(3, 3, -time.Second, func(int, int) {}); err != nil {
				t.Fatalf("SetWithCallback: %v", err)
			}
			waitForWrites(t, c)

			// Check Size first, because Exists removes entries it finds expired.
			if got := c.Size(); got != 0 {
				t.Fatalf("Size()=%d after Sets with negative TTLs, want 0", got)
			}
			for k := 1; k <= 3; k++ {
				if c.Exists(k) {
					t.Errorf("Exists(%d) = true after Set with a negative TTL", k)
				}
			}
		})
	}
}

func TestSetNegativeTTLRemovesExistingEntry(t *testing.T) {
	for _, policy := range allPolicies {
		t.Run(policyName(policy), func(t *testing.T) {
			c := newTestCache[int, int](t, Config{MaxSize: 64, EvictionPolicy: policy})
			defer c.Close()

			if err := c.Set(1, 1, time.Hour); err != nil {
				t.Fatal(err)
			}
			if err := c.Set(1, 2, -time.Second); err != nil {
				t.Fatal(err)
			}
			if got := c.Size(); got != 0 {
				t.Fatalf("Size()=%d, want 0", got)
			}
			if v, ok := c.Get(1); ok {
				t.Fatalf("Get(1) = %d, true after Set with a negative TTL, want a miss", v)
			}
		})
	}
}

func TestSetAsyncNegativeTTLOrdersAfterQueuedSet(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         64,
		ShardCount:      1,
		EvictionPolicy:  LRU,
		WriteBufferSize: 8,
	})
	defer c.Close()

	s := c.shards[0]
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()

	if err := c.SetAsync(1, 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.SetAsync(1, 2, -time.Second) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("SetAsync with a negative TTL waited for the shard lock")
	}

	s.mu.Unlock()
	locked = false
	waitForWrites(t, c)

	if v, ok := c.Get(1); ok {
		t.Fatalf("Get(1) = %d, true, want a miss: the negative TTL must apply after the queued Set", v)
	}
}

func TestSetHugeTTLKeepsEntry(t *testing.T) {
	setters := []struct {
		name string
		set  func(c *Cache[string, int], key string, value int, ttl time.Duration) error
	}{
		{"Set", func(c *Cache[string, int], key string, value int, ttl time.Duration) error {
			return c.Set(key, value, ttl)
		}},
		{"SetAsync", func(c *Cache[string, int], key string, value int, ttl time.Duration) error {
			if err := c.SetAsync(key, value, ttl); err != nil {
				return err
			}
			return c.Sync()
		}},
		{"SetWithCallback", func(c *Cache[string, int], key string, value int, ttl time.Duration) error {
			return c.SetWithCallback(key, value, ttl, func(string, int) {})
		}},
		{"QueuedSetAsync", func(c *Cache[string, int], key string, value int, ttl time.Duration) error {
			// Holding the shard lock keeps SetAsync off the inline path, so the
			// worker stamps the write in a batch.
			s := c.getShard(key)
			s.mu.Lock()
			err := c.SetAsync(key, value, ttl)
			s.mu.Unlock()
			if err != nil {
				return err
			}
			return c.Sync()
		}},
	}
	for _, policy := range allPolicies {
		for _, st := range setters {
			t.Run(policyName(policy)+"/"+st.name, func(t *testing.T) {
				c := newTestCache[string, int](t, Config{MaxSize: 64, EvictionPolicy: policy})
				defer c.Close()

				if err := c.Set("old", 1, time.Hour); err != nil {
					t.Fatal(err)
				}
				if err := st.set(c, "old", 2, time.Duration(math.MaxInt64)); err != nil {
					t.Fatalf("set old with MaxInt64 TTL: %v", err)
				}
				if v, ok := c.Get("old"); !ok || v != 2 {
					t.Errorf("Get(old) = %d, %v after MaxInt64 TTL, want 2, true", v, ok)
				}
				if err := st.set(c, "new", 3, math.MaxInt64-1); err != nil {
					t.Fatalf("set new with MaxInt64-1 TTL: %v", err)
				}
				if v, ok := c.Get("new"); !ok || v != 3 {
					t.Errorf("Get(new) = %d, %v after MaxInt64-1 TTL, want 3, true", v, ok)
				}
			})
		}
	}
}

func TestNoExpirationNeverExpires(t *testing.T) {
	c := newTestCache[string, int](t, Config{MaxSize: 64, DefaultTTL: time.Hour})
	defer c.Close()

	if err := c.Set("k", 1, NoExpiration); err != nil {
		t.Fatal(err)
	}
	if v, ttl, ok := c.GetWithTTL("k"); !ok || v != 1 || ttl != -1 {
		t.Fatalf("GetWithTTL(k) = %d, %v, %v, want 1, -1ns, true", v, ttl, ok)
	}
}

func TestZeroDefaultTTLNeverExpires(t *testing.T) {
	for _, defaultTTL := range []time.Duration{0, NoExpiration} {
		t.Run(defaultTTL.String(), func(t *testing.T) {
			c := newTestCache[string, int](t, Config{DefaultTTL: defaultTTL})
			defer c.Close()

			if err := c.Set("k", 1, DefaultExpiration); err != nil {
				t.Fatal(err)
			}
			if v, ttl, ok := c.GetWithTTL("k"); !ok || v != 1 || ttl != -1 {
				t.Fatalf("GetWithTTL(k) = %d, %v, %v, want 1, -1ns, true", v, ttl, ok)
			}
		})
	}
}

// Each due callback waits for all the others, so they must all run at once.
// There are more of them than one fire takes.
func TestBlockedCallbackDoesNotDelayOthers(t *testing.T) {
	const n = 40
	c := newCallbackCache(t, 1)
	defer c.Close()
	release := make(chan struct{})
	defer close(release)
	var entered sync.WaitGroup
	entered.Add(n)
	all := make(chan struct{})
	go func() { entered.Wait(); close(all) }()
	// Shrinking ttls give every callback the same deadline, so one wake finds them all due.
	start := time.Now()
	for k := range n {
		err := c.SetWithCallback(k, new(cbPayload), 20*time.Millisecond-time.Since(start), func(int, *cbPayload) {
			entered.Done()
			select {
			case <-all:
			case <-release:
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	runWithin(t, "due callbacks that each wait for all the others", func() { <-all })
}

func TestShortCallbackNotHeldByLongerOne(t *testing.T) {
	c := newCallbackCache(t, 1)
	defer c.Close()
	if err := c.SetWithCallback(1, new(cbPayload), time.Hour, noCallback); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	if err := c.SetWithCallback(2, new(cbPayload), time.Millisecond, func(int, *cbPayload) { close(done) }); err != nil {
		t.Fatal(err)
	}
	if err := c.SetWithCallback(3, new(cbPayload), time.Hour, noCallback); err != nil {
		t.Fatal(err)
	}
	runWithin(t, "a 1ms callback scheduled between two 1h ones", func() { <-done })
}

// A callback runs only if its entry is still present and expired. Without
// spacing the four keys share one deadline, so one wake takes them as a batch.
// Spaced deadlines give each key a wake of its own.
func TestCallbackSkippedForDeletedOrRenewedKey(t *testing.T) {
	for _, spacing := range []time.Duration{0, 15 * time.Millisecond} {
		t.Run(fmt.Sprintf("spacing=%v", spacing), func(t *testing.T) {
			tc := newTickCache(t, 4)
			done := make(chan struct{})
			tc.hook = func(k int) {
				if k == 3 {
					close(done)
				}
			}
			d := tc.nowNano() + int64(20*time.Millisecond)
			for k := range 4 {
				tc.put(k, d+int64(k)*int64(spacing))
			}
			tc.Delete(0)
			if err := tc.Set(1, 1, time.Hour); err != nil {
				t.Fatal(err)
			}
			if err := tc.Set(2, 2, NoExpiration); err != nil {
				t.Fatal(err)
			}
			runWithin(t, "the callback of a key left alone", func() { <-done })
			// Other callbacks of a batch may run just after key 3's, so give them a moment.
			time.Sleep(10 * time.Millisecond)
			if a, b, c := tc.calls[0].Load(), tc.calls[1].Load(), tc.calls[2].Load(); a+b+c != 0 {
				t.Fatalf("callbacks ran for a deleted key (%d), a key renewed for 1h (%d) or a key renewed without expiry (%d)", a, b, c)
			}
		})
	}
}

func TestCallbacksRunAfterTheirTTL(t *testing.T) {
	const n = 200
	c := newCallbackCache(t, 4)
	defer c.Close()
	var wg sync.WaitGroup
	wg.Add(n)
	for k := range n {
		ttl := time.Duration(20-k%20) * time.Millisecond
		start := time.Now()
		err := c.SetWithCallback(k, new(cbPayload), ttl, func(int, *cbPayload) {
			if d := time.Since(start); d < ttl {
				t.Errorf("callback ran %v after Set, before its ttl of %v", d, ttl)
			}
			wg.Done()
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	runWithin(t, "callbacks with mixed ttls", wg.Wait)
}

// A SetWithCallback that commits before Close can schedule after Close returns.
func TestScheduleCallbackAfterCloseKeepsNothing(t *testing.T) {
	c := newCallbackCache(t, 1)
	c.Close()
	v := new(cbPayload)
	wv := weak.Make(v)
	c.scheduleCallback(c.shards[0], callbackTask[int, *cbPayload]{
		key:        1,
		value:      v,
		expireTime: c.nowNano() + int64(time.Hour),
		callback:   noCallback,
	})
	runtime.GC()
	if wv.Value() != nil {
		t.Fatal("callback scheduled after Close keeps its value")
	}
}

// Close must stop the shard's timer, also after it was re-armed for an earlier deadline.
func TestCloseStopsShardTimer(t *testing.T) {
	c := newCallbackCache(t, 1)
	if err := c.SetWithCallback(1, new(cbPayload), time.Hour, noCallback); err != nil {
		t.Fatal(err)
	}
	timer := c.shards[0].callbacks.timer
	if err := c.SetWithCallback(2, new(cbPayload), time.Minute, noCallback); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if timer.Stop() {
		t.Fatal("shard timer still scheduled after Close")
	}
}

// A wake before the earliest deadline must re-arm the timer, not take the task early.
func TestEarlyWakeRearmsTimer(t *testing.T) {
	c := newCallbackCache(t, 1)
	defer c.Close()
	done := make(chan struct{})
	if err := c.SetWithCallback(1, new(cbPayload), 50*time.Millisecond, func(int, *cbPayload) { close(done) }); err != nil {
		t.Fatal(err)
	}
	ct := c.shards[0].callbacks
	ct.mu.Lock()
	ct.timer.Stop()
	deadline := ct.heap[0].expireTime
	ct.mu.Unlock()
	ct.fire()
	ct.mu.Lock()
	left := len(ct.heap)
	ct.mu.Unlock()
	if c.nowNano() <= deadline && left != 1 {
		t.Fatal("a wake before the deadline took the task")
	}
	runWithin(t, "a callback after an early wake", func() { <-done })
}

// A ttl at the int64 limit saturates instead of wrapping into the past. Behind
// a short callback it arms the timer for the far future. Alone it starts no
// timer, and Close must still drop it.
func TestSetWithCallbackMaxTTL(t *testing.T) {
	c := newCallbackCache(t, 1)
	defer c.Close()
	var ran atomic.Bool
	if err := c.SetWithCallback(1, new(cbPayload), time.Duration(math.MaxInt64), func(int, *cbPayload) { ran.Store(true) }); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	if err := c.SetWithCallback(2, new(cbPayload), time.Millisecond, func(int, *cbPayload) { close(done) }); err != nil {
		t.Fatal(err)
	}
	runWithin(t, "a 1ms callback ahead of one at the int64 limit", func() { <-done })
	ct := c.shards[0].callbacks
	ct.mu.Lock()
	pending, when := len(ct.heap), ct.when
	ct.mu.Unlock()
	if pending != 1 {
		t.Fatalf("after the 1ms callback, %d callbacks pending, want 1", pending)
	}
	if now := c.nowNano(); when-now < int64(100*365*24*time.Hour) {
		t.Fatalf("after the 1ms callback, the timer is set %v from now, want over 100 years", time.Duration(when-now))
	}
	if _, ok := c.Get(1); !ok {
		t.Fatal("entry with a ttl at the int64 limit is already expired")
	}
	time.Sleep(20 * time.Millisecond)
	if ran.Load() {
		t.Fatal("callback with a ttl at the int64 limit ran")
	}

	alone := newCallbackCache(t, 1)
	if err := alone.SetWithCallback(1, new(cbPayload), time.Duration(math.MaxInt64), noCallback); err != nil {
		t.Fatal(err)
	}
	ct = alone.shards[0].callbacks
	ct.mu.Lock()
	pending, started := len(ct.heap), ct.timer != nil
	ct.mu.Unlock()
	if pending != 1 || started {
		t.Fatalf("alone, %d pending and timer started %v, want 1 and false", pending, started)
	}
	if err := alone.Close(); err != nil {
		t.Fatal(err)
	}
	ct.mu.Lock()
	pending = len(ct.heap)
	ct.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d callbacks pending after Close with no timer started", pending)
	}
}

// A far deadline is armed a little early. A later task due after the armed time
// needs no Reset, and one due before it re-arms the timer.
func TestFarDeadlineArmsTimerEarly(t *testing.T) {
	c := newCallbackCache(t, 1)
	defer c.Close()
	ct := c.shards[0].callbacks
	set := func(k int, ttl time.Duration) (when, deadline int64) {
		t.Helper()
		if err := c.SetWithCallback(k, new(cbPayload), ttl, noCallback); err != nil {
			t.Fatal(err)
		}
		ct.mu.Lock()
		defer ct.mu.Unlock()
		return ct.when, ct.heap[0].expireTime
	}
	start := c.nowNano()
	when, deadline := set(1, time.Hour)
	if when >= deadline || when < start+int64(time.Hour)*15/16 {
		t.Fatalf("a 1h deadline armed the timer %v from now, want from 15/16 h up to 1h", time.Duration(when-start))
	}
	if w, _ := set(2, time.Hour-time.Second); w != when {
		t.Fatal("a deadline after the armed time re-armed the timer")
	}
	if w, d := set(3, time.Minute); w > d || w >= when {
		t.Fatalf("a 1 min deadline left the timer %v from now", time.Duration(w-start))
	}
	// A task due within 1 ms is armed for its first due instant, 1 ns past its deadline.
	ms := int64(time.Millisecond)
	ct.mu.Lock()
	now := c.nowNano()
	ct.arm(now+ms-1, now)
	exact := ct.when
	ct.arm(now+ms, now)
	early := ct.when
	ct.mu.Unlock()
	if exact != now+ms || early >= now+ms+1 {
		t.Fatalf("tasks due in 1ms and 1ms+1ns armed the timer %v and %v from now, want exactly 1ms and less than 1ms+1ns",
			time.Duration(exact-now), time.Duration(early-now))
	}
}

// A task is due once the clock is past its deadline, the rule cache entries expire by.
func TestTakeUsesExpiryRule(t *testing.T) {
	const now = 1000
	take := func(deadlines ...int64) (int, bool) {
		var ct callbackTimers[int, int]
		for _, d := range deadlines {
			ct.heap = append(ct.heap, heapEntry(callbackTask[int, int]{expireTime: d}))
			ct.up(len(ct.heap) - 1)
		}
		return ct.take(make([]*callbackTask[int, int], 1), now)
	}
	if n, more := take(now); n != 0 || more {
		t.Fatalf("a task with its deadline at now: took %d, more %v, want 0 and false", n, more)
	}
	if n, more := take(now-1, now); n != 1 || more {
		t.Fatalf("deadlines now-1 and now: took %d, more %v, want 1 and false", n, more)
	}
	if n, more := take(now-2, now-1); n != 1 || !more {
		t.Fatalf("deadlines now-2 and now-1 with one slot: took %d, more %v, want 1 and true", n, more)
	}
}

// In a 4-ary heap the second earliest task can be any child of the root. One
// fire must see two due tasks and take both, wherever the second one is.
func TestFireTakesSecondEarliestAtEachChild(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for second := 1; second <= 4; second++ {
		c := newCallbackCache(t, 1)
		ct := c.shards[0].callbacks
		now, hour := c.nowNano(), int64(time.Hour)
		deadlines := []int64{now - 4, now + hour, now + hour, now + hour, now + hour}
		deadlines[second] = now - 3
		ct.mu.Lock()
		for k, d := range deadlines {
			ct.heap = append(ct.heap, heapEntry(callbackTask[int, *cbPayload]{key: k, expireTime: d, callback: noCallback}))
		}
		ct.mu.Unlock()
		ct.fire()
		ct.mu.Lock()
		left := len(ct.heap)
		ct.mu.Unlock()
		c.Close()
		if left != 3 {
			t.Fatalf("second earliest at child %d: one fire left %d of 5 tasks, want 3", second, left)
		}
	}
}

// heapEntry wraps a task for tests that build a shard's heap directly.
func heapEntry[K comparable, V any](task callbackTask[K, V]) callbackEntry[K, V] {
	return callbackEntry[K, V]{task.expireTime, &task}
}

// A lone callback that never returns must not hold back later ones.
func TestBlockedLoneCallbackDoesNotDelayLaterOnes(t *testing.T) {
	c := newCallbackCache(t, 1)
	defer c.Close()
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{})
	if err := c.SetWithCallback(1, new(cbPayload), time.Millisecond, func(int, *cbPayload) {
		close(entered)
		<-release
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	if err := c.SetWithCallback(2, new(cbPayload), 50*time.Millisecond, func(int, *cbPayload) { close(done) }); err != nil {
		t.Fatal(err)
	}
	runWithin(t, "the blocking callback", func() { <-entered })
	runWithin(t, "a callback due after one that blocks", func() { <-done })
}

// tickCache is a one-shard cache whose entries and callbacks are set straight on
// the shard with exact deadlines. It counts how often each callback runs and how
// many got a value other than their key. A test may set hook before put.
type tickCache struct {
	*Cache[int, int]
	calls      []atomic.Int32
	ran        atomic.Int32
	wrongValue atomic.Int32
	hook       func(k int)
}

func newTickCache(t *testing.T, n int) *tickCache {
	c, err := New[int, int](Config{MaxSize: 2 * int64(n), ShardCount: 1, EvictionPolicy: LRU})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &tickCache{Cache: c, calls: make([]atomic.Int32, n)}
}

func (tc *tickCache) put(k int, deadline int64) {
	s := tc.shards[0]
	cmd := writeCommand[int, int]{op: writeSet, key: k, value: k, hash: tc.hasher.Sum(k), expireTime: deadline}
	s.mu.Lock()
	tc.applySet(s, &cmd)
	s.mu.Unlock()
	tc.scheduleCallback(s, callbackTask[int, int]{key: k, value: k, expireTime: deadline, callback: func(k, v int) {
		if v != k {
			tc.wrongValue.Add(1)
		}
		tc.calls[k].Add(1)
		tc.ran.Add(1)
		if tc.hook != nil {
			tc.hook(k)
		}
	}})
}

// checkOnce waits for the callbacks, then fails unless each ran exactly once
// with its own value.
func (tc *tickCache) checkOnce(t *testing.T) {
	t.Helper()
	n := int32(len(tc.calls))
	for deadline := time.Now().Add(5 * time.Second); tc.ran.Load() < n && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	for k := range tc.calls {
		if got := tc.calls[k].Load(); got != 1 {
			t.Fatalf("callback %d ran %d times, want 1 (%d of %d ran)", k, got, tc.ran.Load(), n)
		}
	}
	if w := tc.wrongValue.Load(); w != 0 {
		t.Fatalf("%d of %d callbacks got a value other than the one set with their key", w, n)
	}
}

// Each trial puts two tasks one clock tick before a third and wakes the timer in
// a tight loop across those deadlines, so many wakes run on the very tick of the
// third. Every callback must still run exactly once.
func TestWakeOnTheTickLosesNothing(t *testing.T) {
	const trials = 4000
	tc := newTickCache(t, 3*trials)
	ct := tc.shards[0].callbacks
	for trial := range trials {
		d := tc.nowNano() + 20_000
		tc.put(3*trial, d-42)
		tc.put(3*trial+1, d-42)
		tc.put(3*trial+2, d)
		for tc.nowNano() <= d+2000 {
			ct.fire()
		}
	}
	tc.checkOnce(t)
}

// The batch path's version of TestWakeOnTheTickLosesNothing. Each trial puts
// dueBatch tasks 25ns apart and calls fireBatch in a tight loop across their
// deadlines, so most calls find a deadline within a clock tick of now. Every
// callback must still run exactly once.
func TestBatchOnTheTickLosesNothing(t *testing.T) {
	const trials = 400
	tc := newTickCache(t, dueBatch*trials)
	ct := tc.shards[0].callbacks
	for trial := range trials {
		d := tc.nowNano() + 500_000
		for i := range dueBatch {
			tc.put(dueBatch*trial+i, d+int64(25*i))
		}
		for end := d + 25*dueBatch + 2000; tc.nowNano() <= end; {
			ct.fireBatch()
		}
	}
	tc.checkOnce(t)
}

// Two callbacks share one deadline, so they run as a batch, and neither returns.
// They must not keep a later callback from running or a Set on their shard
// from finishing.
func TestBlockedBatchDoesNotDelayLaterCallbacksOrWriters(t *testing.T) {
	tc := newTickCache(t, 3)
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 2)
	done := make(chan struct{})
	tc.hook = func(k int) {
		if k == 2 {
			close(done)
			return
		}
		entered <- struct{}{}
		<-release
	}
	d := tc.nowNano() + int64(20*time.Millisecond)
	tc.put(0, d)
	tc.put(1, d)
	tc.put(2, d+int64(50*time.Millisecond))
	runWithin(t, "the blocking batch", func() {
		<-entered
		<-entered
	})
	runWithin(t, "a callback due after a batch that blocks", func() { <-done })
	var err error
	runWithin(t, "a Set while a batch blocks", func() { err = tc.Set(3, 3, time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
}

// One fire takes several due tasks, and hands any beyond dueBatch to another fire.
func TestFireTakesDueTasksInBatches(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	c := newCallbackCache(t, 1)
	defer c.Close()
	ct := c.shards[0].callbacks
	left := func() int {
		ct.mu.Lock()
		defer ct.mu.Unlock()
		return len(ct.heap)
	}
	push := func(n int) {
		ct.mu.Lock()
		defer ct.mu.Unlock()
		for k := range n {
			ct.heap = append(ct.heap, heapEntry(callbackTask[int, *cbPayload]{key: k, expireTime: 1, callback: noCallback}))
		}
	}
	// With one P, a hand-off cannot run before fire returns.
	push(3)
	ct.fire()
	if n := left(); n != 0 {
		t.Fatalf("one fire left %d of 3 due tasks", n)
	}
	push(dueBatch + 1)
	ct.fire()
	for deadline := time.Now().Add(5 * time.Second); left() > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d due tasks beyond one batch were never taken", left())
		}
	}
}

func TestCallbackHeapOrder(t *testing.T) {
	var ct callbackTimers[int, int]
	want := make([]int64, 1000)
	for i := range want {
		want[i] = 1 + rand.Int64N(100)
		ct.heap = append(ct.heap, heapEntry(callbackTask[int, int]{expireTime: want[i]}))
		ct.up(len(ct.heap) - 1)
	}
	slices.Sort(want)
	for i, w := range want {
		if got := ct.pop().expireTime; got != w {
			t.Fatalf("pop %d returned deadline %d, want %d", i, got, w)
		}
		if h := ct.heap; len(h) > 0 && h[:cap(h)][len(h)] != (callbackEntry[int, int]{}) {
			t.Fatalf("pop %d left its vacated slot filled", i)
		}
	}
}

// The heap is dropped once nothing is pending, and shrinks as a burst fires,
// down to a capacity of 64. A 200ms burst is all in the heap before it fires.
func TestFiredBurstReleasesHeap(t *testing.T) {
	const n = 10000
	for _, tc := range []struct {
		pending int
		ttl     time.Duration
	}{
		{0, time.Nanosecond},
		{10, 200 * time.Millisecond},
	} {
		pending := tc.pending
		t.Run(fmt.Sprintf("%d pending", pending), func(t *testing.T) {
			c := newCallbackCache(t, 1)
			defer c.Close()
			for k := range pending {
				if err := c.SetWithCallback(k, new(cbPayload), time.Hour, noCallback); err != nil {
					t.Fatal(err)
				}
			}
			for k := pending; k < pending+n; k++ {
				if err := c.SetWithCallback(k, new(cbPayload), tc.ttl, noCallback); err != nil {
					t.Fatal(err)
				}
			}
			ct := c.shards[0].callbacks
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
				ct.mu.Lock()
				h := ct.heap
				ct.mu.Unlock()
				if len(h) == pending && (h == nil) == (pending == 0) && cap(h) <= 64 {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("heap still holds %d tasks with capacity %d after its burst fired, want %d tasks", len(h), cap(h), pending)
				}
			}
		})
	}
}
