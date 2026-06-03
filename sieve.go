package kioshun

import (
	"sync"
	"sync/atomic"
)

const (
	defaultProbationRatio = 10
	defaultGhostRatio     = 100
)

const (
	maxItemReuse            = 3
	maxEvictionWork         = 32
	defaultMainVictimScan   = 8
	probationPromotionReuse = 1
)

// sieveQueueID mirrors queue ownership on cacheItem for cheap state inspection.
// the authoritative ownership pointer is cacheItem.sieveQ.
type sieveQueueID uint8

const (
	probationQueue sieveQueueID = iota
	mainQueue
)

// sieveVisited marks recent reuse. Readers write it at, while the
// serialized maintenance path consumes and clears it during SIEVE scans.
const sieveVisited = uint32(1)

func sieveItemVisited[K comparable, V any](it *cacheItem[K, V]) bool {
	return it != nil && atomic.LoadUint32(&it.visited) != 0
}

func markSieveItemVisited[K comparable, V any](it *cacheItem[K, V]) {
	if it != nil && atomic.LoadUint32(&it.visited) == 0 {
		atomic.StoreUint32(&it.visited, sieveVisited)
	}
}

func clearSieveItemVisited[K comparable, V any](it *cacheItem[K, V]) {
	if it != nil {
		atomic.StoreUint32(&it.visited, 0)
	}
}

// sieveQueue is an FIFO queue backed by cacheItem links.
type sieveQueue[K comparable, V any] struct {
	head cacheItem[K, V]
	tail cacheItem[K, V]
	size int64
}

func (q *sieveQueue[K, V]) init() {
	q.head.prev = nil
	q.head.next = &q.tail
	q.head.sieveQ = nil
	q.tail.prev = &q.head
	q.tail.next = nil
	q.tail.sieveQ = nil
	q.size = 0
}

func (q *sieveQueue[K, V]) pushFront(it *cacheItem[K, V]) {
	n := q.head.next
	q.head.next = it
	it.prev = &q.head
	it.next = n
	it.sieveQ = q
	n.prev = it
	q.size++
}

func (q *sieveQueue[K, V]) popBack() *cacheItem[K, V] {
	if q.empty() {
		return nil
	}

	it := q.tail.prev
	q.remove(it)
	return it
}

func (q *sieveQueue[K, V]) remove(it *cacheItem[K, V]) bool {
	if it == nil || it.sieveQ != q || it.prev == nil || it.next == nil {
		return false
	}
	if it.prev.next != it || it.next.prev != it {
		return false
	}

	it.prev.next = it.next
	it.next.prev = it.prev
	it.prev = nil
	it.next = nil
	it.sieveQ = nil
	if q.size > 0 {
		q.size--
	}
	return true
}

func (q *sieveQueue[K, V]) empty() bool {
	return q.size == 0
}

func (q *sieveQueue[K, V]) isSentinel(it *cacheItem[K, V]) bool {
	return it == &q.head || it == &q.tail
}

// holds reports whether it is a live, non-sentinel node currently linked into q.
// remove clears sieveQ on eviction and the head/tail guards never carry a queue
// pointer, so this one check subsumes the nil/guard/ownership tests the policy
// would otherwise spell out at each call site.
func (q *sieveQueue[K, V]) holds(it *cacheItem[K, V]) bool {
	return it != nil && it.sieveQ == q && it != &q.head && it != &q.tail
}

// adaptiveController accumulates the per-cycle signals that drive probation/main
// resizing. Every counter is maintained exclusively on the single-consumer
// maintenance path (the write worker, or a caller holding the shard write lock),
// so all increments, reads, and resets are plain — none of these fields is
// touched from the concurrent read path.
//
// mainSurvivals counts main queue residents the SIEVE hand spared during
// eviction sweeps (a set visited bit buys a second chance). It is the
// maintenance path proxy for "the main cache is earning its capacity", and
// replaces a per-read main-hit counter whose atomic increment contended on the
// read hot path. Counting survivors instead of raw hits also tracks how many
// distinct main items reads keep alive, rather than being skewed by a single
// "hammered" key.
type adaptiveController struct {
	ghostHits           uint64
	probationEvictions  uint64
	promotions          uint64
	mainSurvivals       uint64
	observationsInCycle uint64
}

func (c *adaptiveController) resetCycle() {
	c.ghostHits = 0
	c.probationEvictions = 0
	c.promotions = 0
	c.mainSurvivals = 0
	c.observationsInCycle = 0
}

type sieveTinyLFU[K comparable, V any] struct {
	probation sieveQueue[K, V]
	main      sieveQueue[K, V]
	ghost     ghostQueue
	sketch    countMinSketch
	door      doorkeeper

	controller adaptiveController
	stats      PolicyStats
	hand       *cacheItem[K, V]

	capacity        int64
	probationCap    int64
	mainCap         int64
	ghostCap        int64
	minProbationCap int64
	maxProbationCap int64
	adaptStep       int64
	adaptive        bool
}

// newSieveTinyLFU builds the per-shard admission state for a bounded shard.
// Probation is clamped between 1% and 60% of capacity so main has room for
// protected entries when capacity permits, while the ghost queue is sized as a
// fraction of main.
func newSieveTinyLFU[K comparable, V any](c int64, pr, gr uint8) *sieveTinyLFU[K, V] {
	p := &sieveTinyLFU[K, V]{capacity: c}
	p.probation.init()
	p.main.init()

	if pr == 0 {
		pr = defaultProbationRatio
	}
	if gr == 0 {
		gr = defaultGhostRatio
	}

	lo := max(int64(1), c/100)
	hi := max(lo, c*60/100)
	if hi >= c && c > 1 {
		hi = c - 1
	}

	pc := min(max(c*int64(pr)/100, lo), hi)

	mc := c - pc
	gc := mc * int64(gr) / 100
	if mc > 0 && gc < 1 {
		gc = 1
	}

	p.probationCap = pc
	p.mainCap = mc
	p.ghostCap = gc
	p.minProbationCap = lo
	p.maxProbationCap = hi
	p.adaptStep = max(int64(1), c/100)
	samples := uint64(max(c*10, int64(sketchMinCounters)))
	p.ghost = newGhostQueue(int(gc))
	p.sketch = newCountMinSketch(samples)
	p.door = newDoorkeeper(samples)
	return p
}

func (p *sieveTinyLFU[K, V]) recordAccess(h uint64) {
	p.incrementFrequency(h)
}

// incrementFrequency records one access in the doorkeeper/sketch pair and ages
// the window after resetAt samples. It runs on the serialized maintenance path,
// including sampled reads drained by the shard worker.
func (p *sieveTinyLFU[K, V]) incrementFrequency(h uint64) {
	if p.door.add(h) {
		p.sketch.add(h)
	}
	p.sketch.samples++
	if p.sketch.resetAt > 0 && p.sketch.samples >= p.sketch.resetAt {
		p.sketch.age()
		p.door.clear()
	}
	p.tick()
}

// estimate includes the doorkeeper bit as one recent access, so a key seen once
// can compete without immediately consuming count-min sketch counters.
func (p *sieveTinyLFU[K, V]) estimate(h uint64) uint8 {
	e := p.sketch.estimate(h)
	if p.door.contains(h) && e < sketchMaxCounter {
		e++
	}
	return e
}

// owns reports whether it currently resides in either SIEVE queue (probation or main).
func (p *sieveTinyLFU[K, V]) owns(it *cacheItem[K, V]) bool {
	return p.probation.holds(it) || p.main.holds(it)
}

func (p *sieveTinyLFU[K, V]) recordReadHit(it *cacheItem[K, V]) {
	// Reads only set the visited bit; queue ownership is maintained by the write.
	// markSieveItemVisited is a conditional atomic store (skipped once the bit is
	// set), so a hot item costs at most one shared-state load here. The adaptive
	// controller's "main is useful" signal is gathered on the maintenance path
	// (see findMainVictim), so the read path stays free of contended writes.
	if p.owns(it) {
		markSieveItemVisited(it)
	}
}

// recordUpdate handles Set on an existing resident. Updates are treated as
// reuse signals: main entries get another SIEVE chance, while probation entries
// can be promoted before they reach the probation tail.
func (p *sieveTinyLFU[K, V]) recordUpdate(it *cacheItem[K, V]) {
	switch it.sieveQ {
	case &p.main:
		it.queue = mainQueue
		markSieveItemVisited(it)
		if it.reuse < maxItemReuse {
			it.reuse++
		}
	case &p.probation:
		it.queue = probationQueue
		wasVisited := sieveItemVisited(it)
		if it.reuse < maxItemReuse {
			it.reuse++
		}
		if (wasVisited || it.reuse >= probationPromotionReuse) && p.mainCap > 0 {
			p.promote(it)
		} else {
			markSieveItemVisited(it)
		}
	}
}

// insert places a newly created resident into probation unless a ghost hit has
// already shown that the item was evicted too soon; ghost hits bypass probation
// and enter main as protected entries.
func (p *sieveTinyLFU[K, V]) insert(it *cacheItem[K, V], gh bool) {
	if gh && p.mainCap > 0 {
		p.ghost.remove(it.hash, it.tag)
		p.controller.ghostHits++
		p.stats.GhostHits++
		if p.adaptive && p.probationCap < p.maxProbationCap {
			p.setProbationCap(p.probationCap + p.adaptStep)
		}
		p.insertMain(it)
		return
	}

	it.queue = probationQueue
	it.reuse = 0
	clearSieveItemVisited(it)
	p.probation.pushFront(it)
}

// insertMain creates a visited main queue resident and seeds the SIEVE hand.
func (p *sieveTinyLFU[K, V]) insertMain(it *cacheItem[K, V]) {
	it.queue = mainQueue
	it.reuse = 1
	markSieveItemVisited(it)
	p.main.pushFront(it)
	if p.hand == nil {
		p.hand = it
	}
}

// remove unlinks an item from whichever SIEVE queue owns it and repairs the
// main hand if it was pointing at the removed node.
func (p *sieveTinyLFU[K, V]) remove(it *cacheItem[K, V]) bool {
	if it == nil {
		return false
	}

	removed := false
	switch it.sieveQ {
	case &p.main:
		if p.hand == it {
			p.hand = p.previousMainItem(it)
		}
		removed = p.main.remove(it)
	case &p.probation:
		removed = p.probation.remove(it)
	default:
		if p.hand == it {
			p.hand = nil
		}
	}
	if !removed {
		return false
	}

	it.queue = probationQueue
	it.sieveQ = nil
	it.reuse = 0
	clearSieveItemVisited(it)
	return true
}

func (p *sieveTinyLFU[K, V]) reset() {
	p.probation.init()
	p.main.init()
	p.ghost.clear()
	p.sketch.clear()
	p.door.clear()
	p.controller.resetCycle()
	p.stats = PolicyStats{}
	p.hand = nil
}

func (p *sieveTinyLFU[K, V]) promote(it *cacheItem[K, V]) {
	if it == nil || it.sieveQ == &p.main || p.mainCap <= 0 {
		return
	}
	if it.sieveQ != &p.probation {
		return
	}

	p.probation.remove(it)
	p.insertMain(it)
	p.controller.promotions++
	p.stats.Promotions++
}

// dropProbationVictim records a probation eviction and remembers the key for
// possible ghost hit readmission.
func (s *shard[K, V]) dropProbationVictim(it *cacheItem[K, V], pool *sync.Pool, stats bool) bool {
	p := s.sieve
	h, tag := it.hash, it.tag
	if !s.dropSieveItem(it, pool, stats, RemovedCapacity) {
		return false
	}
	p.controller.probationEvictions++
	p.stats.ProbationEvictions++
	p.ghost.add(h, tag)
	return true
}

// evictProbation inspects the oldest probation entry. A recently reused entry
// is promoted and returned as an in-flight main candidate; a cold entry is
// evicted and recorded in the ghost queue.
func (s *shard[K, V]) evictProbation(pool *sync.Pool, stats bool) *cacheItem[K, V] {
	p := s.sieve
	if p.probation.empty() {
		return nil
	}

	it := p.probation.tail.prev
	if !p.probation.holds(it) {
		return nil
	}
	if it.reuse >= probationPromotionReuse || sieveItemVisited(it) {
		p.promote(it)
		return it
	}

	s.dropProbationVictim(it, pool, stats)
	return nil
}

// evictMain runs the SIEVE hand over main and applies TinyLFU admission when an
// in-flight candidate competes with the selected victim. Dropping a rejected
// candidate is not counted as an eviction because the policy rejected the
// candidate rather than selecting a replacement victim.
func (s *shard[K, V]) evictMain(
	pool *sync.Pool,
	stats bool,
	in *cacheItem[K, V],
	tie bool,
	scan int64,
	force bool,
) bool {
	p := s.sieve
	if in != nil && (!s.ownsItem(in) || !p.owns(in)) {
		in = nil
		tie = false
	}

	v := p.findMainVictim(scan, force)
	if v == nil {
		if force && in != nil {
			s.dropSieveItem(in, pool, stats, RemovedRejected)
			return true
		}
		return false
	}

	if in != nil && in != v && !p.shouldAdmit(in, v, tie) {
		return s.dropSieveItem(in, pool, stats, RemovedRejected)
	}

	if s.dropSieveItem(v, pool, stats, RemovedCapacity) {
		p.stats.MainEvictions++
		return true
	}
	return false
}

// enforceSieveCapacity restores the shard cap after an insert may have
// overfilled the cache. The bounded pass favors normal SIEVE decisions; the
// forced pass is a last-resort repair so promotions cannot leave the shard over
// capacity.
func (s *shard[K, V]) enforceSieveCapacity(
	pool *sync.Pool,
	stats bool,
	in *cacheItem[K, V],
	tie bool,
) {
	p := s.sieve
	// evicts the probation tail; a promoted survivor becomes
	// the in-flight candidate weighed against the next main victim.
	admitFromProbation := func() {
		if it := s.evictProbation(pool, stats); it != nil {
			in, tie = it, true
		}
	}

	work := maxEvictionWork
	for work > 0 && s.overCapacity() {
		switch {
		case p.probation.size > p.probationCap && !p.probation.empty():
			admitFromProbation()
		case (p.main.size > p.mainCap || s.overCapacity()) && !p.main.empty():
			if s.evictMain(pool, stats, in, tie, defaultMainVictimScan, false) {
				in, tie = nil, false
			}
		case s.overCapacity() && !p.probation.empty():
			admitFromProbation()
		default:
			return
		}
		work--
	}

	if !s.overCapacity() {
		return
	}

	// A Set can only overfill the shard by one item, but the bounded pass above
	// may spend its work budget promoting probation entries instead of dropping
	// them. The forced tail keeps admission bounded while restoring capacity.
	if (p.probation.size > p.probationCap || s.overCapacity()) && !p.probation.empty() {
		admitFromProbation()
	}
	if (p.main.size > p.mainCap || s.overCapacity()) && !p.main.empty() {
		s.evictMain(pool, stats, in, tie, defaultMainVictimScan, true)
	}
	if s.overCapacity() {
		s.forceDropSieveItem(pool, stats)
	}
}

func (s *shard[K, V]) overCapacity() bool {
	// warm-fill to the shard cap; enforce probation/main pressure only after a
	// new admission would exceed resident capacity.
	return atomic.LoadInt64(&s.size) > s.cap
}

// mainCandidate normalizes a traversal cursor to a live main node: it falls back
// to the main tail when the cursor has drifted off the queue and reports false
// when main has no real (non-sentinel) node left to consider.
func (p *sieveTinyLFU[K, V]) mainCandidate(it *cacheItem[K, V]) (*cacheItem[K, V], bool) {
	if !p.main.holds(it) {
		it = p.main.tail.prev
	}
	if p.main.isSentinel(it) {
		return nil, false
	}
	return it, true
}

// findMainVictim advances the SIEVE hand through main. Visited entries get a
// second chance and have their bit cleared; unvisited entries are returned as
// victims. A forced scan returns a victim even after the regular scan budget is
// exhausted.
func (p *sieveTinyLFU[K, V]) findMainVictim(scan int64, force bool) *cacheItem[K, V] {
	if p.main.empty() {
		return nil
	}
	if scan <= 0 {
		scan = 1
	}

	it := p.hand
	n := scan
	for n > 0 {
		var ok bool
		if it, ok = p.mainCandidate(it); !ok {
			return nil
		}
		if sieveItemVisited(it) {
			clearSieveItemVisited(it)
			if p.adaptive {
				// a maintenance-path observation that reads are keeping main entries alive.
				// This is the signal the adaptive shrink decision consumes, gathered here
				// instead of via a contended counter on every main read hit.
				p.controller.mainSurvivals++
			}
			if it.reuse > 0 {
				it.reuse--
			}
			it = p.previousMainItem(it)
			n--
			continue
		}

		p.hand = p.previousMainItem(it)
		return it
	}

	if force {
		it, ok := p.mainCandidate(it)
		if !ok {
			return nil
		}
		p.hand = p.previousMainItem(it)
		return it
	}

	p.hand = it
	return nil
}

// previousMainItem moves the hand toward older main entries and wraps at the
// head sentinel. It returns nil if the next pointer no longer names a live main
// resident.
func (p *sieveTinyLFU[K, V]) previousMainItem(it *cacheItem[K, V]) *cacheItem[K, V] {
	if it == nil {
		return nil
	}

	prev := it.prev
	if prev == nil || prev == &p.main.head {
		prev = p.main.tail.prev
	}
	if prev == it || !p.main.holds(prev) {
		return nil
	}
	return prev
}

// shouldAdmit compares the candidate and victim frequency estimates, with
// recency-based tie breaks for ghost hits and probation promotions.
func (p *sieveTinyLFU[K, V]) shouldAdmit(in, v *cacheItem[K, V], tie bool) bool {
	// ghost hit or probation promotion has already proven short-term reuse.
	// Once SIEVE finds an unvisited victim, that recency proof should beat stale
	// sketch history.
	if tie && !sieveItemVisited(v) {
		return true
	}

	cf := p.estimate(in.hash)
	vf := p.estimate(v.hash)
	if cf > vf {
		return true
	}
	if cf < vf {
		if tie && cf+1 >= vf {
			return true
		}
		return false
	}
	return tie || (!sieveItemVisited(v) && v.reuse == 0)
}

// tick advances the adaptive segment controller. It waits for a window of
// observations before resizing so transient bursts do not immediately move
// capacity between probation and main.
func (p *sieveTinyLFU[K, V]) tick() {
	if !p.adaptive {
		return
	}

	p.controller.observationsInCycle++
	win := uint64(p.capacity * 10)
	if win == 0 || p.controller.observationsInCycle < win {
		return
	}

	switch {
	case p.controller.ghostHits > p.controller.probationEvictions/4:
		if p.probationCap < p.maxProbationCap {
			p.setProbationCap(p.probationCap + p.adaptStep)
		}
	case p.controller.probationEvictions > p.controller.promotions*2 &&
		p.controller.mainSurvivals > p.controller.promotions:
		if p.probationCap > p.minProbationCap {
			p.setProbationCap(p.probationCap - p.adaptStep)
		}
	}

	p.controller.resetCycle()
}

// setProbationCap clamps probation and assigns the remainder to main, preserving
// the fixed per-shard resident capacity.
func (p *sieveTinyLFU[K, V]) setProbationCap(n int64) {
	n = min(max(n, p.minProbationCap), p.maxProbationCap)
	p.probationCap = n
	p.mainCap = p.capacity - n
}

// forceDropSieveItem is the final capacity repair path. It removes the
// probation tail first, then a forced main victim if probation is empty.
func (s *shard[K, V]) forceDropSieveItem(pool *sync.Pool, stats bool) bool {
	p := s.sieve
	if !p.probation.empty() {
		it := p.probation.tail.prev
		if !p.probation.holds(it) {
			return false
		}
		return s.dropProbationVictim(it, pool, stats)
	}
	if !p.main.empty() {
		it := p.findMainVictim(1, true)
		if it == nil {
			return false
		}
		if s.dropSieveItem(it, pool, stats, RemovedCapacity) {
			p.stats.MainEvictions++
			return true
		}
	}
	return false
}

func (s *shard[K, V]) dropSieveItem(it *cacheItem[K, V], pool *sync.Pool, stats bool, reason RemovalReason) bool {
	return s.dropItem(it, pool, stats, reason, dropSieve)
}
