package kioshun

// evictor removes one resident item from a bounded shard.
// Callers hold the shard lock; only choose the victim and leave
// table removal, policy unlinking and statistics to shard.dropItem.
type evictor[K comparable, V any] interface {
	evict(s *shard[K, V], statsEnabled bool)
}

type lruEvictor[K comparable, V any] struct{}

func (e lruEvictor[K, V]) evict(s *shard[K, V], statsEnabled bool) {
	if s.tail.prev == s.head {
		return
	}

	s.dropItem(s.tail.prev, statsEnabled, RemovedCapacity, dropLRU)
}

type lfuEvictor[K comparable, V any] struct{}

func (e lfuEvictor[K, V]) evict(s *shard[K, V], statsEnabled bool) {
	lfu := s.lfuList.removeLFU()
	if lfu == nil {
		return
	}
	// removeLFU already unlinked the LFU bucket; dropLRU unlinks the shared LRU
	// list and table without a redundant lookup.
	s.dropItem(lfu, statsEnabled, RemovedCapacity, dropLRU)
}

type fifoEvictor[K comparable, V any] struct{}

// evict removes the tail entry from the shared LRU list. FIFO reads never move
// entries, so tail.prev remains the oldest inserted resident for this policy.
func (e fifoEvictor[K, V]) evict(s *shard[K, V], statsEnabled bool) {
	if s.tail.prev == s.head {
		return
	}

	s.dropItem(s.tail.prev, statsEnabled, RemovedCapacity, dropLRU)
}

// createEvictor returns the non-Sieve replacement policy for a shard.
// Public config is normalized and validated before this point.
func createEvictor[K comparable, V any](policy EvictionPolicy) evictor[K, V] {
	switch policy {
	case LRU:
		return lruEvictor[K, V]{}
	case LFU:
		return lfuEvictor[K, V]{}
	case FIFO:
		return fifoEvictor[K, V]{}
	default:
		return fifoEvictor[K, V]{}
	}
}
