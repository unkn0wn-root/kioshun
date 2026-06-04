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

const (
	// probationResurrectLow is the dual ghost resurrection rate
	// (cycleB2Hits/cycleMainEvicts) below which evicted main victims count as
	// "abandoned". Below it adaptSize grows the probation recency window.
	probationResurrectLow = 0.10

	// probationGrowStepPct is how much of total capacity the probation window
	// grows per maintenance window when a shifting hot set is detected. Larger
	// than adaptStep (1%) so the window reaches a useful size within the short
	// adaptation budget a shifting workload allows.
	probationGrowStepPct = 10
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
	if !q.holds(it) || it.prev == nil || it.next == nil {
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
// pointer so this one check subsumes the nil/guard/ownership tests the policy
// would otherwise spell out at each call site.
func (q *sieveQueue[K, V]) holds(it *cacheItem[K, V]) bool {
	return it != nil && it.sieveQ == q && it != &q.head && it != &q.tail
}

// adaptiveController accumulates the per-cycle signals that drive probation/main
// resizing. Every counter is maintained exclusively on the single consumer
// maintenance path (the write worker or a caller holding the shard write lock),
// so all increments, reads and resets are plain
//
// mainSurvivals counts main queue residents the SIEVE hand spared during
// eviction sweeps (a set visited bit buys a second chance). It is the
// maintenance path proxy for "the main cache is earning its capacity" annd
// replaces a per-read main hit counter whose increment contended on the
// read hot path. Counting survivors instead of raw hits also tracks how many
// distinct main items reads keep alive, rather than being skewed by a single
// "hammered" key.
type adaptiveController struct {
	ghostHits           uint64
	probationEvictions  uint64
	promotions          uint64
	mainSurvivals       uint64
	observationsInCycle uint64

	// churn cost for the admission tuner: evictions are residents displaced to
	// stay in capacity, rejects are admitted candidates dropped again. Their sum
	// falls when admission keeps a stable working set and rises when it thrashes,
	// so it is the maintenance path proxy the admission tuner watches.
	cycleEvictions uint64
	cycleRejects   uint64

	// per-cycle dual-ghost signals. cycleMainEvicts counts main victims dropped
	// this cycle; cycleB2Hits counts inserts whose key was a recent main victim
	// (a resurrection). The rate cycleB2Hits/cycleMainEvicts is high
	// on loops (we keep evicting items we still need) and near zero on
	// shifting/bursty/zipf workloads which is what the addmision tuner keys on.
	cycleMainEvicts uint64
	cycleB2Hits     uint64
}

func (c *adaptiveController) resetCycle() {
	*c = adaptiveController{}
}

func (c *adaptiveController) churnCost() float64 {
	return float64(c.cycleEvictions + c.cycleRejects)
}

// resurrectionRate is the share of this cycle's main eviction victims that were
// reinserted while still in the B2 ghost: near 1 when a working set larger than
// capacity keeps evicting items it still needs (a cyclic "loop"), near 0 when
// evicted victims are abandoned (a shifting hot set). It is the central
// dual-ghost signal both self-tuning controllers key off - the admission tuner
// to trial frequency, the segment sizer to grow the recency window so it is
// named once here. Zero when main is not evicting (no signal).
func (c *adaptiveController) resurrectionRate() float64 {
	if c.cycleMainEvicts == 0 {
		return 0
	}
	return float64(c.cycleB2Hits) / float64(c.cycleMainEvicts)
}

// selects how shouldAdmit breaks frequency ties between an
// in-flight candidate and the SIEVE victim it would replace. The admission tuner
// switches a shard between the two automatically; neither is configurable.
type admissionMode uint8

const (
	// default. Candidates with proven short-term reuse (ghost
	// hits, probation promotions) win ties, which captures shifting and bursty
	// working sets quickly. The cost is thrashing on stationary cyclic workloads
	// whose footprint exceeds capacity.
	admitRecency admissionMode = iota
	// plain TinyLFU: a candidate is admitted only when its
	// frequency estimate strictly beats the victim's, so the incumbent wins ties.
	// This pins a stable resident set for loop-like workloads.
	admitFrequency
)

// tunerState tracks the admission tuner's guarded switch to frequency admission.
type tunerState uint8

const (
	tunerRecency   tunerState = iota // default; watching for the loop signature
	tunerTrial                       // running frequency one cycle to confirm it helps
	tunerFrequency                   // committed to frequency until churn climbs back
)

const (
	admissionEntryEvidence = 3.0
	admissionResurrectHigh = 0.5 // per-cycle resurrection rate that counts as evidence
	admissionCommitFactor  = 0.9 // trial commits if churn < recency baseline * this
	admissionRevertFactor  = 1.5 // committed reverts if churn > committed low * this
	admissionChurnEWMA     = 0.5 // churn baseline smoothing
	admissionBackoffStart  = 3   // recency cooldown cycles after a reverted trial
	admissionBackoffMax    = 96  // ceiling on the doubling backoff
)

// admissionTuner self-selects a shard's admissionMode with no configuration. It
// defaults to recency and switches to frequency only after the B2 ghost shows
// the loop signature; main eviction victims that keep resurrecting, i.e. the
// cache evicting items it still needs. A guarded one-cycle trial confirms
// frequency actually cuts churn before committing and a committed shard reverts
// (with backoff) the moment churn climbs back, so a workload that
// shifts out of its loop is not starved. All state is maintained on the
// single consumer maintenance path (see tick) so the fields are plain.
type admissionTuner struct {
	mode      admissionMode
	state     tunerState
	baseChurn float64 // EWMA churn while running recency
	lowChurn  float64 // EWMA churn while committed to frequency
	evidence  float64 // accumulated resurrection evidence toward a trial
	cooldown  int
	backoff   int
}

func (t *admissionTuner) reset() {
	*t = admissionTuner{backoff: admissionBackoffStart}
}

// revertToRecency drops a shard back to recency admission and grows the cooldown
// so a workload that does not benefit from frequency stops retrialing.
func (t *admissionTuner) revertToRecency() {
	t.mode = admitRecency
	t.state = tunerRecency
	t.evidence = 0
	t.cooldown = t.backoff
	t.backoff = min(t.backoff*2, admissionBackoffMax)
}

type sieveTinyLFU[K comparable, V any] struct {
	probation sieveQueue[K, V]
	main      sieveQueue[K, V]
	ghost     ghostQueue // B1: recently evicted probation fingerprints
	mghost    ghostQueue // B2: recently evicted main fingerprints (resurrection signal)
	sketch    countMinSketch
	door      doorkeeper

	controller adaptiveController
	tuner      admissionTuner
	stats      PolicyStats
	hand       *cacheItem[K, V]

	capacity        int64
	probationCap    int64
	mainCap         int64
	ghostCap        int64
	minProbationCap int64
	maxProbationCap int64
	adaptStep       int64
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
	p.tuner.reset()
	samples := uint64(max(c*10, int64(sketchMinCounters)))
	p.ghost = newGhostQueue(int(gc))
	// B2 holds roughly one shard-capacity of recent main-eviction fingerprints,
	// enough to detect a loop whose footprint exceeds capacity.
	p.mghost = newGhostQueue(int(c))
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
	// A key that was a recent main eviction victim is "resurrecting": the cache
	// evicted it too soon. The admission tuner counts resurrections to detect a
	// loop-like workload (see tuneAdmission); placement itself is unchanged.
	if p.mghost.contains(it.hash, it.tag) {
		p.mghost.remove(it.hash, it.tag)
		p.controller.cycleB2Hits++
	}

	if gh && p.mainCap > 0 {
		p.ghost.remove(it.hash, it.tag)
		p.controller.ghostHits++
		p.stats.GhostHits++
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
	p.mghost.clear()
	p.sketch.clear()
	p.door.clear()
	p.controller.resetCycle()
	p.tuner.reset()
	p.stats = PolicyStats{}
	p.hand = nil
}

func (p *sieveTinyLFU[K, V]) promote(it *cacheItem[K, V]) {
	if it == nil || it.sieveQ != &p.probation || p.mainCap <= 0 {
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
	p.controller.cycleEvictions++
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
			p.controller.cycleRejects++
			s.dropSieveItem(in, pool, stats, RemovedRejected)
			return true
		}
		return false
	}

	if in != nil && in != v && !p.shouldAdmit(in, v, tie) {
		p.controller.cycleRejects++
		return s.dropSieveItem(in, pool, stats, RemovedRejected)
	}

	vh, vtag := v.hash, v.tag // capture before drop zeroes the item for B2
	if s.dropSieveItem(v, pool, stats, RemovedCapacity) {
		p.controller.cycleEvictions++
		p.controller.cycleMainEvicts++
		p.mghost.add(vh, vtag) // record for resurrection detection
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

	// A Set can only overfill the shard by one item but the bounded pass above
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
	// warm fill to the shard cap; enforce probation/main pressure only after a
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
			// a maintenance path observation that reads are keeping main entries
			// alive. This is the signal the adaptive shrink decision consumes,
			// gathered here instead of via a contended counter on every main read hit.
			p.controller.mainSurvivals++
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

// shouldAdmit compares the candidate and victim frequency estimates. The
// admission tuner (see tuneAdmission) selects how frequency ties are broken: the
// default recency mode lets proven short-term reuse win, while frequency mode is
// plain TinyLFU where the incumbent wins ties to pin a stable set for loops.
func (p *sieveTinyLFU[K, V]) shouldAdmit(in, v *cacheItem[K, V], tie bool) bool {
	if p.tuner.mode == admitFrequency {
		// plain TinyLFU: incumbent wins ties, pinning a stable set for loops.
		return p.estimate(in.hash) > p.estimate(v.hash)
	}

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

// tick advances the self-tuning controllers once per window of observations. The
// window keeps transient bursts from immediately moving the probation/main split
// or flipping admission mode. Both controllers consume the same cycle counters,
// so they run together before the cycle resets.
func (p *sieveTinyLFU[K, V]) tick() {
	p.controller.observationsInCycle++
	win := uint64(p.capacity * 10)
	if win == 0 || p.controller.observationsInCycle < win {
		return
	}

	p.tuneAdmission()
	p.adaptSize()
	p.controller.resetCycle()
}

// adaptSize moves capacity between probation (the recency window for new entries)
// and main (the frequency-protected SIEVE queue), reading only counters already
// maintained on this single consumer maintenance path.
//
// The hard case is telling a stationary skew (which wants a tiny probation so
// main pins the hot set) apart from a shifting hot set (which wants a large
// recency window, like LRU): both show heavy probation churn and frequent B1
// ghost hits, so neither signal separates them. The dual-ghost resurrection rate
// does. When main is churning yet its victims are abandoned (they do not come
// back - low cycleB2Hits/cycleMainEvicts) while probation keeps reevicting
// entries that DO return (ghostHits > promotions), the working set is shifting
// out from under main, so the recency window is grown aggressively. The
// mainEvicts>promotions gate keeps a stable main (where mainEvicts is ~0, making
// the resurrection rate read as 0) from being mistaken for a shift, and the
// loop guard keeps a cyclic workload - which the admission tuner is pinning with
// frequency - on a small probation.
//
// Otherwise it falls back to the original recency heuristic: grow modestly when
// B1 ghost hits dominate probation evictions (probation too small, the loop grow
// path included), shrink when probation churns far more than it promotes while
// main keeps earning its keep (probation too large for a stationary workload).
func (p *sieveTinyLFU[K, V]) adaptSize() {
	c := &p.controller
	resurrect := c.resurrectionRate()
	// A cyclic ("loop") workload is identified by the admission tuner accumulating
	// resurrection evidence (or already committed to frequency); while that
	// signature is present probation stays small so the pinned set holds.
	loopish := p.tuner.mode == admitFrequency || p.tuner.evidence > 0

	switch {
	case !loopish && c.cycleMainEvicts > c.promotions &&
		resurrect < probationResurrectLow && c.ghostHits > c.promotions:
		// Shifting hot set: grow the recency window quickly so newly hot entries
		// survive to their reuse instead of being evicted from a tiny probation.
		if p.probationCap < p.maxProbationCap {
			step := max(int64(1), p.capacity*probationGrowStepPct/100)
			p.setProbationCap(p.probationCap + step)
		}
	case c.ghostHits > c.probationEvictions/4:
		if p.probationCap < p.maxProbationCap {
			p.setProbationCap(p.probationCap + p.adaptStep)
		}
	case c.probationEvictions > c.promotions*2 && c.mainSurvivals > c.promotions:
		if p.probationCap > p.minProbationCap {
			p.setProbationCap(p.probationCap - p.adaptStep)
		}
	}
}

// tuneAdmission self-selects this shard's admission mode from the dual-ghost
// signal, with no configuration. It defaults to recency. When evicted main
// victims keep resurrecting (the B2 ghost) - the cache discarding items it still
// needs, the signature of a cyclic working set larger than capacity - it
// accumulates evidence, then runs a one-cycle frequency trial. The trial commits
// only if it cuts churn cost. A committed shard reverts (with exponential
// backoff) the moment churn climbs back, so a workload that shifts out of its
// loop is never starved. Churn cost (evictions+rejects) tracks the miss rate, so
// it doubles as a maintenance-path proxy for "is this mode helping".
func (p *sieveTinyLFU[K, V]) tuneAdmission() {
	t := &p.tuner
	churn := p.controller.churnCost()
	resurrect := p.controller.resurrectionRate()

	switch t.state {
	case tunerRecency:
		t.baseChurn = ewma(t.baseChurn, churn, admissionChurnEWMA)
		if t.cooldown > 0 {
			t.cooldown--
			return
		}
		// Accumulate resurrection evidence weighted by strength: a pure loop
		// (resurrection ~1.0) reaches the threshold in ~3 cycles; a workload whose
		// victims are abandoned (shifting/bursty, resurrection ~0) never does, so it
		// never trials frequency and never regresses.
		if resurrect >= admissionResurrectHigh {
			t.evidence += resurrect
		} else {
			t.evidence = 0
		}
		if t.evidence >= admissionEntryEvidence {
			t.evidence = 0
			t.mode = admitFrequency
			t.state = tunerTrial
		}

	case tunerTrial:
		if churn < t.baseChurn*admissionCommitFactor {
			t.state = tunerFrequency
			t.lowChurn = churn
			t.backoff = admissionBackoffStart
		} else {
			t.revertToRecency()
		}

	case tunerFrequency:
		// Escape when churn climbs above the pinned low reference (the workload
		// shifted out from under the pinned set) or back toward the recency
		// baseline (frequency stopped helping).
		if churn > t.lowChurn*admissionRevertFactor || churn > t.baseChurn*admissionCommitFactor {
			t.revertToRecency()
			return
		}
		t.lowChurn = ewma(t.lowChurn, churn, admissionChurnEWMA)
	}
}

// ewma folds sample into an exponential moving average, seeding from the first
// nonzero sample so the average is not dragged up from zero.
func ewma(avg, sample, alpha float64) float64 {
	if avg == 0 {
		return sample
	}
	return (1-alpha)*avg + alpha*sample
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
			p.controller.cycleEvictions++
			p.stats.MainEvictions++
			return true
		}
	}
	return false
}

func (s *shard[K, V]) dropSieveItem(it *cacheItem[K, V], pool *sync.Pool, stats bool, reason RemovalReason) bool {
	return s.dropItem(it, pool, stats, reason, dropSieve)
}
