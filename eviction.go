package cache

import (
	"sync"
	"sync/atomic"
)

const (
	// AdmissionLFU sampling parameters
	defaultAdmissionLFUSampleSize = 5
	maxAdmissionLFUSampleSize     = 20
)

// evictor defines the interface for cache eviction policies.
// Select which item to remove when the cache reaches capacity.
type evictor[K comparable, V any] interface {
	evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool
}

// lruEvictor implements Least Recently Used eviction policy.
type lruEvictor[K comparable, V any] struct{}

// evict removes the least recently used item from the shard.
// LRU policy: the item at tail.prev is least recently accessed.
func (e lruEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool {
	// Check if shard is empty (only sentinel nodes remain)
	if s.tail.prev == s.head {
		return false
	}

	// Get the LRU item (closest to tail sentinel)
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
// LFU policy: O(1) frequency list contains the item with lowest access frequency.
func (e lfuEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool {
	lfu := s.lfuList.removeLFU()
	if lfu == nil {
		return false
	}

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
// FIFO policy: treats the LRU list as insertion-order queue (oldest at tail).
func (e fifoEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool {
	if s.tail.prev == s.head {
		return false
	}

	oldest := s.tail.prev
	if oldest != nil && oldest.key != nil {
		if key, ok := oldest.key.(K); ok {
			delete(s.data, key)
		}
		s.removeFromLRU(oldest)

		if s.lfuList != nil {
			s.lfuList.remove(oldest)
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

// admissionLFUEvictor implements approximate LFU eviction with frequency-aware admission.
// Uses random sampling instead of exact heap maintenance.
type admissionLFUEvictor[K comparable, V any] struct {
	sampleSize int
}

// pickVictim does the random-sample scan and returns the least-frequent item,
// or nil if shard is empty.
func (e admissionLFUEvictor[K, V]) pickVictim(s *shard[K, V]) *cacheItem[V] {
	if len(s.data) == 0 {
		return nil
	}

	n := e.sampleSize
	if n <= 0 {
		n = defaultAdmissionLFUSampleSize
	} else if n > maxAdmissionLFUSampleSize {
		n = maxAdmissionLFUSampleSize
	}
	if n > len(s.data) {
		n = len(s.data)
	}

	var victim *cacheItem[V]
	cnt := 0
	for _, it := range s.data {
		if victim == nil ||
			it.frequency < victim.frequency ||
			(it.frequency == victim.frequency && it.lastAccess < victim.lastAccess) {
			victim = it
		}
		cnt++
		if cnt >= n {
			break
		}
	}
	return victim
}

// removeVictim unlinks & deletes the given item and updates size/stats.
func (e admissionLFUEvictor[K, V]) removeVictim(s *shard[K, V], victim *cacheItem[V], itemPool *sync.Pool, statsEnabled bool) {
	if key, ok := victim.key.(K); ok {
		delete(s.data, key)
	}
	s.removeFromLRU(victim)
	// no LFU heap, so no heap removal here
	itemPool.Put(victim)
	atomic.AddInt64(&s.size, -1)
	if statsEnabled {
		atomic.AddInt64(&s.evictions, 1)
	}
}

// evict without admission
func (e admissionLFUEvictor[K, V]) evict(
	s *shard[K, V],
	itemPool *sync.Pool,
	statsEnabled bool,
) bool {
	victim := e.pickVictim(s)
	if victim == nil {
		return false
	}
	if s.admission != nil {
		s.lastVictimFrequency = uint64(victim.frequency)
	}
	e.removeVictim(s, victim, itemPool, statsEnabled)

	return true
}

// evictWithAdmission does sample → shouldAdmit → remove.
// Returns true only if admission succeeded & an eviction occurred.
func (e admissionLFUEvictor[K, V]) evictWithAdmission(
	s *shard[K, V],
	itemPool *sync.Pool,
	statsEnabled bool,
	admission *adaptiveAdmissionFilter,
	keyHash uint64,
) bool {
	victim := e.pickVictim(s)
	if victim == nil {
		return false
	}

	freq := uint64(victim.frequency)
	victimAge := victim.lastAccess
	s.lastVictimFrequency = freq

	if !admission.shouldAdmit(keyHash, freq, victimAge) {
		return false
	}
	e.removeVictim(s, victim, itemPool, statsEnabled)

	admission.RecordEviction()

	return true
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
	case AdmissionLFU:
		return admissionLFUEvictor[K, V]{sampleSize: defaultAdmissionLFUSampleSize}
	default:
		return admissionLFUEvictor[K, V]{sampleSize: defaultAdmissionLFUSampleSize}
	}
}
