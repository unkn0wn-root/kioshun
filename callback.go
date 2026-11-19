package kioshun

import (
	"math"
	"slices"
	"sync"
	"time"
)

const (
	// callbackHeapArity is the number of children of a heap node. fire lists the
	// root's children by index.
	callbackHeapArity = 4

	// callbackHeapMinCap is the capacity at which pop stops shrinking the heap.
	callbackHeapMinCap = 64

	// dueBatch is how many due callbacks fireBatch takes from the heap per lock hold.
	dueBatch = 32

	// arm cuts a wait longer than earlyArmMin short by 1/earlyArmDiv of it.
	earlyArmMin = time.Millisecond
	earlyArmDiv = 16
)

// callbackTimer runs a shard's expiry callbacks from a single timer. Pending
// callbacks wait in a min-heap ordered by deadline.
type callbackTimer[K comparable, V any] struct {
	// mu guards the fields below. Nothing else is locked while it is held, and
	// callbacks run without it.
	mu    sync.Mutex
	c     *Cache[K, V] // nil after close
	timer *time.Timer  // created by the first arm

	// when is the cache-clock time the timer is set for. It is math.MaxInt64
	// from a fire, or from the start, until the next arm.
	when int64

	// heap is 4-ary and hand-written, like the runtime's timer heap.
	// container/heap would allocate for each entry pushed or popped.
	heap []callbackEntry[K, V]
}

// callbackEntry is a heap slot. Tasks live outside the heap, so sifting and
// growing copy a deadline and a pointer per slot, whatever the value size.
type callbackEntry[K comparable, V any] struct {
	expireTime int64
	task       *callbackTask[K, V]
}

func newCallbackTimer[K comparable, V any](c *Cache[K, V]) *callbackTimer[K, V] {
	return &callbackTimer[K, V]{c: c, when: math.MaxInt64}
}

// scheduleCallback adds task to its shard's heap. It arms the timer if task is
// now the earliest and is due before the timer is set to fire.
func (c *Cache[K, V]) scheduleCallback(s *shard[K, V], task callbackTask[K, V]) {
	ct := s.callbacks
	ct.mu.Lock()
	defer ct.mu.Unlock()
	// Close sets the flag before it takes mu, so it drops a task pushed here.
	if c.isClosed() {
		return
	}
	if len(ct.heap) == cap(ct.heap) {
		// Grow to at least twice the length. append grows large slices by only about 1.25x.
		ct.heap = slices.Grow(ct.heap, len(ct.heap))
	}
	ct.heap = append(ct.heap, callbackEntry[K, V]{expireTime: task.expireTime, task: &task})
	if ct.up(len(ct.heap)-1) == 0 && task.expireTime < ct.when {
		ct.arm(task.expireTime, c.nowNano())
	}
}

// arm sets the timer for the first instant a task with this deadline is due,
// 1 ns after it. A wait longer than earlyArmMin is cut short, so that later
// tasks due a little sooner find the timer already set and need no Reset.
// ct.mu must be held.
func (ct *callbackTimer[K, V]) arm(deadline, now int64) {
	// Subtract first, because deadline can be math.MaxInt64.
	d := time.Duration(deadline - now + 1)
	if d > earlyArmMin {
		d -= d / earlyArmDiv
	}
	ct.when = now + int64(d)
	if ct.timer == nil {
		ct.timer = time.AfterFunc(d, ct.fire)
		return
	}
	ct.timer.Reset(d)
}

// fire runs when the shard timer fires. It runs a lone due callback itself and
// leaves two or more to fireBatch.
func (ct *callbackTimer[K, V]) fire() {
	ct.mu.Lock()
	ct.when = math.MaxInt64
	// The heap stays empty after close, so c is not nil past this check.
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
	// The second earliest task is one of the root's four children.
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
		go ct.fireBatch()
	}
	if n == 0 {
		return
	}
	// Keep the tasks whose entries are still present and expired at take's now.
	// The tasks all belong to this shard, so one read lock covers them.
	for i := range n {
		hashes[i] = c.hasher.Sum(tasks[i].key)
	}
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

// runCallback runs task's callback unless the cache has closed.
func (c *Cache[K, V]) runCallback(task *callbackTask[K, V]) {
	if !c.isClosed() {
		task.callback(task.key, task.value)
	}
}

// close stops the timer and drops the pending tasks. Callbacks already started
// keep running.
func (ct *callbackTimer[K, V]) close() {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.timer != nil {
		ct.timer.Stop()
	}
	ct.heap = nil
	// The runtime can hold a stopped timer for a while, and the timer holds ct.
	ct.c = nil
}

// due reports whether heap[i] exists and is due at now. A task is due once now
// is past its deadline, the rule cache entries expire by. ct.mu must be held.
func (ct *callbackTimer[K, V]) due(i int, now int64) bool {
	return i < len(ct.heap) && ct.heap[i].expireTime < now
}

// take moves up to len(dst) tasks that are due at now from the heap into dst.
// It returns how many it took and whether more are due. ct.mu must be held.
func (ct *callbackTimer[K, V]) take(dst []*callbackTask[K, V], now int64) (n int, more bool) {
	for n < len(dst) && ct.due(0, now) {
		dst[n] = ct.pop()
		n++
	}
	return n, ct.due(0, now)
}

// up moves heap[i] toward the root and returns its final index. It moves a
// hole instead of swapping. ct.mu must be held.
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

// pop removes and returns the earliest task. The heap shrinks as it drains, so
// a burst does not keep its peak size. ct.mu must be held.
func (ct *callbackTimer[K, V]) pop() *callbackTask[K, V] {
	h := ct.heap
	top := h[0].task
	n := len(h) - 1
	if n == 0 {
		ct.heap = nil
		return top
	}
	// Move the last entry down from the root.
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
	// Halve at a quarter full, which leaves room to grow again without a copy.
	if cap(h) > callbackHeapMinCap && n < cap(h)/4 {
		h = append(make([]callbackEntry[K, V], 0, cap(h)/2), h...)
	}
	ct.heap = h
	return top
}
