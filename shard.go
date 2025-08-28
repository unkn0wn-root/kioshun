package cache

import (
	"sync"
	"sync/atomic"
)

// shard is a cache partition that confines contention to a subset of keys.
// Concurrency:
//   - All structural mutations (map/links) are protected by mu.
//   - Hot-path counters (size/hits/misses/evictions/expirations) are atomically updated.
//
// Data structures:
//   - data:      key → *cacheItem[V]
//   - LRU list:  intrusive doubly-linked list with sentinels {head, tail}
//     head.next is MRU, tail.prev is LRU/oldest.
//   - lfuList:   optional O(1) frequency-bucket list used by pure LFU.
//   - admission: per-shard admission filter used by AdmissionLFU policy.
type shard[K comparable, V any] struct {
	mu   sync.RWMutex
	data map[K]*cacheItem[V]

	// LRU intrusive list sentinels.
	// Invariant: head.prev == nil, tail.next == nil, and head.next...prev chain forms the list.
	head *cacheItem[V] // MRU sentinel (head.next = most recently used item)
	tail *cacheItem[V] // LRU sentinel (tail.prev = least recently used item)

	lfuList *lfuList[K, V] // Only allocated when policy == LFU.

	size        int64 // number of live items in this shard (atomically adjusted)
	hits        int64 // shard-local hit counter
	misses      int64 // shard-local miss counter
	evictions   int64 // shard-local eviction counter
	expirations int64 // shard-local TTL expiration counter

	// AdmissionLFU-only: reference to the adaptive admission filter.
	admission *adaptiveAdmissionFilter

	// For observability/debugging: frequency of the last evicted victim (AdmissionLFU).
	lastVictimFrequency uint64
}

// initLRU initializes the intrusive LRU list to an empty state with sentinels.
// We use a two-sentinel scheme (head/tail) to eliminate nil checks on insert/remove.
// After init:
//
//	head.next == tail
//	tail.prev == head
//
// Complexity: O(1). Caller must hold s.mu in write mode for structural init.
func (s *shard[K, V]) initLRU() {
	s.head = &cacheItem[V]{}
	s.tail = &cacheItem[V]{}
	// Initialize empty list: head <-> tail
	s.head.next = s.tail
	s.tail.prev = s.head
}

// addToLRUHead inserts 'item' as MRU (immediately after head).
// Invariants preserved:
//   - head.next == item
//   - item.prev == head
//   - item.next == oldNext
//   - oldNext.prev == item
//
// Complexity: O(1). Caller must hold s.mu in write mode.
func (s *shard[K, V]) addToLRUHead(item *cacheItem[V]) {
	oldNext := s.head.next
	// forward pointers: head -> item -> oldNext
	s.head.next = item
	item.next = oldNext
	// backward pointers: head <- item <- oldNext
	item.prev = s.head
	oldNext.prev = item
}

// removeFromLRU excises 'item' from the intrusive list by splicing its neighbors.
// Post-condition: item.prev == nil && item.next == nil (defensive cleanup).
// Complexity: O(1). Caller must hold s.mu (read or write depending on caller's protocol,
// but list mutation requires write lock in our usage).
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

// moveToLRUHead promotes 'item' to MRU position unless it is already MRU.
// Steps:
//  1. Unlink item from current position (O(1)).
//  2. Insert after head (same as addToLRUHead without reusing helper to avoid extra loads).
//
// Complexity: O(1). Requires s.mu write lock.
func (s *shard[K, V]) moveToLRUHead(item *cacheItem[V]) {
	// Fast-path: already MRU.
	if s.head.next == item {
		return
	}
	// Unlink from current position if linked.
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

// cleanup removes expired items and applies light, per-item frequency aging for AdmissionLFU.
// Contract:
//   - now: wall-clock UnixNano captured once by the caller per cleanup pass.
//   - evictionPolicy: caller's current policy; determines whether to age frequency and
//     whether to unlink from lfuList.
//   - itemPool: returned nodes are recycled to reduce GC pressure.
//   - statsEnabled: if true, increments shard-level counters.
//
// Algorithm:
//
//	Phase 1: Scan data map under write lock; collect keys to delete to avoid mutating
//	         the map while ranging. Also apply in-place frequency halving for AdmissionLFU.
//	Phase 2: Iterate the deletion list; for each key still present, unlink from lists,
//	         recycle node, adjust size/stats.
//
// Notes:
//   - Expiration is best-effort; lazy expiration also happens on reads.
//   - Frequency halving prevents "immortal" popularity for AdmissionLFU without scanning
//     global sketches—kept intentionally simple and cheap here.
//
// Complexity: O(n) per shard in the worst case (n = items in shard).
func (s *shard[K, V]) cleanup(now int64, evictionPolicy EvictionPolicy, itemPool *sync.Pool, statsEnabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var keysToDelete []K
	for key, item := range s.data {
		// TTL check: collect key if expired at the time anchor 'now'.
		if item.expireTime > 0 && now > item.expireTime {
			keysToDelete = append(keysToDelete, key)
			continue
		}
		// Lightweight per-item aging for AdmissionLFU to avoid stale high frequencies.
		// This is intentionally coarse (shift) and only applied to items that have grown
		// past 1 to reduce churn on mostly-cold entries.
		if evictionPolicy == AdmissionLFU && item.frequency > 1 {
			item.frequency >>= 1 // halve in place
		}
	}

	// Phase 2: destructive pass over collected keys. We re-check existence since
	// items may have been concurrently removed/updated prior to acquiring the lock.
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
