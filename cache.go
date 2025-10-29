package cache

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unkn0wn-root/kioshun/internal/mathutil"
)

const (
	// Expiration sentinels for Set(): NoExpiration => never expires; DefaultExpiration => use Config.DefaultTTL.
	NoExpiration      time.Duration = -1
	DefaultExpiration time.Duration = 0

	// Default capacities/intervals (used by DefaultConfig()).
	defaultMaxSize         = 10000
	defaultCleanupInterval = 5 * time.Minute
	defaultTTL             = 30 * time.Minute

	// Sharding: cap shard count, scale by CPUs, and round to power-of-two for mask-based modulo.
	maxShardCount   = 256
	shardMultiplier = 4
)

// EvictionPolicy selects the global replacement strategy.
type EvictionPolicy int

const (
	LRU          EvictionPolicy = iota // least-recently-used
	LFU                                // least-frequently-used
	FIFO                               // first-in-first-out (uses LRU list insertion order)
	AdmissionLFU                       // victim sampling + adaptive admission (approx-LFU)
)

// Cache is the public API of the sharded in-memory cache.
type Cache[K comparable, V any] interface {
	Set(key K, value V, ttl time.Duration) error
	Get(key K) (V, bool)
	Delete(key K) bool
	Clear()
	Size() int64
	Stats() Stats
	Close() error
}

// cacheItem is the node stored in shard maps/lists; fields updated per policy.
type cacheItem[V any] struct {
	value      V
	expireTime int64 // absolute ns; 0 => no expiration
	lastAccess int64 // last touch time (ns) for policies that use it
	frequency  int64 // LFU counter (policy-dependent)
	prev       *cacheItem[V]
	next       *cacheItem[V]
	key        any // original key for deletions
}

// Stats exposes approximate telemetry aggregated across shards.
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

// Config groups capacity, sharding, TTL, policy, and telemetry options.
type Config struct {
	MaxSize                int64
	ShardCount             int
	CleanupInterval        time.Duration
	DefaultTTL             time.Duration
	EvictionPolicy         EvictionPolicy
	StatsEnabled           bool
	AdmissionResetInterval time.Duration
}

// DefaultConfig returns sane defaults for a general-purpose cache.
func DefaultConfig() Config {
	return Config{
		MaxSize:                defaultMaxSize,
		ShardCount:             0,
		CleanupInterval:        defaultCleanupInterval,
		DefaultTTL:             defaultTTL,
		EvictionPolicy:         AdmissionLFU,
		StatsEnabled:           true,
		AdmissionResetInterval: 1 * time.Minute,
	}
}

// InMemoryCache is a sharded, lock-based cache with per-policy metadata.
type InMemoryCache[K comparable, V any] struct {
	shards      []*shard[K, V] // shard-local maps + lists + counters
	shardMask   uint64         // shards is power-of-two; mask = shards-1
	config      Config
	globalStats Stats         // mirrors aggregated Stats()
	perShardCap int64         // floor(MaxSize / shards); 0 => unlimited
	cleanupCh   chan struct{} // manual cleanup trigger (coalesced)
	closeCh     chan struct{} // background goroutine shutdown
	closeOnce   sync.Once
	closed      int32         // 1 => cache closed; hot-path check
	itemPool    sync.Pool     // *cacheItem[V] reuse to lower GC pressure
	hasher      hasher[K]     // fast, stable key hash
	evictor     evictor[K, V] // policy implementation
}

// New builds a cache, sizes shards/capacity, initializes policy data, and starts cleaner if configured.
func New[K comparable, V any](config Config) *InMemoryCache[K, V] {
	shardCount := config.ShardCount
	if shardCount <= 0 {
		// Over-provision shards to reduce lock contention.
		shardCount = runtime.NumCPU() * shardMultiplier
		if shardCount > maxShardCount {
			shardCount = maxShardCount
		}
	}

	// Guard against zero per-shard capacity when MaxSize is small; cap shards accordingly.
	if config.MaxSize > 0 {
		limit := config.MaxSize
		if limit > int64(maxShardCount) {
			limit = int64(maxShardCount)
		}
		maxPow2 := 1
		for (int64(maxPow2) << 1) <= limit {
			maxPow2 <<= 1
		}
		if shardCount > maxPow2 {
			shardCount = maxPow2
		}
	}
	shardCount = mathutil.NextPowerOf2(shardCount)

	cache := &InMemoryCache[K, V]{
		shards:    make([]*shard[K, V], shardCount),
		shardMask: uint64(shardCount - 1),
		config:    config,
		cleanupCh: make(chan struct{}, 1),
		closeCh:   make(chan struct{}),
		itemPool: sync.Pool{
			New: func() any { return &cacheItem[V]{} },
		},
	}

	// Precompute per-shard capacity for Set()-time eviction decisions.
	if config.MaxSize > 0 {
		cache.perShardCap = config.MaxSize / int64(shardCount)
	}

	cache.hasher = newHasher[K]()
	cache.evictor = createEvictor[K, V](config.EvictionPolicy)

	// Pre-size shard maps; init per-policy structures.
	capHint := 0
	if cache.perShardCap > 0 {
		capHint = int(cache.perShardCap)
	}

	for i := 0; i < shardCount; i++ {
		s := &shard[K, V]{data: make(map[K]*cacheItem[V], capHint)}
		s.initLRU() // LRU list is used for LRU, FIFO ordering, and uniform removal

		if config.EvictionPolicy == LFU {
			s.lfuList = newLFUList[K, V]() // O(1) freq buckets
		}
		if config.EvictionPolicy == AdmissionLFU && cache.perShardCap > 0 {
			// Size frequency sketch counters (~10× capacity; min 1024) and set doorkeeper reset cadence.
			nc := uint64(cache.perShardCap * 10)
			if nc < 1024 {
				nc = 1024
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

// NewWithDefaults constructs a cache using DefaultConfig().
func NewWithDefaults[K comparable, V any]() *InMemoryCache[K, V] {
	return New[K, V](DefaultConfig())
}

// getShard maps key → shard using hash & mask (fast modulo).
func (c *InMemoryCache[K, V]) getShard(key K) *shard[K, V] {
	hash := c.hasher.hash(key)
	return c.shards[hash&c.shardMask]
}

// get reads an item, applies expiration, and updates policy metadata (locking varies by policy).
func (c *InMemoryCache[K, V]) get(key K) (*cacheItem[V], int64, bool) {
	if atomic.LoadInt32(&c.closed) == 1 {
		return nil, 0, false
	}
	shard := c.getShard(key)

	nw := c.config.EvictionPolicy == LRU || c.config.EvictionPolicy == LFU
	shardLockedWrite := false
	if nw {
		shard.mu.Lock()
		shardLockedWrite = true
	} else {
		shard.mu.RLock()
	}
	defer func() {
		if shardLockedWrite {
			shard.mu.Unlock()
		} else {
			shard.mu.RUnlock()
		}
	}()

	item, exists := shard.data[key]
	if !exists {
		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.misses, 1)
		}
		return nil, 0, false
	}

	var now int64
	for {
		now = time.Now().UnixNano()
		if item.expireTime > 0 && now > item.expireTime {
			if !shardLockedWrite {
				shard.mu.RUnlock()
				shard.mu.Lock()
				shardLockedWrite = true

				item, exists = shard.data[key]
				if !exists {
					if c.config.StatsEnabled {
						atomic.AddInt64(&shard.misses, 1)
					}
					return nil, now, false
				}
				// Item might have been refreshed while upgrading the lock; re-evaluate.
				continue
			}

			delete(shard.data, key)
			shard.removeFromLRU(item)

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
		break
	}

	switch c.config.EvictionPolicy {
	case LRU:
		shard.moveToLRUHead(item)
	case LFU:
		item.lastAccess = now
		item.frequency++
		shard.lfuList.increment(item)
	case AdmissionLFU:
		atomic.StoreInt64(&item.lastAccess, now)
		atomic.AddInt64(&item.frequency, 1)
	}

	if c.config.StatsEnabled {
		atomic.AddInt64(&shard.hits, 1)
	}

	return item, now, true
}

// Get returns the value for key, if present and not expired.
func (c *InMemoryCache[K, V]) Get(key K) (V, bool) {
	var zero V
	item, _, ok := c.get(key)
	if !ok {
		return zero, false
	}
	return item.value, true
}

// GetWithTTL returns the value and the remaining TTL (-1 if never expires).
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

// Set inserts/updates a key with TTL, evicting if needed (or consulting AdmissionLFU to skip pollution).
func (c *InMemoryCache[K, V]) Set(key K, value V, ttl time.Duration) error {
	if atomic.LoadInt32(&c.closed) == 1 {
		return ErrCacheClosed
	}
	if ttl == 0 {
		ttl = c.config.DefaultTTL
	}

	kh := c.hasher.hash(key)
	shard := c.shardByHash(kh)
	now := time.Now().UnixNano()

	var expireTime int64
	if ttl > 0 {
		expireTime = now + ttl.Nanoseconds()
	}

	shard.mu.Lock()
	defer shard.mu.Unlock()

	// In-place update for existing key to preserve list links.
	if ex, exists := shard.data[key]; exists {
		ex.value = value
		ex.lastAccess = now
		ex.expireTime = expireTime

		switch c.config.EvictionPolicy {
		case LRU:
			shard.moveToLRUHead(ex)
		case LFU:
			shard.lfuList.remove(ex)
			ex.frequency = 1
			shard.lfuList.add(ex)
		case AdmissionLFU:
			ex.frequency = 1
		}
		return nil
	}

	// Capacity enforcement: either evict (LRU/LFU/FIFO) or consult admission filter (AdmissionLFU).
	if c.perShardCap > 0 && atomic.LoadInt64(&shard.size) >= c.perShardCap {
		if c.config.EvictionPolicy == AdmissionLFU && shard.admission != nil {
			admissionEvictor := c.evictor.(admissionLFUEvictor[K, V])
			// Sample → admission → eviction; admission may reject without evicting.
			if !admissionEvictor.evictWithAdmission(
				shard, &c.itemPool, c.config.StatsEnabled,
				shard.admission, kh,
			) {
				return nil
			}
		} else {
			// Other policies always evict one victim when full.
			c.evictor.evict(shard, &c.itemPool, c.config.StatsEnabled)
		}
	}

	item := c.itemPool.Get().(*cacheItem[V])
	item.value = value
	item.lastAccess = now
	item.frequency = 1
	item.key = key
	item.expireTime = expireTime

	shard.data[key] = item
	shard.addToLRUHead(item)

	if c.config.EvictionPolicy == LFU {
		shard.lfuList.add(item)
	}

	atomic.AddInt64(&shard.size, 1)
	return nil
}

// SetWithCallback sets a key and executes a callback once its TTL elapses (re-validates under lock).
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
				// Check under lock to avoid firing for updated/removed items.
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

// Delete removes a key (if present), unlinks it from lists, and recycles the node.
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

// Clear empties all shards and reinitializes policy structures (resets AdmissionLFU state).
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

// Exists checks membership without mutating recency/frequency (removes if expired).
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

// Keys returns a point-in-time snapshot of non-expired keys across shards.
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

// Size sums per-shard sizes via atomic loads (O(shards)).
func (c *InMemoryCache[K, V]) Size() int64 {
	var totalSize int64
	for _, shard := range c.shards {
		totalSize += atomic.LoadInt64(&shard.size)
	}
	return totalSize
}

// Stats aggregates counters and computes hit ratio.
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

// Close idempotently shuts down background work, clears shards, and marks the cache closed.
func (c *InMemoryCache[K, V]) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closeCh)
		c.Clear()
		atomic.StoreInt32(&c.closed, 1)
	})
	return err
}

// TriggerCleanup requests a cleanup run (inline if ticker disabled; coalesced otherwise).
func (c *InMemoryCache[K, V]) TriggerCleanup() {
	if atomic.LoadInt32(&c.closed) == 1 {
		return
	}

	if c.config.CleanupInterval <= 0 {
		c.cleanup()
		return
	}

	select {
	case c.cleanupCh <- struct{}{}:
	default:
		// already scheduled
	}
}

// cleanupWorker runs the periodic cleaner until Close() signals exit.
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

// cleanup removes expired items across shards using a single time anchor.
func (c *InMemoryCache[K, V]) cleanup() {
	if atomic.LoadInt32(&c.closed) == 1 {
		return
	}

	now := time.Now().UnixNano()
	for _, shard := range c.shards {
		shard.cleanup(now, c.config.EvictionPolicy, &c.itemPool, c.config.StatsEnabled)
	}
}

// shardByHash maps a precomputed hash → shard (saves rehash in hot paths).
func (c *InMemoryCache[K, V]) shardByHash(hash uint64) *shard[K, V] {
	return c.shards[hash&c.shardMask]
}
