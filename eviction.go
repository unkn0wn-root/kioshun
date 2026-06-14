package kioshun

// RemovalReason explains why a key left the cache in an OnRemove notification.
type RemovalReason uint8

const (
	RemovedCapacity RemovalReason = iota
	RemovedRejected
	RemovedExpired
	RemovedDeleted
)

// String returns a lowercase label for the reason.
func (r RemovalReason) String() string {
	switch r {
	case RemovedCapacity:
		return "capacity"
	case RemovedRejected:
		return "rejected"
	case RemovedExpired:
		return "expired"
	case RemovedDeleted:
		return "deleted"
	default:
		return "unknown"
	}
}

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

type removalNotifyMask uint8

// each reason's notify bit is its position in the RemovalReason enum.
const (
	notifyRemovedCapacity removalNotifyMask = 1 << RemovedCapacity
	notifyRemovedRejected removalNotifyMask = 1 << RemovedRejected
	notifyRemovedExpired  removalNotifyMask = 1 << RemovedExpired
	notifyRemovedDeleted  removalNotifyMask = 1 << RemovedDeleted
	notifyAllRemovals                       = notifyRemovedCapacity | notifyRemovedRejected | notifyRemovedExpired | notifyRemovedDeleted
)

func removalNotifyBit(reason RemovalReason) removalNotifyMask {
	return removalNotifyMask(1) << reason
}

// WithOnRemove registers a listener invoked once for every key removed from the
// cache, with a RemovalReason describing why: RemovedCapacity (a resident was
// displaced to stay within capacity), RemovedRejected (SieveTinyLFU declined to
// keep a candidate), RemovedExpired (TTL) or RemovedDeleted (Delete). It is not
// called for Clear or when an existing key's value is replaced. Only
// RemovedCapacity removals are counted in Stats().Evictions, so the notification
// volume can legitimately exceed that counter.
func WithOnRemove[K comparable, V any](listener func(key K, value V, reason RemovalReason)) Option[K, V] {
	return func(c *Cache[K, V]) { c.onRemove = listener }
}

// WithOnEvict registers a listener invoked only when an existing cache entry is
// displaced to keep the shard within capacity. It is a narrow convenience for
// callers that do not need delete, expiry or admission rejection notifications.
func WithOnEvict[K comparable, V any](listener func(key K, value V)) Option[K, V] {
	return func(c *Cache[K, V]) { c.onEvict = listener }
}

// removedEntry stages a removed (key, value, reason) for async delivery to
// removal listeners; removeNotifyWorker drains them once the lock is released.
type removedEntry[K comparable, V any] struct {
	key    K
	value  V
	reason RemovalReason
}

// listenerNotifyMask reports which removal reasons need staging for the
// configured listeners.
func (c *Cache[K, V]) listenerNotifyMask() removalNotifyMask {
	var mask removalNotifyMask
	if c.onRemove != nil {
		mask |= notifyAllRemovals
	}
	if c.onEvict != nil {
		mask |= notifyRemovedCapacity
	}
	return mask
}

// removeNotifyWorker delivers buffered removal notifications to configured
// listeners. It runs only when a listener is configured. The worker holds no
// shard lock while invoking the listener so the listener may reenter the cache
// without risking deadlock or reentrancy on the shard mutex.
func (c *Cache[K, V]) removeNotifyWorker() {
	defer c.workers.Done()
	for {
		select {
		case <-c.removeWake:
			c.drainRemovals()
		case <-c.closeCh:
			c.drainRemovals()
			return
		}
	}
}

// drainRemovals hands each shard's staged removals to configured listeners.
func (c *Cache[K, V]) drainRemovals() {
	for _, s := range c.shards {
		if !s.removePending.Load() {
			continue
		}

		s.mu.Lock()
		buf := s.removeBuf
		s.removeBuf = nil
		s.removePending.Store(false)
		s.mu.Unlock()

		for _, e := range buf {
			if c.onRemove != nil {
				c.onRemove(e.key, e.value, e.reason)
			}
			if e.reason == RemovedCapacity && c.onEvict != nil {
				c.onEvict(e.key, e.value)
			}
		}
	}
}
