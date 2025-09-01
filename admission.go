package cache

import (
    "math/bits"
    "sync/atomic"
    "time"

    "github.com/unkn0wn-root/kioshun/internal/mathutil"
)

const (
	// Hash mixing (xxHash64-style avalanche), then mask into power-of-two bitsets.
	bloomMixPrime1 = 0xff51afd7ed558ccd
	bloomMixPrime2 = 0xc4ceb9fe1a85ec53

	// Admission policy.
	baseAdmissionRate  = 70 // seed; live value set in newAdaptiveAdmissionFilter
	frequencyThreshold = 3  // admit if estimated CMS freq ≥ 3

	// Eviction-pressure feedback (nudges global admission prob once per second).
	highEvictionRateThreshold = 100
	lowEvictionRateThreshold  = 10
	admissionProbabilityStep  = 5

	// 4-bit CMS counters (16 per uint64); periodic aging halves counts.
	bitsPerWord   = 64
	frequencyMask = 0x0F
	maxFrequency  = 15
	agingFactor   = 2

	// Per-hash rotations for 4 CMS hashes.
	hash1Rotate = 0
	hash2Rotate = 17
	hash3Rotate = 31
	hash4Rotate = 47

	// Scan detection (switch to recency-biased policy).
	scanAdmissionThreshold = 100 // admissions/sec
	scanMissThreshold      = 50  // consecutive misses

	// Time thresholds (ns).
	scanModeRecencyThreshold = 100e6 // 100ms
	recencyTieBreakThreshold = 1e9   // 1s

	// Feedback cadence.
	evictionPressureInterval = 1e9 // 1s
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

// bloomFilter is a 3-hash doorkeeper bitset (caller provides any needed synchronization).
type bloomFilter struct {
	bits []uint64
	size uint64
	mask uint64
}

// newBloomFilter allocates a power-of-two-sized bitset (at least one uint64 word).
func newBloomFilter(size uint64) *bloomFilter {
    size = uint64(mathutil.NextPowerOf2(int(size)))
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

	bit1 := bf.bits[h1/bitsPerWord] & (1 << (h1 % bitsPerWord))
	bit2 := bf.bits[h2/bitsPerWord] & (1 << (h2 % bitsPerWord))
	bit3 := bf.bits[h3/bitsPerWord] & (1 << (h3 % bitsPerWord))

	return bit1 != 0 && bit2 != 0 && bit3 != 0
}

// reset zeroes the bitset.
func (bf *bloomFilter) reset() {
	clear(bf.bits)
}

// frequencyBloomFilter is a 4-hash Count–Min Sketch using 4-bit counters packed 16 per uint64.
type frequencyBloomFilter struct {
	counters []uint64
	size     uint64
	mask     uint64

	totalIncrements uint64
	agingThreshold  uint64
}

// newFrequencyBloomFilter allocates the packed counter array (size rounded to power of two).
func newFrequencyBloomFilter(numCounters uint64) *frequencyBloomFilter {
    size := uint64(mathutil.NextPowerOf2(int(numCounters)))
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

// increment bumps all four counters for keyHash (unless saturated) and returns the new min estimate.
func (fbf *frequencyBloomFilter) increment(keyHash uint64) uint64 {
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

	if freq < maxFrequency {
		fbf.incrementCounter(h1)
		fbf.incrementCounter(h2)
		fbf.incrementCounter(h3)
		fbf.incrementCounter(h4)
		freq++
	}

	if atomic.AddUint64(&fbf.totalIncrements, 1) >= fbf.agingThreshold {
		fbf.age()
	}
	return freq
}

// estimateFrequency reads the current CMS estimate (min of the four hashed counters).
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

// getCounterValue extracts the 4-bit counter at the logical index.
func (fbf *frequencyBloomFilter) getCounterValue(index uint64) uint64 {
	arrayIndex := index / 16
	counterIndex := index % 16
	shift := counterIndex * 4
	return (fbf.counters[arrayIndex] >> shift) & frequencyMask
}

// incrementCounter CAS-increments a single 4-bit counter; saturated counters are left unchanged.
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

// age halves every 4-bit counter in-place: shift right then mask each nibble (0b0111) to prevent spill.
func (fbf *frequencyBloomFilter) age() {
	const halfMask uint64 = 0x7777777777777777
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

// workloadDetector tracks admissions/sec (windowed) and consecutive misses; triggers scan mode on thresholds.
type workloadDetector struct {
	recentAdmissions    uint64
	recentAdmissionTime int64
	admissionRate       uint64
	consecutiveMisses   uint64
}

// newWorkloadDetector initializes the time window for rate calculations.
func newWorkloadDetector() *workloadDetector {
	return &workloadDetector{
		recentAdmissionTime: time.Now().UnixNano(),
	}
}

// detectScan updates admissionRate roughly once per second and returns true if rate or miss streak is high.
func (wd *workloadDetector) detectScan(keyHash uint64) bool {
	now := time.Now().UnixNano()

	elapsed := now - atomic.LoadInt64(&wd.recentAdmissionTime)
	if elapsed > 1e9 {
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

// recordAdmission bumps the windowed admissions count and resets the miss streak.
func (wd *workloadDetector) recordAdmission() {
	atomic.AddUint64(&wd.recentAdmissions, 1)
	atomic.StoreUint64(&wd.consecutiveMisses, 0)
}

// recordRejection increases the consecutive miss streak (used by detectScan()).
func (wd *workloadDetector) recordRejection() {
	atomic.AddUint64(&wd.consecutiveMisses, 1)
}

// adaptiveAdmissionFilter coordinates doorkeeper, CMS, scan mode, and eviction-pressure feedback.
type adaptiveAdmissionFilter struct {
	frequencyFilter *frequencyBloomFilter
	doorkeeper      *bloomFilter
	detector        *workloadDetector

	admissionProbability uint32 // base prob for low-freq items (0..100)
	minProbability       uint32
	maxProbability       uint32

	recentEvictions uint64
	evictionWindow  int64

	resetInterval int64
	lastReset     int64

	admissionRequests uint64
	admissionGrants   uint64
	scanRejections    uint64
}

// newAdaptiveAdmissionFilter wires up components and seeds probabilities and timers.
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

// shouldAdmit decides whether to admit a candidate at capacity using doorkeeper, scan mode, freq/recency, and probability.
func (aaf *adaptiveAdmissionFilter) shouldAdmit(keyHash uint64, victimFrequency uint64, victimAge int64) bool {
	atomic.AddUint64(&aaf.admissionRequests, 1)

	// Fast path: seen recently → refresh doorkeeper, sync sketch, admit.
	if aaf.doorkeeper.contains(keyHash) {
		aaf.doorkeeper.add(keyHash)
		_ = aaf.frequencyFilter.increment(keyHash)
		aaf.detector.recordAdmission()
		atomic.AddUint64(&aaf.admissionGrants, 1)
		return true
	}

	// Scan mode prefers recency.
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

	// Normal mode: update sketch/doorkeeper and apply core policy.
	newFreq := aaf.frequencyFilter.increment(keyHash)
	aaf.doorkeeper.add(keyHash)

	admit := aaf.makeAdmissionDecision(keyHash, newFreq, victimFrequency, victimAge)

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

// admitDuringScan uses recency to keep fresh items; otherwise admits with a small minimum probability.
func (aaf *adaptiveAdmissionFilter) admitDuringScan(keyHash uint64, victimAge int64) bool {
	now := time.Now().UnixNano()
	if victimAge > 0 && now-victimAge > scanModeRecencyThreshold {
		return true
	}
	return (keyHash % 100) < uint64(aaf.minProbability)
}

// makeAdmissionDecision applies the non-scan policy:
// freq threshold/compare, recency tie-break
// then probabilistic with capped victim penalty.
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

// adjustAdmissionProbability nudges the global probability once per interval based on recent evictions.
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
	atomic.AddUint64(&aaf.recentEvictions, 1)
}

// GetFrequencyEstimate exposes the current CMS estimate for a key hash (telemetry/debug).
func (aaf *adaptiveAdmissionFilter) GetFrequencyEstimate(keyHash uint64) uint64 {
	return aaf.frequencyFilter.estimateFrequency(keyHash)
}

// GetStats returns approximate counters and current probability (atomics; may be slightly stale).
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

// Reset clears the doorkeeper and decays the sketch (administrative use).
func (a *adaptiveAdmissionFilter) Reset() {
	a.doorkeeper.reset()
	a.frequencyFilter.age()
}
