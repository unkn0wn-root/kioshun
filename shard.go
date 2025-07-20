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

// initLRU initializes the doubly-linked list for LRU tracking.
// Creates a circular list with sentinel head and tail nodes.
func (s *shard[K, V]) initLRU() {
	s.head = &cacheItem[V]{heapIndex: -1}
	s.tail = &cacheItem[V]{heapIndex: -1}
	s.head.next = s.tail
	s.tail.prev = s.head
}

// addToLRUHead inserts an item as the most recently used item.
func (s *shard[K, V]) addToLRUHead(item *cacheItem[V]) {
	oldNext := s.head.next
	s.head.next = item
	item.prev = s.head
	item.next = oldNext
	oldNext.prev = item
}

// removeFromLRU removes an item from the LRU doubly-linked list.
func (s *shard[K, V]) removeFromLRU(item *cacheItem[V]) {
	if item.prev != nil {
		item.prev.next = item.next
	}
	if item.next != nil {
		item.next.prev = item.prev
	}
	item.prev = nil
	item.next = nil
}

// moveToLRUHead promotes an item to the most recently used position.
func (s *shard[K, V]) moveToLRUHead(item *cacheItem[V]) {
	// skip if already at head
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

// cleanup removes expired items from this shard.
// Items are identified and removed in two phases to avoid
// modifying the map while iterating over it.
func (s *shard[K, V]) cleanup(now int64, evictionPolicy EvictionPolicy, itemPool *sync.Pool, statsEnabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1: Collect expired keys
	var keysToDelete []K
	for key, item := range s.data {
		if item.expireTime > 0 && now > item.expireTime {
			keysToDelete = append(keysToDelete, key)
		}
	}

	// 2: Remove collected items
	for _, key := range keysToDelete {
		if item, exists := s.data[key]; exists {
			delete(s.data, key)
			s.removeFromLRU(item)
			if evictionPolicy == LFU && item.heapIndex != -1 {
				heap.Remove(s.lfuHeap, item.heapIndex)
			}
			itemPool.Put(item)
			atomic.AddInt64(&s.size, -1)
			if statsEnabled {
				atomic.AddInt64(&s.expirations, 1)
			}
		}
	}
}
