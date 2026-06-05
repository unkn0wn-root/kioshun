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

// inlineAckBuf sizes the stack allocated ack slice so small batches avoid a heap alloc.
const inlineAckBuf = 8

// writeCommand is the shard write-queue payload.
// Plain Set keeps its hot fields inline.
// Callback data stays behind extra so the common path avoids it.
type writeCommand[K comparable, V any] struct {
	key        K
	value      V
	hash       uint64
	expireTime int64
	now        int64
	cost       int64
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

// newCallbackTask returns the expiry callback task for a committed Set, with
// ok=false when the command did not commit, never expires, or has no callback.
func (cmd *writeCommand[K, V]) newCallbackTask(committed bool) (callbackTask[K, V], bool) {
	if !committed || cmd.expireTime <= 0 || cmd.extra == nil || cmd.extra.callback == nil {
		return callbackTask[K, V]{}, false
	}
	return callbackTask[K, V]{
		key:        cmd.key,
		value:      cmd.value,
		expireTime: cmd.expireTime,
		callback:   cmd.extra.callback,
	}, true
}

func (c *Cache[K, V]) set(key K, value V, ttl time.Duration, callback func(K, V)) error {
	s, cmd, err := c.setCommand(key, value, ttl, callback)
	if err != nil {
		return err
	}
	if c.tryApplyInline(s, &cmd) {
		return nil
	}
	return c.enqueue(s, cmd)
}

// tryApplyInline opportunistically applies a Set synchronously when the shard is
// completely uncontended: the drain token is free, the write queue is fully
// quiescent (no slot reserved or published), and the shard lock is immediately
// available. Applying inline gives immediate read-after-write visibility and skips
// the async handoff, which otherwise lets a re-read miss the not-yet-applied write
// and enqueue a redundant Set.
//
// Holding the drain token makes this the sole shard consumer, and quiescent rules
// out any accepted-but-unpublished write, so applying ahead of the queue cannot
// reorder against a queued write. It mirrors the worker's drain order - buffered
// reads are replayed into the frequency sketch before admission - so an inline Set
// makes the same SieveTinyLFU decision a queued one would.
//
// Every acquisition is non-blocking (TryLock), so this never delays SetAsync: any
// contention - a busy drain worker, queued or in-flight writes, or an active
// reader/writer on the shard - makes it return false and leave the write for the
// async queue, preserving both the enqueue-only SetAsync contract and the batching
// that amortizes eviction on write-heavy shards.
func (c *Cache[K, V]) tryApplyInline(s *shard[K, V], cmd *writeCommand[K, V]) bool {
	if !s.drainMu.TryLock() {
		return false
	}
	if c.isClosed() || !s.queue.quiescent() || !s.mu.TryLock() {
		s.drainMu.Unlock()
		return false
	}

	// Replay buffered reads into the sketch before admission so the inline apply
	// makes the same SieveTinyLFU decision a queued write would. This runs only
	// after winning s.mu, so a SetAsync that cannot commit inline bails to the
	// queue cheaply; the worker drains read samples before applying it anyway.
	s.drainReadSamples()
	committed := c.applySet(s, cmd)
	s.mu.Unlock()
	s.drainMu.Unlock()

	if task, ok := cmd.newCallbackTask(committed); ok {
		c.scheduleCallback(task)
	}
	return true
}

func (c *Cache[K, V]) setAndWait(key K, value V, ttl time.Duration, callback func(K, V)) error {
	s, cmd, err := c.setCommand(key, value, ttl, callback)
	if err != nil {
		return err
	}
	return c.applySetSync(s, cmd)
}

func (c *Cache[K, V]) setCommand(key K, value V, ttl time.Duration, callback func(K, V)) (*shard[K, V], writeCommand[K, V], error) {
	if ttl == DefaultExpiration {
		ttl = c.config.DefaultTTL
	}

	var now, expireTime int64
	if ttl > 0 {
		now = time.Now().UnixNano()
		expireTime = now + ttl.Nanoseconds()
	}

	kh := c.hasher.Sum(key)
	cost, err := c.itemCost(key, value)
	if err != nil {
		return nil, writeCommand[K, V]{}, err
	}
	shard := c.shardByHash(kh)
	if shard.costCap > 0 && cost > shard.costCap {
		return nil, writeCommand[K, V]{}, ErrItemTooLarge
	}
	cmd := writeCommand[K, V]{
		op:         writeSet,
		key:        key,
		value:      value,
		hash:       kh,
		now:        now,
		expireTime: expireTime,
		cost:       cost,
	}
	if callback != nil {
		cmd.extra = &writeExtra[K, V]{callback: callback}
	}
	return shard, cmd, nil
}

func (c *Cache[K, V]) itemCost(key K, value V) (int64, error) {
	if !c.trackCost {
		return 0, nil
	}
	cost := int64(1)
	if c.weigher != nil {
		cost = c.weigher(key, value)
	}
	if cost < 0 {
		return 0, ErrInvalidCost
	}
	if cost == 0 {
		return 0, nil
	}
	return cost, nil
}

// enqueue is the async producer boundary: closed caches fail immediately and a
// full queue applies backpressure until the shard worker frees space.
func (c *Cache[K, V]) enqueue(s *shard[K, V], cmd writeCommand[K, V]) error {
	if c.isClosed() {
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
	if c.isClosed() {
		return ErrCacheClosed
	}

	s.drainMu.Lock()
	defer s.drainMu.Unlock()

	if c.isClosed() {
		return ErrCacheClosed
	}

	c.drainShardQueue(s)

	s.mu.Lock()
	apply()
	s.mu.Unlock()
	return nil
}

func (c *Cache[K, V]) applySetSync(s *shard[K, V], cmd writeCommand[K, V]) error {
	var task callbackTask[K, V]
	var hasTask bool
	err := c.syncMutate(s, func() {
		committed := c.applySet(s, &cmd)
		task, hasTask = cmd.newCallbackTask(committed)
	})
	if err != nil {
		return err
	}
	if hasTask {
		c.scheduleCallback(task)
	}
	return nil
}

func (c *Cache[K, V]) deleteSync(s *shard[K, V], key K) (bool, error) {
	var deleted bool
	err := c.syncMutate(s, func() {
		deleted = c.deleteKey(s, key)
	})
	return deleted, err
}

// Sync blocks until all writes accepted before the barrier are committed.
func (c *Cache[K, V]) Sync() error {
	return c.enqueueAllAndWait(writeBarrier)
}

func (c *Cache[K, V]) enqueueAllAndWait(op writeOp) error {
	if c.isClosed() {
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
	for _, s := range c.shards {
		s.mu.Lock()
		c.clearShard(s)
		s.mu.Unlock()
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
	c.drainShardQueue(s)
	s.drainMu.Unlock()
}

func (c *Cache[K, V]) drainShard(s *shard[K, V]) {
	s.drainMu.Lock()
	c.drainShardQueue(s)
	s.drainMu.Unlock()
}

// drainShardQueue consumes read samples and queued writes. The caller must hold
// s.drainMu so there is only one shard consumer.
func (c *Cache[K, V]) drainShardQueue(s *shard[K, V]) {
	batch := s.writeBatch

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
	var ackBuf [inlineAckBuf]chan struct{}
	acks := ackBuf[:0]
	var callbacks []callbackTask[K, V]

	s.mu.Lock()
	for i := range batch {
		cmd := &batch[i]
		switch cmd.op {
		case writeSet:
			committed := c.applySet(s, cmd)
			if t, ok := cmd.newCallbackTask(committed); ok {
				callbacks = append(callbacks, t)
			}
			if ch := cmd.resultChan(); ch != nil {
				acks = append(acks, ch)
			}
		case writeClear:
			c.clearShard(s)
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

// newItem returns a pooled, populated item for cmd. acquireCacheItem zeroes the
// node, so list links and lfuFreq start clean.
func (c *Cache[K, V]) newItem(cmd *writeCommand[K, V]) *cacheItem[K, V] {
	item := acquireCacheItem[K, V](&c.itemPool)
	item.value = cmd.value
	item.lastAccess = cmd.now
	item.key = cmd.key
	item.hash = cmd.hash
	item.expireTime = cmd.expireTime
	item.cost = cmd.cost
	return item
}

// applySet mutates shard contents and policy state. The caller must hold s.mu.
func (c *Cache[K, V]) applySet(s *shard[K, V], cmd *writeCommand[K, V]) bool {
	cmdCost := cmd.cost
	if ex, exists := s.data[cmd.key]; exists {
		costDelta := cmdCost - ex.cost
		ex.value = cmd.value
		ex.lastAccess = cmd.now
		ex.expireTime = cmd.expireTime
		ex.hash = cmd.hash
		if costDelta != 0 {
			ex.cost = cmdCost
			atomic.AddInt64(&s.cost, costDelta)
		}

		switch c.config.EvictionPolicy {
		case LRU:
			s.moveToLRUHead(ex)
		case LFU:
			s.lfuList.remove(ex)
			ex.lfuFreq = 1
			s.lfuList.add(ex)
		case SieveTinyLFU:
			ex.lfuFreq = 0
			if s.sieve != nil && !s.belowSieveWarmup() {
				s.sieve.recordUpdate(ex)
			}
		}
		c.enforcePostUpdateCapacity(s)
		return true
	}

	if c.config.EvictionPolicy == SieveTinyLFU && s.sieve != nil {
		warmup := s.belowSieveWarmup()
		if !warmup {
			s.sieve.recordAccess(cmd.hash)
		}

		item := c.newItem(cmd)
		s.data[cmd.key] = item
		atomic.AddInt64(&s.size, 1)
		if item.cost != 0 {
			atomic.AddInt64(&s.cost, item.cost)
		}

		ghostHit := !warmup && s.sieve.ghost.contains(cmd.hash)
		s.sieve.insert(item, ghostHit)
		if !warmup || s.overCapacity() {
			s.enforceSieveCapacity(&c.itemPool, c.config.StatsEnabled, item, ghostHit)
		}
		if _, ok := s.data[cmd.key]; ok {
			s.sieve.stats.Admits++
			return true
		}
		s.sieve.stats.Rejects++
		return false
	}

	// non-Sieve policies evict before inserting so the new item is not selected.
	for s.wouldOverCapacity(cmdCost) && len(s.data) > 0 {
		c.evictor.evict(s, &c.itemPool, c.config.StatsEnabled)
	}

	item := c.newItem(cmd)
	s.data[cmd.key] = item
	s.addToLRUHead(item)

	if c.config.EvictionPolicy == LFU {
		s.lfuList.add(item)
	}

	atomic.AddInt64(&s.size, 1)
	if item.cost != 0 {
		atomic.AddInt64(&s.cost, item.cost)
	}
	return true
}

// enforcePostUpdateCapacity restores capacity after updating an existing item.
// The caller must hold s.mu.
func (c *Cache[K, V]) enforcePostUpdateCapacity(s *shard[K, V]) {
	if !s.overCapacity() {
		return
	}
	if c.config.EvictionPolicy == SieveTinyLFU && s.sieve != nil {
		s.enforceSieveCapacity(&c.itemPool, c.config.StatsEnabled, nil, false)
		return
	}
	for s.overCapacity() && len(s.data) > 0 {
		c.evictor.evict(s, &c.itemPool, c.config.StatsEnabled)
	}
}

// deleteKey removes key from a shard. The caller must hold s.mu.
func (c *Cache[K, V]) deleteKey(s *shard[K, V], key K) bool {
	item, exists := s.data[key]
	if !exists {
		return false
	}
	c.removeItem(s, item, RemovedDeleted)
	return true
}

// clearShard resets a shard's contents and policy state. The caller must hold s.mu.
func (c *Cache[K, V]) clearShard(s *shard[K, V]) {
	for _, item := range s.data {
		releaseCacheItem(&c.itemPool, item)
	}
	s.data = make(map[K]*cacheItem[K, V])
	s.initLRU()
	if c.config.EvictionPolicy == LFU {
		s.lfuList = newLFUList[K, V]()
	}
	if c.config.EvictionPolicy == SieveTinyLFU && s.sieve != nil {
		s.sieve.reset()
	}
	atomic.StoreInt64(&s.size, 0)
	atomic.StoreInt64(&s.cost, 0)
}

func (c *Cache[K, V]) scheduleCallback(task callbackTask[K, V]) {
	delay := max(time.Duration(task.expireTime-time.Now().UnixNano()), 0)

	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-timer.C:
			s := c.getShard(task.key)
			s.mu.RLock()
			item, exists := s.data[task.key]
			if exists && item.expireTime > 0 && time.Now().UnixNano() > item.expireTime {
				s.mu.RUnlock()
				task.callback(task.key, task.value)
			} else {
				s.mu.RUnlock()
			}
		case <-c.closeCh:
			return
		}
	}()
}
