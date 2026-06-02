package kioshun

import (
	"math/bits"
	"sync"
	"sync/atomic"

	"github.com/unkn0wn-root/kioshun/internal/mathutil"
)

const (
	defaultProbationRatio = 10
	defaultGhostRatio     = 100
)

const (
	sketchMinCounters     = 1024
	sketchAgingMultiplier = 10
	sketchCountersPerWord = 16
	sketchCounterBits     = 4
	sketchCounterMask     = 0x0f
	sketchMaxCounter      = 15
	// aging shifts packed 4-bit counters right by one; this mask clears bits
	// that would bleed across nibble boundaries. Keep tied to sketchCounterBits.
	sketchCounterAgingMask  = 0x7777777777777777
	sketchHashRotation0     = 0
	sketchHashRotation1     = 17
	sketchHashRotation2     = 31
	sketchHashRotation3     = 47
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

type ghostEntry struct {
	hash uint64
	tag  uint16
}

type ghostQueue struct {
	entries []ghostEntry
	index   map[ghostEntry]int
	next    int
}

func newGhostQueue(n int) ghostQueue {
	if n <= 0 {
		return ghostQueue{}
	}

	return ghostQueue{
		entries: make([]ghostEntry, n),
		index:   make(map[ghostEntry]int, n),
	}
}

func (g *ghostQueue) contains(h uint64, t uint16) bool {
	if g.index == nil {
		return false
	}

	_, ok := g.index[ghostEntry{hash: h, tag: t}]
	return ok
}

func (g *ghostQueue) add(h uint64, t uint16) {
	if len(g.entries) == 0 {
		return
	}

	e := ghostEntry{hash: h, tag: t}
	if g.index == nil {
		g.index = make(map[ghostEntry]int, len(g.entries))
	}
	if _, ok := g.index[e]; ok {
		return
	}

	old := g.entries[g.next]
	if i, ok := g.index[old]; ok && i == g.next {
		delete(g.index, old)
	}

	g.entries[g.next] = e
	g.index[e] = g.next
	g.next = (g.next + 1) % len(g.entries)
}

func (g *ghostQueue) remove(h uint64, t uint16) bool {
	if g.index == nil {
		return false
	}

	e := ghostEntry{hash: h, tag: t}
	i, ok := g.index[e]
	if !ok {
		return false
	}

	delete(g.index, e)
	if i >= 0 && i < len(g.entries) && g.entries[i] == e {
		g.entries[i] = ghostEntry{}
	}
	return true
}

func (g *ghostQueue) clear() {
	clear(g.entries)
	clear(g.index)
	g.next = 0
}

type doorkeeper struct {
	bits []uint64
	mask uint64
}

func newDoorkeeper(n uint64) doorkeeper {
	if n < 64 {
		n = 64
	}
	n = uint64(mathutil.NextPowerOf2(int(n)))
	return doorkeeper{
		bits: make([]uint64, n/64),
		mask: n - 1,
	}
}

func (d *doorkeeper) add(h uint64) bool {
	if len(d.bits) == 0 {
		return true
	}

	i, j := d.indexes(h)
	exists := d.has(i) && d.has(j)
	d.set(i)
	d.set(j)
	return exists
}

func (d *doorkeeper) contains(h uint64) bool {
	if len(d.bits) == 0 {
		return false
	}

	i, j := d.indexes(h)
	return d.has(i) && d.has(j)
}

func (d *doorkeeper) clear() {
	clear(d.bits)
}

func (d *doorkeeper) indexes(h uint64) (uint64, uint64) {
	return sketchIndex(h, d.mask, sketchHashRotation1), sketchIndex(h, d.mask, sketchHashRotation3)
}

func (d *doorkeeper) has(i uint64) bool {
	return d.bits[i/64]&(uint64(1)<<(i%64)) != 0
}

func (d *doorkeeper) set(i uint64) {
	d.bits[i/64] |= uint64(1) << (i % 64)
}

type countMinSketch struct {
	counters []uint64
	mask     uint64
	samples  uint64
	resetAt  uint64
}

func newCountMinSketch(n uint64) countMinSketch {
	if n < sketchMinCounters {
		n = sketchMinCounters
	}
	n = uint64(mathutil.NextPowerOf2(int(n)))

	w := n / sketchCountersPerWord
	if w == 0 {
		w = 1
	}

	return countMinSketch{
		counters: make([]uint64, w),
		mask:     n - 1,
		resetAt:  n * sketchAgingMultiplier,
	}
}

func (s *countMinSketch) increment(h uint64) {
	s.add(h)
	s.samples++
	if s.resetAt > 0 && s.samples >= s.resetAt {
		s.age()
	}
}

func (s *countMinSketch) add(h uint64) {
	if len(s.counters) == 0 {
		return
	}

	idx := s.indexes(h)
	if s.minCounter(idx) < sketchMaxCounter {
		for _, i := range idx {
			s.incrementCounter(i)
		}
	}
}

func (s *countMinSketch) estimate(h uint64) uint8 {
	if len(s.counters) == 0 {
		return 0
	}

	return s.minCounter(s.indexes(h))
}

func (s *countMinSketch) indexes(h uint64) [4]uint64 {
	return [4]uint64{
		sketchIndex(h, s.mask, sketchHashRotation0),
		sketchIndex(h, s.mask, sketchHashRotation1),
		sketchIndex(h, s.mask, sketchHashRotation2),
		sketchIndex(h, s.mask, sketchHashRotation3),
	}
}

func (s *countMinSketch) minCounter(idx [4]uint64) uint8 {
	min := s.counter(idx[0])
	for _, i := range idx[1:] {
		if v := s.counter(i); v < min {
			min = v
		}
	}

	return min
}

func (s *countMinSketch) age() {
	for i := range s.counters {
		s.counters[i] = (s.counters[i] >> 1) & sketchCounterAgingMask
	}
	s.samples = 0
}

func (s *countMinSketch) clear() {
	clear(s.counters)
	s.samples = 0
}

func (s *countMinSketch) counter(i uint64) uint8 {
	wi := i / sketchCountersPerWord
	ci := i % sketchCountersPerWord
	sh := ci * sketchCounterBits
	return uint8((s.counters[wi] >> sh) & sketchCounterMask)
}

func (s *countMinSketch) incrementCounter(i uint64) {
	wi := i / sketchCountersPerWord
	ci := i % sketchCountersPerWord
	sh := ci * sketchCounterBits
	v := (s.counters[wi] >> sh) & sketchCounterMask
	if v < sketchMaxCounter {
		s.counters[wi] += 1 << sh
	}
}

func sketchIndex(h, m uint64, rot int) uint64 {
	h = bits.RotateLeft64(h, rot)
	return xxHash64Avalanche(h) & m
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
