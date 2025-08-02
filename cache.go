package cache

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	NoExpiration      time.Duration = -1 // Never expires
	DefaultExpiration time.Duration = 0  // Use default config value

	// default configuration values
	defaultMaxSize         = 10000
	defaultCleanupInterval = 5 * time.Minute
	defaultTTL             = 30 * time.Minute
	maxShardCount          = 256
	shardMultiplier        = 4
)

// EvictionPolicy defines the eviction algorithm
type EvictionPolicy int

const (
	LRU EvictionPolicy = iota
	LFU
	FIFO
	AdmissionLFU
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

// cacheItem represents a single cache entry with metadata for eviction policies
type cacheItem[V any] struct {
	value      V
	expireTime int64
	lastAccess int64
	frequency  int64
	prev       *cacheItem[V]
	next       *cacheItem[V]
	key        any
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
	MaxSize                int64          // Maximum number of items (0 = unlimited)
	ShardCount             int            // Number of shards (0 = auto-detect)
	CleanupInterval        time.Duration  // How often to run cleanup (0 = no cleanup)
	DefaultTTL             time.Duration  // Default TTL for items (0 = no expiration)
	EvictionPolicy         EvictionPolicy // Eviction algorithm
	StatsEnabled           bool           // Enable statistics collection
	AdmissionResetInterval time.Duration  // Timeout for AdmissionLFU
}

func DefaultConfig() Config {
	return Config{
		MaxSize:                defaultMaxSize,
		ShardCount:             0,
		CleanupInterval:        defaultCleanupInterval,
		DefaultTTL:             defaultTTL,
		EvictionPolicy:         LRU,
		StatsEnabled:           true,
		AdmissionResetInterval: 1 * time.Minute,
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
// Shard count is auto-detected based on CPU cores (if not specified),
// and is rounded to the next power of 2 for hash distribution.
func New[K comparable, V any](config Config) *InMemoryCache[K, V] {
	shardCount := config.ShardCount
	if shardCount <= 0 {
		// auto-detect based on CPU cores, capped at maximum
		// multiplier provides more shards than cores to reduce lock contention
		shardCount = runtime.NumCPU() * shardMultiplier
		if shardCount > maxShardCount {
			shardCount = maxShardCount
		}
	}
	shardCount = nextPowerOf2(shardCount)

	cache := &InMemoryCache[K, V]{
		shards:    make([]*shard[K, V], shardCount),
		shardMask: uint64(shardCount - 1), // hash & (shardCount-1) ≡ hash % shardCount
		config:    config,
		cleanupCh: make(chan struct{}, 1),
		closeCh:   make(chan struct{}),
		itemPool: sync.Pool{
			New: func() any {
				return &cacheItem[V]{}
			},
		},
	}

	cache.hasher = newHasher[K]()
	cache.evictor = createEvictor[K, V](config.EvictionPolicy)

	shardMaxSize := config.MaxSize / int64(shardCount)
	for i := 0; i < shardCount; i++ {
		s := &shard[K, V]{
			data: make(map[K]*cacheItem[V]),
		}
		// LRU list is used for all eviction policies to maintain insertion order
		s.initLRU()

		if config.EvictionPolicy == LFU {
			s.lfuList = newLFUList[K, V]()
		}

		if config.EvictionPolicy == AdmissionLFU && shardMaxSize > 0 {
			nc := uint64(shardMaxSize * 10)
			if nc < 1024 {
				nc = 1024 // Minimum counters per shard
			}

			resetInterval := config.AdmissionResetInterval
			if resetInterval == 0 {
				resetInterval = 1 * time.Minute
			}
			s.admission = newAdaptiveAdmissionFilter(nc, resetInterval)
		}

		cache.shards[i] = s
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

// getShard returns the shard for a given key using hash distribution.
// shardCount is always a power of 2, making shardMask = shardCount-1.
func (c *InMemoryCache[K, V]) getShard(key K) *shard[K, V] {
	hash := c.hasher.hash(key)
	return c.shards[hash&c.shardMask] // hash & (2^n - 1) ≡ hash % 2^n
}

// get retrieves an item from the cache.
// LRU/LFU policies require write locks to update access metadata,
// while FIFO policies only need read locks for retrieval.
func (c *InMemoryCache[K, V]) get(key K) (*cacheItem[V], int64, bool) {
	if atomic.LoadInt32(&c.closed) == 1 {
		return nil, 0, false
	}

	shard := c.getShard(key)

	// LRU/LFU policies require write access to update metadata,
	// while FIFO use read locks since they don't update on access
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

	// Lazy expiration: remove expired items during access instead of background scan
	now := time.Now().UnixNano()
	if item.expireTime > 0 && now > item.expireTime {
		delete(shard.data, key)
		shard.removeFromLRU(item)
		// Remove from LFU list if present
		if c.config.EvictionPolicy == LFU {
			shard.lfuList.remove(item)
		}
		c.itemPool.Put(item)
		atomic.AddInt64(&shard.size, -1)

		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.expirations, 1)
			atomic.AddInt64(&shard.misses, 1)
		}

		return nil, now, false
	}

	switch c.config.EvictionPolicy {
	case LRU:
		shard.moveToLRUHead(item)
	case LFU:
		item.lastAccess = now
		atomic.AddInt64(&item.frequency, 1)
		shard.lfuList.increment(item)
	case AdmissionLFU:
		item.lastAccess = now
		atomic.AddInt64(&item.frequency, 1)
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
func (c *InMemoryCache[K, V]) Set(key K, value V, ttl time.Duration) error {
	if atomic.LoadInt32(&c.closed) == 1 {
		return ErrCacheClosed
	}

	if ttl == 0 {
		ttl = c.config.DefaultTTL
	}

	shard := c.getShard(key)
	now := time.Now().UnixNano()

	var expireTime int64
	if ttl > 0 {
		expireTime = now + ttl.Nanoseconds()
	}

	shard.mu.Lock()
	defer shard.mu.Unlock()

	if ex, exists := shard.data[key]; exists {
		// in-place update: reuse existing item
		ex.value = value
		ex.lastAccess = now
		ex.expireTime = expireTime

		switch c.config.EvictionPolicy {
		case LRU:
			shard.moveToLRUHead(ex)
		case LFU:
			// reset frequency counter for new value.
			// we do not want to preserve last frequanency since this is a new value
			// reusing the same key so set freq. to 1
			shard.lfuList.remove(ex)
			ex.frequency = 1
			shard.lfuList.add(ex)
		case AdmissionLFU:
			ex.frequency = 1
		}
		return nil
	}

	// Proactive eviction: check capacity before allocation to prevent oversized shards
	// Distribute total capacity evenly across shards
	maxShardSize := c.config.MaxSize / int64(len(c.shards))
	if c.config.MaxSize > 0 && maxShardSize > 0 && atomic.LoadInt64(&shard.size) >= maxShardSize {
		if c.config.EvictionPolicy == AdmissionLFU && shard.admission != nil {
			admissionEvictor := c.evictor.(admissionLFUEvictor[K, V])
			// sample → admission → eviction:
			if !admissionEvictor.evictWithAdmission(
				shard, &c.itemPool, c.config.StatsEnabled,
				shard.admission, c.hasher.hash(key),
			) {
				// lost admission → reject without ejecting
				return nil
			}
		} else {
			// Standard eviction for other policies
			c.evictor.evict(shard, &c.itemPool, c.config.StatsEnabled)
		}
	}

	// Allocate from pool after eviction to ensure space availability
	item := c.itemPool.Get().(*cacheItem[V])
	item.value = value
	item.lastAccess = now
	item.frequency = 1 // Start with access count of 1
	item.key = key
	item.expireTime = expireTime

	shard.data[key] = item
	shard.addToLRUHead(item) // Add to LRU list head (most recently used position)

	if c.config.EvictionPolicy == LFU {
		shard.lfuList.add(item)
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
	if c.config.EvictionPolicy == LFU {
		shard.lfuList.remove(item)
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
			shard.lfuList = newLFUList[K, V]()
		}
		if c.config.EvictionPolicy == AdmissionLFU && shard.admission != nil {
			shard.admission.Reset()
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
		if c.config.EvictionPolicy == LFU {
			shard.lfuList.remove(item)
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
