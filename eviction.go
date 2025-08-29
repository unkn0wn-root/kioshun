package cache

import (
	"sync"
	"sync/atomic"
)

const (
	// AdmissionLFU sampling: take a small randomized sample and pick the least-frequent (tie: oldest).
	defaultAdmissionLFUSampleSize = 5
	maxAdmissionLFUSampleSize     = 20
)

// evictor defines a policy that evicts exactly one item when a shard is full (called under shard write lock).
type evictor[K comparable, V any] interface {
	evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool
}

// lruEvictor removes the least-recently-used item using the shard's intrusive LRU list.
type lruEvictor[K comparable, V any] struct{}

// evict unlinks and recycles the tail.prev (LRU) item; updates size/stats; O(1).
func (e lruEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool {
	// Empty list check: only sentinels present.
	if s.tail.prev == s.head {
		return false
	}

	// Victim is the node just before the tail sentinel.
	lru := s.tail.prev
	if lru != nil && lru.key != nil {
		// cacheItem.key is stored as 'any' to allow deletion without recomputing the hash.
		// We assert to K here; inserts always set key with the correct type.
		if key, ok := lru.key.(K); ok {
			delete(s.data, key)
		}

		s.removeFromLRU(lru) // O(1) unlink from intrusive list
		itemPool.Put(lru)    // recycle to reduce GC churn
		atomic.AddInt64(&s.size, -1)

		if statsEnabled {
			atomic.AddInt64(&s.evictions, 1)
		}
		return true
	}
	return false
}

// lfuEvictor removes the global least-frequent item via the O(1) LFU bucket list.
type lfuEvictor[K comparable, V any] struct{}

// evict pops the LFU item, unlinks from LRU too, recycles, and updates size/stats; O(1).
func (e lfuEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool {
	lfu := s.lfuList.removeLFU()
	if lfu == nil {
		return false
	}

	if lfu.key != nil {
		if key, ok := lfu.key.(K); ok {
			delete(s.data, key)
		}

		// We maintain the LRU list for uniform unlinking/cleanup across policies.
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

// fifoEvictor treats the LRU list as insertion order and removes the oldest item.
type fifoEvictor[K comparable, V any] struct{}

// evict deletes the earliest inserted (tail.prev), unlinks from optional LFU, and updates stats; O(1).
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

// admissionLFUEvictor does approximate-LFU by sampling and optionally gating via the admission filter.
type admissionLFUEvictor[K comparable, V any] struct {
	sampleSize int // desired sample size; clamped to [1, maxAdmissionLFUSampleSize]
}

// pickVictim scans up to sampleSize items (randomized map order) and returns the worst (freq↑, age↑); nil if empty.
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
		// Lower frequency is worse; if equal, older lastAccess is worse.
		if victim == nil ||
			it.frequency < victim.frequency ||
			(it.frequency == victim.frequency && it.lastAccess < victim.lastAccess) {
			victim = it
		}
		cnt++
		if cnt >= n {
			break // bounded sample scan
		}
	}
	return victim
}

// removeVictim unlinks victim from shard structures, recycles it, updates size/stats (caller holds write lock).
func (e admissionLFUEvictor[K, V]) removeVictim(s *shard[K, V], victim *cacheItem[V], itemPool *sync.Pool, statsEnabled bool) {
	if key, ok := victim.key.(K); ok {
		delete(s.data, key)
	}
	s.removeFromLRU(victim)
	// No LFU heap: AdmissionLFU doesn't maintain the O(1) LFU list.
	itemPool.Put(victim)
	atomic.AddInt64(&s.size, -1)
	if statsEnabled {
		atomic.AddInt64(&s.evictions, 1)
	}
}

// evict samples, picks a victim, evicts it, and records lastVictimFrequency for observability.
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

// evictWithAdmission runs sample → shouldAdmit → (optional) evict; denies return false to let Set() skip pollution.
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
	s.lastVictimFrequency = freq // stored for deb/metrics

	// Admission gate: compare candidate(keyHash) vs victim(freq/age).
	if !admission.shouldAdmit(keyHash, freq, victimAge) {
		return false
	}
	e.removeVictim(s, victim, itemPool, statsEnabled)

	// Feed back pressure to the admission filter for adaptive probability.
	admission.RecordEviction()

	return true
}

// createEvictor returns the policy implementation for the selected EvictionPolicy.
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
