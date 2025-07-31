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
	h1 := hashN(keyHash, bf.mask, 0)
	h2 := hashN(keyHash, bf.mask, 17)
	h3 := hashN(keyHash, bf.mask, 31)

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
	// Reset the increment counter to start a new aging cycle
	atomic.StoreUint64(&fbf.totalIncrements, 0)
}

// frequencyAdmissionFilter makes cache admission decisions by combining a "doorkeeper"
// (to catch recently seen items) with the more stable frequency estimates above.
type frequencyAdmissionFilter struct {
	frequencyFilter *frequencyBloomFilter
	doorkeeper      *bloomFilter

	resetInterval int64 // nanoseconds between automatic resets
	lastReset     int64 // timestamp of the last reset

	admissionRequests uint64 // total calls to shouldAdmit
	admissionGrants   uint64 // total times shouldAdmit returned true
}

// newFrequencyAdmissionFilter sets up the two-layer filter. The doorkeeper uses 1/8 the size of the
// frequency sketch to cheaply track very recent accesses.
func newFrequencyAdmissionFilter(numCounters uint64, resetInterval time.Duration) *frequencyAdmissionFilter {
	return &frequencyAdmissionFilter{
		frequencyFilter: newFrequencyBloomFilter(numCounters),
		doorkeeper:      newBloomFilter(numCounters / 8),
		resetInterval:   int64(resetInterval),
		lastReset:       time.Now().UnixNano(),
	}
}

// shouldAdmit returns true if keyHash should be admitted to cache, comparing its estimated frequency
// against a victim's frequency, with special cases for ties and rare items.
func (faf *frequencyAdmissionFilter) shouldAdmit(keyHash uint64, victimFrequency uint64) bool {
	atomic.AddUint64(&faf.admissionRequests, 1)

	// Phase 1: if it's in the doorkeeper, always admit and refresh its bit
	if faf.doorkeeper.contains(keyHash) {
		faf.doorkeeper.add(keyHash)
		atomic.AddUint64(&faf.admissionGrants, 1)
		return true
	}

	// Phase 2: update and retrieve frequency estimate
	newFreq := faf.frequencyFilter.increment(keyHash)
	faf.doorkeeper.add(keyHash)

	// Phase 3: admission logic based on frequency thresholds and comparisons
	var admit bool
	// Guaranteed admission if seen at least frequencyThreshold times
	if newFreq >= frequencyThreshold {
		admit = true
	} else if newFreq > victimFrequency {
		// Admit if this item's frequency exceeds the victim's
		admit = true
	} else if newFreq == victimFrequency && newFreq > 0 {
		// Tie: admit half the time based on a bit of the hash
		admit = (keyHash % 2) == 0
	} else {
		// Otherwise, admit with a base probability reduced by victim's frequency
		// baseAdmissionRate is in percent, victimFrequency*10 scales it down
		admit = (keyHash % 100) < uint64(baseAdmissionRate-int(victimFrequency*10))
	}

	// Periodic reset of the doorkeeper based on elapsed time
	now := time.Now().UnixNano()
	if now-faf.lastReset > faf.resetInterval {
		faf.doorkeeper.reset()
		faf.lastReset = now
	}

	if admit {
		atomic.AddUint64(&faf.admissionGrants, 1)
	}
	return admit
}

// getFrequencyEstimate returns the sketch's current count estimate for keyHash.
func (faf *frequencyAdmissionFilter) getFrequencyEstimate(keyHash uint64) uint64 {
	return faf.frequencyFilter.estimateFrequency(keyHash)
}

// getStats reports total requests, grants, and the current admission rate (grants/requests)
func (faf *frequencyAdmissionFilter) getStats() (requests, grants uint64, rate float64) {
	requests = atomic.LoadUint64(&faf.admissionRequests)
	grants = atomic.LoadUint64(&faf.admissionGrants)

	if requests > 0 {
		rate = float64(grants) / float64(requests)
	}
	return
}
