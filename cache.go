package kioshun

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unkn0wn-root/kioshun/internal/keyhash"
	"github.com/unkn0wn-root/kioshun/internal/mathx"
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

// Stats is approximate telemetry aggregated across shards.
type Stats struct {
	Hits        int64   // lookups that found a live entry
	Misses      int64   // lookups that found nothing or an expired entry
	Evictions   int64   // entries removed to stay within capacity
	Expirations int64   // entries removed because their TTL elapsed
	Size        int64   // resident item count
	Cost        int64   // resident weighted cost (equals Size when unweighted)
	Capacity    int64   // configured MaxSize; 0 => unlimited
	MaxCost     int64   // configured MaxCost; 0 => disabled
	HitRatio    float64 // Hits/(Hits + Misses); 0 when no lookups
	Shards      int     // shard count
}

// PolicyStats exposes SieveTinyLFU admission and replacement decisions.
type PolicyStats struct {
	Admits             int64 // candidates admitted into the cache
	Rejects            int64 // candidates TinyLFU declined to admit
	GhostHits          int64 // inserts whose key was a recent eviction (B1 ghost hit)
	Promotions         int64 // probation entries promoted to the main (protected) queue
	ProbationEvictions int64 // entries evicted from the probation queue
	MainEvictions      int64 // entries evicted from the main queue
}

// Cache is a sharded in-memory cache with per-policy metadata. SieveTinyLFU reads
// are lock-free; writers (and other policies' reads) are serialized per shard.
type Cache[K comparable, V any] struct {
	shards       []*shard[K, V] // tables + lists + counters
	shardMask    uint64         // shards is 2^n; mask = shards-1
	config       Config
	perShardCap  int64
	perShardCost int64
	clockBase    time.Time
	closeCh      chan struct{}
	closeOnce    sync.Once
	closed       atomic.Bool
	workers      sync.WaitGroup
	waiterPool   sync.Pool
	hasher       keyhash.Hasher[K]
	weigher      Weigher[K, V]
	trackCost    bool
	evictor      evictor[K, V] // nil for SieveTinyLFU
	onRemove     func(K, V, RemovalReason)
	onEvict      func(K, V)
	removeWake   chan struct{}

	stats *stats // per-P; nil unless StatsEnabled
}

type Option[K comparable, V any] func(*Cache[K, V])

// Weigher reports the relative capacity cost for a cache entry. The default
// cost is 1, preserving entry count when MaxCost is unset.
type Weigher[K comparable, V any] func(K, V) int64

// WithWeigher configures typed item weights for MaxCost enforcement and
// cost-aware SieveTinyLFU admission. A nil weigher is ignored.
func WithWeigher[K comparable, V any](weigher Weigher[K, V]) Option[K, V] {
	return func(c *Cache[K, V]) {
		if weigher != nil {
			c.weigher = weigher
			c.trackCost = true
		}
	}
}

// New constructs a Cache from config, returning an error if config is invalid.
// Shard count is normalized to 2^n (and bounded by MaxSize); background
// workers start immediately so callers must Close the cache to release them.
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
		shardCount = runtime.NumCPU() * shardMultiplier
		shardCount = min(shardCount, maxShardCount)
	}

	// bound shards by capacity so tiny MaxSize/MaxCost values do not create empty shards.
	if config.MaxSize > 0 {
		shardCount = min(shardCount, int(mathx.PrevPowerOf2(min(config.MaxSize, int64(maxShardCount)))))
	}
	if config.MaxCost > 0 {
		shardCount = min(shardCount, int(mathx.PrevPowerOf2(min(config.MaxCost, int64(maxShardCount)))))
	}
	shardCount = mathx.NextPowerOf2(shardCount)

	cache := &Cache[K, V]{
		shards:    make([]*shard[K, V], shardCount),
		shardMask: uint64(shardCount - 1),
		config:    config,
		clockBase: time.Now(),
		closeCh:   make(chan struct{}),
		trackCost: config.MaxCost > 0 || config.CostAdmission != CostAdmissionFrequency,
		waiterPool: sync.Pool{
			New: func() any { return &writeWaiter{ch: make(chan struct{}, 1)} },
		},
	}

	// precompute the base per-shard capacity. Individual shards receive the
	// remainder below so the aggregate capacity is exactly MaxSize.
	if config.MaxSize > 0 {
		cache.perShardCap = config.MaxSize / int64(shardCount)
	}
	if config.MaxCost > 0 {
		cache.perShardCost = config.MaxCost / int64(shardCount)
	}

	cache.hasher = keyhash.New[K]()
	if config.StatsEnabled {
		cache.stats = newStats(runtime.GOMAXPROCS(0))
	}
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
		var costCap int64
		if config.MaxCost > 0 {
			costCap = cache.perShardCost
			if int64(i) < config.MaxCost%int64(shardCount) {
				costCap++
			}
		}
		s := &shard[K, V]{
			tab:        newHtable[K, V](int(sc)),
			cap:        sc,
			costCap:    costCap,
			stats:      cache.stats,
			wake:       make(chan struct{}, 1),
			writeBatch: make([]writeCommand[K, V], config.WriteBatchSize),
		}
		s.queue = newMPSCQueue[K, V](config.WriteBufferSize, s.wake, cache.closeCh)

		if config.EvictionPolicy == LFU {
			s.lfuList = newLFUList[K, V]()
		}
		// Config.Validate rejects SieveTinyLFU with a cost budget but no MaxSize so
		// a bounded Sieve shard always has s.cap > 0 to size its policy from; an
		// unbounded cache (no MaxSize, no MaxCost) keeps s.sieve nil and never evicts.
		if config.EvictionPolicy == SieveTinyLFU && s.cap > 0 {
			// shard index is the queue owner tag; shardCount <= maxShardCount (256)
			// so it fits a byte and is unique per shard.
			s.sieve = newSieveTinyLFU[K, V](
				s.cap,
				uint8(i),
				config.ProbationRatio,
				config.GhostRatio,
				config.CostAdmission,
			)
			s.readBuf = newReadBuffer() // per-shard read sampling for the sketch
		}
		// Only policies backed by the shared LRU list need its sentinels; bounded
		// SieveTinyLFU keeps residents in its own queues, so it skips them.
		if s.sieve == nil {
			s.initLRU()
		}
		cache.shards[i] = s
	}

	for _, opt := range opts {
		opt(cache)
	}

	nm := cache.listenerNotifyMask()
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

// NewDefault constructs a Cache with DefaultConfig. It panics only if the
// built-in default config is invalid (your fault).
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

// SetAsync accepts an insert/update command for key with TTL. A nil error means
// the command was accepted. When the owning shard is uncontended the write is
// applied inline before returning (immediate visibility, no async handoff);
// otherwise it is queued without blocking and Sync gives committed visibility.
func (c *Cache[K, V]) SetAsync(key K, value V, ttl time.Duration) error {
	return c.set(key, value, ttl, nil)
}

// SetWithCallback sets key and schedules the callback after the item is committed.
// The callback fires once the TTL elapses and re-validates the item under lock.
func (c *Cache[K, V]) SetWithCallback(key K, value V, ttl time.Duration, callback func(K, V)) error {
	return c.setAndWait(key, value, ttl, callback)
}

// Delete removes key if present, reporting whether it was.
func (c *Cache[K, V]) Delete(key K) bool {
	kh := c.hasher.Sum(key)
	deleted, err := c.deleteSync(c.shardByHash(kh), kh, key)
	if err != nil {
		return false
	}
	return deleted
}

// Clear empties all shards and resets policy state.
func (c *Cache[K, V]) Clear() {
	_ = c.enqueueAllAndWait(writeClear)
}

// Exists checks membership without mutating recency/frequency (removes if expired).
func (c *Cache[K, V]) Exists(key K) bool {
	if c.isClosed() {
		return false
	}

	kh := c.hasher.Sum(key)
	shard := c.shardByHash(kh)
	now := c.nowNano()

	shard.mu.Lock()
	defer shard.mu.Unlock()

	item, exists := shard.tab.lookup(kh, key)
	if !exists {
		return false
	}

	if item.expireTime > 0 && now > item.expireTime {
		c.removeItem(shard, item, RemovedExpired)
		if c.config.StatsEnabled {
			c.stats.recordExpiration()
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
	now := c.nowNano()

	for _, shard := range c.shards {
		shard.mu.RLock()
		shard.tab.forEach(func(item *cacheItem[K, V]) bool {
			if item.expireTime == 0 || now <= item.expireTime {
				keys = append(keys, item.key)
			}
			return true
		})
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

// Cost sums resident item weights across shards (O(shards)).
func (c *Cache[K, V]) Cost() int64 {
	if !c.trackCost {
		return c.Size()
	}
	var totalCost int64
	for _, shard := range c.shards {
		totalCost += atomic.LoadInt64(&shard.cost)
	}
	return totalCost
}

// Stats aggregates counters and computes hit ratio.
func (c *Cache[K, V]) Stats() Stats {
	var stats Stats
	stats.Size = c.Size()
	if c.trackCost {
		stats.Cost = c.Cost()
	} else {
		stats.Cost = stats.Size
	}
	stats.Capacity = c.config.MaxSize
	stats.MaxCost = c.config.MaxCost
	stats.Shards = len(c.shards)

	if c.config.StatsEnabled {
		stats.Hits, stats.Misses, stats.Evictions, stats.Expirations = c.stats.aggregate()
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
		c.closed.Store(true)
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

	now := c.nowNano()
	for _, shard := range c.shards {
		shard.cleanup(now, c.config.EvictionPolicy, c.config.StatsEnabled)
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
	return c.closed.Load()
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

	// bounded SieveTinyLFU reads are lock-free via getSieve. An unbounded (cap==0)
	// sieve has no policy state (shard.sieve == nil) and never evicts, so it falls
	// through to the lock path below with no per-read update - the same as FIFO.
	if shard.sieve != nil {
		return c.getSieve(key, kh, shard)
	}

	needsWriteLock := c.config.EvictionPolicy == LRU || c.config.EvictionPolicy == LFU
	shardLockedWrite := false
	if needsWriteLock {
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

	item, exists := shard.tab.lookup(kh, key)
	if !exists {
		if c.config.StatsEnabled {
			c.stats.recordMiss()
		}
		return getResult[V]{}
	}

	var now int64
	for {
		now = c.nowNano()
		if item.expireTime > 0 && now > item.expireTime {
			if !shardLockedWrite {
				shard.mu.RUnlock()
				shard.mu.Lock()
				shardLockedWrite = true

				item, exists = shard.tab.lookup(kh, key)
				if !exists {
					if c.config.StatsEnabled {
						c.stats.recordMiss()
					}
					return getResult[V]{now: now}
				}
				// might have been refreshed while upgrading the lock; re-evaluate.
				continue
			}

			c.removeItem(shard, item, RemovedExpired)
			if c.config.StatsEnabled {
				c.stats.recordExpiration()
				c.stats.recordMiss()
			}

			return getResult[V]{now: now}
		}
		break
	}

	switch c.config.EvictionPolicy {
	case LRU:
		shard.moveToLRUHead(item)
	case LFU:
		shard.lfuList.increment(item)
	}

	if c.config.StatsEnabled {
		c.stats.recordHit(stripeID())
	}

	return getResult[V]{
		value:      item.value,
		expireTime: item.expireTime,
		now:        now,
		ok:         true,
	}
}

// getSieve is the lock-free SieveTinyLFU read path: it probes the table without a
// lock, and a hit's only shared write is the visited bit. Item fields are
// immutable after publication, so a reader racing an eviction still gets a
// consistent snapshot - the item lives until both reader and GC are done.
func (c *Cache[K, V]) getSieve(key K, kh uint64, shard *shard[K, V]) getResult[V] {
	item, exists := shard.tab.lookup(kh, key)

	// during warmup admission is unconditional, so the sketch is never consulted;
	// skip the visited-bit update and read sampling so a working set that fits under
	// capacity (never leaving warmup) pays no sketch-feeding cost on reads.
	warmup := shard.belowSieveWarmup()

	if !exists {
		// a read never waits for the writer, so a miss may be a Set still queued for
		// this shard. Drain and re-check before declaring a miss, so a Get racing a
		// Set of the same key sees it without making writes synchronous. The miss is
		// not sampled: if it becomes a Set, recordAccess counts it at insert.
		if it, ok := c.drainMissAndLookup(shard, kh, key); ok {
			item = it
			warmup = shard.belowSieveWarmup()
		} else {
			if c.config.StatsEnabled {
				c.stats.recordMiss()
			}
			return getResult[V]{}
		}
	}

	if !warmup {
		shard.sieve.recordReadHit(item)
	}

	res := getResult[V]{
		value:      item.value,
		expireTime: item.expireTime,
		ok:         true,
	}

	// resolve expiry off the hot path; only an expired hit takes the write lock to
	// remove the entry. recordReadHit above only set the visited bit, so recording
	// a read on an entry we then find expired is harmless.
	if res.expireTime > 0 {
		res.now = c.nowNano()
		if res.now > res.expireTime {
			shard.mu.Lock()
			if cur, ok := shard.tab.lookup(kh, key); ok && cur.expireTime > 0 && res.now > cur.expireTime {
				c.removeItem(shard, cur, RemovedExpired)
				if c.config.StatsEnabled {
					c.stats.recordExpiration()
					c.stats.recordMiss()
				}
				shard.mu.Unlock()
				return getResult[V]{now: res.now}
			}
			shard.mu.Unlock()

			return c.getSieve(key, kh, shard)
		}
	}

	// fetch the stripe id once; the hit counter and the read sample share it.
	if c.config.StatsEnabled || !warmup {
		id := stripeID()
		if c.config.StatsEnabled {
			c.stats.recordHit(id)
		}
		// feed the read into the frequency sketch via the per-shard read buffer
		// so TinyLFU admission reflects read popularity, not just write traffic.
		if !warmup {
			shard.sampleRead(kh, id)
		}
	}
	return res
}

func (c *Cache[K, V]) shardByHash(hash uint64) *shard[K, V] {
	return c.shards[hash&c.shardMask]
}

// nowNano is the cache's clock: monotonic nanoseconds since clockBase. Item expiry
// is stamped and compared in this domain, so TTLs ignore wall-clock jumps (NTP
// steps, manual changes).
func (c *Cache[K, V]) nowNano() int64 {
	return time.Since(c.clockBase).Nanoseconds()
}

func (c *Cache[K, V]) removeItem(s *shard[K, V], item *cacheItem[K, V], reason RemovalReason) {
	s.dropItem(item, c.config.StatsEnabled, reason, dropModeFor(c.config.EvictionPolicy))
}
