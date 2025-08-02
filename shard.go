package cache

import (
	"sync"
	"sync/atomic"
)


// shard represents a cache partition to reduce lock contention
type shard[K comparable, V any] struct {
	mu                  sync.RWMutex
	data                map[K]*cacheItem[V]
	head                *cacheItem[V] // LRU head (most recently used)
	tail                *cacheItem[V] // LRU tail (least recently used)
	lfuList             *lfuList[K, V] // For LFU eviction
	size                int64          // Current number of items
	hits                int64
	misses              int64
	evictions           int64
	expirations         int64
	admission           *adaptiveAdmissionFilter
	lastVictimFrequency uint64
}

// initLRU initializes the doubly-linked list for LRU tracking.
// Creates a circular list with sentinel head and tail nodes.
// Sentinel nodes eliminate null pointer checks during insertion/removal operations.
func (s *shard[K, V]) initLRU() {
	s.head = &cacheItem[V]{}
	s.tail = &cacheItem[V]{}
	// Initialize circular structure: head <-> tail
	s.head.next = s.tail
	s.tail.prev = s.head
}

// addToLRUHead inserts an item as the most recently used item.
// Performs doubly-linked list insertion between head sentinel and first real node.
func (s *shard[K, V]) addToLRUHead(item *cacheItem[V]) {
	oldNext := s.head.next
	// forward pointers: head -> item -> oldNext
	s.head.next = item
	item.next = oldNext
	// backward pointers: head <- item <- oldNext
	item.prev = s.head
	oldNext.prev = item
}

// removeFromLRU removes an item from the LRU doubly-linked list.
// Updates adjacent nodes to bypass the removed item, maintaining list integrity.
func (s *shard[K, V]) removeFromLRU(item *cacheItem[V]) {
	// Bridge the gap: prev -> next (skip item)
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
// skip operation if item is already at head.
func (s *shard[K, V]) moveToLRUHead(item *cacheItem[V]) {
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

// cleanup removes expired items from this shard using a two-phase approach.
// Phase 1: Collect expired keys to avoid concurrent modification during iteration.
// Phase 2: Remove collected items and update data structures.
func (s *shard[K, V]) cleanup(now int64, evictionPolicy EvictionPolicy, itemPool *sync.Pool, statsEnabled bool) {
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
			s.removeFromLRU(item)
			if evictionPolicy == LFU {
				s.lfuList.remove(item)
			}
			itemPool.Put(item)
			atomic.AddInt64(&s.size, -1)
			if statsEnabled {
				atomic.AddInt64(&s.expirations, 1)
			}
		}
	}
}
