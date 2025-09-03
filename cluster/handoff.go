package cluster

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"

	cbor "github.com/fxamacker/cbor/v2"
)

type hintItem struct {
	key     []byte
	val     []byte // nil for delete
	exp     int64
	ver     uint64
	cp      bool
	isDel   bool
	addedAt int64
	nextAt  int64 // next eligible send time (for backoff)
	tries   int
	size    int64 // cached (key+val overhead)
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

type hintQueue struct {
	mu       sync.Mutex
	index    map[string]*list.Element // keyString -> *Element(hintItem)
	ll       *list.List               // FIFO for fairness / DropOldest
	bytes    int64
	items    int
	maxItems int
	maxBytes int64
	ttl      time.Duration
	drop     DropPolicy
}

// newHintQueue builds a per-peer queue with caps and TTL used by the manager.
func newHintQueue(cfg HandoffConfig) *hintQueue {
	return &hintQueue{
		index:    make(map[string]*list.Element),
		ll:       list.New(),
		maxItems: cfg.PerPeerCap,
		maxBytes: cfg.PerPeerBytes,
		ttl:      cfg.TTL,
		drop:     cfg.DropPolicy,
	}
}

// removeElementLocked removes an element and updates all counters.
func (q *hintQueue) removeElementLocked(e *list.Element) {
	hi := e.Value.(*hintItem)
	delete(q.index, string(hi.key))
	q.ll.Remove(e)
	q.items--
	q.bytes -= hi.size
}

// enqueue adds or coalesces a hint. It keeps only the newest version per key.
// Returns (accepted, bytesDelta, itemsDelta) for manager-wide accounting.
func (q *hintQueue) enqueue(it *hintItem) (bool, int64, int) {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now().UnixNano()
	if it.expired(q.ttl, now) {
		return false, 0, 0
	}

	keyStr := string(it.key)
	if e, ok := q.index[keyStr]; ok {
		old := e.Value.(*hintItem)
		// keep newer version; drop the older one.
		if it.ver <= old.ver {
			return false, 0, 0
		}
		// replace in place
		delta := it.size - old.size
		e.Value = it
		q.bytes += delta
		return true, delta, 0
	}

	// new entry: honor caps by policy (DropOldest/DropNewest/DropNone).
	wouldItems := q.items + 1
	wouldBytes := q.bytes + it.size

	overCap := func(items int, bytes int64) bool {
		return (q.maxItems > 0 && items > q.maxItems) ||
			(q.maxBytes > 0 && bytes > q.maxBytes)
	}

	switch q.drop {
	case DropOldest:
		for overCap(wouldItems, wouldBytes) {
			fe := q.ll.Front()
			if fe == nil {
				break
			}
			q.removeElementLocked(fe)
			wouldItems = q.items + 1
			wouldBytes = q.bytes + it.size
		}
	default: // DropNone and DropNewest: reject when over cap
		if overCap(wouldItems, wouldBytes) {
			return false, 0, 0
		}
	}

	e := q.ll.PushBack(it)
	q.index[keyStr] = e
	q.items++
	q.bytes += it.size
	return true, it.size, 1
}

// popReady pops the oldest item if it's ready (nextAt<=now) and not expired.
// Returns nil if queue empty or head is not yet ready (head-of-line backoff).
func (q *hintQueue) popReady(now int64) *hintItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		fe := q.ll.Front()
		if fe == nil {
			return nil
		}

		it := fe.Value.(*hintItem)
		if it.expired(q.ttl, now) {
			q.removeElementLocked(fe)
			continue
		}

		if it.nextAt > 0 && now < it.nextAt {
			return nil // head-of-line backoff. Keep simple to avoid scans
		}

		q.removeElementLocked(fe)
		return it
	}
}

// requeue puts a failed item back at the tail with updated backoff state.
func (q *hintQueue) requeue(it *hintItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	keyStr := string(it.key)
	if _, ok := q.index[keyStr]; ok {
		// already there (shouldn't happen), prefer keeping the newer slot.
		return
	}
	e := q.ll.PushBack(it)
	q.index[keyStr] = e
	q.items++
	q.bytes += it.size
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

func (h *handoff[K, V]) enqueueSet(addr string, key, val []byte, exp int64, ver uint64, cp bool) {
	if !h.cfg.IsEnabled() {
		return
	}

	const hintOverheadSet = int64(24) // bookkeeping bytes per SET
	it := &hintItem{
		key:     append([]byte(nil), key...),
		val:     append([]byte(nil), val...),
		exp:     exp,
		ver:     ver,
		cp:      cp,
		isDel:   false,
		addedAt: time.Now().UnixNano(),
		size:    int64(len(key)+len(val)) + hintOverheadSet,
	}
	h.enqueue(addr, it)
}

func (h *handoff[K, V]) enqueueDel(addr string, key []byte, ver uint64) {
	if !h.cfg.IsEnabled() {
		return
	}

	const hintOverheadDel = int64(16) // bookkeeping bytes per DEL
	it := &hintItem{
		key:     append([]byte(nil), key...),
		val:     nil,
		exp:     0,
		ver:     ver,
		cp:      false,
		isDel:   true,
		addedAt: time.Now().UnixNano(),
		size:    int64(len(key)) + hintOverheadDel,
	}
	h.enqueue(addr, it)
}

func (h *handoff[K, V]) enqueue(addr string, it *hintItem) {
	// when auto-paused due to backlog, drop newest enqueues to allow draining.
	if h.paused.Load() {
		return
	}

	q := h.queue(addr)

	ok, byDelta, itDelta := q.enqueue(it)
	if !ok {
		return
	}
	newItems := h.totalItems.Add(int64(itDelta))
	newBytes := h.totalBytes.Add(byDelta)

	// circuit breaker based on backlog (operator Pause is honored in loop)
	// auto-pauses new enqueues while allowing replay to drain.
	if (h.cfg.AutopauseItems > 0 && int(newItems) >= h.cfg.AutopauseItems) ||
		(h.cfg.AutopauseBytes > 0 && newBytes >= h.cfg.AutopauseBytes) {
		h.paused.Store(true)
	}
}

func (h *handoff[K, V]) loop() {
	if h.cfg.ReplayRPS <= 0 {
		<-h.stop
		return
	}

	const slotsPerSec = 100
	tick := time.NewTicker(time.Second / slotsPerSec)
	defer tick.Stop()

	perTick := h.cfg.ReplayRPS / slotsPerSec
	extra := h.cfg.ReplayRPS % slotsPerSec
	slot := 0

	house := time.NewTicker(2 * time.Second)
	defer house.Stop()

	// exponential backoff for retries; capped to keep queues moving.
	backoff := func(tries int) time.Duration {
		// 100ms, 200ms, 400ms ... cap at 5s
		d := 100 * time.Millisecond << uint(min(6, tries)) // up to ~6.4s
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
			// Operator pause fully stops replay; auto-pause only stops enqueues,
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
				// touch stats (and drop via popReady in steady state)
				i, b := q.stats()
				totalI += int64(i)
				totalB += b
			}
			h.mu.Unlock()
			h.totalItems.Store(totalI)
			h.totalBytes.Store(totalB)

			// auto-resume when sufficiently drained (hysteresis at ~60% of autopause threshold).
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
	hi.nextAt = time.Now().Add(backoff(hi.tries)).UnixNano()
	q.requeue(hi)
}

func (h *handoff[K, V]) replayOne(backoff func(int) time.Duration) bool {
	// fairshare round-robin over peers to avoid starving any one queue.
	h.mu.Lock()
	if len(h.peers) == 0 {
		h.mu.Unlock()
		return false
	}
	// collect keys to iterate deterministically: don't need order here,
	// just pick an index and iterate at most N peers.
	idx := int(h.rr)
	h.rr++

	// snapshot addresses
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
			msg := &MsgSet{Base: Base{T: MTSet, ID: id}, Key: hi.key, Val: hi.val, Exp: hi.exp, Ver: hi.ver, Cp: hi.cp}
			raw, err := pc.request(msg, id, h.node.cfg.Sec.WriteTimeout)
			if err != nil {
				h.node.resetPeer(addr)
				h.scheduleRetry(q, hi, backoff)
				continue
			}

			var resp MsgSetResp
			if e := cbor.Unmarshal(raw, &resp); e != nil || !resp.OK {
				h.node.resetPeer(addr)
				h.scheduleRetry(q, hi, backoff)
				continue
			}
			// success: drop
			return true
		}

		// delete hint
		msg := &MsgDel{Base: Base{T: MTDelete, ID: id}, Key: hi.key, Ver: hi.ver}
		raw, err := pc.request(msg, id, h.node.cfg.Sec.WriteTimeout)
		if err != nil {
			h.scheduleRetry(q, hi, backoff)
			continue
		}

		var resp MsgDelResp
		if e := cbor.Unmarshal(raw, &resp); e != nil || !resp.OK {
			h.scheduleRetry(q, hi, backoff)
			continue
		}
		return true
	}
	return false
}

func (h *handoff[K, V]) Stop() {
	select {
	case <-h.stop:
		return
	default:
		close(h.stop)
	}
}

func (h *handoff[K, V]) Pause() {
	h.paused.Store(true)
}

func (h *handoff[K, V]) Resume() {
	h.paused.Store(false)
}
