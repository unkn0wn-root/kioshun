package kioshun

import "sync"

// cacheItem is the entry stored in shard.data. It carries value/expiry metadata
// plus the links and policy state used by LRU, LFU and SieveTinyLFU so policy
// maintenance does not allocate wrapper nodes.
type cacheItem[K comparable, V any] struct {
	value      V
	expireTime int64 // absolute ns; 0 => no expiration
	lastAccess int64 // last touch time (ns) for policies that use it
	lfuFreq    int64 // exact LFU counter
	cost       int64 // weigher-reported capacity cost;
	prev       *cacheItem[K, V]
	next       *cacheItem[K, V]
	sieveQ     *sieveQueue[K, V]
	key        K // original key for deletions
	hash       uint64
	queue      sieveQueueID
	reuse      uint8
	visited    uint32
}

func acquireCacheItem[K comparable, V any](pool *sync.Pool) *cacheItem[K, V] {
	it := pool.Get().(*cacheItem[K, V])
	*it = cacheItem[K, V]{}
	return it
}

func releaseCacheItem[K comparable, V any](pool *sync.Pool, it *cacheItem[K, V]) {
	if it == nil {
		return
	}
	*it = cacheItem[K, V]{}
	pool.Put(it)
}
