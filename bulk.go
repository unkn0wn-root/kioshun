package cache

import (
	"sync/atomic"
	"time"
)

func (c *InMemoryCache[K, V]) GetBulk(keys []K) ([]V, []bool) {
	out := make([]V, len(keys))
	hit := make([]bool, len(keys))

	type grp struct {
		idxs []int
		ks   []K
	}
	groups := make(map[*shard[K, V]]*grp, 8)
	for i, k := range keys {
		s := c.getShard(k)
		g := groups[s]
		if g == nil {
			g = &grp{}
			groups[s] = g
		}
		g.idxs = append(g.idxs, i)
		g.ks = append(g.ks, k)
	}

	for s, g := range groups {
		nw := c.config.EvictionPolicy == LRU || c.config.EvictionPolicy == LFU
		if nw {
			s.mu.Lock()
		} else {
			s.mu.RLock()
		}
		now := time.Now().UnixNano()
		for j, k := range g.ks {
			i := g.idxs[j]
			it, ok := s.data[k]
			if !ok {
				hit[i] = false
				continue
			}
			if it.expireTime > 0 && now > it.expireTime {
				delete(s.data, k)
				s.removeFromLRU(it)
				if c.config.EvictionPolicy == LFU {
					s.lfuList.remove(it)
				}
				c.itemPool.Put(it)
				atomic.AddInt64(&s.size, -1)
				hit[i] = false
				continue
			}
			switch c.config.EvictionPolicy {
			case LRU:
				s.moveToLRUHead(it)
			case LFU:
				it.lastAccess = now
				it.frequency++
				s.lfuList.increment(it)
			case AdmissionLFU:
				it.lastAccess = now
				it.frequency++
			}
			out[i] = it.value
			hit[i] = true
		}
		if nw {
			s.mu.Unlock()
		} else {
			s.mu.RUnlock()
		}
	}
	return out, hit
}

func (c *InMemoryCache[K, V]) SetBulk(items map[K]V, ttl time.Duration) {
	if ttl == 0 {
		ttl = c.config.DefaultTTL
	}
	type pair struct {
		k K
		v V
	}
	buckets := make(map[*shard[K, V]][]pair, 8)
	for k, v := range items {
		s := c.getShard(k)
		buckets[s] = append(buckets[s], pair{k, v})
	}
	now := time.Now().UnixNano()
	expire := int64(0)
	if ttl > 0 {
		expire = now + ttl.Nanoseconds()
	}
	for s, arr := range buckets {
		s.mu.Lock()
		for _, p := range arr {
			if ex, ok := s.data[p.k]; ok {
				ex.value = p.v
				ex.expireTime = expire
				ex.lastAccess = now
				switch c.config.EvictionPolicy {
				case LRU:
					s.moveToLRUHead(ex)
				case LFU:
					s.lfuList.remove(ex)
					ex.frequency = 1
					s.lfuList.add(ex)
				case AdmissionLFU:
					ex.frequency = 1
				}
				continue
			}
			if c.perShardCap > 0 && atomic.LoadInt64(&s.size) >= c.perShardCap {
				c.evictor.evict(s, &c.itemPool, c.config.StatsEnabled)
			}
			it := c.itemPool.Get().(*cacheItem[V])
			it.value, it.lastAccess, it.frequency, it.key, it.expireTime = p.v, now, 1, p.k, expire
			s.data[p.k] = it
			s.addToLRUHead(it)
			if c.config.EvictionPolicy == LFU {
				s.lfuList.add(it)
			}
			atomic.AddInt64(&s.size, 1)
		}
		s.mu.Unlock()
	}
}
