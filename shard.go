package cache

import (
	"sync"
	"sync/atomic"
)

// shard is a per-partition structure that confines contention; map/list mutations under mu, counters via atomics.
type shard[K comparable, V any] struct {
	mu   sync.RWMutex
	data map[K]*cacheItem[V]

	// Intrusive LRU list sentinels (head.next = MRU, tail.prev = LRU).
	// Invariant: head.prev == nil, tail.next == nil, and head↔…↔tail forms the chain.
	head *cacheItem[V]
	tail *cacheItem[V]

	lfuList *lfuList[K, V] // Allocated only for pure LFU policy.

	size        int64 // live items (atomic)
	hits        int64 // per-shard hits (atomic)
	misses      int64 // per-shard misses (atomic)
	evictions   int64 // per-shard evictions (atomic)
	expirations int64 // per-shard TTL expirations (atomic)

	// AdmissionLFU-only: shard-local adaptive admission filter.
	admission *adaptiveAdmissionFilter

	// Observability: frequency of last evicted victim (AdmissionLFU).
	lastVictimFrequency uint64
}

// initLRU sets up an empty LRU list with head/tail sentinels (no nil checks on operations).
func (s *shard[K, V]) initLRU() {
	s.head = &cacheItem[V]{}
	s.tail = &cacheItem[V]{}
	// head <-> tail (empty)
	s.head.next = s.tail
	s.tail.prev = s.head
}

// addToLRUHead inserts item as MRU directly after head (O(1)).
func (s *shard[K, V]) addToLRUHead(item *cacheItem[V]) {
	oldNext := s.head.next
	// head -> item -> oldNext
	s.head.next = item
	item.next = oldNext
	// head <- item <- oldNext
	item.prev = s.head
	oldNext.prev = item
}

// removeFromLRU unlinks item from the list by splicing neighbors; clears item links.
func (s *shard[K, V]) removeFromLRU(item *cacheItem[V]) {
	// prev -> next (skip item)
	if item.prev != nil {
		item.prev.next = item.next
	}
	if item.next != nil {
		item.next.prev = item.prev
	}
	item.prev = nil
	item.next = nil
}

// moveToLRUHead promotes item to MRU unless already MRU (unlink then insert after head).
func (s *shard[K, V]) moveToLRUHead(item *cacheItem[V]) {
	if s.head.next == item {
		return
	}
	// Unlink from current position.
	if item.prev != nil {
		item.prev.next = item.next
	}
	if item.next != nil {
		item.next.prev = item.prev
	}

	// Insert right after head.
	oldNext := s.head.next
	s.head.next = item
	item.prev = s.head
	item.next = oldNext
	oldNext.prev = item
}

// cleanup removes expired items and halves per-item frequency for AdmissionLFU (cheap aging).
// Phase 1: collect expired keys and apply in-place aging under write lock.
// Phase 2: delete collected keys, unlink from structures, recycle nodes, update stats.
func (s *shard[K, V]) cleanup(now int64, evictionPolicy EvictionPolicy, itemPool *sync.Pool, statsEnabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var keysToDelete []K
	for key, item := range s.data {
		if item.expireTime > 0 && now > item.expireTime {
			keysToDelete = append(keysToDelete, key)
			continue
		}
		// Lightweight aging (AdmissionLFU only) to prevent stale, permanently high frequencies.
		if evictionPolicy == AdmissionLFU && item.frequency > 1 {
			item.frequency >>= 1
		}
	}

	// Destructive pass over collected keys (re-check existence under lock).
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
