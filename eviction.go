package cache

import (
	"container/heap"
	"sync/atomic"
	"time"
)

// EvictionPolicy defines the eviction algorithm
type EvictionPolicy int

const (
	LRU EvictionPolicy = iota
	LFU
	FIFO
	Random
	TinyLFU
)

// evictItem orchestrates cache eviction using a policy dispatch
// Routes to specific eviction algorithm based on c.config.EvictionPolicy
//
// Supported eviction policies:
// - LRU (Least Recently Used): Optimal for temporal locality patterns
// - LFU (Least Frequently Used): Optimal for frequency-based access patterns
// - FIFO (First In First Out): Insertion-order eviction
// - Random: Pseudo-random selection for uniform distribution
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
		c.evictLRU(s)
	}
}

// evictLRU removes the least recently used item
// Empty cache check: if tail.prev == head, no actual items exist (only sentinels)
//
// Implements LRU eviction algorithm with O(1)
//
// - Least recently used item is always at tail.prev
// - Sentinel nodes (head/tail) enable direct access without list traversal
// - Most recent items are near head, least recent items are near tail
//
// Eviction process:
// 1. Identify LRU item at tail.prev position
// 2. Extract and type-assert the stored key for map deletion
// 3. Remove item from hash map (breaks key -> item association)
// 4. Remove item from LRU (updates neighbors)
// 5. Return item to object pool
// 6. Update counters for size and eviction statistics
//
// - O(1) due to direct tail access
// - O(1) space
// - Optimal for temporal locality access patterns
func (c *InMemoryCache[K, V]) evictLRU(s *shard[K, V]) {
	if s.tail.prev == s.head {
		return
	}

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

// evictLFU removes the least frequently used item using heap-based priority queue
//
// Implements LFU eviction algorithm with O(log n) using min-heap:
//
// - Maintains min-heap where root always contains least frequently used item
// - Heap ordered by frequency (primary) and last access time (secondary for tie-breaking)
// - heap.Pop() extracts minimum element in O(log n) time
// - Automatically rebalances after removal to maintain heap property
//
// Frequency tracking mechanism:
// - Each cache item maintains atomic frequency counter
// - Incremented on every Get() operation for accurate usage tracking
// - Heap uses frequency as primary comparison key in Less() method
// - Tie-breaking uses lastAccess timestamp for deterministic behavior
//
// 1. Heap existence: validates s.lfuHeap != nil (policy might be changed dynamically)
// 2. Non-empty heap: checks s.lfuHeap.Len() > 0 before attempting Pop()
//
// Eviction process:
// 1. Extract least frequently used item from heap root via Pop()
// 2. Remove item from map
// 3. Remove item from LRU
// 4. Return item to object pool
// 5. Update counters
//
// - O(log n) due to heap operations
// - Better than O(n) linear scan for frequency-based eviction
// - Optimal for scenarios with clear frequency patterns
// - Higher memory overhead due to heap maintenance
func (c *InMemoryCache[K, V]) evictLFU(s *shard[K, V]) {
	if s.lfuHeap == nil || s.lfuHeap.Len() == 0 {
		return
	}

	lfu := heap.Pop(s.lfuHeap).(*cacheItem[V])
	if lfu.key != nil {
		if key, ok := lfu.key.(K); ok {
			delete(s.data, key)
		}
		c.removeFromLRU(s, lfu)
		c.itemPool.Put(lfu)
		atomic.AddInt64(&s.size, -1)
		if c.config.StatsEnabled {
			atomic.AddInt64(&s.evictions, 1)
		}
	}
}

// evictFIFO removes the first item that was inserted (same as LRU tail)
func (c *InMemoryCache[K, V]) evictFIFO(s *shard[K, V]) {
	c.evictLRU(s)
}

// evictRandom removes a pseudo-randomly selected item
//
// Simple random eviction using time-based pseudo-randomness:
//
// - Collects all cache keys into a slice for random access
// - Uses time.Now().UnixNano() as entropy source for index selection
// - Modulo operation maps nanosecond timestamp to valid array index
//
// Known limitations as for version 0.0.4:
// - Not cryptographically secure randomness
// - Potential for patterns due to time-based entropy
// - Could benefit from proper PRNG (e.g., math/rand) in future iterations
//
// Two-phase collection process:
// 1. Key collection: iterates through s.data map to gather all keys
// 2. Random selection: applies pseudo-random index to select victim
//
// Eviction process:
// 1. Collect all keys from hash map into slice
// 2. Generate pseudo-random index using nanosecond timestamp
// 3. Select key at random index and verify item still exists
// 4. Remove from map, LRU list, LFU heap (if applicable)
// 5. Return item to object pool and update statistics
//
// - O(n) due to key collection phase
// - O(n) space overhead for temporary key slice
func (c *InMemoryCache[K, V]) evictRandom(s *shard[K, V]) {
	if len(s.data) == 0 {
		return
	}

	keys := make([]K, 0, len(s.data))
	for key := range s.data {
		keys = append(keys, key)
	}

	if len(keys) == 0 {
		return
	}

	randomIndex := int(time.Now().UnixNano()) % len(keys)
	randomKey := keys[randomIndex]
	if item, exists := s.data[randomKey]; exists {
		delete(s.data, randomKey)
		c.removeFromLRU(s, item)
		if c.config.EvictionPolicy == LFU && item.heapIndex != -1 {
			heap.Remove(s.lfuHeap, item.heapIndex)
		}
		c.itemPool.Put(item)
		atomic.AddInt64(&s.size, -1)
		if c.config.StatsEnabled {
			atomic.AddInt64(&s.evictions, 1)
		}
	}
}

func (c *InMemoryCache[K, V]) evictTinyLFU(s *shard[K, V]) {
	// evictTinyLFU implements simplified TinyLFU (currently delegates to LFU)
	// todo: can be enhanced with bloom filters
	c.evictLFU(s)
}
