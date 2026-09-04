package kioshun

import (
	"cmp"
	"math/bits"
	"sync/atomic"

	"github.com/unkn0wn-root/kioshun/internal/keyhash"
)

const (
	defaultProbationRatio = 1
	// defaultGhostRatio is the B1 ghost size as a percentage of main capacity.
	defaultGhostRatio = 75
)

const (
	maxItemReuse            = 3
	maxEvictionWork         = 32
	defaultMainVictimScan   = 8
	probationPromotionReuse = 1
)

const (
	// Stationary workloads count one observation per insert in the sketch.
	insertWeightStationary uint8 = 1
	// Shifting workloads count both the miss and the following Set.
	insertWeightShifting uint8 = 2
)

const (
	// Below this B2 hit rate, main victims are considered abandoned.
	probationResurrectLow = 0.10

	// Growth per cycle after detecting a shifting working set.
	probationGrowStepPct = 10
)

// sieveQueueID records which SIEVE queue owns an item. queueNone is the zero
// value, so a new item starts unlinked.
type sieveQueueID uint8

const (
	queueNone sieveQueueID = iota
	probationQueue
	mainQueue
)

// Readers set visitedBit; SIEVE scans consume and clear it.
const visitedBit = uint32(1)

func itemVisited[K comparable, V any](it *cacheItem[K, V]) bool {
	return it != nil && atomic.LoadUint32(&it.visited) != 0
}

func markItemVisited[K comparable, V any](it *cacheItem[K, V]) {
	if it != nil && atomic.LoadUint32(&it.visited) == 0 {
		atomic.StoreUint32(&it.visited, visitedBit)
	}
}

func clearItemVisited[K comparable, V any](it *cacheItem[K, V]) {
	if it != nil {
		atomic.StoreUint32(&it.visited, 0)
	}
}

// sieveQueue is a FIFO built from cacheItem links. The ID and shard owner stored
// on each item identify the exact queue without a pointer back to it.
type sieveQueue[K comparable, V any] struct {
	head  cacheItem[K, V]
	tail  cacheItem[K, V]
	size  int64
	id    sieveQueueID
	owner uint8
}

func (q *sieveQueue[K, V]) init(id sieveQueueID, owner uint8) {
	q.id = id
	q.owner = owner
	q.head.prev = nil
	q.head.next = &q.tail
	q.head.queue = queueNone
	q.tail.prev = &q.head
	q.tail.next = nil
	q.tail.queue = queueNone
	q.size = 0
}

func (q *sieveQueue[K, V]) pushFront(it *cacheItem[K, V]) {
	n := q.head.next
	q.head.next = it
	it.prev = &q.head
	it.next = n
	it.queue = q.id
	it.queueOwner = q.owner
	n.prev = it
	q.size++
}

func (q *sieveQueue[K, V]) ownsTag(it *cacheItem[K, V]) bool {
	return it != nil && it.queue == q.id && it.queueOwner == q.owner
}

func (q *sieveQueue[K, V]) remove(it *cacheItem[K, V]) bool {
	if !q.ownsTag(it) || it.prev == nil || it.next == nil {
		return false
	}
	if it.prev.next != it || it.next.prev != it {
		return false
	}

	it.prev.next = it.next
	it.next.prev = it.prev
	it.prev = nil
	it.next = nil
	it.queue = queueNone
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

func (q *sieveQueue[K, V]) holds(it *cacheItem[K, V]) bool {
	return q.ownsTag(it) && !q.isSentinel(it)
}

// adaptiveController holds counters used to resize probation and tune admission.
// Only the single writer reads or changes them.
type adaptiveController struct {
	ghostHits          uint64
	probationEvictions uint64
	promotions         uint64
	// mainSurvivals counts visited main entries spared by an eviction scan.
	mainSurvivals       uint64
	observationsInCycle uint64

	// The admission tuner treats evictions plus rejections as churn.
	cycleEvictions uint64
	cycleRejects   uint64

	// B2 hits are inserts whose key was recently evicted from main.
	cycleMainEvicts uint64
	cycleB2Hits     uint64
}

func (c *adaptiveController) resetCycle() {
	*c = adaptiveController{}
}

func (c *adaptiveController) churnCost() float64 {
	return float64(c.cycleEvictions + c.cycleRejects)
}

// resurrectionRate is the share of main victims reinserted while still in B2.
// A high rate suggests a repeating working set larger than the cache; a low rate
// suggests that the working set has moved on.
func (c *adaptiveController) resurrectionRate() float64 {
	if c.cycleMainEvicts == 0 {
		return 0
	}
	return float64(c.cycleB2Hits) / float64(c.cycleMainEvicts)
}

// admissionMode controls how a candidate and victim break frequency ties.
type admissionMode uint8

const (
	// admitRecency lets a recently reused candidate win a tie.
	admitRecency admissionMode = iota
	// admitFrequency lets the resident win a tie, as in standard TinyLFU.
	admitFrequency
)

type tunerState uint8

const (
	tunerRecency tunerState = iota
	tunerTrial
	tunerFrequency
)

const (
	admissionEntryEvidence  = 1.0
	admissionResurrectHigh  = 0.5
	admissionCommitFactor   = 0.9
	admissionRevertFactor   = 1.5
	admissionChurnEWMA      = 0.5
	admissionBackoffStart   = 3
	admissionBackoffMax     = 96
	adaptiveCycleMultiplier = 4
	adaptiveMinCycleCap     = 1024
	adaptiveMinCycle        = 8192
)

// admissionTuner chooses between recency and frequency admission. Repeated B2
// hits start a one-cycle frequency trial. The tuner keeps that mode only if it
// reduces churn, and waits longer before retrying after each failed trial.
type admissionTuner struct {
	mode      admissionMode
	state     tunerState
	baseChurn float64
	lowChurn  float64
	evidence  float64
	cooldown  int
	backoff   int
}

func (t *admissionTuner) reset() {
	*t = admissionTuner{backoff: admissionBackoffStart}
}

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
	ghost     ghostQueue // B1: recent probation victims
	mghost    ghostQueue // B2: recent main victims
	sketch    countMinSketch
	door      doorkeeper

	controller adaptiveController
	tuner      admissionTuner
	stats      PolicyStats
	hand       *cacheItem[K, V]

	// insertWeight controls how many frequency samples an insert records.
	insertWeight uint8

	capacity        int64
	probationCap    int64
	mainCap         int64
	ghostCap        int64
	minProbationCap int64
	maxProbationCap int64
	adaptStep       int64
	costAdmission   CostAdmission
	owner           uint8
}

// newSieveTinyLFU builds policy state for a bounded shard. Probation stays between
// 1% and 60% of capacity, leaving room for protected entries in main.
func newSieveTinyLFU[K comparable, V any](c int64, owner uint8, pr, gr uint8, mode CostAdmission) *sieveTinyLFU[K, V] {
	p := &sieveTinyLFU[K, V]{capacity: c, costAdmission: mode, owner: owner}
	p.probation.init(probationQueue, owner)
	p.main.init(mainQueue, owner)

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
	p.insertWeight = insertWeightShifting
	p.tuner.reset()
	samples := uint64(max(c*10, int64(sketchMinCounters)))
	p.ghost = newGhostQueue(int(gc))
	// One shard of B2 history can detect a repeating set larger than capacity.
	p.mghost = newGhostQueue(int(c))
	p.sketch = newCountMinSketch(samples)
	p.door = newDoorkeeper(samples)
	return p
}

// recordAccess counts an insert as two observations: the unsampled miss and its
// Set. insertWeight controls whether both also update the frequency estimate.
func (p *sieveTinyLFU[K, V]) recordAccess(h uint64) {
	p.incrementFrequency(h)
	if p.insertWeight > 1 {
		p.incrementFrequency(h)
		return
	}
	p.tickObservation()
}

// incrementFrequency records an access in the doorkeeper and sketch.
func (p *sieveTinyLFU[K, V]) incrementFrequency(h uint64) {
	av := keyhash.Avalanche(h)
	if p.door.add(av) {
		p.sketch.add(av)
	}
	p.tickObservation()
}

// tickObservation advances aging and tuning by one observed access, including
// accesses filtered out by the doorkeeper.
func (p *sieveTinyLFU[K, V]) tickObservation() {
	p.sketch.samples++
	if p.sketch.resetAt > 0 && p.sketch.samples >= p.sketch.resetAt {
		p.sketch.age()
		p.door.clear()
	}
	p.tick()
}

// estimate counts a doorkeeper hit as one recent access.
func (p *sieveTinyLFU[K, V]) estimate(h uint64) uint8 {
	av := keyhash.Avalanche(h)
	e := p.sketch.estimate(av)
	if p.door.contains(av) && e < sketchMaxCounter {
		e++
	}
	return e
}

func (p *sieveTinyLFU[K, V]) owns(it *cacheItem[K, V]) bool {
	return p.probation.holds(it) || p.main.holds(it)
}

func (p *sieveTinyLFU[K, V]) recordReadHit(it *cacheItem[K, V]) {
	// Queue ownership is writer-only, so lock-free reads set only this atomic bit.
	markItemVisited(it)
}

// recordUpdate treats a Set on an existing item as reuse.
func (p *sieveTinyLFU[K, V]) recordUpdate(it *cacheItem[K, V]) {
	switch it.queue {
	case mainQueue:
		markItemVisited(it)
		if it.reuse < maxItemReuse {
			it.reuse++
		}
	case probationQueue:
		wasVisited := itemVisited(it)
		if it.reuse < maxItemReuse {
			it.reuse++
		}
		if (wasVisited || it.reuse >= probationPromotionReuse) && p.mainCap > 0 {
			p.promote(it)
		} else {
			markItemVisited(it)
		}
	}
}

// insert sends new items to probation and B1 hits directly to main.
func (p *sieveTinyLFU[K, V]) insert(it *cacheItem[K, V], gh bool) {
	if p.mghost.contains(it.hash) {
		p.mghost.remove(it.hash)
		p.controller.cycleB2Hits++
	}

	if gh && p.mainCap > 0 {
		p.ghost.remove(it.hash)
		p.controller.ghostHits++
		p.stats.GhostHits++
		p.insertMain(it)
		return
	}

	it.queue = probationQueue
	it.reuse = 0
	clearItemVisited(it)
	p.probation.pushFront(it)
}

func (p *sieveTinyLFU[K, V]) insertMain(it *cacheItem[K, V]) {
	it.queue = mainQueue
	it.reuse = 1
	markItemVisited(it)
	p.main.pushFront(it)
	if p.hand == nil {
		p.hand = it
	}
}

// remove unlinks an item and moves the hand if it pointed to that item.
func (p *sieveTinyLFU[K, V]) remove(it *cacheItem[K, V]) bool {
	if it == nil {
		return false
	}

	removed := false
	switch it.queue {
	case mainQueue:
		if p.hand == it {
			p.hand = p.previousMainItem(it)
		}
		removed = p.main.remove(it)
	case probationQueue:
		removed = p.probation.remove(it)
	default:
		if p.hand == it {
			p.hand = nil
		}
	}
	if !removed {
		return false
	}

	it.reuse = 0
	clearItemVisited(it)
	return true
}

func (p *sieveTinyLFU[K, V]) reset() {
	p.probation.init(probationQueue, p.owner)
	p.main.init(mainQueue, p.owner)
	p.ghost.clear()
	p.mghost.clear()
	p.sketch.clear()
	p.door.clear()
	p.controller.resetCycle()
	p.insertWeight = insertWeightShifting
	p.tuner.reset()
	p.stats = PolicyStats{}
	p.hand = nil
}

func (p *sieveTinyLFU[K, V]) promote(it *cacheItem[K, V]) {
	if it == nil || it.queue != probationQueue || p.mainCap <= 0 {
		return
	}

	p.probation.remove(it)
	p.insertMain(it)
	p.controller.promotions++
	p.stats.Promotions++
}

// replaceNode puts an immutable replacement in the old item's queue position and
// preserves its reuse state.
func (p *sieveTinyLFU[K, V]) replaceNode(old, new *cacheItem[K, V]) {
	new.queue = old.queue
	new.queueOwner = old.queueOwner
	new.reuse = old.reuse
	new.prev = old.prev
	new.next = old.next
	if old.prev != nil {
		old.prev.next = new
	}
	if old.next != nil {
		old.next.prev = new
	}
	if itemVisited(old) {
		markItemVisited(new)
	}
	if p.hand == old {
		p.hand = new
	}
	old.prev = nil
	old.next = nil
	old.queue = queueNone
}

// dropProbationVictim records a probation eviction in B1.
func (s *shard[K, V]) dropProbationVictim(it *cacheItem[K, V], stats bool) bool {
	p := s.sieve
	h := it.hash
	if !s.dropSieveItem(it, stats, RemovedCapacity) {
		return false
	}
	p.controller.probationEvictions++
	p.controller.cycleEvictions++
	p.stats.ProbationEvictions++
	p.ghost.add(h)
	return true
}

// evictProbation promotes a reused tail item or evicts a cold one.
func (s *shard[K, V]) evictProbation(stats bool) *cacheItem[K, V] {
	p := s.sieve
	if p.probation.empty() {
		return nil
	}

	it := p.probation.tail.prev
	if !p.probation.holds(it) {
		return nil
	}
	if it.reuse >= probationPromotionReuse || itemVisited(it) {
		p.promote(it)
		return it
	}

	s.dropProbationVictim(it, stats)
	return nil
}

// evictMain scans for a victim and compares it with the incoming candidate.
func (s *shard[K, V]) evictMain(
	stats bool,
	in *cacheItem[K, V],
	tie bool,
	scan int64,
	force bool,
) bool {
	p := s.sieve
	if in != nil && !p.owns(in) {
		in = nil
		tie = false
	}

	v := p.findMainVictim(scan, force)
	if v == nil {
		if force && in != nil {
			p.controller.cycleRejects++
			s.dropSieveItem(in, stats, RemovedRejected)
			return true
		}
		return false
	}

	if in != nil && in != v && !p.shouldAdmit(in, v, tie) {
		p.controller.cycleRejects++
		return s.dropSieveItem(in, stats, RemovedRejected)
	}

	vh := v.hash
	if s.dropSieveItem(v, stats, RemovedCapacity) {
		p.controller.cycleEvictions++
		p.controller.cycleMainEvicts++
		p.mghost.add(vh)
		p.stats.MainEvictions++
		return true
	}
	return false
}

// enforceSieveCapacity first follows normal policy decisions, then forces a
// victim if the bounded scan could not restore the shard limit.
func (s *shard[K, V]) enforceSieveCapacity(
	stats bool,
	in *cacheItem[K, V],
	tie bool,
) {
	p := s.sieve
	admitFromProbation := func() {
		if it := s.evictProbation(stats); it != nil {
			in, tie = it, true
		}
	}

	work := maxEvictionWork
	for work > 0 && s.overCapacity() {
		switch {
		case p.probation.size > p.probationCap && !p.probation.empty():
			admitFromProbation()
		case (p.main.size > p.mainCap || s.overCapacity()) && !p.main.empty():
			evictIn, evictTie := in, tie

			inProbation := in != nil &&
				in.queue == probationQueue &&
				p.probation.size <= p.probationCap

			// Treat a pending frequency trial like frequency mode to avoid changing
			// policy at the cycle boundary.
			loopish := p.tuner.mode == admitFrequency || p.tuner.evidence > 0

			shouldKeep := inProbation &&
				p.costAdmission == CostAdmissionFrequency &&
				s.costCap == 0 &&
				!loopish &&
				(p.controller.cycleMainEvicts == 0 ||
					p.controller.resurrectionRate() < probationResurrectLow)

			if shouldKeep {
				// Give an underfilled probation window one slot from main. Weighted
				// items skip this because one candidate could displace several victims.
				evictIn, evictTie = nil, false
			}
			if s.evictMain(stats, evictIn, evictTie, defaultMainVictimScan, false) {
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

	if (p.probation.size > p.probationCap || s.overCapacity()) && !p.probation.empty() {
		admitFromProbation()
	}
	if (p.main.size > p.mainCap || s.overCapacity()) && !p.main.empty() {
		s.evictMain(stats, in, tie, defaultMainVictimScan, true)
	}
	for s.overCapacity() && s.tab.length() > 0 {
		if !s.forceEvictSieveItem(stats) {
			return
		}
	}
}

func (s *shard[K, V]) overCapacity() bool {
	if s.cap > 0 && atomic.LoadInt64(&s.size) > s.cap {
		return true
	}
	return s.costCap > 0 && atomic.LoadInt64(&s.cost) > s.costCap
}

func (s *shard[K, V]) wouldOverCapacity(addCost int64) bool {
	if s.cap > 0 && atomic.LoadInt64(&s.size) >= s.cap {
		return true
	}
	return s.costCap > 0 && atomic.LoadInt64(&s.cost)+addCost > s.costCap
}

// mainCandidate returns a live cursor, falling back to the main tail.
func (p *sieveTinyLFU[K, V]) mainCandidate(it *cacheItem[K, V]) (*cacheItem[K, V], bool) {
	if !p.main.holds(it) {
		it = p.main.tail.prev
	}
	if p.main.isSentinel(it) {
		return nil, false
	}
	return it, true
}

// findMainVictim gives visited entries a second chance. A forced scan returns the
// next live item after the scan budget is exhausted.
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
		if itemVisited(it) {
			clearItemVisited(it)
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
		cit, ok := p.mainCandidate(it)
		if !ok {
			return nil
		}
		p.hand = p.previousMainItem(cit)
		return cit
	}

	p.hand = it
	return nil
}

// previousMainItem moves toward older entries and wraps at the head.
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

// shouldAdmit compares a candidate with its victim. Recency mode can favor a
// recently reused candidate; frequency mode lets the resident win ties.
func (p *sieveTinyLFU[K, V]) shouldAdmit(in, v *cacheItem[K, V], tie bool) bool {
	if p.tuner.mode == admitFrequency {
		return p.compareAdmissionScore(in, v) > 0
	}

	if tie && !itemVisited(v) {
		return true
	}

	switch c := p.compareAdmissionScore(in, v); {
	case c > 0:
		return true
	case c < 0:
		return tie && p.closeAdmissionScore(in, v)
	default:
		return tie || (!itemVisited(v) && v.reuse == 0)
	}
}

// compareAdmissionScore compares access counts directly or cross-multiplies
// weighted scores to avoid division.
func (p *sieveTinyLFU[K, V]) compareAdmissionScore(in, v *cacheItem[K, V]) int {
	cf := uint64(p.estimate(in.hash))
	vf := uint64(p.estimate(v.hash))
	if p.costAdmission == CostAdmissionFrequency {
		return cmp.Compare(cf, vf)
	}
	return cmp.Compare(mulScore(cf, p.costDenom(v)), mulScore(vf, p.costDenom(in)))
}

// closeAdmissionScore reports whether reuse puts the candidate within one access
// of the victim.
func (p *sieveTinyLFU[K, V]) closeAdmissionScore(in, v *cacheItem[K, V]) bool {
	cf := uint64(p.estimate(in.hash))
	vf := uint64(p.estimate(v.hash))
	if p.costAdmission == CostAdmissionFrequency {
		return cf+1 >= vf
	}
	return mulScore(cf+1, p.costDenom(v)) >= mulScore(vf, p.costDenom(in))
}

// costDenom returns cost for Density and its square root for Balanced.
func (p *sieveTinyLFU[K, V]) costDenom(it *cacheItem[K, V]) uint64 {
	if p.costAdmission == CostAdmissionBalanced {
		return isqrt64(scoreCost(it.cost))
	}
	return scoreCost(it.cost)
}

// scoreCost keeps the weighted comparison denominator positive.
func scoreCost(cost int64) uint64 {
	if cost <= 1 {
		return 1
	}
	return uint64(cost)
}

// mulScore clamps overflow instead of wrapping to a smaller score.
func mulScore(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	if hi != 0 {
		return ^uint64(0)
	}
	return lo
}

func isqrt64(n uint64) uint64 {
	if n <= 1 {
		return 1
	}
	x := n
	y := (x + 1) >> 1
	for y < x {
		x = y
		y = (x + n/x) >> 1
	}
	return x
}

// tick runs both tuners after a full observation window, keeping short bursts from
// changing the policy immediately.
func (p *sieveTinyLFU[K, V]) tick() {
	p.controller.observationsInCycle++
	win := uint64(p.capacity * adaptiveCycleMultiplier)
	if p.capacity >= adaptiveMinCycleCap {
		win = max(win, uint64(adaptiveMinCycle))
	}
	if win == 0 || p.controller.observationsInCycle < win {
		return
	}

	p.tuneAdmission()
	p.adaptSize()
	p.controller.resetCycle()
}

// adaptSize moves capacity between probation and main.
//
// B1 hits alone cannot distinguish a repeating set from a changing one. B2 does:
// a high B2 hit rate means recently evicted main items are still needed, while a
// low rate means the old set was abandoned. A changing set grows probation so new
// items live long enough to be reused. A stable main with little useful probation
// traffic shrinks probation instead.
//
// The same choice controls whether an insert records both its miss and Set in the
// frequency estimate. An inconclusive cycle keeps the previous setting.
func (p *sieveTinyLFU[K, V]) adaptSize() {
	c := &p.controller
	resurrect := c.resurrectionRate()
	loopish := p.tuner.mode == admitFrequency || p.tuner.evidence > 0
	shifting := !loopish && resurrect < probationResurrectLow
	fastGrowth := shifting &&
		c.cycleMainEvicts > c.promotions &&
		c.ghostHits > c.promotions
	growth := shifting &&
		c.ghostHits > c.probationEvictions/4
	shrink := c.probationEvictions > c.promotions*2 &&
		c.mainSurvivals > c.promotions

	if loopish {
		p.insertWeight = insertWeightStationary
	}

	switch {
	case fastGrowth:
		// Grow quickly when the working set is moving.
		p.insertWeight = insertWeightShifting
		if p.probationCap < p.maxProbationCap {
			step := max(int64(1), p.capacity*probationGrowStepPct/100)
			p.setProbationCap(p.probationCap + step)
		}
	case growth:
		p.insertWeight = insertWeightShifting
		if p.probationCap < p.maxProbationCap {
			p.setProbationCap(p.probationCap + p.adaptStep)
		}
	case shrink:
		// Give more space to a main queue that is retaining reused items.
		p.insertWeight = insertWeightStationary
		if p.probationCap > p.minProbationCap {
			p.setProbationCap(p.probationCap - p.adaptStep)
		}
	}
}

// tuneAdmission starts a frequency trial when B2 shows that main victims keep
// returning. It keeps frequency mode only while evictions plus rejections stay
// below the recency baseline. Failed trials increase the wait before another try.
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
		// Stronger B2 evidence reaches a trial sooner.
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
		// Revert when frequency mode loses its advantage.
		if churn > t.lowChurn*admissionRevertFactor || churn > t.baseChurn*admissionCommitFactor {
			t.revertToRecency()
			return
		}
		t.lowChurn = ewma(t.lowChurn, churn, admissionChurnEWMA)
	}
}

// ewma seeds an exponential moving average from its first sample.
func ewma(avg, sample, alpha float64) float64 {
	if avg == 0 {
		return sample
	}
	return (1-alpha)*avg + alpha*sample
}

// setProbationCap gives the remaining fixed capacity to main.
func (p *sieveTinyLFU[K, V]) setProbationCap(n int64) {
	n = min(max(n, p.minProbationCap), p.maxProbationCap)
	p.probationCap = n
	p.mainCap = p.capacity - n
}

// forceEvictSieveItem removes from probation first, then main.
func (s *shard[K, V]) forceEvictSieveItem(stats bool) bool {
	p := s.sieve
	if !p.probation.empty() {
		it := p.probation.tail.prev
		if !p.probation.holds(it) {
			return false
		}
		return s.dropProbationVictim(it, stats)
	}
	if !p.main.empty() {
		it := p.findMainVictim(1, true)
		if it == nil {
			return false
		}
		if s.dropSieveItem(it, stats, RemovedCapacity) {
			p.controller.cycleEvictions++
			p.stats.MainEvictions++
			return true
		}
	}
	return false
}

func (s *shard[K, V]) dropSieveItem(it *cacheItem[K, V], stats bool, reason RemovalReason) bool {
	return s.dropItem(it, stats, reason, dropSieve)
}
