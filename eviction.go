package cache

import (
	"sync"
	"sync/atomic"
)

const (
	// AdmissionLFU sampling parameters.
	// We use a tiny random sample of the shard's items and pick the least-frequent
	// (ties by oldest lastAccess). Small, bounded samples keep eviction O(1) on
	// average and avoid maintaining a global heap.
	defaultAdmissionLFUSampleSize = 5
	maxAdmissionLFUSampleSize     = 20
)

// evictor is the policy interface. Implementations remove exactly one item
// when a shard is at capacity and return whether they evicted.
// Concurrency: called under the shard's write lock by Set(); implementations
// assume they have exclusive access to s.{data,LRU,lfuList} and may update stats.
type evictor[K comparable, V any] interface {
	evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) bool
}

// lruEvictor implements classic LRU using the shard's intrusive list.
// The shard maintains a sentinel {head,tail}; tail.prev is the least-recently-used.
type lruEvictor[K comparable, V any] struct{}

// evict removes the least recently used item from the shard.
// Complexity: O(1). Also unlinks from the LRU list and recycles the node.
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

// lfuEvictor picks the least-frequently-used item using the shard's O(1) LFU list.
// The LFU list maintains buckets by frequency; removeLFU() returns the global min.
type lfuEvictor[K comparable, V any] struct{}

// evict removes the LFU item and updates shared structures.
// Complexity: O(1) for removeLFU + O(1) list unlink.
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

// fifoEvictor uses the same intrusive list as LRU but treats it as insertion order.
// Oldest item sits at tail.prev.
type fifoEvictor[K comparable, V any] struct{}

// evict removes the earliest inserted item.
// Complexity: O(1). If an LFU list exists (policy changes), we unlink there too
// to maintain internal invariants.
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

		// If LFU structures are present, keep them consistent.
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

// admissionLFUEvictor implements approximate LFU with admission control.
// Approach:
//   - Randomly sample up to N items from the shard's map (iteration order over
//     a Go map is randomized; we early-break after N to get a cheap sample).
//   - Choose the least-frequent; on ties, choose the older lastAccess.
//   - With admission disabled: evict that victim.
//   - With admission enabled: first call shouldAdmit(keyHash, victimFreq, victimAge).
//   - If admission denies: return false (caller rejects the Set without evicting).
//   - If admission allows: evict victim and return true.
//
// Rationale: bounded, allocation-free eviction without maintaining a heap or tree.
type admissionLFUEvictor[K comparable, V any] struct {
	sampleSize int // desired sample size; clamped to [1, maxAdmissionLFUSampleSize]
}

// pickVictim performs a bounded scan over the shard's map and returns the
// "worst" item under (frequency ASC, lastAccess ASC). If the shard is empty
// it returns nil.
// Complexity: O(sampleSize) expected. We rely on Go's randomized map iteration
// order to approximate uniform sampling without extra RNG.
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

// removeVictim unlinks 'victim' from all shard structures, updates size/stats,
// and recycles the node via sync.Pool. Caller must hold the shard write lock.
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

// evict implements "approximate LFU without admission". It samples,
// picks a victim, and evicts it. lastVictimFrequency is recorded on the shard
// for observability/telemetry (used by admission logic elsewhere).
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

// evictWithAdmission performs: sample → shouldAdmit → (optionally) evict.
// Returns true only when admission approves and we actually evict a victim.
// If admission denies, the caller (Set) will reject the incoming item without
// evicting, which prevents cache pollution during scans/cold bursts.
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
	s.lastVictimFrequency = freq // stored for debugging/metrics

	// Admission gate: compare candidate(keyHash) vs victim(freq/age).
	if !admission.shouldAdmit(keyHash, freq, victimAge) {
		return false
	}
	e.removeVictim(s, victim, itemPool, statsEnabled)

	// Feed back pressure to the admission filter for adaptive probability.
	admission.RecordEviction()

	return true
}

// createEvictor constructs the policy implementation for the configured strategy.
// Default is AdmissionLFU with a small fixed sample (defensive default).
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
