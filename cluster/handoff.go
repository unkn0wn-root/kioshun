package cluster

import (
	"container/heap"
	"container/list"
	"sync"
	"sync/atomic"
	"time"

	cbor "github.com/fxamacker/cbor/v2"
)

type hintItem struct {
	key       []byte
	val       []byte
	exp       int64
	ver       uint64
	cp        bool
	isDel     bool
	addedAt   int64         // UnixNano when enqueued
	nextAt    int64         // next eligible send time (backoff), UnixNano
	tries     int           // number of attempts so far
	size      int64         // cached (key+val+overhead) bytes
	elem      *list.Element // if in ready FIFO
	heapIndex int           // index in delayed heap, or -1 if not in heap
}

// expired returns true when the hint itself should not be replayed anymore.
// It drops: (a) hints older than handoff TTL, and (b) value SET hints whose
// target expiration has already passed; DELETE hints are retained until TTL.
func (h *hintItem) expired(ttl time.Duration, now int64) bool {
	if ttl > 0 && now-h.addedAt >= ttl.Nanoseconds() {
		return true
	}
	// if value itself is already expired, don't replicate it.
	if h.exp > 0 && now >= h.exp && !h.isDel {
		return true
	}
	return false
}

type delayHeap []*hintItem

func (h delayHeap) Len() int { return len(h) }
func (h delayHeap) Less(i, j int) bool {
	// earlier nextAt has higher priority
	return h[i].nextAt < h[j].nextAt
}
func (h delayHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIndex = i
	h[j].heapIndex = j
}
func (h *delayHeap) Push(x any) {
	hi := x.(*hintItem)
	hi.heapIndex = len(*h)
	*h = append(*h, hi)
}
func (h *delayHeap) Pop() any {
	old := *h
	n := len(old)
	hi := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	hi.heapIndex = -1
	return hi
}

type hintQueue struct {
	mu       sync.Mutex
	index    map[string]*hintItem // keyString -> hintItem in either ready or delayed
	ready    *list.List           // FIFO of *hintItem
	delayed  delayHeap            // min-heap by nextAt
	bytes    int64
	items    int
	maxItems int
	maxBytes int64
	ttl      time.Duration
	drop     DropPolicy
}

func newHintQueue(cfg HandoffConfig) *hintQueue {
	return &hintQueue{
		index:    make(map[string]*hintItem),
		ready:    list.New(),
		delayed:  delayHeap{},
		maxItems: cfg.PerPeerCap,
		maxBytes: cfg.PerPeerBytes,
		ttl:      cfg.TTL,
		drop:     cfg.DropPolicy,
	}
}

// removeItemLocked removes the item from whichever container it resides in and
// updates counters and index.
func (q *hintQueue) removeItemLocked(hi *hintItem) {
	delete(q.index, string(hi.key))
	if hi.elem != nil {
		q.ready.Remove(hi.elem)
		hi.elem = nil
	} else if hi.heapIndex >= 0 {
		heap.Remove(&q.delayed, hi.heapIndex) // heap callbacks clear heapIndex
	}
	q.items--
	q.bytes -= hi.size
}

// moveDueLocked shifts any delayed items whose nextAt <= now into ready FIFO.
func (q *hintQueue) moveDueLocked(now int64) {
	for q.delayed.Len() > 0 {
		front := q.delayed[0]
		if front.nextAt > now {
			break
		}
		hi := heap.Pop(&q.delayed).(*hintItem)
		hi.elem = q.ready.PushBack(hi)
	}
}

// overCap evaluates capacity against hypothetical items/bytes.
func (q *hintQueue) overCap(items int, bytes int64) bool {
	return (q.maxItems > 0 && items > q.maxItems) ||
		(q.maxBytes > 0 && bytes > q.maxBytes)
}

// dropOldest drops from the oldest end
// ready front if available, otherwise earliest nextAt from delayed.
func (q *hintQueue) dropOldest(avoid *hintItem) (int, int64) {
	droppedCount := 0
	droppedBytes := int64(0)

	tryDropReady := func() bool {
		fe := q.ready.Front()
		if fe == nil {
			return false
		}

		h := fe.Value.(*hintItem)
		if h == avoid {
			// do not drop the just-inserted/replaced item if it's at head
			return false
		}
		q.removeItemLocked(h)
		droppedCount++
		droppedBytes += h.size
		return true
	}

	tryDropDelayed := func() bool {
		if q.delayed.Len() == 0 {
			return false
		}
		h := q.delayed[0]
		if h == avoid {
			return false
		}

		heap.Pop(&q.delayed) // remove root
		delete(q.index, string(h.key))
		q.items--
		q.bytes -= h.size
		droppedCount++
		droppedBytes += h.size
		return true
	}

	if tryDropReady() || tryDropDelayed() {
		return droppedCount, droppedBytes
	}
	return 0, 0
}

// enqueue adds or coalesces a hint. It keeps only the newest version per key.
// Returns (accepted, bytesDelta, itemsDelta). The deltas reflect NET change,
// including any drops performed to enforce caps.
func (q *hintQueue) enqueue(it *hintItem) (bool, int64, int) {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now().UnixNano()
	if it.expired(q.ttl, now) {
		return false, 0, 0
	}

	keyStr := string(it.key)
	// coalesce with existing key if present.
	if old, ok := q.index[keyStr]; ok {
		if it.ver <= old.ver {
			return false, 0, 0
		}

		// preserve scheduling/backoff state if old was delayed.
		if old.heapIndex >= 0 {
			it.nextAt = old.nextAt
			it.tries = old.tries
		}
		// replace in same container.
		var placement string
		if old.elem != nil {
			placement = "ready"
			old.elem.Value = it
			it.elem = old.elem
			it.heapIndex = -1
		} else {
			placement = "delayed"
			idx := old.heapIndex
			it.heapIndex = idx
			it.elem = nil
			q.delayed[idx] = it
		}

		// bytes accounting + cap enforcement.
		delta := it.size - old.size
		q.bytes += delta
		q.index[keyStr] = it

		if q.overCap(q.items, q.bytes) {
			switch q.drop {
			case DropOldest:
				totalDroppedB := int64(0)
				totalDroppedI := 0
				for q.overCap(q.items, q.bytes) {
					dc, db := q.dropOldest(it)
					if dc == 0 {
						// Can't drop anything else without dropping 'it' -> revert replacement.
						// Restore old placement/value.
						if placement == "ready" {
							it.elem.Value = old
							old.elem = it.elem
							it.elem = nil
						} else {
							q.delayed[it.heapIndex] = old
							old.heapIndex = it.heapIndex
							it.heapIndex = -1
						}
						q.index[keyStr] = old
						q.bytes -= delta

						return false, totalDroppedB, -totalDroppedI
					}
					totalDroppedB += db
					totalDroppedI += dc
				}
				return true, delta - totalDroppedB, -totalDroppedI
			default:
				if old.elem != nil {
					old.elem.Value = old
				} else if old.heapIndex >= 0 {
					q.delayed[old.heapIndex] = old
				}
				q.index[keyStr] = old
				q.bytes -= delta
				return false, 0, 0
			}
		}
		return true, delta, 0
	}

	// choose container based on nextAt.
	if it.nextAt > now {
		heap.Push(&q.delayed, it)
		it.elem = nil
	} else {
		it.elem = q.ready.PushBack(it)
		it.heapIndex = -1
	}
	q.index[keyStr] = it
	q.items++
	q.bytes += it.size

	// enforce caps for new items.
	if q.overCap(q.items, q.bytes) {
		switch q.drop {
		case DropOldest:
			totalDroppedB := int64(0)
			totalDroppedI := 0
			for q.overCap(q.items, q.bytes) {
				dc, db := q.dropOldest(it)
				if dc == 0 {
					// can't drop others; drop the just added item itself.
					q.removeItemLocked(it)
					delete(q.index, keyStr)
					return false, -totalDroppedB, -totalDroppedI
				}
				totalDroppedB += db
				totalDroppedI += dc
			}
			return true, it.size - totalDroppedB, 1 - totalDroppedI
		default: // DropNewest/DropNone -> reject new item
			q.removeItemLocked(it)
			delete(q.index, keyStr)
			return false, 0, 0
		}
	}

	return true, it.size, 1
}

// popReady pops the head of the ready (after moving due delayed items).
func (q *hintQueue) popReady(now int64) *hintItem {
	q.mu.Lock()
	defer q.mu.Unlock()

	// oromote due delayed items.
	q.moveDueLocked(now)

	fe := q.ready.Front()
	if fe == nil {
		return nil
	}

	it := fe.Value.(*hintItem)
	// skip expired items.
	if it.expired(q.ttl, now) {
		q.removeItemLocked(it)
		return nil
	}

	q.removeItemLocked(it)
	return it
}

// requeue puts a failed item back respecting backoff.
func (q *hintQueue) requeue(it *hintItem) {
	_, _, _ = q.enqueue(it)
}

// snapshot stats (for breaker/metrics)
func (q *hintQueue) stats() (items int, bytes int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items, q.bytes
}

type handoff[K comparable, V any] struct {
	node       *Node[K, V]
	cfg        HandoffConfig
	mu         sync.Mutex
	peers      map[string]*hintQueue
	totalItems atomic.Int64
	totalBytes atomic.Int64
	paused     atomic.Bool
	stop       chan struct{}
	rr         uint32 // round-robin cursor over peers
}

func newHandoff[K comparable, V any](n *Node[K, V]) *handoff[K, V] {
	h := &handoff[K, V]{
		node:  n,
		cfg:   n.cfg.Handoff,
		peers: make(map[string]*hintQueue),
		stop:  make(chan struct{}),
	}
	go h.loop()
	return h
}

// queue returns the per-peer queue, creating it on demand.
func (h *handoff[K, V]) queue(addr string) *hintQueue {
	h.mu.Lock()
	defer h.mu.Unlock()
	q := h.peers[addr]
	if q == nil {
		q = newHintQueue(h.cfg)
		h.peers[addr] = q
	}
	return q
}

// enqueueSet enqueues a SET hint for addr with value bytes
// (optionally compressed), absolute expiry, and version.
// Dropped when handoff disabled or auto-paused due to backlog.
func (h *handoff[K, V]) enqueueSet(addr string, key, val []byte, exp int64, ver uint64, cp bool) {
	if !h.cfg.IsEnabled() {
		return
	}
	if h.paused.Load() {
		return
	}

	const hintOverheadSet = int64(24) // bookkeeping bytes per SET
	it := &hintItem{
		key:       append([]byte(nil), key...),
		val:       append([]byte(nil), val...),
		exp:       exp,
		ver:       ver,
		cp:        cp,
		isDel:     false,
		addedAt:   time.Now().UnixNano(),
		size:      int64(len(key)+len(val)) + hintOverheadSet,
		heapIndex: -1, // nextAt==0 -> ready FIFO
	}
	q := h.queue(addr)
	ok, byDelta, itDelta := q.enqueue(it)
	if !ok {
		return
	}

	newItems := h.totalItems.Add(int64(itDelta))
	newBytes := h.totalBytes.Add(byDelta)
	if (h.cfg.AutopauseItems > 0 && int(newItems) >= h.cfg.AutopauseItems) ||
		(h.cfg.AutopauseBytes > 0 && newBytes >= h.cfg.AutopauseBytes) {
		h.paused.Store(true)
	}
}

// enqueueDel enqueues a DELETE hint for addr with a version. The value field
// is empty for deletes. Dropped when handoff disabled or auto-paused.
func (h *handoff[K, V]) enqueueDel(addr string, key []byte, ver uint64) {
	if !h.cfg.IsEnabled() {
		return
	}
	if h.paused.Load() {
		return
	}

	const hintOverheadDel = int64(16) // bookkeeping bytes per DEL
	it := &hintItem{
		key:       append([]byte(nil), key...),
		val:       nil,
		exp:       0,
		ver:       ver,
		cp:        false,
		isDel:     true,
		addedAt:   time.Now().UnixNano(),
		size:      int64(len(key)) + hintOverheadDel,
		heapIndex: -1,
	}
	q := h.queue(addr)
	ok, byDelta, itDelta := q.enqueue(it)
	if !ok {
		return
	}
	newItems := h.totalItems.Add(int64(itDelta))
	newBytes := h.totalBytes.Add(byDelta)
	if (h.cfg.AutopauseItems > 0 && int(newItems) >= h.cfg.AutopauseItems) ||
		(h.cfg.AutopauseBytes > 0 && newBytes >= h.cfg.AutopauseBytes) {
		h.paused.Store(true)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// loop drives replay at a configured RPS using per-tick slices. It supports
// operator pause and auto-pause based on backlog. Items that fail are
// rescheduled with exponential backoff.
func (h *handoff[K, V]) loop() {
	if h.cfg.ReplayRPS <= 0 {
		<-h.stop
		return
	}

	const slotsPerSec = 20
	tick := time.NewTicker(time.Second / slotsPerSec)
	defer tick.Stop()

	perTick := h.cfg.ReplayRPS / slotsPerSec
	extra := h.cfg.ReplayRPS % slotsPerSec
	slot := 0

	house := time.NewTicker(2 * time.Second)
	defer house.Stop()

	// exponential backoff for retries; capped to keep queues moving.
	// attemptIndex: 0 -> 200ms, 1 -> 400ms, ... cap ~5s.
	backoff := func(attemptIndex int) time.Duration {
		if attemptIndex < 0 {
			attemptIndex = 0
		}
		shift := min(6, attemptIndex) // 0..6
		d := 200 * time.Millisecond << uint(shift)
		if d > 5*time.Second {
			d = 5 * time.Second
		}
		return d
	}

	for {
		select {
		case <-tick.C:
			n := perTick
			if extra > 0 && slot < extra {
				n++
			}
			slot++
			if slot == slotsPerSec {
				slot = 0
			}
			if n == 0 {
				continue
			}
			// operator pause fully stops replay; auto-pause only stops enqueues,
			// allowing replay to continue draining backlog.
			if h.cfg.Pause {
				continue
			}
			for i := 0; i < n; i++ {
				if !h.replayOne(backoff) {
					break
				}
			}

		case <-house.C:
			// update backlog counters and auto-resume when sufficiently drained.
			totalI, totalB := int64(0), int64(0)
			h.mu.Lock()
			for _, q := range h.peers {
				i, b := q.stats()
				totalI += int64(i)
				totalB += b
			}
			h.mu.Unlock()
			h.totalItems.Store(totalI)
			h.totalBytes.Store(totalB)

			// auto-resume when sufficiently drained (hysteresis ~60%).
			if h.paused.Load() {
				if (h.cfg.AutopauseItems == 0 || totalI < int64(float64(h.cfg.AutopauseItems)*0.6)) &&
					(h.cfg.AutopauseBytes == 0 || totalB < int64(float64(h.cfg.AutopauseBytes)*0.6)) {
					h.paused.Store(false)
				}
			}

		case <-h.stop:
			return
		}
	}
}

// scheduleRetry updates backoff state and requeues the item.
func (h *handoff[K, V]) scheduleRetry(q *hintQueue, hi *hintItem, backoff func(int) time.Duration) {
	hi.tries++
	// First retry (tries==1) -> attemptIndex 0 -> 200ms.
	hi.nextAt = time.Now().Add(backoff(hi.tries - 1)).UnixNano()
	q.requeue(hi) // re-enqueue respects TTL/caps/coalescing
}

// replayOne attempts to send a single hint across peers using round-robin
// fairness. On success the item is dropped; on failure it is rescheduled.
// Returns true when an item was processed (success or scheduled), false when
// no ready items were found.
func (h *handoff[K, V]) replayOne(backoff func(int) time.Duration) bool {
	// fair-share round-robin over peers to avoid starving any one queue.
	h.mu.Lock()
	if len(h.peers) == 0 {
		h.mu.Unlock()
		return false
	}
	idx := int(h.rr)
	h.rr++

	// snapshot addresses .
	addrs := make([]string, 0, len(h.peers))
	for a := range h.peers {
		addrs = append(addrs, a)
	}
	h.mu.Unlock()

	now := time.Now().UnixNano()
	for k := 0; k < len(addrs); k++ {
		addr := addrs[(idx+k)%len(addrs)]
		q := h.queue(addr)
		hi := q.popReady(now)
		if hi == nil {
			continue
		}

		pc := h.node.getPeer(addr)
		if pc == nil {
			pc = h.node.ensurePeer(addr)
			if pc == nil {
				h.scheduleRetry(q, hi, backoff)
				continue
			}
		}

		id := h.node.nextReqID()
		if !hi.isDel {
			// SET hint
			msg := &MsgSet{
				Base: Base{
					T:  MTSet,
					ID: id,
				},
				Key: hi.key,
				Val: hi.val,
				Exp: hi.exp,
				Ver: hi.ver,
				Cp:  hi.cp,
			}

			raw, err := pc.request(msg, id, h.node.cfg.Sec.WriteTimeout)
			if err != nil {
				if isFatalTransport(err) {
					h.node.resetPeer(addr)
				}
				h.scheduleRetry(q, hi, backoff)
				continue
			}

			var resp MsgSetResp
			if e := cbor.Unmarshal(raw, &resp); e != nil {
				// likely broken stream → reset.
				h.node.resetPeer(addr)
				h.scheduleRetry(q, hi, backoff)
				continue
			}
			if !resp.OK {
				// don't reset, just retry.
				h.scheduleRetry(q, hi, backoff)
				continue
			}

			// success - drop (counters recomputed by housekeeping)
			return true
		}

		msg := &MsgDel{Base: Base{T: MTDelete, ID: id}, Key: hi.key, Ver: hi.ver}
		raw, err := pc.request(msg, id, h.node.cfg.Sec.WriteTimeout)
		if err != nil {
			if isFatalTransport(err) {
				h.node.resetPeer(addr)
			}
			h.scheduleRetry(q, hi, backoff)
			continue
		}

		var resp MsgDelResp
		if e := cbor.Unmarshal(raw, &resp); e != nil {
			h.node.resetPeer(addr)
			h.scheduleRetry(q, hi, backoff)
			continue
		}
		if !resp.OK {
			h.scheduleRetry(q, hi, backoff)
			continue
		}

		return true
	}
	return false
}

// Stop shuts down the handoff replay loop.
func (h *handoff[K, V]) Stop() {
	select {
	case <-h.stop:
		return
	default:
		close(h.stop)
	}
}

// Pause prevents new items from being enqueued; replay may continue.
func (h *handoff[K, V]) Pause() {
	h.paused.Store(true)
}

// Resume re-enables enqueues after a manual or automatic pause.
func (h *handoff[K, V]) Resume() {
	h.paused.Store(false)
}
