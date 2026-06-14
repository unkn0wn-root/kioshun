package kioshun

import (
	"math/bits"
	"sync"
	"sync/atomic"
)

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
	// unpublished marks a SieveTinyLFU candidate that is live in the policy
	// queues but not yet stored in the table: admission is still deciding its
	// fate, so there is no slot to reclaim on removal.
	unpublished bool
	visited     uint32
}

type shard[K comparable, V any] struct {
	mu      sync.RWMutex // serializes writers (and cold scans); reads of tab are lock-free
	tab     *htable[K, V]
	stats   *stats // shared per-P telemetry
	cap     int64  // resident item limit for this shard; 0 => unlimited
	costCap int64  // resident cost limit for this shard; 0 => unlimited

	queue *mpscQueue[K, V] // async mutation transport consumed by this shard's worker

	// worker's wake-up: producers ping it after enqueuing a
	// write and read sampling pings it when a stripe fills. Buffered size 1.
	wake chan struct{}

	// keeps the MPSC queue single consumer while allowing synchronous
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

	lfuList *lfuList[K, V] // allocated only for pure LFU policy.

	size int64 // live items
	cost int64 // live item cost

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

// dropModeFor maps an eviction policy to the unlink path that keeps its
// auxiliary state consistent.
func dropModeFor(policy EvictionPolicy) itemDropMode {
	switch policy {
	case LFU:
		return dropLFU
	case SieveTinyLFU:
		return dropSieve
	default:
		return dropLRU
	}
}

// dropItem removes a resident item from the table and unlinks its policy
// metadata. removeExact both rejects stale queue or hand pointers (it only
// removes when the slot still holds exactly this item) and frees the slot in a
// single probe, so the downstream unlinks always operate on a confirmed resident.
// The item itself is not recycled: a lock-free reader may still hold it, so the
// GC reclaims it.
func (s *shard[K, V]) dropItem(
	item *cacheItem[K, V],
	statsEnabled bool,
	reason RemovalReason,
	mode itemDropMode,
) bool {
	if item == nil {
		return false
	}
	// an unpublished SieveTinyLFU candidate is live in the policy queues but was
	// never stored, so it has no table slot to reclaim; unlink policy-only. Every
	// other item is a confirmed resident and removeExact both rejects stale
	// queue/hand pointers and frees the slot.
	if item.unpublished {
		item.unpublished = false
	} else if !s.tab.removeExact(item) {
		return false
	}

	switch mode {
	case dropLFU:
		s.lfuList.remove(item)
		s.removeFromLRU(item)
	case dropSieve:
		if s.sieve != nil {
			s.sieve.remove(item)
		} else {
			s.removeFromLRU(item)
		}
	default:
		s.removeFromLRU(item)
	}

	if s.stageRemoval(item.key, item.value, reason) {
		s.removePending.Store(true)
		signal(s.removeWake)
	}
	if item.cost != 0 {
		atomic.AddInt64(&s.cost, -item.cost)
	}
	atomic.AddInt64(&s.size, -1)
	if statsEnabled && reason == RemovedCapacity {
		s.stats.recordEviction()
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

// belowSieveWarmup reports the initial fill phase for bounded SieveTinyLFU
// shards. During warmup admission is unconditional so the cache can seed resident
// state before the frequency sketch starts rejecting candidates.
func (s *shard[K, V]) belowSieveWarmup() bool {
	return s.cap > 0 && atomic.LoadInt64(&s.size)*2 < s.cap
}

// sampleRead records a read access into the per-shard read buffer. sample()
// reports backlog when the consumer has fallen a full window behind; the reader
// then drains that one stripe itself if it can claim the single-consumer token
// without blocking (bounding inline work to a single stripe), otherwise it wakes
// the worker. Below that backlog this is just the buffer append.
func (s *shard[K, V]) sampleRead(h, id uint64) {
	if s.wake == nil {
		return
	}
	stripe, needDrain := s.readBuf.sample(h, id)
	if !needDrain {
		return
	}
	if s.drainMu.TryLock() {
		s.drainStripe(s.sieve, &s.readBuf.stripes[stripe])
		s.drainMu.Unlock()
		return
	}
	signal(s.wake) // may already be pending
}

// drainReadSamples replays the dirty stripes buffered read fingerprints into
// the frequency sketch. The dirty mask keeps the usual quiescent check to a
// single load. A bit is cleared only once its stripe turns out to be quiet,
// so a busy stripe costs no mask writes and a producer racing the clear
// rearms the bit on its next sample.
func (s *shard[K, V]) drainReadSamples() {
	p := s.sieve
	if p == nil {
		return
	}
	d := s.readBuf.dirty.Load()
	for d != 0 {
		i := bits.TrailingZeros32(d)
		d &^= 1 << i
		st := &s.readBuf.stripes[i]
		if st.tail.Load() == st.head.Load() {
			s.readBuf.dirty.And(^(uint32(1) << i))
			continue
		}
		s.drainStripe(p, st)
	}
}

// drainStripe replays one stripe's fingerprints into the sketch.
// When producers have lapped the consumer only the most recent window survives.
// Older samples are dropped.
func (s *shard[K, V]) drainStripe(p *sieveTinyLFU[K, V], st *readStripe) {
	t := st.tail.Load()
	h := st.head.Load()
	if t == h {
		return
	}
	if t-h > readStripeSlots {
		h = t - readStripeSlots
	}
	for ; h < t; h++ {
		if v := st.buf[h&readSlotMask].Swap(0); v != 0 {
			p.incrementFrequency(v)
		}
	}
	st.head.Store(t)
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
	s.removeFromLRU(item)
	s.addToLRUHead(item)
}

// cleanup expires items under the shard lock through the same dropItem path as
// normal deletion. It collects the expired items first so the scan phase stays
// separate from unlinking and removal.
func (s *shard[K, V]) cleanup(now int64, evictionPolicy EvictionPolicy, statsEnabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var expired []*cacheItem[K, V]
	s.tab.forEach(func(item *cacheItem[K, V]) bool {
		if item.expireTime > 0 && now > item.expireTime {
			expired = append(expired, item)
		}
		return true
	})

	mode := dropModeFor(evictionPolicy)
	for _, item := range expired {
		if !s.dropItem(item, statsEnabled, RemovedExpired, mode) {
			continue
		}
		if statsEnabled {
			s.stats.recordExpiration()
		}
	}
}
