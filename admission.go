package cache

import (
	"math/bits"
	"sync/atomic"
	"time"
)

const (
	// Bloom/doorkeeper hash mixing (xxHash64-style avalanche). These constants are
	// used to decorrelate low-entropy inputs before masking into a power-of-two bitset.
	bloomMixPrime1 = 0xff51afd7ed558ccd
	bloomMixPrime2 = 0xc4ceb9fe1a85ec53

	// Admission thresholds (policy-level, not hard caps).
	// frequencyThreshold: if the estimated CMS frequency of a candidate reaches this value,
	// it is admitted unconditionally (fast-path).
	// baseAdmissionRate is informational here; the live value is seeded in newAdaptiveAdmissionFilter().
	baseAdmissionRate  = 70 // %
	frequencyThreshold = 3  // admit if estimated freq ≥ 3

	// Eviction-pressure feedback: we adapt the base admission probability up/down once
	// per interval, based on the recent eviction count. These are deliberately coarse
	// to keep SET hot paths branch-/atomic-light.
	highEvictionRateThreshold = 100 // if evictions/sec > this → lower admission prob
	lowEvictionRateThreshold  = 10  // if evictions/sec < this → raise admission prob
	admissionProbabilityStep  = 5   // +/- step size in percentage points

	// Counter packing for the frequency sketch:
	// - 4-bit counters (0..15) packed 16-per-uint64.
	// - "aging" halves all counters to decay old history.
	bitsPerWord   = 64
	frequencyMask = 0x0F
	maxFrequency  = 15
	agingFactor   = 2

	// Per-hash rotations to derive multiple hash functions from a single 64-bit key hash.
	hash1Rotate = 0
	hash2Rotate = 17
	hash3Rotate = 31
	hash4Rotate = 47

	// Scan detection knobs. When detectScan() returns true we switch to a more
	// recency-biased policy and clamp probabilistic admission.
	scanAdmissionThreshold = 100 // admissions/sec considered a scan if counted
	scanMissThreshold      = 50  // consecutive GET misses considered a scan

	// Time thresholds (nanoseconds).
	// During scan mode we prefer to keep victims that were touched recently.
	// For tie-breaks in normal mode, very old victims lose to fresh candidates.
	scanModeRecencyThreshold = 100e6 // 100ms
	recencyTieBreakThreshold = 1e9   // 1s

	// Feedback/adaptation cadence.
	evictionPressureInterval = 1e9 // 1s
)

// hashN rotates+mixes a 64-bit input, then masks into a power-of-two bit-space.
// Assumes mask = size-1 (size is a power-of-two), so `h & mask` == `h % size`.
func hashN(hash, mask uint64, rotate int) uint64 {
	h := bits.RotateLeft64(hash, int(rotate))
	h ^= h >> 33
	h *= bloomMixPrime1
	h ^= h >> 29
	h *= bloomMixPrime2
	h ^= h >> 32
	return h & mask
}

// Doorkeeper: a tiny 3-hash Bloom filter
//
// bloomFilter is a fixed-size bitset addressed by three independent hashes.
// Concurrency: this structure is mutated under the shard's write lock; no atomics.
type bloomFilter struct {
	bits []uint64 // 16× 4B bytes per word; each bit is an event.
	size uint64   // logical number of bits (power of two)
	mask uint64   // size-1 for fast modulo
}

// newBloomFilter allocates a bitset rounded up to next power-of-two bits.
// The underlying slice length is size/64 (at least 1).
func newBloomFilter(size uint64) *bloomFilter {
	size = uint64(nextPowerOf2(int(size)))
	arraySize := size / bitsPerWord
	if arraySize == 0 {
		arraySize = 1
	}
	return &bloomFilter{
		bits: make([]uint64, arraySize),
		size: size,
		mask: size - 1,
	}
}

// add sets three hashed bit-positions. Idempotent.
func (bf *bloomFilter) add(keyHash uint64) {
	h1 := hashN(keyHash, bf.mask, hash1Rotate)
	h2 := hashN(keyHash, bf.mask, hash2Rotate)
	h3 := hashN(keyHash, bf.mask, hash3Rotate)

	bf.bits[h1/bitsPerWord] |= 1 << (h1 % bitsPerWord)
	bf.bits[h2/bitsPerWord] |= 1 << (h2 % bitsPerWord)
	bf.bits[h3/bitsPerWord] |= 1 << (h3 % bitsPerWord)
}

// contains returns true if all three hashed bits are set (standard Bloom semantics).
func (bf *bloomFilter) contains(keyHash uint64) bool {
	h1 := hashN(keyHash, bf.mask, hash1Rotate)
	h2 := hashN(keyHash, bf.mask, hash2Rotate)
	h3 := hashN(keyHash, bf.mask, hash3Rotate)

	bit1 := bf.bits[h1/bitsPerWord] & (1 << (h1 % bitsPerWord))
	bit2 := bf.bits[h2/bitsPerWord] & (1 << (h2 % bitsPerWord))
	bit3 := bf.bits[h3/bitsPerWord] & (1 << (h3 % bitsPerWord))

	return bit1 != 0 && bit2 != 0 && bit3 != 0
}

// reset zeroes the bitset. Called under shard lock (so no atomics needed).
func (bf *bloomFilter) reset() {
	clear(bf.bits)
}

//	Frequency sketch: 4-bit Count–Min Sketch with periodic aging
//	Purpose: approximate frequency estimates without per-key state.
//
// frequencyBloomFilter is a 4-hash Count–Min Sketch with 4-bit counters, packed
// 16 per uint64 word. Each increment CASes a single word; aging is a halving pass.
// Concurrency: counter updates are lock-free per-word via CAS; safe for parallel use.
type frequencyBloomFilter struct {
	counters []uint64 // len = size/16 words; each word packs 16 nybble counters
	size     uint64   // number of individual 4-bit counters (power of two)
	mask     uint64   // size-1 for fast modulo

	totalIncrements uint64 // global increment count (for aging cadence)
	agingThreshold  uint64 // trigger an aging pass after ~size*10 increments
}

// newFrequencyBloomFilter allocates counters (size rounded to next power-of-two).
// Memory: size/16 uint64 words (at least 1 word).
func newFrequencyBloomFilter(numCounters uint64) *frequencyBloomFilter {
	size := uint64(nextPowerOf2(int(numCounters)))
	arraySize := size / 16
	if arraySize == 0 {
		arraySize = 1
	}
	return &frequencyBloomFilter{
		counters:       make([]uint64, arraySize),
		size:           size,
		mask:           size - 1,
		agingThreshold: size * 10, // ~10 events per counter between halvings
	}
}

// increment bumps the four counters addressed by the four hashes (unless saturated),
// and returns the *new* minimum of those four counters (standard CMS estimate).
// Concurrency: per-increment cost is 4 CAS loops worst case (one per word).
func (fbf *frequencyBloomFilter) increment(keyHash uint64) uint64 {
	h1 := hashN(keyHash, fbf.mask, hash1Rotate)
	h2 := hashN(keyHash, fbf.mask, hash2Rotate)
	h3 := hashN(keyHash, fbf.mask, hash3Rotate)
	h4 := hashN(keyHash, fbf.mask, hash4Rotate)

	// Query pass: min of four nybble counters.
	freq := fbf.getCounterValue(h1)
	if f2 := fbf.getCounterValue(h2); f2 < freq {
		freq = f2
	}
	if f3 := fbf.getCounterValue(h3); f3 < freq {
		freq = f3
	}
	if f4 := fbf.getCounterValue(h4); f4 < freq {
		freq = f4
	}

	// Update pass: increment all four, if not saturated.
	if freq < maxFrequency {
		fbf.incrementCounter(h1)
		fbf.incrementCounter(h2)
		fbf.incrementCounter(h3)
		fbf.incrementCounter(h4)
		freq++
	}

	// Light-touch aging trigger (coarse-grained to avoid per-op overhead).
	if atomic.AddUint64(&fbf.totalIncrements, 1) >= fbf.agingThreshold {
		fbf.age()
	}

	return freq
}

// estimateFrequency returns the current CMS estimate (min of four counters).
// This is a read-only query (no updates).
func (fbf *frequencyBloomFilter) estimateFrequency(keyHash uint64) uint64 {
	h1 := hashN(keyHash, fbf.mask, hash1Rotate)
	h2 := hashN(keyHash, fbf.mask, hash2Rotate)
	h3 := hashN(keyHash, fbf.mask, hash3Rotate)
	h4 := hashN(keyHash, fbf.mask, hash4Rotate)

	freq := fbf.getCounterValue(h1)
	if f2 := fbf.getCounterValue(h2); f2 < freq {
		freq = f2
	}
	if f3 := fbf.getCounterValue(h3); f3 < freq {
		freq = f3
	}
	if f4 := fbf.getCounterValue(h4); f4 < freq {
		freq = f4
	}

	return freq
}

// getCounterValue extracts a nybble (4-bit) counter at logical index.
func (fbf *frequencyBloomFilter) getCounterValue(index uint64) uint64 {
	arrayIndex := index / 16
	counterIndex := index % 16
	shift := counterIndex * 4
	return (fbf.counters[arrayIndex] >> shift) & frequencyMask
}

// incrementCounter performs a CAS loop to bump a single nybble within a packed word.
// If the nybble is saturated (15), it is left unchanged.
func (fbf *frequencyBloomFilter) incrementCounter(index uint64) {
	arrayIndex := index / 16
	counterIndex := index % 16
	shift := counterIndex * 4

	for {
		current := atomic.LoadUint64(&fbf.counters[arrayIndex])
		val := (current >> shift) & frequencyMask
		if val >= maxFrequency {
			return
		}
		newVal := current + (1 << shift)
		if atomic.CompareAndSwapUint64(&fbf.counters[arrayIndex], current, newVal) {
			return
		}
	}
}

// age halves every nybble counter in-place via per-word CAS.
// Vectorized form: shift the whole word right by 1 and mask off each nybble's MSB
// so bits don't "spill" across nybble boundaries.
func (fbf *frequencyBloomFilter) age() {
	const halfMask uint64 = 0x7777777777777777 // 0b0111 repeated per nybble
	for i := range fbf.counters {
		for {
			current := atomic.LoadUint64(&fbf.counters[i])
			aged := (current >> 1) & halfMask
			if atomic.CompareAndSwapUint64(&fbf.counters[i], current, aged) {
				break
			}
		}
	}
	atomic.StoreUint64(&fbf.totalIncrements, 0)
}

//  Workload detector: cheap signals for scan/pollution detection
//

// workloadDetector tracks two signals:
//  1. admissionRate (admissions/sec) — if maintained; see note in recordAdmission()
//  2. consecutiveMisses                — consecutive GET misses
//
// If either exceeds its threshold within a 1s window, detectScan() returns true.
//
// Concurrency: all fields are atomically updated and can be called under shard lock.
type workloadDetector struct {
	recentAdmissions    uint64 // count of admissions in current window
	recentAdmissionTime int64  // window start (ns)
	admissionRate       uint64 // computed admissions/sec (updated once per ~1s)
	consecutiveMisses   uint64 // monotonic miss streak until reset
}

// newWorkloadDetector seeds the time window.
func newWorkloadDetector() *workloadDetector {
	return &workloadDetector{
		recentAdmissionTime: time.Now().UnixNano(),
	}
}

// detectScan recomputes admissionRate once per second and checks thresholds.
// Note: admissionRate depends on recentAdmissions being incremented on admission.
// If not incremented, the admissionRate branch will never trigger; the miss-streak
// path is still fully effective.
func (wd *workloadDetector) detectScan(keyHash uint64) bool {
	now := time.Now().UnixNano()

	elapsed := now - atomic.LoadInt64(&wd.recentAdmissionTime)
	if elapsed > 1e9 { // 1s window
		admissions := atomic.LoadUint64(&wd.recentAdmissions)
		if elapsed > 0 {
			rate := admissions * 1e9 / uint64(elapsed)
			atomic.StoreUint64(&wd.admissionRate, rate)
		}
		atomic.StoreUint64(&wd.recentAdmissions, 0)
		atomic.StoreInt64(&wd.recentAdmissionTime, now)
	}

	return atomic.LoadUint64(&wd.admissionRate) > scanAdmissionThreshold ||
		atomic.LoadUint64(&wd.consecutiveMisses) > scanMissThreshold
}

// recordAdmission resets the miss streak on a successful admission.
func (wd *workloadDetector) recordAdmission() {
	atomic.AddUint64(&wd.recentAdmissions, 1)
	atomic.StoreUint64(&wd.consecutiveMisses, 0)
}

// recordRejection bumps the miss streak. On long cold streams this will
// cause detectScan() to return true and switch the policy to scan mode.
func (wd *workloadDetector) recordRejection() {
	atomic.AddUint64(&wd.consecutiveMisses, 1)
}

//  Adaptive admission filter
//  - Doorkeeper (Bloom) short-circuits "already seen" keys.
//  - Frequency sketch estimates popularity.
//  - Scan mode dials admission toward recency.
//  - Eviction-pressure feedback nudges global probability up/down.
//

// adaptiveAdmissionFilter is per-shard and expected to run under the shard write lock
// during admission/eviction decisions. The sketch uses atomics so it remains correct
// even if called without external synchronization (e.g., future reuse).
type adaptiveAdmissionFilter struct {
	frequencyFilter *frequencyBloomFilter
	doorkeeper      *bloomFilter
	detector        *workloadDetector

	// Probability of admitting a low-frequency key (0..100).
	admissionProbability uint32
	minProbability       uint32
	maxProbability       uint32

	// Eviction-pressure feedback (coarse, 1s cadence).
	recentEvictions uint64
	evictionWindow  int64

	// Periodic doorkeeper reset to clear stale bits.
	resetInterval int64
	lastReset     int64

	// Stats (all atomic).
	admissionRequests uint64
	admissionGrants   uint64
	scanRejections    uint64
}

// newAdaptiveAdmissionFilter wires all subcomponents. The base admission
// probability is seeded to 70% and limited to [5%, 95%].
func newAdaptiveAdmissionFilter(numCounters uint64, resetInterval time.Duration) *adaptiveAdmissionFilter {
	return &adaptiveAdmissionFilter{
		frequencyFilter:      newFrequencyBloomFilter(numCounters),
		doorkeeper:           newBloomFilter(numCounters / 8),
		detector:             newWorkloadDetector(),
		admissionProbability: 70,
		minProbability:       5,
		maxProbability:       95,
		resetInterval:        int64(resetInterval),
		lastReset:            time.Now().UnixNano(),
		evictionWindow:       time.Now().UnixNano(),
	}
}

// shouldAdmit is called from the eviction path when a shard is at capacity.
// Inputs:
//
//	keyHash          — 64-bit hash of the candidate
//	victimFrequency  — estimated frequency of the sampled victim
//	victimAge        — last-access time (ns) of the victim (0 if unavailable)
//
// Decision order:
//  1. Doorkeeper fast-path: if seen recently → admit (and refresh sketch).
//  2. Scan detection: if scan → admit by recency/min-probability.
//  3. Otherwise use frequency comparison, tie-break by recency,
//     else probabilistic admission adjusted by victim frequency.
//
// Also updates feedback (eviction pressure) and performs periodic doorkeeper resets.
func (aaf *adaptiveAdmissionFilter) shouldAdmit(keyHash uint64, victimFrequency uint64, victimAge int64) bool {
	atomic.AddUint64(&aaf.admissionRequests, 1)

	// Fast-path: "already seen" → admit and refresh sketch.
	if aaf.doorkeeper.contains(keyHash) {
		aaf.doorkeeper.add(keyHash) // refresh
		_ = aaf.frequencyFilter.increment(keyHash)
		aaf.detector.recordAdmission()
		atomic.AddUint64(&aaf.admissionGrants, 1)
		return true
	}

	// Scan-mode check (cheap): switches policy toward recency if true.
	isScanning := aaf.detector.detectScan(keyHash)
	if isScanning {
		atomic.AddUint64(&aaf.scanRejections, 1)
		admit := aaf.admitDuringScan(keyHash, victimAge)
		if admit {
			aaf.detector.recordAdmission()
			atomic.AddUint64(&aaf.admissionGrants, 1)
		} else {
			aaf.detector.recordRejection()
		}
		return admit
	}

	// Normal mode: update sketch and doorkeeper, then run core decision logic.
	newFreq := aaf.frequencyFilter.increment(keyHash)
	aaf.doorkeeper.add(keyHash)

	admit := aaf.makeAdmissionDecision(keyHash, newFreq, victimFrequency, victimAge)

	// Background adaptations are intentionally coarse to keep the hot path light.
	aaf.adjustAdmissionProbability()
	aaf.checkPeriodicReset()

	if admit {
		aaf.detector.recordAdmission()
		atomic.AddUint64(&aaf.admissionGrants, 1)
	} else {
		aaf.detector.recordRejection()
	}
	return admit
}

// admitDuringScan prefers recency over frequency. If the victim hasn't been
// touched recently (older than scanModeRecencyThreshold), we admit. Otherwise
// fall back to a small min-probability to allow some churn.
func (aaf *adaptiveAdmissionFilter) admitDuringScan(keyHash uint64, victimAge int64) bool {
	now := time.Now().UnixNano()
	if victimAge > 0 && now-victimAge > scanModeRecencyThreshold {
		return true
	}
	return (keyHash % 100) < uint64(aaf.minProbability)
}

// makeAdmissionDecision implements the non-scan policy:
//
//   - If candidate freq ≥ threshold → admit.
//   - Else if candidate freq > victim freq → admit.
//   - Else if equal and >0 → break tie by victim recency (older loses),
//     or with a 50/50 coin if recency is unknown.
//   - Else → probabilistic admission: base probability minus a capped penalty
//     derived from victim frequency (older/hotter victims reduce the chance).
//
// The penalty cap prevents "immortal" hot items from blocking churn forever.
func (aaf *adaptiveAdmissionFilter) makeAdmissionDecision(keyHash, newFreq, victimFrequency uint64, victimAge int64) bool {
	if newFreq >= frequencyThreshold {
		return true
	}
	if newFreq > victimFrequency {
		return true
	}
	if newFreq == victimFrequency && newFreq > 0 {
		if victimAge > 0 {
			now := time.Now().UnixNano()
			victimRecency := now - victimAge
			return victimRecency > recencyTieBreakThreshold
		}
		return (keyHash % 2) == 0
	}

	prob := atomic.LoadUint32(&aaf.admissionProbability)

	// Cap penalty at 50 percentage points (vf > 5 is clamped).
	vf := victimFrequency
	if vf > 5 {
		vf = 5
	}
	adjustedProb := int64(prob) - int64(vf*10)
	if adjustedProb < int64(aaf.minProbability) {
		adjustedProb = int64(aaf.minProbability)
	}
	return (keyHash % 100) < uint64(adjustedProb)
}

// adjustAdmissionProbability applies a once-per-interval nudge to the global
// admission probability based on recent evictions. High pressure reduces the
// chance of admitting low-frequency items; low pressure increases it.
func (aaf *adaptiveAdmissionFilter) adjustAdmissionProbability() {
	now := time.Now().UnixNano()
	window := atomic.LoadInt64(&aaf.evictionWindow)

	if now-window > evictionPressureInterval {
		evictions := atomic.LoadUint64(&aaf.recentEvictions)
		currentProb := atomic.LoadUint32(&aaf.admissionProbability)

		if evictions > highEvictionRateThreshold {
			newProb := currentProb
			if newProb > admissionProbabilityStep {
				newProb -= admissionProbabilityStep
			}
			if newProb < aaf.minProbability {
				newProb = aaf.minProbability
			}
			atomic.StoreUint32(&aaf.admissionProbability, newProb)
		} else if evictions < lowEvictionRateThreshold {
			newProb := currentProb + admissionProbabilityStep
			if newProb > aaf.maxProbability {
				newProb = aaf.maxProbability
			}
			atomic.StoreUint32(&aaf.admissionProbability, newProb)
		}

		atomic.StoreUint64(&aaf.recentEvictions, 0)
		atomic.StoreInt64(&aaf.evictionWindow, now)
	}
}

// checkPeriodicReset clears the doorkeeper every resetInterval to avoid
// saturating its bitset with long-term history.
func (aaf *adaptiveAdmissionFilter) checkPeriodicReset() {
	now := time.Now().UnixNano()
	if now-aaf.lastReset > aaf.resetInterval {
		aaf.doorkeeper.reset()
		aaf.lastReset = now
	}
}

// RecordEviction feeds back pressure to the adaptive probability logic.
// Call this after an eviction is performed.
func (aaf *adaptiveAdmissionFilter) RecordEviction() {
	atomic.AddUint64(&aaf.recentEvictions, 1)
}

// GetFrequencyEstimate exposes the current CMS estimate for a given key hash.
// Useful for debugging/telemetry; not used on the hot path.
func (aaf *adaptiveAdmissionFilter) GetFrequencyEstimate(keyHash uint64) uint64 {
	return aaf.frequencyFilter.estimateFrequency(keyHash)
}

// GetStats returns cheap, approximate stats for observability. These are
// maintained with atomics and may be slightly stale when read concurrently.
func (aaf *adaptiveAdmissionFilter) GetStats() (requests, grants, scanRejections uint64, admissionRate float64, currentProbability uint32) {
	requests = atomic.LoadUint64(&aaf.admissionRequests)
	grants = atomic.LoadUint64(&aaf.admissionGrants)
	scanRejections = atomic.LoadUint64(&aaf.scanRejections)
	currentProbability = atomic.LoadUint32(&aaf.admissionProbability)

	if requests > 0 {
		admissionRate = float64(grants) / float64(requests)
	}
	return
}

// Reset reinitializes the doorkeeper and decays the frequency sketch (via age()).
// Intended for administrative "clear-like" operations, not for steady-state use.
func (a *adaptiveAdmissionFilter) Reset() {
	a.doorkeeper.reset()
	a.frequencyFilter.age()
}
