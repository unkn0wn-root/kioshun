package kioshun

import (
	"fmt"
	"math"
	"sync/atomic"
	"time"
)

type writeOp uint8

const (
	writeSet writeOp = iota
	writeClear
	writeBarrier
	writeFlush // Close's barrier
)

// inlineAckBuf keeps acknowledgements for small batches on the stack.
const inlineAckBuf = 8

// writeCommand keeps fields used by every Set inline. Callback data is optional.
type writeCommand[K comparable, V any] struct {
	key        K
	value      V
	hash       uint64
	expireTime int64
	cost       int64
	op         writeOp
	result     chan struct{}
	extra      *writeExtra[K, V]
}

type writeExtra[K comparable, V any] struct {
	callback func(K, V)
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

// newCallbackTask returns a task only for a committed Set with an expiry callback.
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

// tryApplyInline applies a Set immediately when the queue and shard are idle. This
// gives read-after-write visibility without making SetAsync wait under contention.
//
// The drain lock makes this the only consumer, and the second empty check rules out
// a producer that reserved a slot in the meantime. Read samples are drained first
// to preserve the worker's admission order. Every lock attempt is non-blocking.
func (c *Cache[K, V]) tryApplyInline(s *shard[K, V], cmd *writeCommand[K, V]) bool {
	if !s.queue.quiescent() {
		return false
	}
	if !s.drainMu.TryLock() {
		return false
	}
	if c.isClosed() || !s.queue.quiescent() || !s.mu.TryLock() {
		s.drainMu.Unlock()
		return false
	}
	s.drainReadSamples()
	if cmd.expireTime > 0 {
		c.stampExpireTime(cmd, c.nowNano())
	}
	committed := c.applySet(s, cmd)
	s.mu.Unlock()
	s.drainMu.Unlock()

	if task, ok := cmd.newCallbackTask(committed); ok {
		c.scheduleCallback(s, task)
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

func (c *Cache[K, V]) setCommand(
	key K,
	value V,
	ttl time.Duration,
	callback func(K, V),
) (*shard[K, V], writeCommand[K, V], error) {
	if ttl == DefaultExpiration {
		ttl = c.config.DefaultTTL
	}

	// A negative expireTime marks an entry that is already expired.
	var expireTime int64
	switch {
	case ttl > 0:
		expireTime = ttl.Nanoseconds()
	case ttl < 0 && ttl != NoExpiration:
		expireTime = -1
	}

	kh := c.hasher.Sum(key)
	shard := c.shardByHash(kh)
	cmd := writeCommand[K, V]{
		op:         writeSet,
		key:        key,
		value:      value,
		hash:       kh,
		expireTime: expireTime,
	}
	// An already expired write stores nothing, so its cost is not checked.
	if expireTime >= 0 {
		cost, err := c.itemCost(key, value)
		if err != nil {
			return nil, writeCommand[K, V]{}, err
		}
		if shard.costCap > 0 && cost > shard.costCap {
			return nil, writeCommand[K, V]{}, fmt.Errorf("%w: cost %d, budget %d (MaxCost %d over %d shards)",
				ErrItemTooLarge, cost, shard.costCap, c.config.MaxCost, len(c.shards))
		}
		cmd.cost = cost
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

// enqueue waits for space when the shard's write queue is full.
func (c *Cache[K, V]) enqueue(s *shard[K, V], cmd writeCommand[K, V]) error {
	if c.isClosed() {
		return ErrCacheClosed
	}
	return s.queue.enqueue(cmd)
}

// awaitResult stops waiting if the cache closes.
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

// syncMutate orders a direct mutation after writes already claimed on the shard.
func (c *Cache[K, V]) syncMutate(s *shard[K, V], apply func()) error {
	if c.isClosed() {
		return ErrCacheClosed
	}

	s.drainMu.Lock()
	defer s.drainMu.Unlock()

	if c.isClosed() {
		return ErrCacheClosed
	}

	c.drainClaimed(s)

	s.mu.Lock()
	apply()
	s.mu.Unlock()
	return nil
}

// drainClaimed drains to a single head snapshot, waiting for producers that still need to publish.
// The caller holds drainMu.
func (c *Cache[K, V]) drainClaimed(s *shard[K, V]) {
	end := s.queue.head.Load()
	c.drainShardQueue(s, end)
	for s.queue.waitClaimed(end) {
		c.drainShardQueue(s, end)
	}
}

// applyBarrierDirect runs a barrier on the calling goroutine when the ring is full.
func (c *Cache[K, V]) applyBarrierDirect(s *shard[K, V], op writeOp) bool {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	// Recheck under drainMu in case Close started while we waited.
	if op != writeFlush && c.isClosed() {
		return false
	}
	c.drainClaimed(s)
	if op == writeClear {
		s.mu.Lock()
		c.clearShard(s)
		s.mu.Unlock()
	}
	return true
}

func (c *Cache[K, V]) applySetSync(s *shard[K, V], cmd writeCommand[K, V]) error {
	var task callbackTask[K, V]
	var hasTask bool
	err := c.syncMutate(s, func() {
		if cmd.expireTime > 0 {
			c.stampExpireTime(&cmd, c.nowNano())
		}
		committed := c.applySet(s, &cmd)
		task, hasTask = cmd.newCallbackTask(committed)
	})
	if err != nil {
		return err
	}
	if hasTask {
		c.scheduleCallback(s, task)
	}
	return nil
}

func (c *Cache[K, V]) deleteSync(s *shard[K, V], kh uint64, key K) (bool, error) {
	var deleted bool
	err := c.syncMutate(s, func() {
		deleted = c.deleteKey(s, kh, key)
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
	return c.enqueueAllShardsAndWait(op)
}

func (c *Cache[K, V]) flush() {
	_ = c.enqueueAllShardsAndWait(writeFlush)
}

// enqueueAllShardsAndWait queues barriers where possible so shard workers can run them in parallel.
func (c *Cache[K, V]) enqueueAllShardsAndWait(op writeOp) error {
	waiters := make([]*writeWaiter, 0, len(c.shards))
	for _, s := range c.shards {
		waiter := c.acquireWriteWaiter()
		cmd := writeCommand[K, V]{
			op:     op,
			result: waiter.ch,
		}
		if !s.queue.tryEnqueue(cmd) {
			c.releaseWriteWaiter(waiter)
			if !c.applyBarrierDirect(s, op) {
				return ErrCacheClosed
			}
			continue
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

// clearDirect resets each shard during Close.
// Hold drainMu through the reset because drainers can update the frequency sketch without mu.
func (c *Cache[K, V]) clearDirect() {
	for _, s := range c.shards {
		s.drainMu.Lock()
		s.mu.Lock()
		c.clearShard(s)
		s.mu.Unlock()
		s.drainMu.Unlock()
	}
}

func (c *Cache[K, V]) writeWorker(s *shard[K, V]) {
	defer c.workers.Done()

	for {
		select {
		case <-s.wake:
			for {
				c.drainShard(s)
				// Clear wakeState first so a producer that publishes at tail after the check can wake us.
				s.queue.wakeState.Store(0)
				if !s.queue.ready() {
					break
				}
				if !s.queue.wakeState.CompareAndSwap(0, 1) {
					break
				}
			}
		case <-c.closeCh:
			// Process writes accepted before shutdown.
			c.drainShard(s)
			return
		}
	}
}

// tryDrainShard lets callers help drain an open cache without waiting for the consumer lock.
func (c *Cache[K, V]) tryDrainShard(s *shard[K, V]) {
	if !s.drainMu.TryLock() {
		return
	}
	if !c.isClosed() {
		c.drainShardQueue(s, s.queue.head.Load())
	}
	s.drainMu.Unlock()
}

func (c *Cache[K, V]) drainShard(s *shard[K, V]) {
	s.drainMu.Lock()
	c.drainShardQueue(s, s.queue.head.Load())
	s.drainMu.Unlock()
}

// drainMissAndLookup checks whether a queued Set can satisfy a lock-free miss.
func (c *Cache[K, V]) drainMissAndLookup(s *shard[K, V], kh uint64, key K) (*cacheItem[K, V], bool) {
	if s.queue.quiescent() || !s.drainMu.TryLock() {
		return nil, false
	}
	if c.isClosed() {
		s.drainMu.Unlock()
		return nil, false
	}
	c.drainShardQueue(s, s.queue.head.Load())
	s.drainMu.Unlock()
	return s.tab.lookup(kh, key)
}

// drainShardQueue applies published writes before end. The caller holds drainMu.
// A fixed end bounds the drain to one ring's worth of writes even as producers refill it.
func (c *Cache[K, V]) drainShardQueue(s *shard[K, V], end uint64) {
	batch := s.writeBatch

	s.drainReadSamples()
	for {
		n := s.queue.tryDequeue(batch, end)
		if n == 0 {
			break
		}
		c.applyWriteBatch(s, batch[:n])
		s.drainReadSamples()
	}
	// A ready command can be left behind if its producer checked tail before this drain reached it.
	// Wake the worker in case that producer skipped the wake-up.
	if s.queue.ready() {
		s.queue.wakeConsumer()
	}
}

func (c *Cache[K, V]) applyWriteBatch(s *shard[K, V], batch []writeCommand[K, V]) {
	var ackBuf [inlineAckBuf]chan struct{}
	acks := ackBuf[:0]
	var callbacks []callbackTask[K, V]
	var now int64

	s.mu.Lock()
	for i := range batch {
		cmd := &batch[i]
		switch cmd.op {
		case writeSet:
			if cmd.expireTime > 0 && now == 0 {
				now = c.nowNano()
			}
			c.stampExpireTime(cmd, now)
			committed := c.applySet(s, cmd)
			if t, ok := cmd.newCallbackTask(committed); ok {
				callbacks = append(callbacks, t)
			}
		case writeClear:
			c.clearShard(s)
		}
		if cmd.result != nil {
			acks = append(acks, cmd.result)
		}
	}
	s.mu.Unlock()

	for _, task := range callbacks {
		c.scheduleCallback(s, task)
	}
	for _, ch := range acks {
		ch <- struct{}{}
	}
}

func (c *Cache[K, V]) stampExpireTime(cmd *writeCommand[K, V], now int64) {
	if cmd.expireTime > 0 {
		// Saturate so a very long TTL cannot wrap into the expired range.
		cmd.expireTime = min(cmd.expireTime, math.MaxInt64-now) + now
	}
}

// newItem does not use a pool because lock-free readers may retain evicted items.
func (c *Cache[K, V]) newItem(cmd *writeCommand[K, V]) *cacheItem[K, V] {
	return &cacheItem[K, V]{
		value:      cmd.value,
		key:        cmd.key,
		hash:       cmd.hash,
		expireTime: cmd.expireTime,
		cost:       cmd.cost,
	}
}

func (c *Cache[K, V]) applySet(s *shard[K, V], cmd *writeCommand[K, V]) bool {
	if cmd.expireTime < 0 {
		c.deleteKey(s, cmd.hash, cmd.key)
		return false
	}
	if s.sieve != nil {
		return c.applySieve(s, cmd)
	}

	// Evict before insert so the new item cannot be selected as the victim.
	if ex, exists := s.tab.lookup(cmd.hash, cmd.key); exists {
		return c.applyUpdate(s, cmd, ex)
	}
	for s.wouldOverCapacity(cmd.cost) && s.tab.length() > 0 {
		c.evictor.evict(s, c.config.StatsEnabled)
	}

	item := c.newItem(cmd)
	s.tab.store(item)
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

// New candidates enter the policy before the table, so rejection never exposes
// them to lock-free readers or creates a deleted slot.
func (c *Cache[K, V]) applySieve(s *shard[K, V], cmd *writeCommand[K, V]) bool {
	warmup := s.belowSieveWarmup()
	prev, slot, cur := s.tab.probe(cmd.hash, cmd.key)

	if prev != nil {
		// Publish an immutable replacement and preserve its policy position.
		item := c.newItem(cmd)
		s.tab.swapAt(slot, item)
		if d := cmd.cost - prev.cost; d != 0 {
			atomic.AddInt64(&s.cost, d)
		}
		s.sieve.replaceNode(prev, item)
		if !warmup {
			s.sieve.recordUpdate(item)
		}
		c.enforcePostUpdateCapacity(s)
		return true
	}

	// Keep the candidate out of the table until admission decides its fate.
	ghostHit := !warmup && s.sieve.ghost.contains(cmd.hash)
	item := c.newItem(cmd)
	item.unpublished = true
	if !warmup {
		s.sieve.recordAccess(cmd.hash)
	}
	atomic.AddInt64(&s.size, 1)
	if item.cost != 0 {
		atomic.AddInt64(&s.cost, item.cost)
	}
	s.sieve.insert(item, ghostHit)
	if !warmup || s.overCapacity() {
		s.enforceSieveCapacity(c.config.StatsEnabled, item, ghostHit)
	}
	if s.sieve.owns(item) {
		item.unpublished = false
		s.tab.publish(item, cur)
		s.sieve.stats.Admits++
		return true
	}
	// Rejection already unlinked the candidate, so release its reserved table slot.
	s.tab.unpin()
	s.sieve.stats.Rejects++
	return false
}

// applyUpdate changes an item for policies whose reads hold the shard lock.
// SieveTinyLFU uses applySieve because its published items are immutable.
func (c *Cache[K, V]) applyUpdate(s *shard[K, V], cmd *writeCommand[K, V], ex *cacheItem[K, V]) bool {
	costDelta := cmd.cost - ex.cost
	ex.value = cmd.value
	ex.expireTime = cmd.expireTime
	if costDelta != 0 {
		ex.cost = cmd.cost
		atomic.AddInt64(&s.cost, costDelta)
	}
	switch c.config.EvictionPolicy {
	case LRU:
		s.moveToLRUHead(ex)
	case LFU:
		s.lfuList.remove(ex)
		s.lfuList.add(ex)
	}
	c.enforcePostUpdateCapacity(s)
	return true
}

func (c *Cache[K, V]) enforcePostUpdateCapacity(s *shard[K, V]) {
	if !s.overCapacity() {
		return
	}
	if s.sieve != nil {
		s.enforceSieveCapacity(c.config.StatsEnabled, nil, false)
		return
	}
	for s.overCapacity() && s.tab.length() > 0 {
		c.evictor.evict(s, c.config.StatsEnabled)
	}
}

func (c *Cache[K, V]) deleteKey(s *shard[K, V], kh uint64, key K) bool {
	item, exists := s.tab.lookup(kh, key)
	if !exists {
		return false
	}
	c.removeItem(s, item, RemovedDeleted)
	return true
}

func (c *Cache[K, V]) clearShard(s *shard[K, V]) {
	s.tab.clear()
	if s.sieve == nil {
		s.initLRU()
	}
	if c.config.EvictionPolicy == LFU {
		s.lfuList = newLFUList[K, V]()
	}
	if s.sieve != nil {
		s.sieve.reset()
	}
	atomic.StoreInt64(&s.size, 0)
	atomic.StoreInt64(&s.cost, 0)
}
