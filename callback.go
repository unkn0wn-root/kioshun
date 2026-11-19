package kioshun

import (
	"math"
	"slices"
	"sync"
	"time"
)

const (
	callbackHeapArity  = 4
	callbackHeapMinCap = 64
	dueBatch           = 32
	earlyArmMin        = time.Millisecond
	earlyArmDiv        = 16
)

// callbackTimer uses one timer per shard for all expiry callbacks.
type callbackTimer[K comparable, V any] struct {
	// mu protects the fields below. Hold it when calling arm, due, take, up, or pop.
	// Keep mu and shard.mu separate. Run callbacks outside both locks.
	mu    sync.Mutex
	c     *Cache[K, V] // nil after close
	timer *time.Timer
	when  int64                 // next wakeup in cache time, or math.MaxInt64 until armed
	heap  []callbackEntry[K, V] // 4-ary min-heap by deadline
}

// callbackEntry keeps heap slots small even when keys or values are large.
type callbackEntry[K comparable, V any] struct {
	expireTime int64
	task       *callbackTask[K, V]
}

func newCallbackTimer[K comparable, V any](c *Cache[K, V]) *callbackTimer[K, V] {
	return &callbackTimer[K, V]{c: c, when: math.MaxInt64}
}

func (c *Cache[K, V]) scheduleCallback(s *shard[K, V], task callbackTask[K, V]) {
	ct := s.callbacks
	ct.mu.Lock()
	defer ct.mu.Unlock()
	// Close sets the flag before clearing the heap under mu.
	if c.isClosed() {
		return
	}
	if len(ct.heap) == cap(ct.heap) {
		// Grow by at least 2x to reduce copies for large heaps.
		ct.heap = slices.Grow(ct.heap, len(ct.heap))
	}
	ct.heap = append(ct.heap, callbackEntry[K, V]{expireTime: task.expireTime, task: &task})
	if ct.up(len(ct.heap)-1) == 0 && task.expireTime < ct.when {
		ct.arm(task.expireTime, c.nowNano())
	}
}

func (ct *callbackTimer[K, V]) arm(deadline, now int64) {
	// Expiry starts 1 ns after the deadline. Subtract first to avoid overflow.
	d := time.Duration(deadline - now + 1)
	if d > earlyArmMin {
		// Wake early to avoid resets for slightly earlier deadlines.
		d -= d / earlyArmDiv
	}
	ct.when = now + int64(d)
	if ct.timer == nil {
		ct.timer = time.AfterFunc(d, ct.fire)
		return
	}
	ct.timer.Reset(d)
}

func (ct *callbackTimer[K, V]) fire() {
	ct.mu.Lock()
	ct.when = math.MaxInt64
	// A closed timer has an empty heap and a nil cache.
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
	// The second-earliest task is a root child.
	// Keep these indexes in sync with callbackHeapArity.
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

func (ct *callbackTimer[K, V]) fireBatch() {
	var tasks [dueBatch]*callbackTask[K, V]
	var hashes [dueBatch]uint64
	ct.mu.Lock()
	if len(ct.heap) == 0 {
		ct.mu.Unlock()
		return
	}
	c := ct.c
	now := c.nowNano()
	n, more := ct.take(tasks[:], now)
	if !more && len(ct.heap) > 0 {
		ct.arm(ct.heap[0].expireTime, now)
	}
	ct.mu.Unlock()
	if more {
		// Start the next batch before a callback can block this goroutine.
		go ct.fireBatch()
	}
	if n == 0 {
		return
	}
	for i := range n {
		hashes[i] = c.hasher.Sum(tasks[i].key)
	}
	// All tasks came from this shard, so one read lock covers the batch.
	s := c.shardByHash(hashes[0])
	kept := 0
	s.mu.RLock()
	for i := range n {
		item, ok := s.tab.lookup(hashes[i], tasks[i].key)
		if ok && item.expireTime > 0 && now > item.expireTime {
			tasks[kept] = tasks[i]
			kept++
		}
	}
	s.mu.RUnlock()
	if kept == 0 {
		return
	}
	for _, task := range tasks[1:kept] {
		go c.runCallback(task)
	}
	c.runCallback(tasks[0])
}

func (c *Cache[K, V]) runCallback(task *callbackTask[K, V]) {
	if !c.isClosed() {
		task.callback(task.key, task.value)
	}
}

func (ct *callbackTimer[K, V]) close() {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.timer != nil {
		ct.timer.Stop()
	}
	ct.heap = nil
	// Let the cache be collected while the runtime still references the timer.
	ct.c = nil
}

func (ct *callbackTimer[K, V]) due(i int, now int64) bool {
	return i < len(ct.heap) && ct.heap[i].expireTime < now
}

func (ct *callbackTimer[K, V]) take(dst []*callbackTask[K, V], now int64) (n int, more bool) {
	for n < len(dst) && ct.due(0, now) {
		dst[n] = ct.pop()
		n++
	}
	return n, ct.due(0, now)
}

func (ct *callbackTimer[K, V]) up(i int) int {
	h := ct.heap
	e := h[i]
	for i > 0 {
		p := (i - 1) / callbackHeapArity
		if h[p].expireTime <= e.expireTime {
			break
		}
		h[i] = h[p]
		i = p
	}
	h[i] = e
	return i
}

func (ct *callbackTimer[K, V]) pop() *callbackTask[K, V] {
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
		child := callbackHeapArity*i + 1
		if child >= n {
			break
		}
		m := child
		for j := child + 1; j < min(child+callbackHeapArity, n); j++ {
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
	// Shrink after a burst and leave room for new tasks.
	if cap(h) > callbackHeapMinCap && n < cap(h)/4 {
		h = append(make([]callbackEntry[K, V], 0, cap(h)/2), h...)
	}
	ct.heap = h
	return top
}
