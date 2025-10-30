package cache

import (
	"sync/atomic"
	"time"
)

// Item is a wire-friendly export with absolute expiry.
// NOTE:
//   - Version here is NOT the cluster LWW/HLC version. Export() currently uses a
//     placeholder (frequency) which is suitable for application-level cache dumps
//     and warm starts, but not for cluster snapshots.
//   - Cluster replication/backfill paths supply their own authoritative versions
//     and call InMemoryCache.Import directly with those values.
type Item[K comparable, V any] struct {
	Key       K
	Val       V
	ExpireAbs int64  // 0 = no expiration
	Version   uint64 // reserved for LWW if you add a real version later
}

// Export up to max items for which selectFn(key) is true.
// Intended for application-level dump/restore. Not used by cluster state
// transfer, because it does not carry the cluster's LWW versions.
func (c *InMemoryCache[K, V]) Export(selectFn func(K) bool, max int) []Item[K, V] {
	out := make([]Item[K, V], 0, max)
outer:
	for _, s := range c.shards {
		s.mu.RLock()
		now := time.Now().UnixNano()
		for k, it := range s.data {
			if max > 0 && len(out) >= max {
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
				Version:   uint64(it.frequency), // placeholder
			})
		}
		s.mu.RUnlock()
	}
	return out
}

// Import inserts/overwrites with absolute expiry. Cluster replication,
// backfill, and rebalancing use this to apply authoritative state (including
// LWW versions) without admission/eviction decisions.
func (c *InMemoryCache[K, V]) Import(items []Item[K, V]) {
	now := time.Now().UnixNano()
	for _, it := range items {
		s := c.getShard(it.Key)
		s.mu.Lock()
		ex, ok := s.data[it.Key]
		if !ok {
			ex = c.itemPool.Get().(*cacheItem[V])
			s.data[it.Key] = ex
			s.addToLRUHead(ex)
			atomic.AddInt64(&s.size, 1)
		} else if c.config.EvictionPolicy == LFU {
			s.lfuList.remove(ex)
		}
		ex.key, ex.value = it.Key, it.Val
		ex.lastAccess = now
		ex.expireTime = it.ExpireAbs
		switch c.config.EvictionPolicy {
		case LRU:
			s.moveToLRUHead(ex)
		case LFU:
			ex.frequency = 1
			s.lfuList.add(ex)
		case AdmissionLFU:
			ex.frequency = 1
		}
		s.mu.Unlock()
	}
}
