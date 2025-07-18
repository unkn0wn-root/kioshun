package cache

import (
	"container/heap"
	"sync"
	"sync/atomic"
)

// shard represents a cache partition to reduce lock contention
type shard[K comparable, V any] struct {
	mu          sync.RWMutex
	data        map[K]*cacheItem[V]
	head        *cacheItem[V] // LRU head (most recently used)
	tail        *cacheItem[V] // LRU tail (least recently used)
	lfuHeap     *lfuHeap[V]   // For LFU eviction
	size        int64         // Current number of items
	hits        int64
	misses      int64
	evictions   int64
	expirations int64
}

// initLRU initializes the doubly-linked list for LRU tracking
//
// Creates a circular list with sentinel nodes:
// - head: sentinel node representing the most recently used position
// - tail: sentinel node representing the least recently used position
// - Connects head.next -> tail and tail.prev -> head to form empty list
//
// - head.next always points to the MRU item (or tail if empty)
// - tail.prev always points to the LRU item (or head if empty)
//
// heapIndex is set to -1 to indicate sentinel nodes are not part of LFU heap
func (c *InMemoryCache[K, V]) initLRU(s *shard[K, V]) {
	s.head = &cacheItem[V]{heapIndex: -1}
	s.tail = &cacheItem[V]{heapIndex: -1}
	s.head.next = s.tail
	s.tail.prev = s.head
}

// addToLRUHead inserts an item as the most recently used item in the LRU list
//
// Performs list insertion after the head sentinel:
// 1. Save current first item (head.next) as oldNext
// 2. Link head -> item: head.next = item, item.prev = head
// 3. Link item -> oldNext: item.next = oldNext, oldNext.prev = item
//
// This maintains the LRU ordering where:
// - Items closest to head are most recently used
// - Items closest to tail are least recently used
// - New items are always inserted at head position (most recent)
func (c *InMemoryCache[K, V]) addToLRUHead(s *shard[K, V], item *cacheItem[V]) {
	oldNext := s.head.next
	s.head.next = item
	item.prev = s.head
	item.next = oldNext
	oldNext.prev = item
}

// removeFromLRU removes an item from the LRU
//
// 1. If item has previous node: link prev.next to item.next (bypass item)
// 2. If item has next node: link next.prev to item.prev (bypass item)
// 3. Clear item's prev/next pointers to prevent memory leaks and dangling references
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

// moveToLRUHead promotes an item to most recently used position
// - Checks if item is already at head position (most recently used)
// - If so, returns immediately without any list manipulation
// - Avoids unnecessary pointer updates
//
// Promotion logic (when item is not at head):
// 1. Remove item from current position by updating neighboring nodes' pointers
// 2. Insert item at head position
func (c *InMemoryCache[K, V]) moveToLRUHead(s *shard[K, V], item *cacheItem[V]) {
	if s.head.next == item {
		return
	}

	if item.prev != nil {
		item.prev.next = item.next
	}
	if item.next != nil {
		item.next.prev = item.prev
	}

	oldNext := s.head.next
	s.head.next = item
	item.prev = s.head
	item.next = oldNext
	oldNext.prev = item
}

// cleanupShard removes expired items from a specific shard
// Phase 1: Collection
// - Iterates through shard's hash map to identify expired items
// - Collects keys of expired items into a separate slice
// - Uses provided timestamp (now)
// - No modifications to data structures during this phase
//
// Phase 2: Removal
// - Iterates through collected keys and removes each expired item
// - Double-checks existence and expiration under lock
// - Removes item: map, LRU list, LFU heap
// - Returns item to object pool
// - Updates shard size and expiration statistics
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
			if c.config.EvictionPolicy == LFU && item.heapIndex != -1 {
				heap.Remove(s.lfuHeap, item.heapIndex)
			}
			c.itemPool.Put(item)
			atomic.AddInt64(&s.size, -1)
			if c.config.StatsEnabled {
				atomic.AddInt64(&s.expirations, 1)
			}
		}
	}
}
