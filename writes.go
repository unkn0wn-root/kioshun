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
	c.stampExpireTimeNow(cmd)
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

func (c *Cache[K, V]) setCommand(
	key K,
	value V,
	ttl time.Duration,
	callback func(K, V),
) (*shard[K, V], writeCommand[K, V], error) {
	if ttl == DefaultExpiration {
		ttl = c.config.DefaultTTL
	}

	var expireTime int64
	if ttl > 0 {
		expireTime = ttl.Nanoseconds()
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

// syncMutate drains earlier writes before applying a direct mutation under the
// shard lock. drainMu keeps queue consumption ordered.
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
		c.stampExpireTimeNow(&cmd)
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
	_ = c.enqueueAllShardsAndWait(writeBarrier)
}

// enqueueAllShardsAndWait places an ordered barrier on every shard.
func (c *Cache[K, V]) enqueueAllShardsAndWait(op writeOp) error {
	waiters := make([]*writeWaiter, 0, len(c.shards))
	for _, s := range c.shards {
		waiter := c.acquireWriteWaiter()
		cmd := writeCommand[K, V]{
			op:     op,
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
			// Clear wakeState before checking the ring. A concurrent producer is then
			// either visible to ready or responsible for sending a new wake-up.
			for {
				c.drainShard(s)
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

// tryDrainShard processes read samples and queued writes if no drain is active.
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

// drainMissAndLookup checks whether a lock-free miss is a queued Set. It drains
// only when it can take the consumer lock without waiting.
func (c *Cache[K, V]) drainMissAndLookup(s *shard[K, V], kh uint64, key K) (*cacheItem[K, V], bool) {
	if s.queue.quiescent() || !s.drainMu.TryLock() {
		return nil, false
	}
	c.drainShardQueue(s)
	s.drainMu.Unlock()
	return s.tab.lookup(kh, key)
}

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
		c.scheduleCallback(task)
	}
	for _, ch := range acks {
		ch <- struct{}{}
	}
}

func (c *Cache[K, V]) stampExpireTime(cmd *writeCommand[K, V], now int64) {
	if cmd.expireTime > 0 {
		cmd.expireTime += now
	}
}

func (c *Cache[K, V]) stampExpireTimeNow(cmd *writeCommand[K, V]) {
	if cmd.expireTime > 0 {
		cmd.expireTime += c.nowNano()
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

func (c *Cache[K, V]) scheduleCallback(task callbackTask[K, V]) {
	delay := max(time.Duration(task.expireTime-c.nowNano()), 0)

	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-timer.C:
			kh := c.hasher.Sum(task.key)
			s := c.shardByHash(kh)
			s.mu.RLock()
			item, exists := s.tab.lookup(kh, task.key)
			expired := exists && item.expireTime > 0 && c.nowNano() > item.expireTime
			s.mu.RUnlock()
			if expired {
				task.callback(task.key, task.value)
			}
		case <-c.closeCh:
			return
		}
	}()
}
