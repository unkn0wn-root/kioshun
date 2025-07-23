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

// New creates a new cache instance with the given configuration.
// Shard count is auto-detected based on CPU cores if not specified,
// and is rounded to the next power of 2 for hash distribution.
func New[K comparable, V any](config Config) *InMemoryCache[K, V] {
	shardCount := config.ShardCount
	if shardCount <= 0 {
		// Auto-detect based on CPU cores, capped at 256
		shardCount = runtime.NumCPU() * 4
		if shardCount > 256 {
			shardCount = 256
		}
	}

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

// getShard returns the shard for a given key using hash distribution
func (c *InMemoryCache[K, V]) getShard(key K) *shard[K, V] {
	hash := c.hasher.hash(key)
	return c.shards[hash&c.shardMask] // Use bitmask for fast modulo
}

// get acquires different lock types based on eviction policy:
// LRU/LFU need write locks to update access order/frequency, FIFO/Random use read locks.
func (c *InMemoryCache[K, V]) get(key K) (*cacheItem[V], int64, bool) {
	if atomic.LoadInt32(&c.closed) == 1 {
		return nil, 0, false
	}

	shard := c.getShard(key)

	// LRU and LFU need write locks
	needsWriteLock := c.config.EvictionPolicy == LRU || c.config.EvictionPolicy == LFU
	if needsWriteLock {
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
		return nil, 0, false
	}

	// Check expiration
	now := time.Now().UnixNano()
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
			atomic.AddInt64(&shard.misses, 1)
		}

		return nil, now, false
	}

	// Update based on eviction policy
	switch c.config.EvictionPolicy {
	case LRU:
		shard.moveToLRUHead(item)
	case LFU:
		item.lastAccess = now
		atomic.AddInt64(&item.frequency, 1)
		if item.heapIndex != -1 {
			shard.lfuHeap.update(item)
		}
	case FIFO, Random:
		// FIFO doesn't update on access - maintain insertion order
		// Random doesn't need any update
	}

	if c.config.StatsEnabled {
		atomic.AddInt64(&shard.hits, 1)
	}

	return item, now, true
}

// Get retrieves a value from the cache
func (c *InMemoryCache[K, V]) Get(key K) (V, bool) {
	var zero V
	item, _, ok := c.get(key)
	if !ok {
		return zero, false
	}
	return item.value, true
}

// GetWithTTL retrieves a value along with its remaining TTL
func (c *InMemoryCache[K, V]) GetWithTTL(key K) (V, time.Duration, bool) {
	var zero V
	item, now, ok := c.get(key)
	if !ok {
		return zero, 0, false
	}

	if item.expireTime == 0 {
		return item.value, -1, true
	}

	ttl := time.Duration(item.expireTime - now)
	return item.value, ttl, true
}

// Set stores a key-value pair with the specified TTL.
// The method handles item replacement, capacity management, and eviction policy updates.
func (c *InMemoryCache[K, V]) Set(key K, value V, ttl time.Duration) error {
	if atomic.LoadInt32(&c.closed) == 1 {
		return ErrCacheClosed
	}

	// Normalize TTL: 0 means use default
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
		item.expireTime = 0
	}

	shard.mu.Lock()
	defer shard.mu.Unlock()

	// Remove existing item if present
	if existing, exists := shard.data[key]; exists {
		shard.removeFromLRU(existing)
		if c.config.EvictionPolicy == LFU && existing.heapIndex != -1 {
			heap.Remove(shard.lfuHeap, existing.heapIndex)
		}
		c.itemPool.Put(existing)
		atomic.AddInt64(&shard.size, -1)
	}

	// Evict if shard exceeds capacity
	maxShardSize := c.config.MaxSize / int64(len(c.shards))
	if c.config.MaxSize > 0 && maxShardSize > 0 && atomic.LoadInt64(&shard.size) >= maxShardSize {
		c.evictor.evict(shard, &c.itemPool, c.config.StatsEnabled)
	}

	shard.data[key] = item
	shard.addToLRUHead(item)

	if c.config.EvictionPolicy == LFU {
		heap.Push(shard.lfuHeap, item)
	}

	atomic.AddInt64(&shard.size, 1)
	return nil
}

// SetWithCallback stores a key-value pair and executes a callback function when the item expires
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

// Stats returns performance statistics
func (c *InMemoryCache[K, V]) Stats() Stats {
	var stats Stats
	stats.Size = c.Size()
	stats.Capacity = c.config.MaxSize
	stats.Shards = len(c.shards)

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

// cleanupWorker runs in a background goroutine to periodically remove expired items
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
type cacheItem[V any] struct {
	value      V
	expireTime int64
	lastAccess int64
	frequency  int64
	prev       *cacheItem[V]
	next       *cacheItem[V]
	key        any
	heapIndex  int
}

// lfuHeap implements a min-heap for LFU eviction policy.
// Items are ordered by access frequency, with ties broken by access time.
type lfuHeap[V any] []*cacheItem[V]

// Len returns the number of items in the heap.
func (h lfuHeap[V]) Len() int { return len(h) }

// Less reports whether the item at index i should sort before the item at index j.
// Items with lower frequency are prioritized; ties are broken by access time.
func (h lfuHeap[V]) Less(i, j int) bool {
	if h[i].frequency != h[j].frequency {
		return h[i].frequency < h[j].frequency
	}
	// Break ties by access time (older items have priority for eviction)
	return h[i].lastAccess < h[j].lastAccess
}

// Swap exchanges the elements at indices i and j.
func (h lfuHeap[V]) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIndex = i
	h[j].heapIndex = j
}

// Push adds an element to the heap.
func (h *lfuHeap[V]) Push(x any) {
	item := x.(*cacheItem[V])
	item.heapIndex = len(*h)
	*h = append(*h, item)
}

// Pop removes and returns the minimum element from the heap.
func (h *lfuHeap[V]) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	item.heapIndex = -1
	*h = old[0 : n-1]
	return item
}

// update re-establishes the heap ordering after an item's priority changes.
func (h *lfuHeap[V]) update(item *cacheItem[V]) {
	heap.Fix(h, item.heapIndex)
}
