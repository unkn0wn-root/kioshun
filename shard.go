package kioshun

import (
	"sync"
	"sync/atomic"
)

type shard[K comparable, V any] struct {
	mu   sync.RWMutex
	data map[K]*cacheItem[K, V]
	cap  int64 // resident item limit for this shard; 0 => unlimited

	queue writeQueue[K, V] // async mutation transport consumed by this shard's worker

	// worker's wake-up: producers ping it after enqueuing a
	// write and read sampling pings it when a stripe fills. Buffered size 1.
	wake chan struct{}

	// keeps the MPSC queue single-consumer while allowing synchronous
	// callers to help drain their own shard instead of always waiting for the
	// background worker to run.
	drainMu    sync.Mutex
	writeBatch []writeCommand[K, V]

	// BP-Wrapper read sampling (SieveTinyLFU only). Readers push access
	// fingerprints into readBuf without taking the shard lock; the write worker
	// drains them into the frequency sketch on each wake.
	readBuf readBuffer

	// LRU list sentinels (head.next = MRU, tail.prev = LRU).
	// head.prev == nil, tail.next == nil, and the head-to-tail chain is linked.
	head *cacheItem[K, V]
	tail *cacheItem[K, V]

	lfuList *lfuList[K, V] // Allocated only for pure LFU policy.

	size        int64 // live items
	hits        int64 // per-shard hits
	misses      int64 // per-shard misses
	evictions   int64 // per-shard evictions
	expirations int64 // per-shard TTL expirations

	sieve *sieveTinyLFU[K, V]

	// removal notification staging, used only when the cache has at least one
	// listener (removeWake is the shared worker wakeup, nil otherwise). dropItem
	// and cleanup append removed (key, value, reason) to removeBuf under
	// mu; the notify worker drains it after the lock is released. removePending
	// lets the worker skip shards with nothing staged without taking the lock.
	removeWake       chan struct{}
	removeNotifyMask removalNotifyMask
	removeBuf        []removedEntry[K, V]
	removePending    atomic.Bool
}

// itemDropMode selects which policy owns an item's intrusive links.
// Sieve and LFU maintain extra metadata, so generic removal must know which
// unlink path keeps auxiliary state consistent with data.
type itemDropMode uint8

const (
	dropLRU itemDropMode = iota
	dropLFU
	dropSieve
)

// dropItem removes a resident item from data, unlinks its policy metadata, and
// returns the cacheItem to the pool. The data check rejects stale queue or hand
// pointers that may still reference an item after another path already removed
// or replaced it.
func (s *shard[K, V]) dropItem(
	item *cacheItem[K, V],
	itemPool *sync.Pool,
	statsEnabled bool,
	reason RemovalReason,
	mode itemDropMode,
) bool {
	key, ok := s.itemKey(item)
	if !ok {
		return false
	}
	if s.data[key] != item {
		return false
	}

	switch mode {
	case dropLFU:
		s.lfuList.remove(item)
		s.removeFromLRU(item)
	case dropSieve:
		if s.sieve != nil {
			if !s.sieve.remove(item) {
				return false
			}
		} else {
			s.removeFromLRU(item)
		}
	default:
		s.removeFromLRU(item)
	}

	delete(s.data, key)
	if s.stageRemoval(key, item.value, reason) {
		s.removePending.Store(true)
		signal(s.removeWake)
	}
	releaseCacheItem(itemPool, item)
	atomic.AddInt64(&s.size, -1)
	if statsEnabled && reason == RemovedCapacity {
		atomic.AddInt64(&s.evictions, 1)
	}
	return true
}

func (s *shard[K, V]) stageRemoval(key K, value V, reason RemovalReason) bool {
	if s.removeWake == nil || s.removeNotifyMask&removalNotifyBit(reason) == 0 {
		return false
	}
	s.removeBuf = append(s.removeBuf, removedEntry[K, V]{key: key, value: value, reason: reason})
	return true
}

func (s *shard[K, V]) itemKey(item *cacheItem[K, V]) (K, bool) {
	if item == nil {
		var zero K
		return zero, false
	}
	return item.key, true
}

func (s *shard[K, V]) ownsItem(item *cacheItem[K, V]) bool {
	key, ok := s.itemKey(item)
	return ok && s.data[key] == item
}

// belowSieveWarmupLocked reports the initial fill phase for bounded
// SieveTinyLFU shards. During warmup admission is unconditional so the cache can
// seed resident state before the frequency sketch starts rejecting candidates.
func (s *shard[K, V]) belowSieveWarmupLocked() bool {
	return s.cap > 0 && s.size*2 < s.cap
}

// sampleRead records a read access into the per-shard read buffer and wakes the
// write worker to drain it when a stripe fills; never touches the policy directly.
func (s *shard[K, V]) sampleRead(h uint64) {
	if s.wake == nil {
		return
	}
	if s.readBuf.sample(h) {
		signal(s.wake) // may already be pending
	}
}

// drainReadSamples replays buffered read fingerprints into the frequency sketch.
// Queue drains are serialized by drainMu so policy maintenance remains
// single-consumer even when synchronous callers help drain their shard.
func (s *shard[K, V]) drainReadSamples() {
	p := s.sieve
	if p == nil {
		return
	}
	for i := range s.readBuf.stripes {
		st := &s.readBuf.stripes[i]
		t := st.tail.Load()
		h := st.head
		if t-h > readStripeSlots {
			// producers lapped the consumer; only the most recent window survives.
			h = t - readStripeSlots
		}
		for ; h < t; h++ {
			if v := st.buf[h&readSlotMask].Swap(0); v != 0 {
				p.incrementFrequency(v)
			}
		}
		st.head = t
	}
}

func (s *shard[K, V]) initLRU() {
	s.head = &cacheItem[K, V]{}
	s.tail = &cacheItem[K, V]{}
	s.head.next = s.tail
	s.tail.prev = s.head
}

func (s *shard[K, V]) addToLRUHead(item *cacheItem[K, V]) {
	oldNext := s.head.next
	s.head.next = item
	item.next = oldNext
	item.prev = s.head
	oldNext.prev = item
}

func (s *shard[K, V]) removeFromLRU(item *cacheItem[K, V]) {
	if item.prev != nil {
		item.prev.next = item.next
	}
	if item.next != nil {
		item.next.prev = item.prev
	}
	item.prev = nil
	item.next = nil
}

func (s *shard[K, V]) moveToLRUHead(item *cacheItem[K, V]) {
	if s.head.next == item {
		return
	}
	if item.prev != nil {
		item.prev.next = item.next
	}
	if item.next != nil {
		item.next.prev = item.prev
	}

	oldNext := s.head.next
	s.head.next = item
	item.prev = s.head
	item.next = oldNext
	oldNext.prev = item
}

// cleanup expires items under the shard lock and mirrors the same policy unlink
// rules used by normal deletion. It gathers keys first so the scan phase stays
// separate from unlinking and item pooling.
func (s *shard[K, V]) cleanup(now int64, evictionPolicy EvictionPolicy, itemPool *sync.Pool, statsEnabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var keysToDelete []K
	for key, item := range s.data {
		if item.expireTime > 0 && now > item.expireTime {
			keysToDelete = append(keysToDelete, key)
		}
	}

	staged := false
	for _, key := range keysToDelete {
		if item, exists := s.data[key]; exists {
			delete(s.data, key)
			if s.stageRemoval(key, item.value, RemovedExpired) {
				staged = true
			}
			switch {
			case evictionPolicy == LFU:
				s.lfuList.remove(item)
				s.removeFromLRU(item)
			case evictionPolicy == SieveTinyLFU && s.sieve != nil:
				s.sieve.remove(item)
			default:
				s.removeFromLRU(item)
			}
			releaseCacheItem(itemPool, item)
			atomic.AddInt64(&s.size, -1)
			if statsEnabled {
				atomic.AddInt64(&s.expirations, 1)
			}
		}
	}

	if staged {
		s.removePending.Store(true)
		signal(s.removeWake)
	}
}
