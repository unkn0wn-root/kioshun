package kioshun

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unkn0wn-root/kioshun/internal/keyhash"
)

// Store is the key/value store implemented by Cache.
type Store[K comparable, V any] interface {
	Set(key K, value V, ttl time.Duration) error
	Get(key K) (V, bool)
	Delete(key K) bool
	Clear()
	Close() error
}

var _ Store[string, int] = (*Cache[string, int])(nil)

// Stats exposes approx. telemetry aggregated across shards.
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

// PolicyStats exposes SieveTinyLFU admission and replacement decisions.
type PolicyStats struct {
	Admits             int64
	Rejects            int64
	GhostHits          int64
	Promotions         int64
	ProbationEvictions int64
	MainEvictions      int64
}

// Cache is a sharded, lock-based in-memory cache with per-policy metadata.
type Cache[K comparable, V any] struct {
	shards      []*shard[K, V] // maps + lists + counters
	shardMask   uint64         // shards is 2^n; mask = shards-1
	config      Config
	perShardCap int64 // floor(MaxSize/shards); 0 => unlimited
	closeCh     chan struct{}
	closeOnce   sync.Once
	closed      int32 // 1 => cache closed
	workers     sync.WaitGroup
	itemPool    sync.Pool // *cacheItem[K, V] reuse to lower GC pressure
	waiterPool  sync.Pool // write ack waiters for sync mutation paths
	hasher      keyhash.Hasher[K]
	evictor     evictor[K, V] // nil for SieveTinyLFU; evicts through shard admission state
	onRemove    func(K, V, RemovalReason)
	onEvict     func(K, V)
	removeWake  chan struct{}
}

type Option[K comparable, V any] func(*Cache[K, V])

// New constructs a Cache from config, returning an error if config is invalid.
// Shard count is normalized to 2^n (and bounded by MaxSize); background
// workers start immediately, so callers must Close the cache to release them.
func New[K comparable, V any](config Config, opts ...Option[K, V]) (*Cache[K, V], error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.EvictionPolicy == DefaultEvictionPolicy {
		config.EvictionPolicy = DefaultConfig().EvictionPolicy
	}
	if config.WriteBufferSize == 0 {
		config.WriteBufferSize = defaultWriteBufferSize
	}
	if config.WriteBatchSize == 0 {
		config.WriteBatchSize = defaultWriteBatchSize
	}

	shardCount := config.ShardCount
	if shardCount <= 0 {
		// overprovision to reduce lock contention.
		shardCount = runtime.NumCPU() * shardMultiplier
		shardCount = min(shardCount, maxShardCount)
	}

	// guard against zero per-shard capacity when MaxSize is small.
	if config.MaxSize > 0 {
		limit := min(config.MaxSize, int64(maxShardCount))
		maxPow2 := 1
		for (int64(maxPow2) << 1) <= limit {
			maxPow2 <<= 1
		}
		shardCount = min(shardCount, maxPow2)
	}
	shardCount = nextPowerOf2(shardCount)

	cache := &Cache[K, V]{
		shards:    make([]*shard[K, V], shardCount),
		shardMask: uint64(shardCount - 1),
		config:    config,
		closeCh:   make(chan struct{}),
		itemPool: sync.Pool{
			New: func() any { return &cacheItem[K, V]{} },
		},
		waiterPool: sync.Pool{
			New: func() any { return &writeWaiter{ch: make(chan struct{}, 1)} },
		},
	}

	// precompute the base per-shard capacity. Individual shards receive the
	// remainder below so the aggregate capacity is exactly MaxSize.
	if config.MaxSize > 0 {
		cache.perShardCap = config.MaxSize / int64(shardCount)
	}

	cache.hasher = keyhash.New[K]()
	if config.EvictionPolicy != SieveTinyLFU {
		cache.evictor = createEvictor[K, V](config.EvictionPolicy)
	}

	for i := range shardCount {
		sc := int64(0)
		if config.MaxSize > 0 {
			sc = cache.perShardCap
			if int64(i) < config.MaxSize%int64(shardCount) {
				sc++
			}
		}
		capHint := 0
		if sc > 0 {
			capHint = int(sc)
		}

		s := &shard[K, V]{
			data:       make(map[K]*cacheItem[K, V], capHint),
			cap:        sc,
			wake:       make(chan struct{}, 1),
			writeBatch: make([]writeCommand[K, V], config.WriteBatchSize),
		}
		s.queue = newWriteQueue[K, V](config.WriteBufferSize, s.wake, cache.closeCh)
		s.initLRU()

		if config.EvictionPolicy == LFU {
			s.lfuList = newLFUList[K, V]()
		}
		if config.EvictionPolicy == SieveTinyLFU && s.cap > 0 {
			s.sieve = newSieveTinyLFU[K, V](s.cap, config.ProbationRatio, config.GhostRatio)
			s.readBuf = newReadBuffer() // pershard read sampling for the sketch
		}
		cache.shards[i] = s
	}

	for _, opt := range opts {
		opt(cache)
	}

	nm := cache.removalNotifyMask()
	if nm != 0 {
		cache.removeWake = make(chan struct{}, 1)
		for _, s := range cache.shards {
			s.removeWake = cache.removeWake
			s.removeNotifyMask = nm
		}
		cache.workers.Add(1)
		go cache.removeNotifyWorker()
	}

	for _, s := range cache.shards {
		cache.workers.Add(1)
		go cache.writeWorker(s)
	}

	if config.CleanupInterval > 0 {
		go cache.cleanupWorker()
	}

	return cache, nil
}

// NewDefault constructs a Cache with DefaultConfig.
// It panics only if the built-in default configuration is invalid,
// which is programmer error and should never happen.
func NewDefault[K comparable, V any]() *Cache[K, V] {
	cache, err := New[K, V](DefaultConfig())
	if err != nil {
		panic(fmt.Sprintf("kioshun: invalid default config: %v", err))
	}
	return cache
}

// Get returns the value for key, if present and not expired.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	var zero V
	res := c.get(key)
	if !res.ok {
		return zero, false
	}
	return res.value, true
}

// GetWithTTL returns the value and the remaining TTL (-1 if never expires).
func (c *Cache[K, V]) GetWithTTL(key K) (V, time.Duration, bool) {
	var zero V
	res := c.get(key)
	if !res.ok {
		return zero, 0, false
	}

	if res.expireTime == 0 {
		return res.value, -1, true
	}

	ttl := time.Duration(res.expireTime - res.now)
	return res.value, ttl, true
}

// Set inserts or updates key and waits until the owning shard has committed
// the write, giving immediate read-after-write visibility for that key.
func (c *Cache[K, V]) Set(key K, value V, ttl time.Duration) error {
	return c.setAndWait(key, value, ttl, nil)
}

// SetAsync accepts a queued insert/update command for key with TTL. A nil error
// means the command was accepted; call Sync for committed visibility.
func (c *Cache[K, V]) SetAsync(key K, value V, ttl time.Duration) error {
	return c.set(key, value, ttl, nil)
}

// SetWithCallback sets key and schedules the callback after the item is committed.
// The callback fires once the TTL elapses and re-validates the item under lock.
func (c *Cache[K, V]) SetWithCallback(key K, value V, ttl time.Duration, callback func(K, V)) error {
	return c.setAndWait(key, value, ttl, callback)
}

// Delete removes a key (if present), unlinks it from lists and recycles the node.
func (c *Cache[K, V]) Delete(key K) bool {
	kh := c.hasher.Sum(key)
	deleted, err := c.deleteSync(c.shardByHash(kh), key)
	if err != nil {
		return false
	}
	return deleted
}

// Clear empties all shards and reinitializes policy structures (resets SieveTinyLFU state).
func (c *Cache[K, V]) Clear() {
	_ = c.enqueueAllAndWait(writeClear)
}

// Exists checks membership without mutating recency/frequency (removes if expired).
func (c *Cache[K, V]) Exists(key K) bool {
	if c.isClosed() {
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
		c.removeItem(shard, item, RemovedExpired)
		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.expirations, 1)
		}
		return false
	}
	return true
}

// Keys returns a point-in-time snapshot of non-expired keys across shards.
func (c *Cache[K, V]) Keys() []K {
	if c.isClosed() {
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

// Size sums per-shard sizes (O(shards)).
func (c *Cache[K, V]) Size() int64 {
	var totalSize int64
	for _, shard := range c.shards {
		totalSize += atomic.LoadInt64(&shard.size)
	}
	return totalSize
}

// Stats aggregates counters and computes hit ratio.
func (c *Cache[K, V]) Stats() Stats {
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

// PolicyStats aggregates SieveTinyLFU policy counters across shards.
func (c *Cache[K, V]) PolicyStats() PolicyStats {
	var ps PolicyStats
	for _, shard := range c.shards {
		shard.mu.RLock()
		if shard.sieve != nil {
			ps.Admits += shard.sieve.stats.Admits
			ps.Rejects += shard.sieve.stats.Rejects
			ps.GhostHits += shard.sieve.stats.GhostHits
			ps.Promotions += shard.sieve.stats.Promotions
			ps.ProbationEvictions += shard.sieve.stats.ProbationEvictions
			ps.MainEvictions += shard.sieve.stats.MainEvictions
		}
		shard.mu.RUnlock()
	}
	return ps
}

// Close shuts down background work (idempotently), clears shards and marks the cache closed.
func (c *Cache[K, V]) Close() error {
	c.closeOnce.Do(func() {
		// stop accepting new producer writes (sequential Set-after-Close fails).
		atomic.StoreInt32(&c.closed, 1)
		// Drain everything accepted so far via a barrier, then broadcast shutdown:
		// workers do a final drain and exit; producers blocked on a full queue wake
		// and return ErrCacheClosed. No queue is ever closed out from under a sender.
		c.flush()
		close(c.closeCh)
		c.workers.Wait()
		c.clearDirect()
	})
	return nil
}

// Cleanup removes expired items across all shards.
func (c *Cache[K, V]) Cleanup() {
	if c.isClosed() {
		return
	}

	now := time.Now().UnixNano()
	for _, shard := range c.shards {
		shard.cleanup(now, c.config.EvictionPolicy, &c.itemPool, c.config.StatsEnabled)
	}
}

func (c *Cache[K, V]) cleanupWorker() {
	ticker := time.NewTicker(c.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.Cleanup()
		case <-c.closeCh:
			return
		}
	}
}

// isClosed reports whether Close has been called.
func (c *Cache[K, V]) isClosed() bool {
	return atomic.LoadInt32(&c.closed) == 1
}

func (c *Cache[K, V]) getShard(key K) *shard[K, V] {
	return c.shardByHash(c.hasher.Sum(key))
}

type getResult[V any] struct {
	value      V
	expireTime int64
	now        int64
	ok         bool
}

func (c *Cache[K, V]) get(key K) getResult[V] {
	if c.isClosed() {
		return getResult[V]{}
	}
	kh := c.hasher.Sum(key)
	shard := c.shardByHash(kh)

	if c.config.EvictionPolicy == SieveTinyLFU && shard.sieve != nil {
		return c.getSieve(key, kh, shard)
	}

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
		return getResult[V]{}
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
					return getResult[V]{now: now}
				}
				// might have been refreshed while upgrading the lock; re-evaluate.
				continue
			}

			c.removeItem(shard, item, RemovedExpired)
			if c.config.StatsEnabled {
				atomic.AddInt64(&shard.expirations, 1)
				atomic.AddInt64(&shard.misses, 1)
			}

			return getResult[V]{now: now}
		}
		break
	}

	switch c.config.EvictionPolicy {
	case LRU:
		shard.moveToLRUHead(item)
	case LFU:
		item.lastAccess = now
		shard.lfuList.increment(item)
	case SieveTinyLFU:
		// only reached for unlimited (cap==0) SieveTinyLFU shards, which have no
		// policy state; resident reads go through getSieve otherwise.
		item.lastAccess = now
	}

	if c.config.StatsEnabled {
		atomic.AddInt64(&shard.hits, 1)
	}

	return getResult[V]{
		value:      item.value,
		expireTime: item.expireTime,
		now:        now,
		ok:         true,
	}
}

func (c *Cache[K, V]) getSieve(key K, kh uint64, shard *shard[K, V]) getResult[V] {
	if !shard.mu.TryRLock() {
		return c.getSieveContended(key, kh, shard)
	}

	item, exists := shard.data[key]
	if !exists {
		warmup := shard.belowSieveWarmupLocked()
		shard.mu.RUnlock()
		if !warmup {
			shard.sampleRead(kh, true)
		}
		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.misses, 1)
		}
		return getResult[V]{}
	}

	if shard.sieve != nil && !shard.belowSieveWarmupLocked() {
		shard.sieve.recordReadHit(item)
	}

	res := getResult[V]{
		value:      item.value,
		expireTime: item.expireTime,
		ok:         true,
	}
	shard.mu.RUnlock()

	// Resolve expiry only after releasing the read lock. time.Now() is a vDSO
	// call which means that holding the shared lock across it lengthens every reader's
	// section, which under load makes TryRLock fail more often and pushes readers
	// onto the exclusive getSieveContended path. RecordReadHit above only sets the visited
	// bit, so recording a read on an entry we then find expired is harmless: the
	// entry is removed (and its bit cleared) below and the adaptive controller is
	// never touched on the read path.
	if res.expireTime > 0 {
		res.now = time.Now().UnixNano()
		if res.now > res.expireTime {
			shard.mu.Lock()
			if cur, ok := shard.data[key]; ok && cur.expireTime > 0 && res.now > cur.expireTime {
				c.removeItem(shard, cur, RemovedExpired)
				if c.config.StatsEnabled {
					atomic.AddInt64(&shard.expirations, 1)
					atomic.AddInt64(&shard.misses, 1)
				}
				shard.mu.Unlock()
				return getResult[V]{now: res.now}
			}
			shard.mu.Unlock()

			return c.getSieve(key, kh, shard)
		}
	}

	if c.config.StatsEnabled {
		atomic.AddInt64(&shard.hits, 1)
	}
	// feed the read into the frequency sketch via the per-shard read buffer so
	// TinyLFU admission reflects read popularity, not just write traffic.
	shard.sampleRead(kh, true)
	return res
}

func (c *Cache[K, V]) getSieveContended(key K, kh uint64, shard *shard[K, V]) getResult[V] {
	shard.mu.Lock()
	defer shard.mu.Unlock()

	item, exists := shard.data[key]
	if !exists {
		if shard.sieve != nil && !shard.belowSieveWarmupLocked() {
			shard.sampleRead(kh, false)
		}
		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.misses, 1)
		}
		return getResult[V]{}
	}

	now := int64(0)
	if item.expireTime > 0 {
		now = time.Now().UnixNano()
		if now > item.expireTime {
			c.removeItem(shard, item, RemovedExpired)
			if c.config.StatsEnabled {
				atomic.AddInt64(&shard.expirations, 1)
				atomic.AddInt64(&shard.misses, 1)
			}
			return getResult[V]{now: now}
		}
	}

	if shard.sieve != nil && !shard.belowSieveWarmupLocked() {
		shard.sieve.recordReadHit(item)
	}

	if c.config.StatsEnabled {
		atomic.AddInt64(&shard.hits, 1)
	}
	shard.sampleRead(kh, false)
	return getResult[V]{
		value:      item.value,
		expireTime: item.expireTime,
		now:        now,
		ok:         true,
	}
}

func (c *Cache[K, V]) shardByHash(hash uint64) *shard[K, V] {
	return c.shards[hash&c.shardMask]
}

func (c *Cache[K, V]) removeItem(s *shard[K, V], item *cacheItem[K, V], reason RemovalReason) {
	mode := dropLRU
	switch c.config.EvictionPolicy {
	case LFU:
		mode = dropLFU
	case SieveTinyLFU:
		mode = dropSieve
	}
	s.dropItem(item, &c.itemPool, c.config.StatsEnabled, reason, mode)
}
