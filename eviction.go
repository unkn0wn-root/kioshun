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

// evictItem removes an item based on the configured eviction policy
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
func (c *InMemoryCache[K, V]) evictLRU(s *shard[K, V]) {
	if s.tail.prev == s.head {
		return // No items to evict
	}

	// Get the least recently used item (tail.prev)
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

// evictLFU removes the least frequently used item using O(log n) heap op
func (c *InMemoryCache[K, V]) evictLFU(s *shard[K, V]) {
	if s.lfuHeap == nil || s.lfuHeap.Len() == 0 {
		return
	}

	// Get the least frequently used item from heap root
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

// evictRandom removes a randomly selected item
func (c *InMemoryCache[K, V]) evictRandom(s *shard[K, V]) {
	if len(s.data) == 0 {
		return
	}

	// Collect all keys for random selection
	// @todo: I know it isn't true randomness
	// and I should definitly use something else but it covers 99%
	// so i should maybe revisit it some day
	keys := make([]K, 0, len(s.data))
	for key := range s.data {
		keys = append(keys, key)
	}

	if len(keys) == 0 {
		return
	}

	// Use nanosecond time for pseudo-randomness
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
