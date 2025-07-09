package cache

import (
	"fmt"
	"hash/maphash"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrCacheClosed = fmt.Errorf("cache is closed")
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

// defines the eviction algorithm
type EvictionPolicy int

const (
	LRU EvictionPolicy = iota
	LFU
	FIFO
	Random
	TinyLFU
)

const (
	// gratio32 is the golden ratio multiplier for 32-bit integers
	// (φ - 1) * 2^32, where φ = (1 + √5) / 2 (golden ratio)
	// This ensures good distribution of consecutive integers across hash space
	gratio32 = 0x9e3779b9

	// gratio64 is the same but for 64-bit integers
	// (φ - 1) * 2^64
	gratio64 = 0x9e3779b97f4a7c15
)

type Config struct {
	MaxSize         int64           // Maximum number of items (0 = unlimited)
	ShardCount      int             // Number of shards (0 = auto-detect)
	CleanupInterval time.Duration   // How often to run cleanup (0 = no cleanup)
	DefaultTTL      time.Duration   // Default TTL for items (0 = no expiration)
	EvictionPolicy  EvictionPolicy  // Eviction algorithm
	StatsEnabled    bool           // Enable statistics collection
}

// DefaultConfig returns a sensible default configuration
func DefaultConfig() Config {
	return Config{
		MaxSize:         10000,
		ShardCount:      0, // Auto-detect
		CleanupInterval: 5 * time.Minute,
		DefaultTTL:      30 * time.Minute,
		EvictionPolicy:  LRU,
		StatsEnabled:    true,
	}
}

type cacheItem[V any] struct {
	value      V
	expireTime int64 // Unix nano timestamp for better performance
	lastAccess int64 // Unix nano timestamp
	frequency  int64
	prev       *cacheItem[V] // For LRU doubly-linked list
	next       *cacheItem[V] // For LRU doubly-linked list
	key        interface{}   // Store key for eviction
}

// shard represents a cache shard to reduce contention
type shard[K comparable, V any] struct {
	mu         sync.RWMutex
	data       map[K]*cacheItem[V]
	head       *cacheItem[V] // LRU head (most recent)
	tail       *cacheItem[V] // LRU tail (least recent)
	size       int64
	hits       int64
	misses     int64
	evictions  int64
	expirations int64
}

type InMemoryCache[K comparable, V any] struct {
	shards      []*shard[K, V]
	shardMask   uint64
	config      Config
	globalStats Stats
	statsMux    sync.RWMutex
	cleanupCh   chan struct{}
	closeCh     chan struct{}
	closeOnce   sync.Once
	closed      int32 // atomic
	itemPool    sync.Pool
	hasher      maphash.Hash // Reuse hasher
	hasherMu    sync.Mutex
}

func New[K comparable, V any](config Config) *InMemoryCache[K, V] {
	// auto-detect optimal shard count
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
			New: func() interface{} {
				return &cacheItem[V]{}
			},
		},
	}

	cache.hasher.SetSeed(maphash.MakeSeed())

	// shards
	for i := 0; i < shardCount; i++ {
		cache.shards[i] = &shard[K, V]{
			data: make(map[K]*cacheItem[V]),
		}
		cache.initLRU(cache.shards[i])
	}

	if config.CleanupInterval > 0 {
		go cache.cleanupWorker()
	}

	return cache
}

// LRU doubly-linked list for a shard
func (c *InMemoryCache[K, V]) initLRU(s *shard[K, V]) {
	s.head = &cacheItem[V]{}
	s.tail = &cacheItem[V]{}
	s.head.next = s.tail
	s.tail.prev = s.head
}

// shard for a given key using fast hash
func (c *InMemoryCache[K, V]) getShard(key K) *shard[K, V] {
	hash := c.hash(key)
	return c.shards[hash&c.shardMask]
}

// The function uses different hashing strategies for each key type:
// - Strings: Uses Go's maphash
// - Integers: Uses multiplicative hashing with golden ratio constants
// - Other types: Converts to string then uses maphash - compatible with all comparable types
//
// Protected by hasherMu mutex since maphash.Hash is not thread-safe
func (c *InMemoryCache[K, V]) hash(key K) uint64 {
	c.hasherMu.Lock()
	defer c.hasherMu.Unlock()

	// reset the hasher to ensure clean state for each hash operation
	c.hasher.Reset()

	switch k := any(key).(type) {
	case string:
		c.hasher.WriteString(k)
	case int:
		// 32-bit integers: Use multiplicative hashing with golden ratio
		return uint64(k) * gratio32
	case int32:
		// same approach as int
		return uint64(k) * gratio32
	case int64:
		// 64-bit integers: Use 64-bit golden ratio multiplier
		return uint64(k) * gratio64
	case uint:
		// unsigned integers: Use 32-bit golden ratio (uint is usually 32-bit)
		return uint64(k) * gratio32
	case uint32:
		// 32-bit unsigned integers: Same as uint
		return uint64(k) * gratio32

	case uint64:
		// 64-bit unsigned integers: Use 64-bit golden ratio multiplier
		return k * gratio64

	default:
		// All other comparable types: Convert to string representation
		// This works for any comparable type (structs, arrays, etc.)
		c.hasher.WriteString(fmt.Sprintf("%v", k))
	}

	return c.hasher.Sum64()
}

func NewWithDefaults[K comparable, V any]() *InMemoryCache[K, V] {
	return New[K, V](DefaultConfig())
}

// stores a value in the cache with the specified TTL
func (c *InMemoryCache[K, V]) Set(key K, value V, ttl time.Duration) error {
	if atomic.LoadInt32(&c.closed) == 1 {
		return ErrCacheClosed
	}

	if ttl == 0 {
		ttl = c.config.DefaultTTL
	}

	shard := c.getShard(key)
	now := time.Now().UnixNano()

	// get item from pool to reduce allocations
	item := c.itemPool.Get().(*cacheItem[V])
	item.value = value
	item.lastAccess = now
	item.frequency = 1
	item.key = key

	if ttl > 0 {
		item.expireTime = now + ttl.Nanoseconds()
	} else {
		item.expireTime = 0
	}

	shard.mu.Lock()
	defer shard.mu.Unlock()

	if existing, exists := shard.data[key]; exists {
		c.removeFromLRU(shard, existing)
		c.itemPool.Put(existing)
		atomic.AddInt64(&shard.size, -1)
	}

	// check if we need to evict
	maxShardSize := c.config.MaxSize / int64(len(c.shards))
	if c.config.MaxSize > 0 && maxShardSize > 0 && atomic.LoadInt64(&shard.size) >= maxShardSize {
		c.evictItem(shard)
	}

	shard.data[key] = item
	c.addToLRUHead(shard, item)
	atomic.AddInt64(&shard.size, 1)

	return nil
}

// get retrieves a value from the cache
func (c *InMemoryCache[K, V]) Get(key K) (V, bool) {
	var zero V

	if atomic.LoadInt32(&c.closed) == 1 {
		return zero, false
	}

	shard := c.getShard(key)
	now := time.Now().UnixNano()

	shard.mu.Lock()
	defer shard.mu.Unlock()

	item, exists := shard.data[key]
	if !exists {
		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.misses, 1)
		}
		return zero, false
	}

	if item.expireTime > 0 && now > item.expireTime {
		delete(shard.data, key)
		c.removeFromLRU(shard, item)
		c.itemPool.Put(item)
		atomic.AddInt64(&shard.size, -1)
		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.expirations, 1)
			atomic.AddInt64(&shard.misses, 1)
		}
		return zero, false
	}

	item.lastAccess = now
	atomic.AddInt64(&item.frequency, 1)
	value := item.value

	// move to head of LRU (already have write lock)
	c.moveToLRUHead(shard, item)

	if c.config.StatsEnabled {
		atomic.AddInt64(&shard.hits, 1)
	}
	return value, true
}

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
	c.itemPool.Put(item)
	atomic.AddInt64(&shard.size, -1)
	return true
}

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
		atomic.StoreInt64(&shard.size, 0)
		shard.mu.Unlock()
	}
}

func (c *InMemoryCache[K, V]) Size() int64 {
	var totalSize int64
	for _, shard := range c.shards {
		totalSize += atomic.LoadInt64(&shard.size)
	}
	return totalSize
}

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

		// calculate hit ratio
		total := stats.Hits + stats.Misses
		if total > 0 {
			stats.HitRatio = float64(stats.Hits) / float64(total)
		}
	}

	return stats
}

func (c *InMemoryCache[K, V]) Close() error {
	var err error
	c.closeOnce.Do(func() {
		atomic.StoreInt32(&c.closed, 1)
		close(c.closeCh)
		c.Clear()
	})
	return err
}

func (c *InMemoryCache[K, V]) addToLRUHead(s *shard[K, V], item *cacheItem[V]) {
	if s.head == nil || s.head.next == nil {
		return // Safety check
	}
	item.next = s.head.next
	item.prev = s.head
	if s.head.next != nil {
		s.head.next.prev = item
	}
	s.head.next = item
}

func (c *InMemoryCache[K, V]) removeFromLRU(s *shard[K, V], item *cacheItem[V]) {
	if item.prev != nil {
		item.prev.next = item.next
	}
	if item.next != nil {
		item.next.prev = item.prev
	}
	item.prev = nil
	item.next = nil
}

func (c *InMemoryCache[K, V]) moveToLRUHead(s *shard[K, V], item *cacheItem[V]) {
	c.removeFromLRU(s, item)
	c.addToLRUHead(s, item)
}

func (c *InMemoryCache[K, V]) evictItem(s *shard[K, V]) {
	switch c.config.EvictionPolicy {
	case LRU:
		c.evictLRU(s)
	case LFU:
		c.evictLFU(s)
	case FIFO:
		c.evictFIFO(s)
	case Random:
		c.evictRandom(s)
	case TinyLFU:
		c.evictTinyLFU(s)
	default:
		c.evictLRU(s) // Default to LRU
	}
}

func (c *InMemoryCache[K, V]) evictLRU(s *shard[K, V]) {
	if s.tail.prev == s.head {
		return // No items to evict
	}

	// get the least recently used item (tail.prev)
	lru := s.tail.prev
	if lru != nil && lru.key != nil {
		if key, ok := lru.key.(K); ok {
			delete(s.data, key)
		}
		c.removeFromLRU(s, lru)
		c.itemPool.Put(lru)
		atomic.AddInt64(&s.size, -1)
		if c.config.StatsEnabled {
			atomic.AddInt64(&s.evictions, 1)
		}
	}
}

func (c *InMemoryCache[K, V]) evictLFU(s *shard[K, V]) {
	var lfu *cacheItem[V]
	var lfuKey K
	var minFreq int64 = -1

	for key, item := range s.data {
		freq := atomic.LoadInt64(&item.frequency)
		if minFreq == -1 || freq < minFreq {
			minFreq = freq
			lfu = item
			lfuKey = key
		}
	}

	if lfu != nil {
		delete(s.data, lfuKey)
		c.removeFromLRU(s, lfu)
		c.itemPool.Put(lfu)
		atomic.AddInt64(&s.size, -1)
		if c.config.StatsEnabled {
			atomic.AddInt64(&s.evictions, 1)
		}
	}
}

func (c *InMemoryCache[K, V]) evictFIFO(s *shard[K, V]) {
	// in our LRU implementation, FIFO is the tail item
	c.evictLRU(s)
}

func (c *InMemoryCache[K, V]) evictRandom(s *shard[K, V]) {
	if len(s.data) == 0 {
		return
	}

	// get a random key (not cryptographically secure)
    // @todo: should be?
	var randomKey K
	count := 0
	target := int(time.Now().UnixNano()) % len(s.data)

	for key := range s.data {
		if count == target {
			randomKey = key
			break
		}
		count++
	}

	if item, exists := s.data[randomKey]; exists {
		delete(s.data, randomKey)
		c.removeFromLRU(s, item)
		c.itemPool.Put(item)
		atomic.AddInt64(&s.size, -1)
		if c.config.StatsEnabled {
			atomic.AddInt64(&s.evictions, 1)
		}
	}
}

func (c *InMemoryCache[K, V]) evictTinyLFU(s *shard[K, V]) {
	// for now, implement as LFU (can be enhanced with bloom filters later)
	c.evictLFU(s)
}

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

func (c *InMemoryCache[K, V]) cleanup() {
	if atomic.LoadInt32(&c.closed) == 1 {
		return
	}

	now := time.Now().UnixNano()
	for _, shard := range c.shards {
		c.cleanupShard(shard, now)
	}
}

func (c *InMemoryCache[K, V]) cleanupShard(s *shard[K, V], now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var keysToDelete []K
	for key, item := range s.data {
		if item.expireTime > 0 && now > item.expireTime {
			keysToDelete = append(keysToDelete, key)
		}
	}

	for _, key := range keysToDelete {
		if item, exists := s.data[key]; exists {
			delete(s.data, key)
			c.removeFromLRU(s, item)
			c.itemPool.Put(item)
			atomic.AddInt64(&s.size, -1)
			if c.config.StatsEnabled {
				atomic.AddInt64(&s.expirations, 1)
			}
		}
	}
}

func (c *InMemoryCache[K, V]) TriggerCleanup() {
	if atomic.LoadInt32(&c.closed) == 1 {
		return
	}

	select {
	case c.cleanupCh <- struct{}{}:
	default:
		// channel is full, cleanup already scheduled
	}
}

func (c *InMemoryCache[K, V]) GetWithTTL(key K) (V, time.Duration, bool) {
	var zero V

	if atomic.LoadInt32(&c.closed) == 1 {
		return zero, 0, false
	}

	shard := c.getShard(key)
	now := time.Now().UnixNano()

	shard.mu.Lock()
	defer shard.mu.Unlock()

	item, exists := shard.data[key]
	if !exists {
		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.misses, 1)
		}
		return zero, 0, false
	}

	if item.expireTime > 0 && now > item.expireTime {
		delete(shard.data, key)
		c.removeFromLRU(shard, item)
		c.itemPool.Put(item)
		atomic.AddInt64(&shard.size, -1)
		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.expirations, 1)
			atomic.AddInt64(&shard.misses, 1)
		}
		return zero, 0, false
	}

	item.lastAccess = now
	atomic.AddInt64(&item.frequency, 1)
	value := item.value

	var ttl time.Duration
	if item.expireTime > 0 {
		ttl = time.Duration(item.expireTime - now)
	}

	// move to head of LRU (already have write lock)
	c.moveToLRUHead(shard, item)

	if c.config.StatsEnabled {
		atomic.AddInt64(&shard.hits, 1)
	}
	return value, ttl, true
}

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
				// check if item still exists and hasn't been renewed
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
		c.removeFromLRU(shard, item)
		c.itemPool.Put(item)
		atomic.AddInt64(&shard.size, -1)
		if c.config.StatsEnabled {
			atomic.AddInt64(&shard.expirations, 1)
		}
		return false
	}

	return true
}

func (c *InMemoryCache[K, V]) Keys() []K {
	if atomic.LoadInt32(&c.closed) == 1 {
		return nil
	}

	var keys []K
	now := time.Now().UnixNano()

	for _, shard := range c.shards {
		shard.mu.RLock()
		for key, item := range shard.data {
			// Skip expired items
			if item.expireTime > 0 && now > item.expireTime {
				continue
			}
			keys = append(keys, key)
		}
		shard.mu.RUnlock()
	}

	return keys
}

// nextPowerOf2 returns the next power of 2 >= n
func nextPowerOf2(n int) int {
	if n <= 1 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	return n + 1
}
