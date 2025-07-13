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
func (c *InMemoryCache[K, V]) initLRU(s *shard[K, V]) {
	s.head = &cacheItem[V]{heapIndex: -1}
	s.tail = &cacheItem[V]{heapIndex: -1}
	s.head.next = s.tail
	s.tail.prev = s.head
}

// addToLRUHead adds an item to the head of the LRU list
func (c *InMemoryCache[K, V]) addToLRUHead(s *shard[K, V], item *cacheItem[V]) {
	oldNext := s.head.next
	s.head.next = item
	item.prev = s.head
	item.next = oldNext
	oldNext.prev = item
}

// removeFromLRU removes an item from the LRU list
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

// moveToLRUHead moves item to head if not already there
func (c *InMemoryCache[K, V]) moveToLRUHead(s *shard[K, V], item *cacheItem[V]) {
	// fast path: if item is already at head, do nothing
	if s.head.next == item {
		return
	}

	// remove from current position
	if item.prev != nil {
		item.prev.next = item.next
	}
	if item.next != nil {
		item.next.prev = item.prev
	}

	// add to head
	oldNext := s.head.next
	s.head.next = item
	item.prev = s.head
	item.next = oldNext
	oldNext.prev = item
}

// cleanupShard removes expired items from a specific shard
func (c *InMemoryCache[K, V]) cleanupShard(s *shard[K, V], now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Collect expired keys to avoid modifying map during iteration
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
