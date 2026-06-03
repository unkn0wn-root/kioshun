package kioshun

// evictedEntry stages a removed (key, value) pair for async delivery to
// the onEvict listener; evictNotifyWorker drains them once the lock is released.
type evictedEntry[K comparable, V any] struct {
	key   K
	value V
}

// evictNotifyWorker delivers buffered eviction notifications to the onEvict
// listener. It runs only when a listener is configured. The worker holds no
// shard lock while invoking the listener, so the listener may re-enter the cache
// without risking deadlock or reentrancy on the shard mutex.
func (c *Cache[K, V]) evictNotifyWorker() {
	defer c.workers.Done()
	for {
		select {
		case <-c.evictWake:
			c.drainEvictions()
		case <-c.closeCh:
			// deliver whatever was staged before shutdown, then exit. Close waits
			// on this worker before clearing shards, so no eviction can be staged
			// after this final drain returns.
			c.drainEvictions()
			return
		}
	}
}

// drainEvictions hands each shard's staged removals to the listener.
func (c *Cache[K, V]) drainEvictions() {
	for _, s := range c.shards {
		if !s.evictPending.Load() {
			continue
		}

		s.mu.Lock()
		buf := s.evictBuf
		s.evictBuf = nil
		s.evictPending.Store(false)
		s.mu.Unlock()

		for i := range buf {
			c.onEvict(buf[i].key, buf[i].value)
		}
	}
}
