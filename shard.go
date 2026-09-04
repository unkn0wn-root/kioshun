package kioshun

import (
	"math/bits"
	"sync"
	"sync/atomic"
)

// cacheItem stores a value and the links used by the eviction policies.
//
// Fields read without a lock are immutable after publication. A lock-free reader
// may retain an item after eviction, so updates allocate a new item and the
// garbage collector reclaims old ones.
type cacheItem[K comparable, V any] struct {
	value      V
	expireTime int64 // monotonic nanoseconds since cache creation; 0 means no expiry
	cost       int64
	prev       *cacheItem[K, V]
	next       *cacheItem[K, V]
	key        K
	hash       uint64
	queue      sieveQueueID
	queueOwner uint8
	reuse      uint8
	// unpublished marks a candidate in a SIEVE queue but not yet in the table.
	unpublished bool
	visited     uint32
}

type shard[K comparable, V any] struct {
	// These fields share a cache line because every lock-free Get reads them.
	tab   *htable[K, V]
	sieve *sieveTinyLFU[K, V]
	cap   int64 // resident item limit for this shard; 0 => unlimited
	_     [cacheLinePadding]byte

	mu      sync.RWMutex // serializes writers and scans
	stats   *stats
	costCap int64 // resident cost limit for this shard; 0 => unlimited
	queue   *mpscQueue[K, V]

	// wake has capacity one and is shared by writes and full read-sample rings.
	wake chan struct{}

	// drainMu ensures there is only one queue consumer.
	drainMu    sync.Mutex
	writeBatch []writeCommand[K, V]

	// SieveTinyLFU readers append key hashes here without taking the shard lock.
	readBuf readBuffer

	// Shared list for LRU, LFU, and FIFO. head.next is newest; tail.prev is oldest.
	head *cacheItem[K, V]
	tail *cacheItem[K, V]

	lfuList *lfuList[K, V]

	size int64 // live items
	cost int64 // live item cost

	// Removed entries are buffered under mu and delivered after releasing it.
	// removeWake is nil when no listener is registered.
	removeWake       chan struct{}
	removeNotifyMask removalNotifyMask
	removeBuf        []removedEntry[K, V]
	removePending    atomic.Bool
}

// itemDropMode selects the policy state that must be unlinked with an item.
type itemDropMode uint8

const (
	dropLRU itemDropMode = iota
	dropLFU
	dropSieve
)

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

// dropItem removes an item from the table and its eviction policy. removeExact
// protects against stale policy pointers.
func (s *shard[K, V]) dropItem(
	item *cacheItem[K, V],
	statsEnabled bool,
	reason RemovalReason,
	mode itemDropMode,
) bool {
	if item == nil {
		return false
	}
	// An unpublished candidate has policy state but no table slot.
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

// belowSieveWarmup reports whether admission is still unconditional.
func (s *shard[K, V]) belowSieveWarmup() bool {
	return s.cap > 0 && atomic.LoadInt64(&s.size)*2 < s.cap
}

// sampleRead buffers a read. When its ring fills, the reader drains it if the
// consumer lock is free; otherwise it wakes the worker.
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
	signal(s.wake)
}

// drainReadSamples adds pending read hashes to the frequency sketch. A dirty bit
// stays set while its ring remains busy.
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

// drainStripe adds the newest ring window to the sketch and drops older samples.
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

// cleanup collects expired items, then removes them through the normal policy path.
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
