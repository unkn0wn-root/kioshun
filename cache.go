package cache

import (
	"container/heap"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	NoExpiration      time.Duration = -1 // Never expires
	DefaultExpiration time.Duration = 0  // Use default config value
)

// EvictionPolicy defines the eviction algorithm
type EvictionPolicy int

const (
	LRU EvictionPolicy = iota
	LFU
	FIFO
	Random
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
	closed      int32         // Flag indicating cache is closed
	itemPool    sync.Pool     // Object pool
	hasher      hasher[K]     // Hash function
	evictor     evictor[K, V] // Eviction policy
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

	cache.hasher = newHasher[K]()
	cache.evictor = createEvictor[K, V](config.EvictionPolicy)

	// Initialize each shard
	for i := 0; i < shardCount; i++ {
		cache.shards[i] = &shard[K, V]{
			data: make(map[K]*cacheItem[V]),
		}
		cache.shards[i].initLRU()

		if config.EvictionPolicy == LFU {
			h := make(lfuHeap[V], 0)
			cache.shards[i].lfuHeap = &h
			heap.Init(cache.shards[i].lfuHeap)
		}
	}

	if config.CleanupInterval > 0 {
		go cache.cleanupWorker()
	}

	return cache
}

// NewWithDefaults creates a cache with default configuration
func NewWithDefaults[K comparable, V any]() *InMemoryCache[K, V] {
	return New[K, V](DefaultConfig())
}

// getShard returns shard for a given key using hash distribution
//
// - Requires shard count to be power of 2
// - shardMask = shardCount - 1, creating a bitmask
//
// Example: 8 shards -> shardMask = 7 (0111 binary)
// hash = 1010101 -> hash & 7 = 101 = shard 5
func (c *InMemoryCache[K, V]) getShard(key K) *shard[K, V] {
	hash := c.hasher.hash(key)
	return c.shards[hash&c.shardMask] // Use bitmask for fast modulo
}

// Get retrieves a value from the cache
//
// The method handles the complete lifecycle of a cache lookup:
// 1. Closed state check -> return immediately if cache is closed
// 2. Shard selection -> use hash-based distribution to find correct shard
// 3. Lock acquisition -> optimized read/write lock based on eviction policy
// 4. Key lookup -> check if key exists in shard's hash map
// 5. Expiration check -> verify item hasn't expired, clean up if needed
// 6. Access tracking -> update lastAccess time and frequency counter
// 7. Eviction policy handling -> update LRU order or LFU heap as needed
// 8. Statistics -> record cache hit for monitoring
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
		// Cache miss: record statistics and return
		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.misses, 1)
		}
		return zero, false
	}

	// item has no expiration (permanent item)
	if item.expireTime == 0 {
		if needsUpdate {
			shard.moveToLRUHead(item)
		}

		if c.config.EvictionPolicy == LFU {
			item.lastAccess = time.Now().UnixNano()
			atomic.AddInt64(&item.frequency, 1)
			if item.heapIndex != -1 {
				shard.lfuHeap.update(item)
			}
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
		shard.removeFromLRU(item)
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
		shard.moveToLRUHead(item)
	}

	if c.config.EvictionPolicy == LFU {
		item.lastAccess = now
		atomic.AddInt64(&item.frequency, 1)
		if item.heapIndex != -1 {
			shard.lfuHeap.update(item)
		}
	}

	if c.config.StatsEnabled {
		atomic.AddInt64(&shard.hits, 1)
	}

	return item.value, true
}

// GetWithTTL retrieves a value along with its remaining TTL
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
			shard.moveToLRUHead(item)
		}

		if c.config.EvictionPolicy == LFU {
			item.lastAccess = time.Now().UnixNano()
			atomic.AddInt64(&item.frequency, 1)
			if item.heapIndex != -1 {
				shard.lfuHeap.update(item)
			}
		}

		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.hits, 1)
		}

		return item.value, -1, true
	}

	now := time.Now().UnixNano()
	if now > item.expireTime {
		delete(shard.data, key)
		shard.removeFromLRU(item)
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

	ttl := time.Duration(item.expireTime - now)

	if needsUpdate {
		shard.moveToLRUHead(item)
	}

	if c.config.EvictionPolicy == LFU {
		item.lastAccess = now
		atomic.AddInt64(&item.frequency, 1)
		if item.heapIndex != -1 {
			shard.lfuHeap.update(item)
		}
	}

	if c.config.StatsEnabled {
		atomic.AddInt64(&shard.hits, 1)
	}

	return item.value, ttl, true
}

// Set stores a key-value pair with the specified TTL
//
// Handles lifecycle of adding/updating cache items:
// 1. Closed state check -> prevent operations on closed cache
// 2. TTL normalization -> convert 0 to default TTL from config
// 3. Shard selection -> use hash-based distribution to find correct shard
// 4. Item preparation -> get pooled item and initialize all fields
// 5. Existing item cleanup -> remove old item from all data structures
// 6. Capacity management -> evict items if shard exceeds capacity
// 7. Item insertion -> add new item to all appropriate data structures
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
	item.lastAccess = now
	item.frequency = 1
	item.key = key
	item.heapIndex = -1

	if ttl > 0 {
		item.expireTime = now + ttl.Nanoseconds()
	} else {
		item.expireTime = 0 // Permanent item
	}

	shard.mu.Lock()
	defer shard.mu.Unlock()

	if existing, exists := shard.data[key]; exists {
		shard.removeFromLRU(existing)
		if c.config.EvictionPolicy == LFU && existing.heapIndex != -1 {
			heap.Remove(shard.lfuHeap, existing.heapIndex)
		}
		c.itemPool.Put(existing)
		atomic.AddInt64(&shard.size, -1)
	}

	// Check if eviction is needed before adding new item
	// Distribute max capacity evenly across all shards
	maxShardSize := c.config.MaxSize / int64(len(c.shards))
	if c.config.MaxSize > 0 && maxShardSize > 0 && atomic.LoadInt64(&shard.size) >= maxShardSize {
		c.evictor.evict(shard, &c.itemPool, c.config.StatsEnabled) // Evict least valuable item
	}

	shard.data[key] = item
	shard.addToLRUHead(item)

	if c.config.EvictionPolicy == LFU {
		heap.Push(shard.lfuHeap, item) // Add to min-heap for frequency tracking
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
	shard.removeFromLRU(item)
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
		shard.initLRU()
		if c.config.EvictionPolicy == LFU {
			*shard.lfuHeap = (*shard.lfuHeap)[:0]
		}
		atomic.StoreInt64(&shard.size, 0)
		shard.mu.Unlock()
	}
}

// Exists checks if a key exists in the cache without updating access time
func (c *InMemoryCache[K, V]) Exists(key K) bool {
	if atomic.LoadInt32(&c.closed) == 1 {
		return false
	}

	shard := c.getShard(key)
	now := time.Now().UnixNano()

	shard.mu.Lock()
	defer shard.mu.Unlock()

	item, exists := shard.data[key]
	if !exists {
		return false
	}

	if item.expireTime > 0 && now > item.expireTime {
		delete(shard.data, key)
		shard.removeFromLRU(item)
		if c.config.EvictionPolicy == LFU && item.heapIndex != -1 {
			heap.Remove(shard.lfuHeap, item.heapIndex)
		}
		c.itemPool.Put(item)
		atomic.AddInt64(&shard.size, -1)
		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.expirations, 1)
		}
		return false
	}

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

	// If no background worker is running, do cleanup directly
	if c.config.CleanupInterval <= 0 {
		c.cleanup()
		return
	}

	// Otherwise, signal the background worker
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
		shard.cleanup(now, c.config.EvictionPolicy, &c.itemPool, c.config.StatsEnabled)
	}
}

// cacheItem represents a single cache entry with metadata for eviction policies
//
// 1. Stores the actual cached value
// 2. Tracks expiration and access timing
// 3. Maintains frequency count for LFU policy
// 4. Provides doubly-linked list pointers for LRU policy
// 5. Stores heap index for efficient LFU operations
//
// Memory layout is optimized for cache line efficiency and minimal overhead
type cacheItem[V any] struct {
	value      V             // The actual cached value
	expireTime int64         // Unix nano timestamp for expiration (0 = never expires)
	lastAccess int64         // Unix nano timestamp of last access (for LFU tie-breaking)
	frequency  int64         // Access count for LFU policy (atomic operations)
	prev       *cacheItem[V] // Previous item in LRU doubly-linked list
	next       *cacheItem[V] // Next item in LRU doubly-linked list
	key        any           // Store key for eviction callbacks and debugging
	heapIndex  int           // Index in LFU heap for O(log n) updates (-1 = not in heap)
}

// lfuHeap implements a min-heap for LFU eviction policy
//
// The heap maintains cache items ordered by access frequency, with the
// least frequently used item at the root (index 0). This enables O(log n)
// insertion and O(log n) removal of the LFU item.
//
// Heap properties:
// - Min-heap: parent.frequency <= child.frequency
// - Tie-breaking: when frequencies are equal, older lastAccess time wins
// - Root contains the item with lowest frequency (candidate for eviction)
//
// The heap is implemented as a slice of pointers to cacheItem structs,
// with each item maintaining its heapIndex for efficient updates.
type lfuHeap[V any] []*cacheItem[V]

// Len returns the number of items in the heap
func (h lfuHeap[V]) Len() int { return len(h) }

// Less compares items for heap ordering: primary by frequency, secondary by last access time
//
// Comparison logic:
// 1. If frequencies differ: lower frequency has higher priority (min-heap)
// 2. If frequencies are equal: older lastAccess time has higher priority
//
// This ensures that among items with the same access frequency,
// the one that was accessed longest ago will be evicted first (LRU tie-breaking)
func (h lfuHeap[V]) Less(i, j int) bool {
	if h[i].frequency != h[j].frequency {
		return h[i].frequency < h[j].frequency // Min-heap: lower frequency first
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
