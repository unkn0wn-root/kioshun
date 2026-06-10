package kioshun

// cacheItem is the entry stored in the shard table. It carries value/expiry
// metadata plus the links and policy state used by LRU, LFU and SieveTinyLFU so
// policy maintenance does not allocate wrapper nodes.
//
// Reader-visible fields (key, hash, value, expireTime) are written before the
// item is published into the table and never mutated afterwards: lock-free reads
// may hold an item past its eviction so a value update allocates a fresh item
// rather than mutating in place, and evicted items are reclaimed by the GC rather
// than pooled.
type cacheItem[K comparable, V any] struct {
	value      V
	expireTime int64 // cache-relative monotonic ns; 0 => no expiration
	cost       int64 // weigher-reported capacity cost;
	prev       *cacheItem[K, V]
	next       *cacheItem[K, V]
	key        K // original key for deletions
	hash       uint64
	queue      sieveQueueID // which SIEVE queue role owns this (queueNone = unlinked)
	queueOwner uint8
	reuse      uint8
	flags      uint8
	visited    uint32
}

const (
	// marks a SieveTinyLFU candidate that is live in the policy queues but
	// not yet stored in the table: admission is still deciding its fate, so
	// there is no slot to reclaim on removal.
	itemUnpublished uint8 = 1 << iota
	// marks an item allocated from its shard's bump slab rather than on its
	// own. A slab-backed item must not enter the main queue (see
	// unslabForMain): one long lived survivor would pin its whole slab.
	itemSlabbed
)
