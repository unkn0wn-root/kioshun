package cache

import (
	"container/heap"
	"sync"
	"sync/atomic"
	"time"
)

// evictor defines the interface for cache eviction policies
//
// Each eviction policy implements a strategy for selecting which item
// to remove when the cache reaches capacity. The interface provides
// a unified way to handle different eviction algorithms.
//
// Parameters:
// - s: the shard to evict from
// - itemPool: object pool for returning evicted items
// - statsEnabled: whether to update eviction statistics
//
// Returns:
// - bool: true if an item was evicted, false if shard was empty
type evictor[K comparable, V any] interface {
	evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool
}

// lruEvictor implements Least Recently Used eviction policy
//
// LRU evicts the item that was accessed (read or written) least recently.
// Always evict the item at the tail of the LRU list
type lruEvictor[K comparable, V any] struct{}

// evict removes the least recently used item from the shard
//
// LRU eviction process:
// 1. Check if shard is empty (tail.prev == head)
// 2. Get the LRU item (item at tail.prev)
// 3. Remove item from all data structures:
//   - Hash map (for key lookup)
//   - LRU linked list (for access ordering)
//
// 4. Return item to object pool
// 5. Update size and statistics
//
// The LRU item is always at tail.prev because:
// - New/accessed items are added to head
// - Older items naturally move toward tail
// - tail.prev is the oldest accessed item
func (e lruEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool {
	// Check if shard is empty (only sentinel nodes remain)
	if s.tail.prev == s.head {
		return false
	}

	// Get the least recently used item (at tail)
	lru := s.tail.prev
	if lru != nil && lru.key != nil {
		if key, ok := lru.key.(K); ok {
			delete(s.data, key)
		}
		s.removeFromLRU(lru)

		itemPool.Put(lru)

		atomic.AddInt64(&s.size, -1)
		if statsEnabled {
			atomic.AddInt64(&s.evictions, 1) // Track eviction event
		}
		return true
	}
	return false
}

// lfuEvictor implements Least Frequently Used eviction policy
//
// LFU evicts the item with the lowest access frequency.
// Evict the item at the root of the min-heap (lowest frequency)
// When items have the same frequency, evict the one
// with the oldest lastAccess time (LRU among equals)
type lfuEvictor[K comparable, V any] struct{}

// evict removes the least frequently used item from the shard
//
// LFU eviction process:
// 1. Check if heap is empty or uninitialized
// 2. Pop the root item from min-heap (lowest frequency)
// 3. Remove item from all data structures:
//   - Hash map (for key lookup)
//   - LRU linked list (for access ordering)
//   - LFU heap (already removed by Pop)
//
// 4. Return item to object pool
// 5. Update size and statistics
//
// The heap root contains the LFU item because:
// - Min-heap keeps lowest frequency at root
// - Frequency is incremented on each access
// - Ties are broken by lastAccess time (older = higher priority for eviction)
func (e lfuEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool {
	if s.lfuHeap == nil || s.lfuHeap.Len() == 0 {
		return false
	}

	// Pop the least frequently used item from heap
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

type fifoEvictor[K comparable, V any] struct{}

func (e fifoEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool {
	lruEv := lruEvictor[K, V]{}
	return lruEv.evict(s, itemPool, statsEnabled)
}

type randomEvictor[K comparable, V any] struct{}

func (e randomEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool {
	if len(s.data) == 0 {
		return false
	}

	keys := make([]K, 0, len(s.data))
	for key := range s.data {
		keys = append(keys, key)
	}

	if len(keys) == 0 {
		return false
	}

	randomIndex := int(time.Now().UnixNano()) % len(keys)
	randomKey := keys[randomIndex]
	if item, exists := s.data[randomKey]; exists {
		delete(s.data, randomKey)
		s.removeFromLRU(item)
		if s.lfuHeap != nil && item.heapIndex != -1 {
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

// createEvictor returns an evictor implementation based on the specified policy
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
