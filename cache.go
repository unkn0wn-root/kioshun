package cache

import (
	"container/heap"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	NoExpiration      time.Duration = -1 // Never expires
	DefaultExpiration time.Duration = 0  // Use default config value
)

var (
	ErrCacheClosed = errors.New("cache is closed")
)

const (
	// gratio32 is the golden ratio multiplier for 32-bit integers
	// (φ - 1) * 2^32, where φ = (1 + √5) / 2 (golden ratio)
	gratio32 = 0x9e3779b9

	// gratio64 is the same but for 64-bit integers
	// (φ - 1) * 2^64
	gratio64 = 0x9e3779b97f4a7c15

	// FNV-1a constants
	fnvOffset32 = 2166136261
	fnvPrime32  = 16777619
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

type Cache[K comparable, V any] interface {
	Set(key K, value V, ttl time.Duration) error
	Get(key K) (V, bool)
	Delete(key K) bool
	Clear()
	Size() int64
	Stats() Stats
	Close() error
}

// Stats holds cache performance metrics
type Stats struct {
	Hits        int64
	Misses      int64
	Evictions   int64
	Expirations int64
	Size        int64
	Capacity    int64
	HitRatio    float64
	Shards      int
}

type Config struct {
	MaxSize         int64          // Maximum number of items (0 = unlimited)
	ShardCount      int            // Number of shards (0 = auto-detect)
	CleanupInterval time.Duration  // How often to run cleanup (0 = no cleanup)
	DefaultTTL      time.Duration  // Default TTL for items (0 = no expiration)
	EvictionPolicy  EvictionPolicy // Eviction algorithm
	StatsEnabled    bool           // Enable statistics collection
}

func DefaultConfig() Config {
	return Config{
		MaxSize:         10000,
		ShardCount:      0,
		CleanupInterval: 5 * time.Minute,
		DefaultTTL:      30 * time.Minute,
		EvictionPolicy:  LRU,
		StatsEnabled:    true,
	}
}

// InMemoryCache is the main cache implementation using sharding
type InMemoryCache[K comparable, V any] struct {
	shards      []*shard[K, V] // Array of cache shards
	shardMask   uint64         // Bitmask for fast shard selection
	config      Config
	globalStats Stats
	statsMux    sync.RWMutex
	cleanupCh   chan struct{}
	closeCh     chan struct{}
	closeOnce   sync.Once
	closed      int32     // Flag indicating cache is closed
	itemPool    sync.Pool // Object pool
}

// New creates a new cache instance with the given configuration
func New[K comparable, V any](config Config) *InMemoryCache[K, V] {
	// Auto-detect optimal shard count based on CPU cores
	shardCount := config.ShardCount
	if shardCount <= 0 {
		shardCount = runtime.NumCPU() * 4
		if shardCount > 256 {
			shardCount = 256
		}
	}

	// Ensure shard count is power of 2 for masking
	shardCount = nextPowerOf2(shardCount)

	cache := &InMemoryCache[K, V]{
		shards:    make([]*shard[K, V], shardCount),
		shardMask: uint64(shardCount - 1),
		config:    config,
		cleanupCh: make(chan struct{}, 1),
		closeCh:   make(chan struct{}),
		itemPool: sync.Pool{
			New: func() any {
				return &cacheItem[V]{heapIndex: -1}
			},
		},
	}

	// Initialize each shard
	for i := 0; i < shardCount; i++ {
		cache.shards[i] = &shard[K, V]{
			data: make(map[K]*cacheItem[V]),
		}
		cache.initLRU(cache.shards[i])

		// Initialize LFU heap only if LFU policy is selected
		if config.EvictionPolicy == LFU {
			h := make(lfuHeap[V], 0)
			cache.shards[i].lfuHeap = &h
			heap.Init(cache.shards[i].lfuHeap)
		}
	}

	// Start cleanup worker if cleanup interval is configured
	if config.CleanupInterval > 0 {
		go cache.cleanupWorker()
	}

	return cache
}

// NewWithDefaults creates a cache with default configuration
func NewWithDefaults[K comparable, V any]() *InMemoryCache[K, V] {
	return New[K, V](DefaultConfig())
}

// hash implements type-specific hashing strategies
// Uses different algorithms based on key type
//
// String keys: FNV-1a hash
// Integer keys (int, int32, int64, uint, uint32, uint64): Multiplicative hashing
// - Uses golden ratio constants (φ-1) * 2^n
// - gratio32 = 0x9e3779b9 for 32-bit integers
// - gratio64 = 0x9e3779b97f4a7c15 for 64-bit integers
//
// Other types: String conversion fallback
// - Converts to string representation then applies FNV-1a
// - (@todo: should revisit that) Necessary for generics support with arbitrary comparable constraints
func (c *InMemoryCache[K, V]) hash(key K) uint64 {
	switch k := any(key).(type) {
	case string:
		return fnvHash64(k)
	case int:
		return uint64(k) * gratio32
	case int32:
		return uint64(k) * gratio32
	case int64:
		return uint64(k) * gratio64
	case uint:
		return uint64(k) * gratio32
	case uint32:
		return uint64(k) * gratio32
	case uint64:
		return k * gratio64
	default:
		return fnvHash64(fmt.Sprintf("%v", k))
	}
}

// getShard returns shard for a given key using hash distribution
//
// Uses bitmask for shard selection instead of modulo operation:
// - Requires shard count to be power of 2 (enforced in New())
// - shardMask = shardCount - 1, creating a bitmask
// - hash & shardMask is equivalent to hash % shardCount
// - Ensures uniform distribution across shards for LB
func (c *InMemoryCache[K, V]) getShard(key K) *shard[K, V] {
	hash := c.hash(key)
	return c.shards[hash&c.shardMask] // Use bitmask for fast modulo
}

// Get retrieves a value from the cache
//
// The method handles three scenarios:
// 1. Key not found -> record miss, return false
// 2. Key found with no expiration (expireTime == 0) -> update access patterns, return value
// 3. Key found with expiration -> check if expired, clean up if needed, return accordingly
//
// - Uses write lock for LRU/FIFO policies (need to update access order)
// - Uses read lock for LFU policy (only needs to increment frequency counter)
func (c *InMemoryCache[K, V]) Get(key K) (V, bool) {
	var zero V
	if atomic.LoadInt32(&c.closed) == 1 {
		return zero, false
	}

	shard := c.getShard(key)

	// LRU/FIFO need write locks to update access order, LFU only needs read locks
	needsUpdate := c.config.EvictionPolicy == LRU || c.config.EvictionPolicy == FIFO
	if needsUpdate {
		shard.mu.Lock()
		defer shard.mu.Unlock()
	} else {
		shard.mu.RLock()
		defer shard.mu.RUnlock()
	}

	item, exists := shard.data[key]
	if !exists {
		// cache miss: record statistics and return
		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.misses, 1)
		}
		return zero, false
	}

	// item has no expiration (permanent item)
	if item.expireTime == 0 {
		if needsUpdate {
			c.moveToLRUHead(shard, item)
		}

		if c.config.EvictionPolicy == LFU {
			atomic.AddInt64(&item.frequency, 1)
		}

		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.hits, 1)
		}

		return item.value, true
	}

	// item has expiration - check if it's still valid
	now := time.Now().UnixNano()
	if now > item.expireTime {
		delete(shard.data, key)
		c.removeFromLRU(shard, item)
		if c.config.EvictionPolicy == LFU && item.heapIndex != -1 {
			heap.Remove(shard.lfuHeap, item.heapIndex)
		}
		c.itemPool.Put(item)
		atomic.AddInt64(&shard.size, -1)

		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.expirations, 1)
			atomic.AddInt64(&shard.misses, 1)
		}

		return zero, false
	}

	if needsUpdate {
		c.moveToLRUHead(shard, item)
	}

	if c.config.EvictionPolicy == LFU {
		atomic.AddInt64(&item.frequency, 1)
	}

	if c.config.StatsEnabled {
		atomic.AddInt64(&shard.hits, 1)
	}

	return item.value, true
}

// GetWithTTL returns value with remaining TTL (-1 for no expiration)
//
// This method is identical to Get() but additionally calculates and returns the remaining
// time-to-live for items with expiration. The TTL calculation is performed after expiration
// checking to ensure accuracy.
//
// Return values for TTL:
// - For permanent items (no expiration): returns -1
// - For expiring items: returns remaining duration until expiration
// - For expired/missing items: returns 0
func (c *InMemoryCache[K, V]) GetWithTTL(key K) (V, time.Duration, bool) {
	var zero V
	if atomic.LoadInt32(&c.closed) == 1 {
		return zero, 0, false
	}

	shard := c.getShard(key)

	needsUpdate := c.config.EvictionPolicy == LRU || c.config.EvictionPolicy == FIFO
	if needsUpdate {
		shard.mu.Lock()
		defer shard.mu.Unlock()
	} else {
		shard.mu.RLock()
		defer shard.mu.RUnlock()
	}

	item, exists := shard.data[key]
	if !exists {
		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.misses, 1)
		}
		return zero, 0, false
	}

	if item.expireTime == 0 {
		if needsUpdate {
			c.moveToLRUHead(shard, item)
		}

		if c.config.EvictionPolicy == LFU {
			atomic.AddInt64(&item.frequency, 1)
		}

		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.hits, 1)
		}

		return item.value, -1, true
	}

	now := time.Now().UnixNano()
	if now > item.expireTime {
		delete(shard.data, key)
		c.removeFromLRU(shard, item)
		if c.config.EvictionPolicy == LFU && item.heapIndex != -1 {
			heap.Remove(shard.lfuHeap, item.heapIndex)
		}
		c.itemPool.Put(item)
		atomic.AddInt64(&shard.size, -1)

		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.expirations, 1)
			atomic.AddInt64(&shard.misses, 1)
		}
		return zero, 0, false
	}

	ttl := time.Duration(item.expireTime - now) // calculate remaining time until expiration

	if needsUpdate {
		c.moveToLRUHead(shard, item)
	}

	if c.config.EvictionPolicy == LFU {
		atomic.AddInt64(&item.frequency, 1)
	}

	if c.config.StatsEnabled {
		atomic.AddInt64(&shard.hits, 1)
	}

	return item.value, ttl, true
}

// Set stores a key-value pair with the specified TTL
//
// The method handles the lifecycle of adding/updating cache items:
// 1. TTL normalization - converts 0 to default TTL from config
// 2. Item preparation - gets pooled item and initializes all fields
// 3. Existing item cleanup - removes old item from all data structures
// 4. Capacity management - evicts items if shard is at capacity
// 5. Item insertion - adds new item to all appropriate data structures
//
// Eviction policy:
// - Adds items to LRU list head (most recently used position)
// - Adds items to LFU heap for frequency-based eviction
// - Initializes frequency counter to 1 for new items
func (c *InMemoryCache[K, V]) Set(key K, value V, ttl time.Duration) error {
	if atomic.LoadInt32(&c.closed) == 1 {
		return ErrCacheClosed
	}

	if ttl == 0 {
		ttl = c.config.DefaultTTL
	}

	shard := c.getShard(key)
	now := time.Now().UnixNano()

	item := c.itemPool.Get().(*cacheItem[V])
	item.value = value
	item.lastAccess = now // For LRU tracking
	item.frequency = 1    // Initial frequency for LFU
	item.key = key        // Store key for eviction callbacks
	item.heapIndex = -1   // Not in heap initially

	if ttl > 0 {
		item.expireTime = now + ttl.Nanoseconds()
	} else {
		item.expireTime = 0
	}

	// lock for all Set operations
	shard.mu.Lock()
	defer shard.mu.Unlock()

	if existing, exists := shard.data[key]; exists {
		c.removeFromLRU(shard, existing)
		if c.config.EvictionPolicy == LFU && existing.heapIndex != -1 {
			heap.Remove(shard.lfuHeap, existing.heapIndex)
		}
		c.itemPool.Put(existing)
		atomic.AddInt64(&shard.size, -1)
	}

	maxShardSize := c.config.MaxSize / int64(len(c.shards))
	if c.config.MaxSize > 0 && maxShardSize > 0 && atomic.LoadInt64(&shard.size) >= maxShardSize {
		c.evictItem(shard) // remove least valuable item based on eviction policy
	}

	shard.data[key] = item
	c.addToLRUHead(shard, item)

	if c.config.EvictionPolicy == LFU {
		heap.Push(shard.lfuHeap, item)
	}

	atomic.AddInt64(&shard.size, 1)
	return nil
}

// SetWithCallback stores a key-value pair and executes a callback function when the item expires
//
// Flow:
// - First stores the item using normal Set() operation
// - Spawns a background goroutine with a timer for TTL duration
// - When timer expires, verifies the item still exists and has expired before calling callback
// - Callback execution is non-blocking and runs in separate goroutine
// - Automatically cleans up if cache is closed before expiration
// - Only creates timer/goroutine if callback is non-nil and TTL > 0
func (c *InMemoryCache[K, V]) SetWithCallback(key K, value V, ttl time.Duration, callback func(K, V)) error {
	if atomic.LoadInt32(&c.closed) == 1 {
		return ErrCacheClosed
	}

	err := c.Set(key, value, ttl)
	if err != nil {
		return err
	}

	if callback != nil && ttl > 0 {
		go func() {
			timer := time.NewTimer(ttl)
			defer timer.Stop()

			select {
			case <-timer.C:
				shard := c.getShard(key)
				shard.mu.RLock()
				item, exists := shard.data[key]
				if exists && item.expireTime > 0 && time.Now().UnixNano() > item.expireTime {
					shard.mu.RUnlock()
					callback(key, value)
				} else {
					shard.mu.RUnlock()
				}
			case <-c.closeCh:
				return
			}
		}()
	}

	return nil
}

// Delete removes a key from the cache
func (c *InMemoryCache[K, V]) Delete(key K) bool {
	if atomic.LoadInt32(&c.closed) == 1 {
		return false
	}

	shard := c.getShard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	item, exists := shard.data[key]
	if !exists {
		return false
	}

	delete(shard.data, key)
	c.removeFromLRU(shard, item)
	if c.config.EvictionPolicy == LFU && item.heapIndex != -1 {
		heap.Remove(shard.lfuHeap, item.heapIndex)
	}
	c.itemPool.Put(item)
	atomic.AddInt64(&shard.size, -1)
	return true
}

// Clear removes all items from the cache
func (c *InMemoryCache[K, V]) Clear() {
	if atomic.LoadInt32(&c.closed) == 1 {
		return
	}

	for _, shard := range c.shards {
		shard.mu.Lock()
		for _, item := range shard.data {
			c.itemPool.Put(item)
		}
		shard.data = make(map[K]*cacheItem[V])
		c.initLRU(shard)
		if c.config.EvictionPolicy == LFU {
			*shard.lfuHeap = (*shard.lfuHeap)[:0]
		}
		atomic.StoreInt64(&shard.size, 0)
		shard.mu.Unlock()
	}
}

// Exists checks if a key exists in the cache without updating access time or eviction order
//
// Implements an lock upgrade pattern for handling expired items:
// 1. First acquires read lock for fast path - checking existence and expiration
// 2. If item exists and hasn't expired, returns true immediately (most common case)
// 3. If item is expired, upgrades to write lock to remove the expired item
// 4. Double-checks expiration under write lock
// 5. Cleans up expired item
func (c *InMemoryCache[K, V]) Exists(key K) bool {
	if atomic.LoadInt32(&c.closed) == 1 {
		return false
	}

	shard := c.getShard(key)
	shard.mu.RLock()

	item, exists := shard.data[key]
	if !exists {
		shard.mu.RUnlock()
		return false
	}

	if item.expireTime == 0 {
		shard.mu.RUnlock()
		return true
	}

	now := time.Now().UnixNano()
	if now > item.expireTime {
		shard.mu.RUnlock()
		shard.mu.Lock()
		item, exists = shard.data[key]
		if exists && item.expireTime > 0 && now > item.expireTime {
			delete(shard.data, key)
			c.removeFromLRU(shard, item)
			if c.config.EvictionPolicy == LFU && item.heapIndex != -1 {
				heap.Remove(shard.lfuHeap, item.heapIndex)
			}
			c.itemPool.Put(item)
			atomic.AddInt64(&shard.size, -1)
			if c.config.StatsEnabled {
				atomic.AddInt64(&shard.expirations, 1)
			}
		}
		shard.mu.Unlock()
		return false
	}

	shard.mu.RUnlock()
	return true
}

// Keys returns all non-expired keys in the cache
func (c *InMemoryCache[K, V]) Keys() []K {
	if atomic.LoadInt32(&c.closed) == 1 {
		return nil
	}

	var keys []K
	now := time.Now().UnixNano()

	for _, shard := range c.shards {
		shard.mu.RLock()
		for key, item := range shard.data {
			if item.expireTime > 0 && now > item.expireTime {
				continue
			}
			keys = append(keys, key)
		}
		shard.mu.RUnlock()
	}

	return keys
}

// Size returns the total number of items in the cache
func (c *InMemoryCache[K, V]) Size() int64 {
	var totalSize int64
	for _, shard := range c.shards {
		totalSize += atomic.LoadInt64(&shard.size)
	}
	return totalSize
}

// Stats returns cache performance statistics
func (c *InMemoryCache[K, V]) Stats() Stats {
	var stats Stats
	stats.Size = c.Size()
	stats.Capacity = c.config.MaxSize
	stats.Shards = len(c.shards)

	// Aggregate statistics from all shards
	if c.config.StatsEnabled {
		for _, shard := range c.shards {
			stats.Hits += atomic.LoadInt64(&shard.hits)
			stats.Misses += atomic.LoadInt64(&shard.misses)
			stats.Evictions += atomic.LoadInt64(&shard.evictions)
			stats.Expirations += atomic.LoadInt64(&shard.expirations)
		}

		total := stats.Hits + stats.Misses
		if total > 0 {
			stats.HitRatio = float64(stats.Hits) / float64(total)
		}
	}

	return stats
}

// Close gracefully shuts down the cache
func (c *InMemoryCache[K, V]) Close() error {
	var err error
	c.closeOnce.Do(func() {
		atomic.StoreInt32(&c.closed, 1)
		close(c.closeCh)
		c.Clear()
	})
	return err
}

// TriggerCleanup manually triggers cleanup of expired items
func (c *InMemoryCache[K, V]) TriggerCleanup() {
	if atomic.LoadInt32(&c.closed) == 1 {
		return
	}

	select {
	case c.cleanupCh <- struct{}{}:
	default:
		// Channel is full, cleanup already scheduled
	}
}

// cleanupWorker runs in a background task to periodically remove expired items
//
// The worker uses a buffered channel (cleanupCh) to coalesce multiple manual trigger requests,
// preventing cleanup storms. If multiple cleanup requests arrive while one is already running,
// subsequent requests are dropped (channel is full).
func (c *InMemoryCache[K, V]) cleanupWorker() {
	ticker := time.NewTicker(c.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.cleanup()
		case <-c.cleanupCh:
			c.cleanup()
		case <-c.closeCh:
			return
		}
	}
}

// cleanup removes expired items from all shards
//
// Performs cache-wide expired item removal by:
// 1. Taking a single timestamp to ensure consistent expiration checking across all shards
// 2. Delegating to cleanupShard() for each shard to handle the actual removal
// 3. Processing shards sequentially to avoid excessive lock contention
//
// This method is called by both the periodic cleanup worker and manual cleanup triggers.
// The actual expiration logic and statistics updates are handled in cleanupShard()
func (c *InMemoryCache[K, V]) cleanup() {
	if atomic.LoadInt32(&c.closed) == 1 {
		return
	}

	now := time.Now().UnixNano()
	for _, shard := range c.shards {
		c.cleanupShard(shard, now)
	}
}

// cacheItem represents a single cache entry with metadata
type cacheItem[V any] struct {
	value      V
	expireTime int64         // Unix nano timestamp
	lastAccess int64         // Unix nano timestamp
	frequency  int64         // Access count for LFU
	prev       *cacheItem[V] // For LRU doubly-linked list
	next       *cacheItem[V] // For LRU doubly-linked list
	key        any           // Store key for eviction
	heapIndex  int           // For LFU heap
}

// lfuHeap implements a min-heap for LFU eviction policy
type lfuHeap[V any] []*cacheItem[V]

func (h lfuHeap[V]) Len() int { return len(h) }

// Less compares items: primary by frequency, secondary by last access time
func (h lfuHeap[V]) Less(i, j int) bool {
	if h[i].frequency != h[j].frequency {
		return h[i].frequency < h[j].frequency
	}
	// Break ties with last access time (older items evicted first)
	return h[i].lastAccess < h[j].lastAccess
}

// Swap exchanges two items and updates their heap indices
func (h lfuHeap[V]) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIndex = i
	h[j].heapIndex = j
}

// Push adds an item to the heap
func (h *lfuHeap[V]) Push(x any) {
	item := x.(*cacheItem[V])
	item.heapIndex = len(*h)
	*h = append(*h, item)
}

// Pop removes and returns the minimum item from the heap
func (h *lfuHeap[V]) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	item.heapIndex = -1 // Mark as not in heap
	*h = old[0 : n-1]
	return item
}

// update rebalances the heap after an item's frequency changes
func (h *lfuHeap[V]) update(item *cacheItem[V]) {
	heap.Fix(h, item.heapIndex)
}
