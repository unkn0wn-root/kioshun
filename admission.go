package cache

import (
	"math/bits"
	"time"

	"github.com/unkn0wn-root/kioshun/internal/mathutil"
)

const (
	// Hash mixing (xxHash64-style avalanche), then mask into power-of-two bitsets.
	bloomMixPrime1 = 0xff51afd7ed558ccd
	bloomMixPrime2 = 0xc4ceb9fe1a85ec53

	// Admission policy.
	frequencyThreshold = 3 // admit if estimated CMS freq ≥ 3

	// Eviction-pressure feedback (nudges global admission prob once per second).
	highEvictionRateThreshold = 100
	lowEvictionRateThreshold  = 10
	admissionProbabilityStep  = 5

	// 4-bit CMS counters (16 per uint64); periodic aging halves counts.
	bitsPerWord   = 64
	frequencyMask = 0x0F
	maxFrequency  = 15

	// Per-hash rotations for 4 CMS hashes.
	hash1Rotate = 0
	hash2Rotate = 17
	hash3Rotate = 31
	hash4Rotate = 47

	// Scan detection (switch to recency-biased policy) defaults.
	defaultScanAdmissionThreshold = uint64(100)            // admissions/sec
	defaultScanMissThreshold      = uint64(50)             // consecutive misses
	defaultScanRecencyThreshold   = 100 * time.Millisecond // 100ms
	defaultRecencyTieBreak        = 1 * time.Second        // 1s

	// Feedback cadence.
	evictionPressureInterval = int64(time.Second)
)

// hashN rotates+mixes a 64-bit value and masks into [0..mask]; mask should be size-1.
func hashN(hash, mask uint64, rotate int) uint64 {
	h := bits.RotateLeft64(hash, int(rotate))
	h ^= h >> 33
	h *= bloomMixPrime1
	h ^= h >> 29
	h *= bloomMixPrime2
	h ^= h >> 32
	return h & mask
}

// acceptWithProb returns true with probability using high bits of an avalanche-mixed hash.
func acceptWithProb(hash uint64, prob uint32) bool {
	if prob >= 100 {
		return true
	}
	if prob == 0 {
		return false
	}

	r := uint32(xxHash64Avalanche(hash) >> 32)
	thr := uint32((uint64(prob) * (uint64(1) << 32)) / 100)
	return r < thr
}

// bloomFilter is a 3-hash doorkeeper bitset
// caller provides any needed synchronization
type bloomFilter struct {
	bits []uint64
	size uint64
	mask uint64
}

// newBloomFilter allocates a power-of-two-sized bitset (at least one uint64 word).
func newBloomFilter(n uint64) *bloomFilter {
	n = uint64(mathutil.NextPowerOf2(int(n)))
	arrN := n / bitsPerWord
	if arrN == 0 {
		arrN = 1
	}
	return &bloomFilter{
		bits: make([]uint64, arrN),
		size: n,
		mask: n - 1,
	}
}

// add sets three hashed bits for keyHash (idempotent).
func (bf *bloomFilter) add(keyHash uint64) {
	h1 := hashN(keyHash, bf.mask, hash1Rotate)
	h2 := hashN(keyHash, bf.mask, hash2Rotate)
	h3 := hashN(keyHash, bf.mask, hash3Rotate)

	bf.bits[h1/bitsPerWord] |= 1 << (h1 % bitsPerWord)
	bf.bits[h2/bitsPerWord] |= 1 << (h2 % bitsPerWord)
	bf.bits[h3/bitsPerWord] |= 1 << (h3 % bitsPerWord)
}

// contains returns true iff all three hashed bits are set.
func (bf *bloomFilter) contains(keyHash uint64) bool {
	h1 := hashN(keyHash, bf.mask, hash1Rotate)
	h2 := hashN(keyHash, bf.mask, hash2Rotate)
	h3 := hashN(keyHash, bf.mask, hash3Rotate)

	b1 := bf.bits[h1/bitsPerWord] & (1 << (h1 % bitsPerWord))
	b2 := bf.bits[h2/bitsPerWord] & (1 << (h2 % bitsPerWord))
	b3 := bf.bits[h3/bitsPerWord] & (1 << (h3 % bitsPerWord))

	return b1 != 0 && b2 != 0 && b3 != 0
}

// reset zeroes the bitset.
func (bf *bloomFilter) reset() {
	clear(bf.bits)
}

// frequencyBloomFilter is a 4-hash Count–Min Sketch
// using 4-bit counters packed 16 per uint64.
type frequencyBloomFilter struct {
	counters []uint64
	size     uint64
	mask     uint64

	totalIncrements uint64
	agingThreshold  uint64
}

// newFrequencyBloomFilter allocates the packed counter array.
func newFrequencyBloomFilter(n uint64) *frequencyBloomFilter {
	sz := uint64(mathutil.NextPowerOf2(int(n)))
	arrN := sz / 16
	if arrN == 0 {
		arrN = 1
	}
	return &frequencyBloomFilter{
		counters:       make([]uint64, arrN),
		size:           sz,
		mask:           sz - 1,
		agingThreshold: sz * 10, // ~10 events per counter between halvings
	}
}

// increment bumps all four counters for keyHash (unless saturated)
// and returns the new min estimate.
func (fbf *frequencyBloomFilter) increment(keyHash uint64) uint64 {
	h1 := hashN(keyHash, fbf.mask, hash1Rotate)
	h2 := hashN(keyHash, fbf.mask, hash2Rotate)
	h3 := hashN(keyHash, fbf.mask, hash3Rotate)
	h4 := hashN(keyHash, fbf.mask, hash4Rotate)

	f := fbf.getCounterValue(h1)
	if f2 := fbf.getCounterValue(h2); f2 < f {
		f = f2
	}
	if f3 := fbf.getCounterValue(h3); f3 < f {
		f = f3
	}
	if f4 := fbf.getCounterValue(h4); f4 < f {
		f = f4
	}

	if f < maxFrequency {
		fbf.incrementCounter(h1)
		fbf.incrementCounter(h2)
		fbf.incrementCounter(h3)
		fbf.incrementCounter(h4)
		f++
	}

	fbf.totalIncrements++
	if fbf.totalIncrements >= fbf.agingThreshold {
		fbf.age()
	}
	return f
}

// estimateFrequency reads the current CMS estimate (min of the four hashed counters).
func (fbf *frequencyBloomFilter) estimateFrequency(keyHash uint64) uint64 {
	h1 := hashN(keyHash, fbf.mask, hash1Rotate)
	h2 := hashN(keyHash, fbf.mask, hash2Rotate)
	h3 := hashN(keyHash, fbf.mask, hash3Rotate)
	h4 := hashN(keyHash, fbf.mask, hash4Rotate)

	f := fbf.getCounterValue(h1)
	if f2 := fbf.getCounterValue(h2); f2 < f {
		f = f2
	}
	if f3 := fbf.getCounterValue(h3); f3 < f {
		f = f3
	}
	if f4 := fbf.getCounterValue(h4); f4 < f {
		f = f4
	}
	return f
}

// getCounterValue extracts the 4-bit counter at the logical index.
func (fbf *frequencyBloomFilter) getCounterValue(index uint64) uint64 {
	ai := index / 16
	ci := index % 16
	sh := ci * 4
	return (fbf.counters[ai] >> sh) & frequencyMask
}

// incrementCounter CAS-increments a single 4-bit counter
// saturated counters are left unchanged.
func (fbf *frequencyBloomFilter) incrementCounter(index uint64) {
	ai := index / 16
	ci := index % 16
	sh := ci * 4

	cur := fbf.counters[ai]
	v := (cur >> sh) & frequencyMask
	if v < maxFrequency {
		fbf.counters[ai] = cur + (1 << sh)
	}

}

// age halves every 4-bit counter in-place:
// shift right then mask each nibble (0b0111) to prevent spill.
func (fbf *frequencyBloomFilter) age() {
	const halfMask uint64 = 0x7777777777777777
	for i := range fbf.counters {
		cur := fbf.counters[i]
		fbf.counters[i] = (cur >> 1) & halfMask
	}
	fbf.totalIncrements = 0
}

// workloadDetector tracks admissions/sec (windowed) and consecutive misses
// triggers scan mode on thresholds.
type workloadDetector struct {
	recentAdmissions       uint64
	recentAdmissionTime    int64
	admissionRate          uint64
	consecutiveMisses      uint64
	admissionRateThreshold uint64
	missThreshold          uint64
}

// newWorkloadDetector initializes the time window for rate calculations.
func newWorkloadDetector(rateThreshold, missThreshold uint64) *workloadDetector {
	return &workloadDetector{
		recentAdmissionTime:    time.Now().UnixNano(),
		admissionRateThreshold: rateThreshold,
		missThreshold:          missThreshold,
	}
}

// detectScan updates admissionRate roughly once per second
// and returns true if rate or miss streak is high.
func (wd *workloadDetector) detectScan(_ uint64) bool {
	now := time.Now().UnixNano()
	dt := now - wd.recentAdmissionTime
	if dt > int64(time.Second) {
		adm := wd.recentAdmissions
		r := adm * 1e9 / uint64(dt)
		wd.admissionRate = r
		wd.recentAdmissions = 0
		wd.recentAdmissionTime = now
	}
	return (wd.admissionRateThreshold > 0 && wd.admissionRate > wd.admissionRateThreshold) ||
		(wd.missThreshold > 0 && wd.consecutiveMisses > wd.missThreshold)
}

// recordAdmission bumps the windowed admissions count and resets the miss streak.
func (wd *workloadDetector) recordAdmission() {
	wd.recentAdmissions++
	wd.consecutiveMisses = 0
}

// recordRejection increases the consecutive miss streak (used by detectScan()).
func (wd *workloadDetector) recordRejection() {
	wd.consecutiveMisses++
}

// adaptiveAdmissionFilter coordinates doorkeeper, CMS, scan mode, and eviction-pressure feedback.
type adaptiveAdmissionFilter struct {
	frequencyFilter      *frequencyBloomFilter
	doorkeeper           *bloomFilter
	detector             *workloadDetector
	admissionProbability uint32 // base prob for low-freq items (0..100)
	minProbability       uint32
	maxProbability       uint32
	recentEvictions      uint64
	evictionWindow       int64
	resetInterval        int64
	lastReset            int64
	admissionRequests    uint64
	admissionGrants      uint64
	scanRejections       uint64
	scanRecencyThreshold int64
	recencyTieBreak      int64
}

// newAdaptiveAdmissionFilter wires up components and seeds probabilities and timers.
func newAdaptiveAdmissionFilter(
	n uint64,
	ri time.Duration,
	scanRateThreshold uint64,
	scanMissThreshold uint64,
	scanRecency time.Duration,
	recencyTie time.Duration,
) *adaptiveAdmissionFilter {
	if scanRateThreshold == 0 {
		scanRateThreshold = defaultScanAdmissionThreshold
	}
	if scanMissThreshold == 0 {
		scanMissThreshold = defaultScanMissThreshold
	}
	if scanRecency <= 0 {
		scanRecency = defaultScanRecencyThreshold
	}
	if recencyTie <= 0 {
		recencyTie = defaultRecencyTieBreak
	}

	return &adaptiveAdmissionFilter{
		frequencyFilter:      newFrequencyBloomFilter(n),
		doorkeeper:           newBloomFilter(n / 8),
		detector:             newWorkloadDetector(scanRateThreshold, scanMissThreshold),
		admissionProbability: 70,
		minProbability:       5,
		maxProbability:       95,
		resetInterval:        int64(ri),
		lastReset:            time.Now().UnixNano(),
		evictionWindow:       time.Now().UnixNano(),
		scanRecencyThreshold: scanRecency.Nanoseconds(),
		recencyTieBreak:      recencyTie.Nanoseconds(),
	}
}

// shouldAdmit decides whether to admit a candidate at capacity using doorkeeper, scan mode, freq/recency, and probability.
func (aaf *adaptiveAdmissionFilter) shouldAdmit(keyHash uint64, vFreq uint64, vAge int64) bool {
	aaf.admissionRequests++

	// Fast path: seen recently → refresh doorkeeper, sync sketch, admit.
	if aaf.doorkeeper.contains(keyHash) {
		aaf.doorkeeper.add(keyHash)
		_ = aaf.frequencyFilter.increment(keyHash)
		aaf.detector.recordAdmission()
		aaf.admissionGrants++
		return true
	}

	// Scan mode prefers recency.
	scan := aaf.detector.detectScan(keyHash)
	if scan {
		aaf.scanRejections++
		admit := aaf.admitDuringScan(keyHash, vAge)
		if admit {
			aaf.detector.recordAdmission()
			aaf.admissionGrants++
		} else {
			aaf.detector.recordRejection()
		}
		return admit
	}

	// Normal mode: update sketch/doorkeeper and apply core policy.
	nf := aaf.frequencyFilter.increment(keyHash)
	aaf.doorkeeper.add(keyHash)
	admit := aaf.makeAdmissionDecision(keyHash, nf, vFreq, vAge)
	aaf.adjustAdmissionProbability()
	aaf.checkPeriodicReset()

	if admit {
		aaf.detector.recordAdmission()
		aaf.admissionGrants++
	} else {
		aaf.detector.recordRejection()
	}
	return admit
}

// admitDuringScan uses recency to keep fresh items; otherwise admits with a small minimum probability.
func (aaf *adaptiveAdmissionFilter) admitDuringScan(keyHash uint64, vAge int64) bool {
	now := time.Now().UnixNano()
	if vAge > 0 && aaf.scanRecencyThreshold > 0 && now-vAge > aaf.scanRecencyThreshold {
		return true
	}
	return acceptWithProb(keyHash, aaf.minProbability)
}

// makeAdmissionDecision applies the non-scan policy:
// freq threshold/compare, recency tie-break
// then probabilistic with capped victim penalty.
func (aaf *adaptiveAdmissionFilter) makeAdmissionDecision(keyHash, nf, vFreq uint64, vAge int64) bool {
	if nf >= frequencyThreshold {
		return true
	}
	if nf > vFreq {
		return true
	}
	if nf == vFreq && nf > 0 {
		if vAge > 0 {
			now := time.Now().UnixNano()
			vr := now - vAge
			if aaf.recencyTieBreak > 0 {
				return vr > aaf.recencyTieBreak
			}
		}
		return acceptWithProb(keyHash, 50)
	}

	prob := aaf.admissionProbability

	vf := min(vFreq, uint64(5))

	adjustedProb := int64(prob) - int64(vf*10)
	adjustedProb = max(adjustedProb, int64(aaf.minProbability))
	return acceptWithProb(keyHash, uint32(adjustedProb))
}

// adjustAdmissionProbability nudges the global probability once per interval based on recent evictions.
func (aaf *adaptiveAdmissionFilter) adjustAdmissionProbability() {
	now := time.Now().UnixNano()
	win := aaf.evictionWindow

	if now-win > evictionPressureInterval {
		ev := aaf.recentEvictions
		p := aaf.admissionProbability
		if ev > highEvictionRateThreshold {
			np := p
			if np > admissionProbabilityStep {
				np -= admissionProbabilityStep
			}
			np = max(np, aaf.minProbability)
			aaf.admissionProbability = np
		} else if ev < lowEvictionRateThreshold {
			np := p + admissionProbabilityStep
			np = min(np, aaf.maxProbability)
			aaf.admissionProbability = np
		}

		aaf.recentEvictions = 0
		aaf.evictionWindow = now
	}
}

// checkPeriodicReset clears the doorkeeper on schedule to avoid long-term saturation.
func (aaf *adaptiveAdmissionFilter) checkPeriodicReset() {
	now := time.Now().UnixNano()
	if now-aaf.lastReset > aaf.resetInterval {
		aaf.doorkeeper.reset()
		aaf.lastReset = now
	}
}

// RecordEviction feeds back pressure after an eviction.
func (aaf *adaptiveAdmissionFilter) RecordEviction() {
	aaf.recentEvictions++
}

// GetFrequencyEstimate exposes the current CMS estimate for a key hash (telemetry/debug).
func (aaf *adaptiveAdmissionFilter) GetFrequencyEstimate(keyHash uint64) uint64 {
	return aaf.frequencyFilter.estimateFrequency(keyHash)
}

// GetStats returns approximate counters and current probability (atomics; may be slightly stale).
func (aaf *adaptiveAdmissionFilter) GetStats() (reqs, grants, scanRej uint64, rate float64, prob uint32) {
	reqs = aaf.admissionRequests
	grants = aaf.admissionGrants
	scanRej = aaf.scanRejections
	prob = aaf.admissionProbability

	if reqs > 0 {
		rate = float64(grants) / float64(reqs)
	}
	return
}

// Reset clears the doorkeeper and decays the sketch (administrative use).
func (a *adaptiveAdmissionFilter) Reset() {
	a.doorkeeper.reset()
	a.frequencyFilter.age()
}
