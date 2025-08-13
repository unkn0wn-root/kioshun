package cache

import (
	"math/bits"
	"sync/atomic"
	"time"
)

const (
	// Hash mixing primes (based on xxHash64 avalanche steps)
	bloomMixPrime1 = 0xff51afd7ed558ccd
	bloomMixPrime2 = 0xc4ceb9fe1a85ec53

	// Admission thresholds
	baseAdmissionRate  = 70 // Base chance (in percent) to admit low-frequency items
	frequencyThreshold = 3  // Guaranteed admission for items seen at least this many times

	// Eviction pressure thresholds
	highEvictionRateThreshold = 100 // Treshold for reducing admission probability
	lowEvictionRateThreshold  = 10  // Threshold for increasing admission probability
	admissionProbabilityStep  = 5   // Step size for admission probability adjustment

	// Bit-array and counter packing params
	bitsPerWord   = 64   // Bits per uint64 for bit array indexing
	frequencyMask = 0x0F // Mask for 4-bit counter
	maxFrequency  = 15   // Maximum value storable in a 4-bit counter
	agingFactor   = 2    // Divisor used during the periodic aging process

	// Rotation offsets used to derive distinct hash functions from one value
	hash1Rotate = 0  // No rotation for first hash
	hash2Rotate = 17 // Rotate left by 17 bits for second hash
	hash3Rotate = 31 // by 31...
	hash4Rotate = 47

	// Scan detection constants
	scanAdmissionThreshold = 100 // Admissions/second that suggests scanning
	scanSequenceThreshold  = 8   // Sequential accesses that indicate scanning
	scanMissThreshold      = 50  // Consecutive misses that suggest scanning
	sequenceBufferSize     = 16  // Size of circular buffer for sequence detection

	// Time-based thresholds
	scanModeRecencyThreshold = 100e6 // 100ms in nanoseconds - victim age threshold during scanning
	recencyTieBreakThreshold = 1e9   // 1 second in nanoseconds - recency threshold for frequency ties
	evictionPressureInterval = 1e9   // 1 second in nanoseconds - interval for eviction pressure monitoring
)

// hashN applies a bit-rotation and a simple avalanche mixing,
// then reduces the result modulo 'mask+1' (mask is assumed size-1 of a power-of-two).
func hashN(hash, mask uint64, rotate int) uint64 {
	h := bits.RotateLeft64(hash, int(rotate))
	h ^= h >> 33
	h *= bloomMixPrime1
	h ^= h >> 29
	h *= bloomMixPrime2
	h ^= h >> 32
	return h & mask
}

// bloomFilter implements a basic Bloom filter over a bit array of size.
// Uses three hash functions to set or check bits, yielding low false positives.
type bloomFilter struct {
	bits []uint64 // Packed bit array
	size uint64   // Total number of bits (power of two)
	mask uint64   // size-1, for fast bit-positioning
}

// newBloomFilter creates a bloom filter with the specified bit size.
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

// add marks the presence of keyHash by setting three bits in the bit array.
func (bf *bloomFilter) add(keyHash uint64) {
	h1 := hashN(keyHash, bf.mask, hash1Rotate)
	h2 := hashN(keyHash, bf.mask, hash2Rotate)
	h3 := hashN(keyHash, bf.mask, hash3Rotate)

	bf.bits[h1/bitsPerWord] |= 1 << (h1 % bitsPerWord)
	bf.bits[h2/bitsPerWord] |= 1 << (h2 % bitsPerWord)
	bf.bits[h3/bitsPerWord] |= 1 << (h3 % bitsPerWord)
}

// contains returns true if all three bits corresponding to keyHash are set.
func (bf *bloomFilter) contains(keyHash uint64) bool {
	h1 := hashN(keyHash, bf.mask, hash1Rotate)
	h2 := hashN(keyHash, bf.mask, hash2Rotate)
	h3 := hashN(keyHash, bf.mask, hash3Rotate)

	bit1 := bf.bits[h1/bitsPerWord] & (1 << (h1 % bitsPerWord))
	bit2 := bf.bits[h2/bitsPerWord] & (1 << (h2 % bitsPerWord))
	bit3 := bf.bits[h3/bitsPerWord] & (1 << (h3 % bitsPerWord))

	return bit1 != 0 && bit2 != 0 && bit3 != 0
}

// reset clears the filter by zeroing all words.
func (bf *bloomFilter) reset() {
	for i := range bf.bits {
		bf.bits[i] = 0
	}
}

// frequencyBloomFilter maintains approximate counts using 4-bit counters
// packed into uint64 words. It uses a count–min sketch approach with four hashes.
type frequencyBloomFilter struct {
	counters []uint64 // Each uint64 holds 16 4-bit counters
	size     uint64   // Total number of 4-bit counters
	mask     uint64   // size-1, for index masking

	totalIncrements uint64 // Number of increments since last aging
	agingThreshold  uint64 // Trigger aging when totalIncrements ≥ threshold
}

// newFrequencyBloomFilter creates a sketch with counters,
// rounding to next power of two and packing 16 counters per uint64.
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
		agingThreshold: size * 10, // Age after roughly 10× as many increments as counters
	}
}

// increment increases the count for keyHash and returns the new minimum estimate.
// Uses the smallest of four counters to avoid overcounting (CMS).
func (fbf *frequencyBloomFilter) increment(keyHash uint64) uint64 {
	h1 := hashN(keyHash, fbf.mask, hash1Rotate)
	h2 := hashN(keyHash, fbf.mask, hash2Rotate)
	h3 := hashN(keyHash, fbf.mask, hash3Rotate)
	h4 := hashN(keyHash, fbf.mask, hash4Rotate)

	// Find the minimum counter value among the four positions
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

	// Only increment if below the 4-bit maximum
	if freq < maxFrequency {
		fbf.incrementCounter(h1)
		fbf.incrementCounter(h2)
		fbf.incrementCounter(h3)
		fbf.incrementCounter(h4)
		freq++
	}

	// Trigger aging if we've done enough increments
	if atomic.AddUint64(&fbf.totalIncrements, 1) >= fbf.agingThreshold {
		fbf.age()
	}

	return freq
}

// estimateFrequency returns the estimated frequency of a key
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

// getCounterValue extracts 4-bit counter value from packed uint64
func (fbf *frequencyBloomFilter) getCounterValue(index uint64) uint64 {
	arrayIndex := index / 16
	counterIndex := index % 16
	shift := counterIndex * 4
	return (fbf.counters[arrayIndex] >> shift) & frequencyMask
}

// incrementCounter atomically increases the 4-bit counter.
// Uses CAS to retry on concurrent modifications.
func (fbf *frequencyBloomFilter) incrementCounter(index uint64) {
	arrayIndex := index / 16
	counterIndex := index % 16
	shift := counterIndex * 4

	for {
		current := atomic.LoadUint64(&fbf.counters[arrayIndex])
		val := (current >> shift) & frequencyMask
		if val >= maxFrequency {
			return // Already at top value
		}

		newVal := current + (1 << shift)
		if atomic.CompareAndSwapUint64(&fbf.counters[arrayIndex], current, newVal) {
			return
		}
	}
}

// age reduces all counters by half
func (fbf *frequencyBloomFilter) age() {
	for i := range fbf.counters {
		for {
			current := atomic.LoadUint64(&fbf.counters[i])
			aged := uint64(0)
			// Divide each 4-bit counter by 2
			for j := 0; j < 16; j++ {
				shift := j * 4
				val := (current >> shift) & frequencyMask
				aged |= (val / agingFactor) << shift
			}

			if atomic.CompareAndSwapUint64(&fbf.counters[i], current, aged) {
				break
			}
		}
	}
	atomic.StoreUint64(&fbf.totalIncrements, 0)
}

// workloadDetector identifies scanning patterns to prevent cache pollution
type workloadDetector struct {
	recentAdmissions    uint64                     // Count of recent admissions
	recentAdmissionTime int64                      // Timestamp of window start
	admissionRate       uint64                     // Admissions per second
	consecutiveMisses   uint64                     // Consecutive cache misses
	lastKeys            [sequenceBufferSize]uint64 // Recent key hashes for sequence detection
	lastKeyIndex        uint32                     // Current position in circular buffer
	sequenceCount       uint32                     // Count of sequential accesses
}

// newWorkloadDetector creates a new scan detector
func newWorkloadDetector() *workloadDetector {
	return &workloadDetector{
		recentAdmissionTime: time.Now().UnixNano(),
	}
}

// detectScan analyzes access patterns to identify scanning workloads
func (wd *workloadDetector) detectScan(keyHash uint64) bool {
	now := time.Now().UnixNano()

	elapsed := now - atomic.LoadInt64(&wd.recentAdmissionTime)
	if elapsed > 1e9 { // More than 1 second
		admissions := atomic.LoadUint64(&wd.recentAdmissions)
		if elapsed > 0 {
			rate := admissions * 1e9 / uint64(elapsed)
			atomic.StoreUint64(&wd.admissionRate, rate)
		}
		atomic.StoreUint64(&wd.recentAdmissions, 0)
		atomic.StoreInt64(&wd.recentAdmissionTime, now)
	}

	idx := atomic.LoadUint32(&wd.lastKeyIndex)
	lastKey := wd.lastKeys[idx&(sequenceBufferSize-1)]

	if keyHash == lastKey+1 {
		atomic.AddUint32(&wd.sequenceCount, 1)
	} else {
		atomic.StoreUint32(&wd.sequenceCount, 0)
	}

	atomic.AddUint32(&wd.lastKeyIndex, 1)
	wd.lastKeys[(idx+1)&(sequenceBufferSize-1)] = keyHash

	return atomic.LoadUint64(&wd.admissionRate) > scanAdmissionThreshold ||
		atomic.LoadUint32(&wd.sequenceCount) > scanSequenceThreshold ||
		atomic.LoadUint64(&wd.consecutiveMisses) > scanMissThreshold
}

// recordAdmission updates tracking for admission decisions
func (wd *workloadDetector) recordAdmission() {
	atomic.AddUint64(&wd.recentAdmissions, 1)
	atomic.StoreUint64(&wd.consecutiveMisses, 0) // Reset miss counter on admission
}

// recordRejection updates tracking for rejection decisions
func (wd *workloadDetector) recordRejection() {
	atomic.AddUint64(&wd.consecutiveMisses, 1)
}

// adaptiveAdmissionFilter combines frequency-based admission with scan resistance
// and dynamic probability adjustment based on cache pressure.
type adaptiveAdmissionFilter struct {
	frequencyFilter *frequencyBloomFilter
	doorkeeper      *bloomFilter
	detector        *workloadDetector

	// Adaptive parameters
	admissionProbability uint32 // Current admission probability (0-100)
	minProbability       uint32 // Minimum admission probability
	maxProbability       uint32 // Maximum admission probability

	// Eviction tracking for adaptive behavior
	recentEvictions uint64 // Track recent eviction count
	evictionWindow  int64  // Time window for eviction tracking

	// Periodic reset
	resetInterval int64 // nanoseconds between automatic resets
	lastReset     int64 // timestamp of the last reset

	// Statistics
	admissionRequests uint64 // total calls to shouldAdmit
	admissionGrants   uint64 // total times shouldAdmit returned true
	scanRejections    uint64 // rejections due to scan detection
}

// NewAdaptiveAdmissionFilter creates a new adaptive scan-resistant admission filter
func newAdaptiveAdmissionFilter(numCounters uint64, resetInterval time.Duration) *adaptiveAdmissionFilter {
	return &adaptiveAdmissionFilter{
		frequencyFilter:      newFrequencyBloomFilter(numCounters),
		doorkeeper:           newBloomFilter(numCounters / 8),
		detector:             newWorkloadDetector(),
		admissionProbability: 70, // Start at 70%
		minProbability:       5,  // Never go below 5%
		maxProbability:       95, // Never go above 95%
		resetInterval:        int64(resetInterval),
		lastReset:            time.Now().UnixNano(),
		evictionWindow:       time.Now().UnixNano(),
	}
}

// ShouldAdmit makes intelligent admission decisions combining frequency analysis,
// scan detection, and adaptive probability adjustment.
func (aaf *adaptiveAdmissionFilter) shouldAdmit(keyHash uint64, victimFrequency uint64, victimAge int64) bool {
	atomic.AddUint64(&aaf.admissionRequests, 1)

	if aaf.doorkeeper.contains(keyHash) {
		aaf.doorkeeper.add(keyHash) // Refresh the bit
		aaf.detector.recordAdmission()
		atomic.AddUint64(&aaf.admissionGrants, 1)
		return true
	}

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

// admitDuringScan uses recency-based policy during detected scanning workloads
func (aaf *adaptiveAdmissionFilter) admitDuringScan(keyHash uint64, victimAge int64) bool {
	now := time.Now().UnixNano()

	// During scanning, prefer recency over frequency
	// Admit if victim is older than threshold (keep recent items)
	if victimAge > 0 && now-victimAge > scanModeRecencyThreshold {
		return true
	}
	return (keyHash % 100) < uint64(aaf.minProbability)
}

// makeAdmissionDecision implements the core admission logic
func (aaf *adaptiveAdmissionFilter) makeAdmissionDecision(keyHash, newFreq, victimFrequency uint64, victimAge int64) bool {
	// Guaranteed admission for high-frequency items
	if newFreq >= frequencyThreshold {
		return true
	}

	// Admit if higher frequency than victim
	if newFreq > victimFrequency {
		return true
	}

	// Handle ties with recency consideration
	if newFreq == victimFrequency && newFreq > 0 {
		if victimAge > 0 {
			now := time.Now().UnixNano()
			victimRecency := now - victimAge
			// Prefer newer items for ties (victim older than threshold)
			return victimRecency > recencyTieBreakThreshold
		}
		return (keyHash % 2) == 0
	}

	// Probabilistic admission with adaptive threshold
	prob := atomic.LoadUint32(&aaf.admissionProbability)
	adjustedProb := int64(prob) - int64(victimFrequency*10)
	if adjustedProb < int64(aaf.minProbability) {
		adjustedProb = int64(aaf.minProbability)
	}

	return (keyHash % 100) < uint64(adjustedProb)
}

// adjustAdmissionProbability dynamically adjusts based on eviction pressure
func (aaf *adaptiveAdmissionFilter) adjustAdmissionProbability() {
	now := time.Now().UnixNano()
	window := atomic.LoadInt64(&aaf.evictionWindow)

	if now-window > evictionPressureInterval { // Every second
		evictions := atomic.LoadUint64(&aaf.recentEvictions)
		currentProb := atomic.LoadUint32(&aaf.admissionProbability)

		// High eviction rate: decrease admission probability
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
			// Low eviction rate: increase admission probability
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

// checkPeriodicReset performs periodic doorkeeper reset
func (aaf *adaptiveAdmissionFilter) checkPeriodicReset() {
	now := time.Now().UnixNano()
	if now-aaf.lastReset > aaf.resetInterval {
		aaf.doorkeeper.reset()
		aaf.lastReset = now
	}
}

// RecordEviction notifies the admission filter of an eviction for adaptive behavior
func (aaf *adaptiveAdmissionFilter) RecordEviction() {
	atomic.AddUint64(&aaf.recentEvictions, 1)
}

// GetFrequencyEstimate returns the current frequency estimate for a key
func (aaf *adaptiveAdmissionFilter) GetFrequencyEstimate(keyHash uint64) uint64 {
	return aaf.frequencyFilter.estimateFrequency(keyHash)
}

// GetStats returns comprehensive statistics about the admission filter
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

// Reset brings the filter back to its initial state.
func (a *adaptiveAdmissionFilter) Reset() {
	a.doorkeeper.reset()
	a.frequencyFilter.age()
}
