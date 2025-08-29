package cache

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	//   NoExpiration: caller intends the entry to never expire (no time checks on reads).
	//   DefaultExpiration: use Config.DefaultTTL at Set-time.
	NoExpiration      time.Duration = -1
	DefaultExpiration time.Duration = 0

	defaultMaxSize         = 10000
	defaultCleanupInterval = 5 * time.Minute
	defaultTTL             = 30 * time.Minute

	// We cap shards at a hard limit and scale by CPU*multiplier to reduce lock contention.
	// Shard count is rounded to the next power-of-two (see New()); this allows modulo
	// with a bit-mask (hash & (N-1)) which is measurably faster than % for hot paths.
	maxShardCount   = 256
	shardMultiplier = 4
)

// EvictionPolicy is a per-cache global that dictates how victims are chosen at capacity.
// Implementation detail: even for FIFO and AdmissionLFU, we still maintain the LRU
// intrusive list as a general doubly-linked list for "age"/insertion-order.
type EvictionPolicy int

const (
	LRU          EvictionPolicy = iota // least-recently-used; requires write lock on Get() to move nodes
	LFU                                // least-frequently-used; O(1) frequency list; write lock on Get()
	FIFO                               // first-in-first-out; uses LRU list in insertion-order mode
	AdmissionLFU                       // sample-based victim selection + admission filter (approx-LFU)
)

// Cache is the public surface area.
// Value V is stored by reference in cacheItem; caller owns V's interior mutability.
type Cache[K comparable, V any] interface {
	Set(key K, value V, ttl time.Duration) error
	Get(key K) (V, bool)
	Delete(key K) bool
	Clear()
	Size() int64
	Stats() Stats
	Close() error
}

// cacheItem is the node stored in per-shard maps and linked lists.
//   - Under LRU/LFU, we mutate list links and frequency under shard write lock.
//   - Under FIFO/AdmissionLFU, Get() uses RLock and updates only via atomics
//     where needed (see AdmissionLFU branch).
type cacheItem[V any] struct {
	value      V
	expireTime int64 // absolute expiration in UnixNano; 0 => no expiration
	lastAccess int64 // UnixNano of last touch (policy-dependent; not always updated)
	frequency  int64 // LFU counter (increment policy-dependent)
	prev       *cacheItem[V]
	next       *cacheItem[V]
	key        any // stored to allow deletion from map without recomputing hash
}

// Stats is best-effort approximate telemetry. Values are updated via atomics
// and read without coordination, so they may be slightly stale at read time.
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

// Config collects capacity, sharding, TTL, and policy choices.
// MaxSize is a soft capacity; per-shard capacity is floor(MaxSize / shards).
// ShardCount==0 => auto (CPU*multiplier), then rounded to power-of-two.
// AdmissionResetInterval influences only AdmissionLFU doorkeeper reset.
type Config struct {
	MaxSize                int64
	ShardCount             int
	CleanupInterval        time.Duration
	DefaultTTL             time.Duration
	EvictionPolicy         EvictionPolicy
	StatsEnabled           bool
	AdmissionResetInterval time.Duration
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

// InMemoryCache is a sharded, lock-based cache.
//   - Hash(key) selects a shard using a power-of-two mask (hash & (shards-1)).
//   - Each shard maintains its own map and intrusive lists, confining locks.
//   - sync.Pool is used for cacheItem reuse to reduce GC pressure at high churn.
//   - All public methods are safe for concurrent use.
//   - Per-shard mutexes serialize structural mutations (maps, lists).
type InMemoryCache[K comparable, V any] struct {
	shards      []*shard[K, V] // shard-local maps + lists + counters
	shardMask   uint64         // shards == power-of-two; mask = shards-1
	config      Config
	globalStats Stats         // currently only mirrors aggregation (see Stats())
	perShardCap int64         // floor(MaxSize / shards); 0 => unlimited per shard
	cleanupCh   chan struct{} // manual cleanup trigger (non-blocking)
	closeCh     chan struct{} // signals background goroutines to exit
	closeOnce   sync.Once
	closed      int32         // 1 => cache is closed; checked on hot paths
	itemPool    sync.Pool     // pool of *cacheItem[V]; reduces allocations
	hasher      hasher[K]     // key hashing; must be stable and fast
	evictor     evictor[K, V] // chosen policy implementation
}

// New builds a cache instance and spawns the periodic cleaner when configured.
//  1. Determine shard count (auto by CPU * multiplier, bounded, then rounded to pow2).
//  2. Compute per-shard capacity (floor) to guarantee Σ shard sizes ≤ MaxSize.
//  3. Pre-size per-shard maps when capacity is known (reduces map growth cost).
//  4. Initialize policy-specific structures (LFU list, AdmissionLFU admission filter).
func New[K comparable, V any](config Config) *InMemoryCache[K, V] {
	shardCount := config.ShardCount
	if shardCount <= 0 {
		// Over-provision shards relative to CPUs to reduce lock contention.
		shardCount = runtime.NumCPU() * shardMultiplier
		if shardCount > maxShardCount {
			shardCount = maxShardCount
		}
	}

	// If MaxSize is provided, cap shardCount to the largest power-of-two <= min(MaxSize, maxShardCount).
	// Rationale: perShardCap = floor(MaxSize / shards). When shards > MaxSize, perShardCap could be 0,
	// which prevents growth and causes pathological evictions. This guard keeps perShardCap ≥ 1.
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
	shardCount = nextPowerOf2(shardCount)

	cache := &InMemoryCache[K, V]{
		shards:    make([]*shard[K, V], shardCount),
		shardMask: uint64(shardCount - 1), // fast modulo for shard selection
		config:    config,
		cleanupCh: make(chan struct{}, 1), // 1-deep to coalesce signals
		closeCh:   make(chan struct{}),
		itemPool: sync.Pool{
			New: func() any { return &cacheItem[V]{} },
		},
	}

	// Compute per-shard capacity once. This bound is used by Set() to decide when to evict.
	if config.MaxSize > 0 {
		cache.perShardCap = config.MaxSize / int64(shardCount) // floor division
	}

	cache.hasher = newHasher[K]()
	cache.evictor = createEvictor[K, V](config.EvictionPolicy)

	// Pre-size maps when capacity is known; reduces rehashing during warm-up.
	capHint := 0
	if cache.perShardCap > 0 {
		capHint = int(cache.perShardCap)
	}

	for i := 0; i < shardCount; i++ {
		s := &shard[K, V]{data: make(map[K]*cacheItem[V], capHint)}

		//   - LRU: it tracks recency (head=newest, tail=oldest).
		//   - FIFO: it tracks insertion order (we still use the same structure).
		//   - LFU: we still maintain it for uniform removal bookkeeping.
		s.initLRU()

		if config.EvictionPolicy == LFU {
			s.lfuList = newLFUList[K, V]() // O(1) frequency bucket list
		}
		if config.EvictionPolicy == AdmissionLFU && cache.perShardCap > 0 {
			// Counters for the admission filter frequency sketch; rule-of-thumb: ~10× per-shard cap.
			// Lower bound guarantees minimum accuracy at small capacities.
			nc := uint64(cache.perShardCap * 10)
			if nc < 1024 {
				nc = 1024
			}

			// Reset interval controls the doorkeeper's periodic bitset clear; avoids long-term saturation.
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

// NewWithDefaults is a convenience constructor using DefaultConfig values.
func NewWithDefaults[K comparable, V any]() *InMemoryCache[K, V] {
	return New[K, V](DefaultConfig())
}

// getShard uses hash(key) & shardMask to select a shard.
// The power-of-two property lets us replace modulo with a single AND.
func (c *InMemoryCache[K, V]) getShard(key K) *shard[K, V] {
	hash := c.hasher.hash(key)
	return c.shards[hash&c.shardMask]
}

// get retrieves the item and updates policy-specific metadata.
// Locking:
//   - LRU/LFU: take write lock because we mutate list position / frequency.
//   - FIFO/AdmissionLFU: take read lock; we avoid structural mutations on reads
//     (AdmissionLFU updates per-item metadata using atomics).
//
// Expiration:
//   - Lazy expiration: if TTL is set, a read past TTL removes the entry eagerly.
func (c *InMemoryCache[K, V]) get(key K) (*cacheItem[V], int64, bool) {
	if atomic.LoadInt32(&c.closed) == 1 {
		return nil, 0, false
	}
	shard := c.getShard(key)

	nw := c.config.EvictionPolicy == LRU || c.config.EvictionPolicy == LFU
	if nw {
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

	now := time.Now().UnixNano()
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
			atomic.AddInt64(&shard.misses, 1)
		}

		return nil, now, false
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

func (c *InMemoryCache[K, V]) Get(key K) (V, bool) {
	var zero V
	item, _, ok := c.get(key)
	if !ok {
		return zero, false
	}
	return item.value, true
}

// GetWithTTL returns the value and remaining TTL.
//   - expireTime == 0  => returns ttl = -1 (never expires)
//   - expired on read  => get() already removed it and returned ok=false
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

// Set inserts or updates a key/value with the specified TTL.
// If perShardCap > 0 and the shard is full, we evict before inserting.
// AdmissionLFU delegates the decision to its admission filter; a rejection
// short-circuits the Set without evicting (to avoid polluting the cache).
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

	// Update in place if key exists. We keep the same node to preserve list links.
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

	// Enforce capacity; AdmissionLFU may reject the incoming item without eviction.
	if c.perShardCap > 0 && atomic.LoadInt64(&shard.size) >= c.perShardCap {
		if c.config.EvictionPolicy == AdmissionLFU && shard.admission != nil {
			admissionEvictor := c.evictor.(admissionLFUEvictor[K, V])
			// sample → admission → eviction:
			// if admission denies, we return early (reject Set) and avoid ejecting a victim.
			if !admissionEvictor.evictWithAdmission(
				shard, &c.itemPool, c.config.StatsEnabled,
				shard.admission, c.hasher.hash(key),
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

// SetWithCallback inserts a key/value and, after TTL elapses, invokes the callback.
// We use a one-shot timer per call; the callback path double-checks expiration
// under shard RLock because timers are not perfectly aligned with wall clock and
// items may have been updated/removed in the interim.
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
				// Re-validate expiration under lock: prevents early/duplicate callbacks
				// if the item was updated with a newer TTL or removed.
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

// Delete removes the key from its shard and returns whether it existed.
// We unlink from LRU list and, if applicable, LFU structures, then recycle the node.
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

// Clear drops all entries across shards and reinitializes policy structures.
// AdmissionLFU: we call admission.Reset() to clear doorkeeper/sketch state.
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

// Exists checks membership without touching recency/frequency.
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

// Keys enumerates all keys that are not expired at the moment of the scan.
// It holds each shard's RLock while iterating. Returned slice reflects a
// point-in-time view per shard and may exclude keys inserted concurrently.
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

// Size aggregates per-shard sizes via atomic loads.
// This is O(shards) and returns a consistent sum at the instant of reads.
func (c *InMemoryCache[K, V]) Size() int64 {
	var totalSize int64
	for _, shard := range c.shards {
		totalSize += atomic.LoadInt64(&shard.size)
	}
	return totalSize
}

// Stats aggregates counters and computes a derived hit ratio.
// All fields are approximate due to concurrent updates.
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

// Close idempotently tears down the cache. We stop background goroutines,
// clear all shards, and mark the cache as closed (subsequent calls are no-ops).
func (c *InMemoryCache[K, V]) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closeCh)
		c.Clear()
		atomic.StoreInt32(&c.closed, 1)
	})
	return err
}

// TriggerCleanup requests an immediate cleanup run. If the ticker is disabled,
// we run cleanup inline. Otherwise we attempt a non-blocking signal; if a signal
// is already pending, we drop this one to avoid piling up work.
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
		// Channel is full; a cleanup is already scheduled.
	}
}

// cleanupWorker runs the periodic cleaner. It exits when Close() closes closeCh.
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

// cleanup scans shards and removes expired items using a single time anchor
// (now) for the entire pass, which keeps relative TTL checks consistent
// within the run.
func (c *InMemoryCache[K, V]) cleanup() {
	if atomic.LoadInt32(&c.closed) == 1 {
		return
	}

	now := time.Now().UnixNano()
	for _, shard := range c.shards {
		shard.cleanup(now, c.config.EvictionPolicy, &c.itemPool, c.config.StatsEnabled)
	}
}
