package kioshun

import "sync/atomic"

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

// WithOnRemove registers a listener for capacity eviction, admission rejection,
// expiration, and deletion. Clear and value replacement do not call it. Only
// capacity removals increase Stats().Evictions.
func WithOnRemove[K comparable, V any](listener func(key K, value V, reason RemovalReason)) Option[K, V] {
	return func(c *Cache[K, V]) { c.onRemove = listener }
}

// WithOnEvict registers a listener for entries removed to stay within capacity.
func WithOnEvict[K comparable, V any](listener func(key K, value V)) Option[K, V] {
	return func(c *Cache[K, V]) { c.onEvict = listener }
}

type removedEntry[K comparable, V any] struct {
	key    K
	value  V
	reason RemovalReason
}

// maxRetainedRemovals caps the capacity of a staging buffer kept for reuse.
const maxRetainedRemovals = 256

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

// removalWake wakes the notify worker. Only the producer that sets signaled
// sends, and wait clears it after each receive, so the send never blocks.
// Keep it a plain send. A failed non-blocking send gives no ordering.
type removalWake struct {
	signaled atomic.Bool
	ch       chan struct{}
}

func (w *removalWake) signal() {
	if !w.signaled.Load() && w.signaled.CompareAndSwap(false, true) {
		w.ch <- struct{}{}
	}
}

// wait reports whether a wake-up arrived before done was closed. It clears
// signaled only after a wake-up, since one may still be queued at close.
func (w *removalWake) wait(done <-chan struct{}) bool {
	select {
	case <-w.ch:
		w.signaled.Store(false)
		return true
	case <-done:
		return false
	}
}

// removeNotifyWorker calls listeners without holding a shard lock, allowing a
// listener to use the cache safely. On close it delivers what is still staged.
func (c *Cache[K, V]) removeNotifyWorker() {
	defer c.workers.Done()
	var spare []removedEntry[K, V]
	for c.removeWake.wait(c.closeCh) {
		spare = c.drainRemovals(spare)
	}
	c.drainRemovals(spare)
}

// drainRemovals delivers all staged removals. Each pending shard gets the empty
// spare buffer, and its delivered buffer becomes the next spare.
func (c *Cache[K, V]) drainRemovals(spare []removedEntry[K, V]) []removedEntry[K, V] {
	for _, s := range c.shards {
		if !s.removePending.Load() {
			continue
		}

		s.mu.Lock()
		buf := s.removeBuf
		s.removeBuf = spare
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

		// Drop a buffer grown by a burst. Clear a kept one so it does not pin
		// delivered values.
		if cap(buf) > maxRetainedRemovals {
			buf = nil
		}
		clear(buf)
		spare = buf[:0]
	}
	return spare
}
