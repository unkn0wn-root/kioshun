package cache

import (
	"container/heap"
	"sync"
	"sync/atomic"
	"time"
)

// evictor defines the interface for cache eviction policies.
// Select which item to remove when the cache reaches capacity.
type evictor[K comparable, V any] interface {
	evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool
}

// lruEvictor implements Least Recently Used eviction policy.
type lruEvictor[K comparable, V any] struct{}

// evict removes the least recently used item from the shard.
// The LRU item is always at tail.prev in the 2-linked list.
func (e lruEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool {
	// Check if shard is empty (only sentinel nodes remain)
	if s.tail.prev == s.head {
		return false
	}

	// Get the LRU item (at tail of list)
	lru := s.tail.prev
	if lru != nil && lru.key != nil {
		if key, ok := lru.key.(K); ok {
			delete(s.data, key)
		}
		s.removeFromLRU(lru)

		itemPool.Put(lru)

		atomic.AddInt64(&s.size, -1)
		if statsEnabled {
			atomic.AddInt64(&s.evictions, 1)
		}
		return true
	}
	return false
}

// lfuEvictor implements Least Frequently Used eviction policy.
type lfuEvictor[K comparable, V any] struct{}

// evict removes the least frequently used item from the shard.
// The LFU item is always at the root of the min-heap.
func (e lfuEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool {
	// Check if heap is empty
	if s.lfuHeap == nil || s.lfuHeap.Len() == 0 {
		return false
	}

	// Remove the LFU item from heap
	lfu := heap.Pop(s.lfuHeap).(*cacheItem[V])
	if lfu.key != nil {
		if key, ok := lfu.key.(K); ok {
			delete(s.data, key)
		}

		s.removeFromLRU(lfu)
		itemPool.Put(lfu)
		atomic.AddInt64(&s.size, -1)

		if statsEnabled {
			atomic.AddInt64(&s.evictions, 1)
		}
		return true
	}
	return false
}

// fifoEvictor implements First In, First Out eviction policy.
type fifoEvictor[K comparable, V any] struct{}

// evict removes the first inserted item from the shard.
func (e fifoEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool {
	// Check if shard is empty (only sentinel nodes remain)
	if s.tail.prev == s.head {
		return false
	}

	// Get the oldest item (at tail of list)
	oldest := s.tail.prev
	if oldest != nil && oldest.key != nil {
		if key, ok := oldest.key.(K); ok {
			delete(s.data, key)
		}
		s.removeFromLRU(oldest)

		// Clean up LFU heap if present
		if s.lfuHeap != nil && oldest.heapIndex != noHeapIndex {
			heap.Remove(s.lfuHeap, oldest.heapIndex)
		}

		itemPool.Put(oldest)
		atomic.AddInt64(&s.size, -1)
		if statsEnabled {
			atomic.AddInt64(&s.evictions, 1)
		}
		return true
	}
	return false
}

// randomEvictor implements Random eviction policy.
type randomEvictor[K comparable, V any] struct{}

// evict removes a randomly selected item from the shard.
// Uses current time as a pseudo-random seed for key selection.
func (e randomEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool {
	if len(s.data) == 0 {
		return false
	}

	// Collect all keys for random selection
	keys := make([]K, 0, len(s.data))
	for key := range s.data {
		keys = append(keys, key)
	}

	if len(keys) == 0 {
		return false
	}

	// Select random key using time as seed
	randomIndex := int(time.Now().UnixNano()) % len(keys)
	randomKey := keys[randomIndex]
	if item, exists := s.data[randomKey]; exists {
		delete(s.data, randomKey)
		s.removeFromLRU(item)

		if s.lfuHeap != nil && item.heapIndex != noHeapIndex {
			heap.Remove(s.lfuHeap, item.heapIndex)
		}

		itemPool.Put(item)
		atomic.AddInt64(&s.size, -1)
		if statsEnabled {
			atomic.AddInt64(&s.evictions, 1)
		}
		return true
	}
	return false
}

// createEvictor returns an evictor implementation based on the specified policy.
func createEvictor[K comparable, V any](policy EvictionPolicy) evictor[K, V] {
	switch policy {
	case LRU:
		return lruEvictor[K, V]{}
	case LFU:
		return lfuEvictor[K, V]{}
	case FIFO:
		return fifoEvictor[K, V]{}
	case Random:
		return randomEvictor[K, V]{}
	default:
		return lruEvictor[K, V]{} // Safe default
	}
}
