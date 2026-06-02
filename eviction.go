package kioshun

import (
	"sync"
)

type evictor[K comparable, V any] interface {
	evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool)
}

type lruEvictor[K comparable, V any] struct{}

func (e lruEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) {
	if s.tail.prev == s.head {
		return
	}

	s.dropItem(s.tail.prev, itemPool, statsEnabled, true, dropLRU)
}

type lfuEvictor[K comparable, V any] struct{}

func (e lfuEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) {
	lfu := s.lfuList.removeLFU()
	if lfu == nil {
		return
	}
	s.dropItem(lfu, itemPool, statsEnabled, true, dropLRU)
}

type fifoEvictor[K comparable, V any] struct{}

func (e fifoEvictor[K, V]) evict(s *shard[K, V], itemPool *sync.Pool, statsEnabled bool) {
	if s.tail.prev == s.head {
		return
	}

	s.dropItem(s.tail.prev, itemPool, statsEnabled, true, dropLRU)
}

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
