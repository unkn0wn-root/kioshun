package cache

import (
	"sync/atomic"
	"time"
)

// Item is the application dump format. Export currently fills Version with an
// LFU placeholder, so it is not a cluster/LWW snapshot format.
type Item[K comparable, V any] struct {
	Key       K
	Val       V
	ExpireAbs int64  // 0 = no expiration
	Version   uint64 // opaque to Import; Export stores a placeholder
}

// Export returns up to max non-expired items accepted by selectFn.
func (c *InMemoryCache[K, V]) Export(selectFn func(K) bool, mx int) []Item[K, V] {
	out := make([]Item[K, V], 0, mx)
	now := time.Now().UnixNano()
outer:
	for _, s := range c.shards {
		s.mu.RLock()
		for k, it := range s.data {
			if mx > 0 && len(out) >= mx {
				s.mu.RUnlock()
				break outer
			}
			if it.expireTime > 0 && now > it.expireTime {
				continue
			}
			if !selectFn(k) {
				continue
			}
			out = append(out, Item[K, V]{
				Key:       k,
				Val:       it.value,
				ExpireAbs: it.expireTime,
				Version:   uint64(it.lfuFreq), // placeholder
			})
		}
		s.mu.RUnlock()
	}
	return out
}

// Import inserts or overwrites with absolute expiry, bypassing admission policy.
// Cluster replication and backfill apply authoritative state through it; each
// Item.Version travels with the data for the cluster's own LWW bookkeeping and
// is not interpreted here.
func (c *InMemoryCache[K, V]) Import(items []Item[K, V]) {
	if len(items) == 0 {
		return
	}

	if atomic.LoadInt32(&c.closed) == 1 {
		return
	}

	now := time.Now().UnixNano()
	byShard := make(map[*shard[K, V]][]Item[K, V])
	for _, item := range items {
		h := c.hasher.hash(item.Key)
		s := c.shardByHash(h)
		byShard[s] = append(byShard[s], item)
	}

	shards := make([]*shard[K, V], 0, len(byShard))
	waiters := make([]*writeWaiter, 0, len(byShard))
	for shard, shardItems := range byShard {
		waiter := c.acquireWriteWaiter()
		copied := append([]Item[K, V](nil), shardItems...)
		cmd := writeCommand[K, V]{
			op:     writeImport,
			now:    now,
			result: waiter.ch,
			extra: &writeExtra[K, V]{
				items: copied,
			},
		}
		if err := shard.queue.enqueue(cmd); err != nil {
			c.releaseWriteWaiter(waiter)
			break
		}
		shards = append(shards, shard)
		waiters = append(waiters, waiter)
	}
	for _, shard := range shards {
		c.tryDrainShard(shard)
	}
	for _, waiter := range waiters {
		if err := c.awaitResult(waiter.ch); err != nil {
			return
		}
		c.releaseWriteWaiter(waiter)
	}
}
