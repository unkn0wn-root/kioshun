package kioshun

import (
	"sync/atomic"
	"time"
)

type writeOp uint8

const (
	writeSet writeOp = iota
	writeClear
	writeBarrier
)

// writeCommand is the shard write-queue payload.
// Plain Set keeps its hot fields inline.
// Callback data stays behind extra so the common path avoids it.
type writeCommand[K comparable, V any] struct {
	key        K
	value      V
	hash       uint64
	expireTime int64
	now        int64
	tag        uint16
	op         writeOp
	result     chan struct{}
	extra      *writeExtra[K, V]
}

// writeExtra holds the cold fields of a write command.
type writeExtra[K comparable, V any] struct {
	callback func(K, V)
}

// resultChan returns the command's completion signal or nil when none is needed.
func (cmd *writeCommand[K, V]) resultChan() chan struct{} {
	return cmd.result
}

type writeWaiter struct {
	ch chan struct{}
}

type callbackTask[K comparable, V any] struct {
	key        K
	value      V
	expireTime int64
	callback   func(K, V)
}

func (c *Cache[K, V]) set(key K, value V, ttl time.Duration, callback func(K, V)) error {
	s, cmd := c.setCommand(key, value, ttl, callback)
	return c.enqueue(s, cmd)
}

func (c *Cache[K, V]) setAndWait(key K, value V, ttl time.Duration, callback func(K, V)) error {
	s, cmd := c.setCommand(key, value, ttl, callback)
	return c.applySetSync(s, cmd)
}

func (c *Cache[K, V]) setCommand(key K, value V, ttl time.Duration, callback func(K, V)) (*shard[K, V], writeCommand[K, V]) {
	if ttl == DefaultExpiration {
		ttl = c.config.DefaultTTL
	}

	now := int64(0)
	expireTime := int64(0)
	if ttl > 0 {
		now = time.Now().UnixNano()
		expireTime = now + ttl.Nanoseconds()
	}

	kh := c.hasher.hash(key)
	cmd := writeCommand[K, V]{
		op:         writeSet,
		key:        key,
		value:      value,
		hash:       kh,
		tag:        tagFromHash(kh),
		now:        now,
		expireTime: expireTime,
	}
	if callback != nil {
		cmd.extra = &writeExtra[K, V]{callback: callback}
	}
	return c.shardByHash(kh), cmd
}

// enqueue is the async producer boundary: closed caches fail immediately and a
// full queue applies backpressure until the shard worker frees space.
func (c *Cache[K, V]) enqueue(s *shard[K, V], cmd writeCommand[K, V]) error {
	if atomic.LoadInt32(&c.closed) == 1 {
		return ErrCacheClosed
	}
	return s.queue.enqueue(cmd)
}

// awaitResult waits for a command's ack, abandoning the wait if the cache shuts
// down so callers never hang on a command that won't be processed.
func (c *Cache[K, V]) awaitResult(ch chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-c.closeCh:
		return ErrCacheClosed
	}
}

func (c *Cache[K, V]) acquireWriteWaiter() *writeWaiter {
	return c.waiterPool.Get().(*writeWaiter)
}

func (c *Cache[K, V]) releaseWriteWaiter(waiter *writeWaiter) {
	select {
	case <-waiter.ch:
	default:
	}
	c.waiterPool.Put(waiter)
}

// syncMutate is the synchronous write path shared by Set and Delete. It
// takes drainMu (the queue's single-consumer token), flushes queued writes so
// the direct mutation observes prior writes in order, then mutates under the
// shard lock. The closed-check is repeated after drainMu is held so a concurrent
// Close cannot race past the barrier into a shard being torn down.
func (c *Cache[K, V]) syncMutate(s *shard[K, V], apply func()) error {
	if atomic.LoadInt32(&c.closed) == 1 {
		return ErrCacheClosed
	}

	s.drainMu.Lock()
	defer s.drainMu.Unlock()

	if atomic.LoadInt32(&c.closed) == 1 {
		return ErrCacheClosed
	}

	c.drainShardLocked(s)

	s.mu.Lock()
	apply()
	s.mu.Unlock()
	return nil
}

func (c *Cache[K, V]) applySetSync(s *shard[K, V], cmd writeCommand[K, V]) error {
	var task *callbackTask[K, V]
	err := c.syncMutate(s, func() {
		committed := c.applySetLocked(s, cmd.key, cmd.value, cmd.expireTime, cmd.now, cmd.hash, cmd.tag)
		if committed && cmd.expireTime > 0 && cmd.extra != nil && cmd.extra.callback != nil {
			task = &callbackTask[K, V]{
				key:        cmd.key,
				value:      cmd.value,
				expireTime: cmd.expireTime,
				callback:   cmd.extra.callback,
			}
		}
	})
	if err != nil {
		return err
	}
	if task != nil {
		c.scheduleCallback(*task)
	}
	return nil
}

func (c *Cache[K, V]) deleteSync(s *shard[K, V], key K) (bool, error) {
	var deleted bool
	err := c.syncMutate(s, func() {
		deleted = c.deleteLocked(s, key)
	})
	return deleted, err
}

// Wait blocks until all writes accepted before the barrier are committed.
func (c *Cache[K, V]) Wait() error {
	return c.enqueueAllAndWait(writeBarrier)
}

func (c *Cache[K, V]) enqueueAllAndWait(op writeOp) error {
	if atomic.LoadInt32(&c.closed) == 1 {
		return ErrCacheClosed
	}
	return c.enqueueBarrierAll(op)
}

// flush drains every shard's accepted writes during shutdown
func (c *Cache[K, V]) flush() {
	_ = c.enqueueBarrierAll(writeBarrier)
}

// enqueueBarrierAll pushes a barrier command to every shard and waits for each
// ack, giving an ordered fence: all writes a shard accepted before its barrier
// are applied before this returns. Waits abandon on shutdown via awaitResult.
func (c *Cache[K, V]) enqueueBarrierAll(op writeOp) error {
	waiters := make([]*writeWaiter, 0, len(c.shards))
	for _, s := range c.shards {
		waiter := c.acquireWriteWaiter()
		cmd := writeCommand[K, V]{
			op:     op,
			now:    time.Now().UnixNano(),
			result: waiter.ch,
		}
		if err := s.queue.enqueue(cmd); err != nil {
			c.releaseWriteWaiter(waiter)
			return err
		}
		waiters = append(waiters, waiter)
	}
	for _, s := range c.shards {
		c.tryDrainShard(s)
	}
	for _, waiter := range waiters {
		if err := c.awaitResult(waiter.ch); err != nil {
			return err
		}
		c.releaseWriteWaiter(waiter)
	}
	return nil
}

func (c *Cache[K, V]) clearDirect() {
	for _, shard := range c.shards {
		shard.mu.Lock()
		c.clearShardLocked(shard)
		shard.mu.Unlock()
	}
}

func (c *Cache[K, V]) writeWorker(s *shard[K, V]) {
	defer c.workers.Done()

	for {
		select {
		case <-s.wake:
			c.drainShard(s)
		case <-c.closeCh:
			// final drain catches any writes accepted during shutdown then exit.
			c.drainShard(s)
			return
		}
	}
}

// tryDrainShard replays sampled reads, then applies queued writes in batches,
// without blocking behind another active drain. Reads replay
// first (and between batches) so admission/eviction sees current frequencies.
func (c *Cache[K, V]) tryDrainShard(s *shard[K, V]) {
	if !s.drainMu.TryLock() {
		return
	}
	c.drainShardLocked(s)
	s.drainMu.Unlock()
}

func (c *Cache[K, V]) drainShard(s *shard[K, V]) {
	s.drainMu.Lock()
	c.drainShardLocked(s)
	s.drainMu.Unlock()
}

func (c *Cache[K, V]) drainShardLocked(s *shard[K, V]) {
	batch := s.writeBatch
	if len(batch) == 0 {
		batchSize := c.config.WriteBatchSize
		if batchSize <= 0 {
			batchSize = defaultWriteBatchSize
		}
		batch = make([]writeCommand[K, V], batchSize)
	}

	s.drainReadSamples()
	for {
		n := s.queue.tryDequeue(batch)
		if n == 0 {
			return
		}
		c.applyWriteBatch(s, batch[:n])
		s.drainReadSamples()
	}
}

func (c *Cache[K, V]) applyWriteBatch(s *shard[K, V], batch []writeCommand[K, V]) {
	var ackBuf [8]chan struct{}
	acks := ackBuf[:0]
	var callbacks []callbackTask[K, V]

	s.mu.Lock()
	for i := range batch {
		cmd := &batch[i]
		switch cmd.op {
		case writeSet:
			committed := c.applySetLocked(s, cmd.key, cmd.value, cmd.expireTime, cmd.now, cmd.hash, cmd.tag)
			if committed && cmd.expireTime > 0 && cmd.extra != nil && cmd.extra.callback != nil {
				callbacks = append(callbacks, callbackTask[K, V]{
					key:        cmd.key,
					value:      cmd.value,
					expireTime: cmd.expireTime,
					callback:   cmd.extra.callback,
				})
			}
			if ch := cmd.resultChan(); ch != nil {
				acks = append(acks, ch)
			}
		case writeClear:
			c.clearShardLocked(s)
			if ch := cmd.resultChan(); ch != nil {
				acks = append(acks, ch)
			}
		case writeBarrier:
			if ch := cmd.resultChan(); ch != nil {
				acks = append(acks, ch)
			}
		}
	}
	s.mu.Unlock()

	for _, task := range callbacks {
		c.scheduleCallback(task)
	}
	for _, ch := range acks {
		ch <- struct{}{}
	}
}

func (c *Cache[K, V]) applySetLocked(
	shard *shard[K, V],
	key K,
	value V,
	expireTime int64,
	now int64,
	kh uint64,
	tag uint16,
) bool {
	if ex, exists := shard.data[key]; exists {
		ex.value = value
		ex.lastAccess = now
		ex.expireTime = expireTime
		ex.hash = kh
		ex.tag = tag

		switch c.config.EvictionPolicy {
		case LRU:
			shard.moveToLRUHead(ex)
		case LFU:
			shard.lfuList.remove(ex)
			ex.lfuFreq = 1
			shard.lfuList.add(ex)
		case SieveTinyLFU:
			ex.lfuFreq = 0
			if shard.sieve != nil && !shard.belowSieveWarmupLocked() {
				shard.sieve.recordUpdate(ex)
			}
		}
		return true
	}

	if c.config.EvictionPolicy == SieveTinyLFU && shard.sieve != nil {
		warmup := shard.belowSieveWarmupLocked()
		if !warmup {
			shard.sieve.recordAccess(kh)
		}

		item := acquireCacheItem[K, V](&c.itemPool)
		item.value = value
		item.lastAccess = now
		item.lfuFreq = 0
		item.key = key
		item.hash = kh
		item.tag = tag
		item.expireTime = expireTime

		shard.data[key] = item
		atomic.AddInt64(&shard.size, 1)

		ghostHit := !warmup && shard.sieve.ghost.contains(kh, tag)
		shard.sieve.insert(item, ghostHit)
		if !warmup {
			shard.enforceSieveCapacity(&c.itemPool, c.config.StatsEnabled, item, ghostHit)
		}
		if _, ok := shard.data[key]; ok {
			shard.sieve.stats.Admits++
			return true
		}
		shard.sieve.stats.Rejects++
		return false
	}

	// non-Sieve policies evict before inserting so the new item is not selected.
	if shard.cap > 0 && atomic.LoadInt64(&shard.size) >= shard.cap {
		c.evictor.evict(shard, &c.itemPool, c.config.StatsEnabled)
	}

	item := acquireCacheItem[K, V](&c.itemPool)
	item.value = value
	item.lastAccess = now
	item.lfuFreq = 0
	item.key = key
	item.hash = kh
	item.tag = tag
	item.expireTime = expireTime

	shard.data[key] = item
	shard.addToLRUHead(item)

	if c.config.EvictionPolicy == LFU {
		shard.lfuList.add(item)
	}

	atomic.AddInt64(&shard.size, 1)
	return true
}

func (c *Cache[K, V]) deleteLocked(shard *shard[K, V], key K) bool {
	item, exists := shard.data[key]
	if !exists {
		return false
	}
	c.removeItem(shard, item, false)
	return true
}

func (c *Cache[K, V]) clearShardLocked(shard *shard[K, V]) {
	for _, item := range shard.data {
		releaseCacheItem(&c.itemPool, item)
	}
	shard.data = make(map[K]*cacheItem[K, V])
	shard.initLRU()
	if c.config.EvictionPolicy == LFU {
		shard.lfuList = newLFUList[K, V]()
	}
	if c.config.EvictionPolicy == SieveTinyLFU && shard.sieve != nil {
		shard.sieve.reset()
	}
	atomic.StoreInt64(&shard.size, 0)
}

func (c *Cache[K, V]) scheduleCallback(task callbackTask[K, V]) {
	delay := time.Duration(task.expireTime - time.Now().UnixNano())
	if delay < 0 {
		delay = 0
	}

	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-timer.C:
			shard := c.getShard(task.key)
			shard.mu.RLock()
			item, exists := shard.data[task.key]
			if exists && item.expireTime > 0 && time.Now().UnixNano() > item.expireTime {
				shard.mu.RUnlock()
				task.callback(task.key, task.value)
			} else {
				shard.mu.RUnlock()
			}
		case <-c.closeCh:
			return
		}
	}()
}
