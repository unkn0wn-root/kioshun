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

type sieveQueueID uint8

const (
	probationQueue sieveQueueID = iota
	mainQueue
)

const sieveVisited = uint32(1)

func sieveItemVisited[V any](it *cacheItem[V]) bool {
	return it != nil && atomic.LoadUint32(&it.visited) != 0
}

func markSieveItemVisited[V any](it *cacheItem[V]) {
	if it != nil && atomic.LoadUint32(&it.visited) == 0 {
		atomic.StoreUint32(&it.visited, sieveVisited)
	}
}

func clearSieveItemVisited[V any](it *cacheItem[V]) {
	if it != nil {
		atomic.StoreUint32(&it.visited, 0)
	}
}

// sieveQueue is an intrusive FIFO queue backed by cacheItem links.
type sieveQueue[V any] struct {
	head cacheItem[V]
	tail cacheItem[V]
	size int64
}

func (q *sieveQueue[V]) init() {
	q.head.prev = nil
	q.head.next = &q.tail
	q.head.sieveQ = nil
	q.tail.prev = &q.head
	q.tail.next = nil
	q.tail.sieveQ = nil
	q.size = 0
}

func (q *sieveQueue[V]) pushFront(it *cacheItem[V]) {
	n := q.head.next
	q.head.next = it
	it.prev = &q.head
	it.next = n
	it.sieveQ = q
	n.prev = it
	q.size++
}

func (q *sieveQueue[V]) popBack() *cacheItem[V] {
	if q.empty() {
		return nil
	}

	it := q.tail.prev
	q.remove(it)
	return it
}

func (q *sieveQueue[V]) remove(it *cacheItem[V]) bool {
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

func (q *sieveQueue[V]) empty() bool {
	return q.size == 0
}

func (q *sieveQueue[V]) isSentinel(it *cacheItem[V]) bool {
	return it == &q.head || it == &q.tail
}

// holds reports whether it is a live, non-sentinel node currently linked into q.
// remove clears sieveQ on eviction and the head/tail guards never carry a queue
// pointer, so this one check subsumes the nil/guard/ownership tests the policy
// would otherwise spell out at each call site.
func (q *sieveQueue[V]) holds(it *cacheItem[V]) bool {
	return it != nil && it.sieveQ == q && it != &q.head && it != &q.tail
}

type adaptiveController struct {
	ghostHits           uint64
	probationEvictions  uint64
	promotions          uint64
	mainHits            uint64
	observationsInCycle uint64
}

type sieveTinyLFU[V any] struct {
	probation sieveQueue[V]
	main      sieveQueue[V]
	ghost     ghostQueue
	sketch    countMinSketch
	door      doorkeeper

	controller adaptiveController
	stats      PolicyStats
	hand       *cacheItem[V]

	capacity        int64
	probationCap    int64
	mainCap         int64
	ghostCap        int64
	minProbationCap int64
	maxProbationCap int64
	adaptStep       int64
	adaptive        bool
}

func newSieveTinyLFU[V any](c int64, pr, gr uint8) *sieveTinyLFU[V] {
	p := &sieveTinyLFU[V]{capacity: c}
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

func (p *sieveTinyLFU[V]) recordAccess(h uint64) {
	p.incrementFrequency(h)
}

func (p *sieveTinyLFU[V]) incrementFrequency(h uint64) {
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

func (p *sieveTinyLFU[V]) estimate(h uint64) uint8 {
	e := p.sketch.estimate(h)
	if p.door.contains(h) && e < sketchMaxCounter {
		e++
	}
	return e
}

// owns reports whether it currently resides in either SIEVE queue (probation or main).
func (p *sieveTinyLFU[V]) owns(it *cacheItem[V]) bool {
	return p.probation.holds(it) || p.main.holds(it)
}

func (p *sieveTinyLFU[V]) recordReadHit(it *cacheItem[V]) {
	// reads only set the visited bit.
	// queue ownership is maintained by the write.
	if p.owns(it) {
		markSieveItemVisited(it)
	}
}

func (p *sieveTinyLFU[V]) recordUpdate(it *cacheItem[V]) {
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

func (p *sieveTinyLFU[V]) insert(it *cacheItem[V], gh bool) {
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
func (p *sieveTinyLFU[V]) insertMain(it *cacheItem[V]) {
	it.queue = mainQueue
	it.reuse = 1
	markSieveItemVisited(it)
	p.main.pushFront(it)
	if p.hand == nil {
		p.hand = it
	}
}

func (p *sieveTinyLFU[V]) remove(it *cacheItem[V]) bool {
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

func (p *sieveTinyLFU[V]) reset() {
	p.probation.init()
	p.main.init()
	p.ghost.clear()
	p.sketch.clear()
	p.door.clear()
	p.controller = adaptiveController{}
	p.stats = PolicyStats{}
	p.hand = nil
}

func (p *sieveTinyLFU[V]) promote(it *cacheItem[V]) {
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
// possible ghost-hit readmission.
func (s *shard[K, V]) dropProbationVictim(it *cacheItem[V], pool *sync.Pool, stats bool) bool {
	p := s.sieve
	h, tag := it.hash, it.tag
	if !s.dropSieveItem(it, pool, stats, true) {
		return false
	}
	p.controller.probationEvictions++
	p.stats.ProbationEvictions++
	p.ghost.add(h, tag)
	return true
}

func (s *shard[K, V]) evictProbation(pool *sync.Pool, stats bool) *cacheItem[V] {
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

func (s *shard[K, V]) evictMain(
	pool *sync.Pool,
	stats bool,
	in *cacheItem[V],
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
			s.dropSieveItem(in, pool, stats, false)
			return true
		}
		return false
	}

	if in != nil && in != v && !p.shouldAdmit(in, v, tie) {
		return s.dropSieveItem(in, pool, stats, false)
	}

	if s.dropSieveItem(v, pool, stats, true) {
		p.stats.MainEvictions++
		return true
	}
	return false
}

func (s *shard[K, V]) enforceSieveCapacity(
	pool *sync.Pool,
	stats bool,
	in *cacheItem[V],
	tie bool,
) {
	p := s.sieve
	// admitFromProbation evicts the probation tail; a promoted survivor becomes
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
func (p *sieveTinyLFU[V]) mainCandidate(it *cacheItem[V]) (*cacheItem[V], bool) {
	if !p.main.holds(it) {
		it = p.main.tail.prev
	}
	if p.main.isSentinel(it) {
		return nil, false
	}
	return it, true
}

func (p *sieveTinyLFU[V]) findMainVictim(scan int64, force bool) *cacheItem[V] {
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

func (p *sieveTinyLFU[V]) previousMainItem(it *cacheItem[V]) *cacheItem[V] {
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

func (p *sieveTinyLFU[V]) shouldAdmit(in, v *cacheItem[V], tie bool) bool {
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

func (p *sieveTinyLFU[V]) tick() {
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
		p.controller.mainHits > p.controller.promotions:
		if p.probationCap > p.minProbationCap {
			p.setProbationCap(p.probationCap - p.adaptStep)
		}
	}

	p.controller.ghostHits = 0
	p.controller.probationEvictions = 0
	p.controller.promotions = 0
	p.controller.mainHits = 0
	p.controller.observationsInCycle = 0
}

func (p *sieveTinyLFU[V]) setProbationCap(n int64) {
	n = min(max(n, p.minProbationCap), p.maxProbationCap)
	p.probationCap = n
	p.mainCap = p.capacity - n
}

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
		if s.dropSieveItem(it, pool, stats, true) {
			p.stats.MainEvictions++
			return true
		}
	}
	return false
}

func (s *shard[K, V]) dropSieveItem(it *cacheItem[V], pool *sync.Pool, stats bool, evicted bool) bool {
	return s.dropItem(it, pool, stats, evicted, dropSieve)
}
