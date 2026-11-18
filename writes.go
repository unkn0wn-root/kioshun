package kioshun

import (
	"fmt"
	"math"
	"slices"
	"sync"
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

// callbackTimers holds a shard's pending expiry callbacks in a min-heap ordered
// by deadline, served by a single timer.
type callbackTimers[K comparable, V any] struct {
	mu    sync.Mutex
	c     *Cache[K, V]
	timer *time.Timer
	when  int64 // cache-clock time the timer is set for, or math.MaxInt64 if unknown
	heap  []callbackEntry[K, V]
}

// callbackEntry is a heap slot. Tasks live outside the heap, so sifting and
// growing copy a deadline and a pointer per slot, whatever the value size.
type callbackEntry[K comparable, V any] struct {
	expireTime int64
	task       *callbackTask[K, V]
}

// close stops the timer and drops the pending tasks. Callbacks already started
// keep running.
func (ct *callbackTimers[K, V]) close() {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.timer != nil {
		ct.timer.Stop()
	}
	ct.heap, ct.c = nil, nil
}

// dueBatch is how many due callbacks fireBatch takes from the heap per lock hold.
const dueBatch = 32

func (c *Cache[K, V]) scheduleCallback(s *shard[K, V], task callbackTask[K, V]) {
	ct := s.callbacks
	ct.mu.Lock()
	defer ct.mu.Unlock()
	// Close sets the flag before it stops the timer under mu.
	if c.isClosed() {
		return
	}
	if len(ct.heap) == cap(ct.heap) {
		// Grow to at least twice the length. append grows large slices by only about 1.25x.
		ct.heap = slices.Grow(ct.heap, len(ct.heap))
	}
	ct.heap = append(ct.heap, callbackEntry[K, V]{task.expireTime, &task})
	if ct.up(len(ct.heap)-1) == 0 && task.expireTime < ct.when {
		ct.arm(task.expireTime, c.nowNano())
	}
}

// arm sets the timer for the first instant a task with this deadline is due,
// 1 ns after it. A task due a millisecond or more from now is armed a
// sixteenth early, so that later tasks due a little sooner find the timer
// already set and need no Reset. ct.mu must be held.
func (ct *callbackTimers[K, V]) arm(deadline, now int64) {
	d := deadline + 1 - now
	if d > int64(time.Millisecond) {
		d -= d / 16
	}
	ct.when = now + d
	if ct.timer == nil {
		ct.timer = time.AfterFunc(time.Duration(d), ct.fire)
		return
	}
	ct.timer.Reset(time.Duration(d))
}

// fire runs when the shard timer fires. It runs a lone due callback itself and
// leaves two or more to fireBatch.
func (ct *callbackTimers[K, V]) fire() {
	ct.mu.Lock()
	ct.when = math.MaxInt64
	if len(ct.heap) == 0 {
		ct.mu.Unlock()
		return
	}
	c := ct.c
	now := c.nowNano()
	if !ct.due(0, now) {
		ct.arm(ct.heap[0].expireTime, now)
		ct.mu.Unlock()
		return
	}
	// In a 4-ary heap the second earliest task is one of the root's children.
	if ct.due(1, now) || ct.due(2, now) || ct.due(3, now) || ct.due(4, now) {
		ct.mu.Unlock()
		ct.fireBatch()
		return
	}
	task := ct.pop()
	if len(ct.heap) > 0 {
		ct.arm(ct.heap[0].expireTime, now)
	}
	ct.mu.Unlock()
	kh := c.hasher.Sum(task.key)
	s := c.shardByHash(kh)
	s.mu.RLock()
	item, ok := s.tab.lookup(kh, task.key)
	expired := ok && item.expireTime > 0 && now > item.expireTime
	s.mu.RUnlock()
	if expired && !c.isClosed() {
		task.callback(task.key, task.value)
	}
}

// fireBatch takes up to dueBatch due tasks and runs the callbacks of those whose
// entries are still present and expired: the first in this goroutine and the
// others each in a goroutine of its own. If more tasks are due, it first starts
// another fireBatch for them, so that a slow callback delays no other one.
func (ct *callbackTimers[K, V]) fireBatch() {
	var due [dueBatch]*callbackTask[K, V]
	var hashes [dueBatch]uint64
	ct.mu.Lock()
	if len(ct.heap) == 0 {
		ct.mu.Unlock()
		return
	}
	c := ct.c
	now := c.nowNano()
	n, more := ct.take(due[:], now)
	if !more && len(ct.heap) > 0 {
		ct.arm(ct.heap[0].expireTime, now)
	}
	ct.mu.Unlock()
	if more {
		go ct.fireBatch()
	}
	if n == 0 {
		return
	}
	// Keep the tasks whose entries are still present and expired. With take's
	// now, a task take found due stays due unless its entry changed. The tasks
	// all belong to this shard, so one read lock covers them.
	for i := range n {
		hashes[i] = c.hasher.Sum(due[i].key)
	}
	s := c.shardByHash(hashes[0])
	kept := 0
	s.mu.RLock()
	for i := range n {
		item, ok := s.tab.lookup(hashes[i], due[i].key)
		if ok && item.expireTime > 0 && now > item.expireTime {
			due[kept] = due[i]
			kept++
		}
	}
	s.mu.RUnlock()
	if kept == 0 {
		return
	}
	for _, task := range due[1:kept] {
		go c.runCallback(task)
	}
	c.runCallback(due[0])
}

// due reports whether heap[i] exists and is due at now. A task is due once now
// is past its deadline, the rule cache entries expire by. ct.mu must be held.
func (ct *callbackTimers[K, V]) due(i int, now int64) bool {
	return i < len(ct.heap) && ct.heap[i].expireTime < now
}

// take moves up to len(due) tasks that are due at now from the heap into due.
// It returns how many it took and whether more are due. ct.mu must be held.
func (ct *callbackTimers[K, V]) take(due []*callbackTask[K, V], now int64) (n int, more bool) {
	for n < len(due) && ct.due(0, now) {
		due[n] = ct.pop()
		n++
	}
	return n, ct.due(0, now)
}

func (c *Cache[K, V]) runCallback(task *callbackTask[K, V]) {
	if !c.isClosed() {
		task.callback(task.key, task.value)
	}
}

// up moves heap[i] toward the root and returns its final index. The heap is
// 4-ary and moves a hole instead of swapping. ct.mu must be held.
func (ct *callbackTimers[K, V]) up(i int) int {
	h := ct.heap
	e := h[i]
	for i > 0 {
		p := (i - 1) / 4
		if h[p].expireTime <= e.expireTime {
			break
		}
		h[i] = h[p]
		i = p
	}
	h[i] = e
	return i
}

// pop removes the earliest task. The heap shrinks as it drains, so a burst does
// not keep its peak size. ct.mu must be held.
func (ct *callbackTimers[K, V]) pop() *callbackTask[K, V] {
	h := ct.heap
	top := h[0].task
	n := len(h) - 1
	if n == 0 {
		ct.heap = nil
		return top
	}
	last := h[n]
	h[n] = callbackEntry[K, V]{}
	h = h[:n]
	i := 0
	for {
		child := 4*i + 1
		if child >= n {
			break
		}
		m := child
		for j := child + 1; j < min(child+4, n); j++ {
			if h[j].expireTime < h[m].expireTime {
				m = j
			}
		}
		if last.expireTime <= h[m].expireTime {
			break
		}
		h[i] = h[m]
		i = m
	}
	h[i] = last
	if cap(h) > 64 && n < cap(h)/4 {
		h = append(make([]callbackEntry[K, V], 0, cap(h)/2), h...)
	}
	ct.heap = h
	return top
}
