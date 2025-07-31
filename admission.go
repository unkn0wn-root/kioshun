package cache

import (
	"math/bits"
	"sync/atomic"
	"time"
)

const (
	// Hash mixing primes from xxHash64
	bloomMixPrime1 = 0xff51afd7ed558ccd
	bloomMixPrime2 = 0xc4ceb9fe1a85ec53

	// Admission thresholds
	baseAdmissionRate  = 70 // Base admission rate
	frequencyThreshold = 3  // Minimum frequency for guaranteed admission

	bitsPerWord   = 64   // Bits per uint64 for bit array indexing
	frequencyMask = 0x0F // Mask for 4-bit counter
	maxFrequency  = 15   // Maximum frequency value
	agingFactor   = 2    // Divide frequencies by this during aging

	// rotate amounts for hash1…hash4
	hash1Rotate = 0
	hash2Rotate = 17
	hash3Rotate = 31
	hash4Rotate = 47
)

// hashN is mixer.
func hashN(hash, mask uint64, rotate int) uint64 {
	h := bits.RotateLeft64(hash, int(rotate))
	h ^= h >> 33
	h *= bloomMixPrime1
	h ^= h >> 29
	h *= bloomMixPrime2
	h ^= h >> 32
	return h & mask
}

// bloomFilter implements a probabilistic set membership test.
// Uses 3 hash functions for ~1% false positive rate at 10 bits per item.
type bloomFilter struct {
	bits []uint64 // Bit array stored as uint64 slices
	size uint64   // Total number of bits
	mask uint64   // Bitmask for fast modulo (size - 1)
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
		mask: size - 1, // For hash & mask == hash % size
	}
}

// add marks a key as present in the bloom filter
func (bf *bloomFilter) add(keyHash uint64) {
	h1 := hashN(keyHash, bf.mask, hash1Rotate)
	h2 := hashN(keyHash, bf.mask, hash2Rotate)
	h3 := hashN(keyHash, bf.mask, hash3Rotate)

	// Set bits at all three hash positions
	bf.bits[h1/bitsPerWord] |= 1 << (h1 % bitsPerWord)
	bf.bits[h2/bitsPerWord] |= 1 << (h2 % bitsPerWord)
	bf.bits[h3/bitsPerWord] |= 1 << (h3 % bitsPerWord)
}

// contains checks if a key might be in the set
func (bf *bloomFilter) contains(keyHash uint64) bool {
	h1 := hashN(keyHash, bf.mask, 0)
	h2 := hashN(keyHash, bf.mask, 17)
	h3 := hashN(keyHash, bf.mask, 31)

	// Check if all three bits are set
	bit1 := bf.bits[h1/bitsPerWord] & (1 << (h1 % bitsPerWord))
	bit2 := bf.bits[h2/bitsPerWord] & (1 << (h2 % bitsPerWord))
	bit3 := bf.bits[h3/bitsPerWord] & (1 << (h3 % bitsPerWord))

	return bit1 != 0 && bit2 != 0 && bit3 != 0
}

// reset clears all bits in the filter
func (bf *bloomFilter) reset() {
	for i := range bf.bits {
		bf.bits[i] = 0
	}
}

// frequencyBloomFilter combines bloom filter with frequency estimation
// Uses 4-bit counters with Count-Min Sketch approach for frequency tracking
type frequencyBloomFilter struct {
	counters []uint64 // Each uint64 holds 16 4-bit counters
	size     uint64   // Total number of 4-bit counters
	mask     uint64   // Bitmask for fast modulo

	totalIncrements uint64
	agingThreshold  uint64 // When to trigger aging
}

// newFrequencyBloomFilter creates enhanced bloom with frequency tracking
func newFrequencyBloomFilter(numCounters uint64) *frequencyBloomFilter {
	size := uint64(nextPowerOf2(int(numCounters)))

	// Each uint64 holds 16 4-bit counters
	arraySize := size / 16
	if arraySize == 0 {
		arraySize = 1
	}

	return &frequencyBloomFilter{
		counters:       make([]uint64, arraySize),
		size:           size,
		mask:           size - 1,
		agingThreshold: size * 10, // Age when we've seen 10x the number of counters
	}
}

// increment frequency for a key
func (fbf *frequencyBloomFilter) increment(keyHash uint64) uint64 {
	h1 := hashN(keyHash, fbf.mask, hash1Rotate)
	h2 := hashN(keyHash, fbf.mask, hash2Rotate)
	h3 := hashN(keyHash, fbf.mask, hash3Rotate)
	h4 := hashN(keyHash, fbf.mask, hash4Rotate)

	// Get minimum frequency among all hash positions (Count-Min Sketch approach)
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

// estimateFrequency returns the estimated frequency of a key
func (fbf *frequencyBloomFilter) estimateFrequency(keyHash uint64) uint64 {
	h1 := hashN(keyHash, fbf.mask, hash1Rotate)
	h2 := hashN(keyHash, fbf.mask, hash2Rotate)
	h3 := hashN(keyHash, fbf.mask, hash3Rotate)
	h4 := hashN(keyHash, fbf.mask, hash4Rotate)

	// Return minimum frequency
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
	shiftAmount := counterIndex * 4

	counter := fbf.counters[arrayIndex]
	return (counter >> shiftAmount) & frequencyMask
}

// incrementCounter increments a 4-bit counter
func (fbf *frequencyBloomFilter) incrementCounter(index uint64) {
	arrayIndex := index / 16
	counterIndex := index % 16
	shiftAmount := counterIndex * 4

	for {
		current := atomic.LoadUint64(&fbf.counters[arrayIndex])
		currentValue := (current >> shiftAmount) & frequencyMask

		if currentValue >= maxFrequency {
			return // Already at maximum
		}

		newValue := current + (1 << shiftAmount)
		if atomic.CompareAndSwapUint64(&fbf.counters[arrayIndex], current, newValue) {
			return
		}
		// Retry if CAS failed
	}
}

// age reduces all counters by half
func (fbf *frequencyBloomFilter) age() {
	for i := range fbf.counters {
		for {
			current := atomic.LoadUint64(&fbf.counters[i])

			// Divide each 4-bit counter by 2
			aged := uint64(0)
			for j := 0; j < 16; j++ {
				shiftAmount := j * 4
				counterValue := (current >> shiftAmount) & frequencyMask
				agedValue := counterValue / agingFactor
				aged |= agedValue << shiftAmount
			}

			if atomic.CompareAndSwapUint64(&fbf.counters[i], current, aged) {
				break
			}
		}
	}

	atomic.StoreUint64(&fbf.totalIncrements, 0)
}

// FrequencyAdmissionFilter makes admission decisions based on access frequency
type frequencyAdmissionFilter struct {
	frequencyFilter *frequencyBloomFilter
	doorkeeper      *bloomFilter // Still keep simple doorkeeper for recent items

	resetInterval int64
	lastReset     int64

	admissionRequests uint64
	admissionGrants   uint64
}

// newFrequencyAdmissionFilter creates frequency-based admission filter
func newFrequencyAdmissionFilter(numCounters uint64, resetInterval time.Duration) *frequencyAdmissionFilter {
	return &frequencyAdmissionFilter{
		frequencyFilter: newFrequencyBloomFilter(numCounters),
		doorkeeper:      newBloomFilter(numCounters / 8), // Smaller doorkeeper
		resetInterval:   int64(resetInterval),
		lastReset:       time.Now().UnixNano(),
	}
}

// shouldAdmit makes frequency-based admission decision
func (faf *frequencyAdmissionFilter) shouldAdmit(keyHash uint64, victimFrequency uint64) bool {
	atomic.AddUint64(&faf.admissionRequests, 1)

	// Always admit if recently seen (doorkeeper)
	if faf.doorkeeper.contains(keyHash) {
		faf.doorkeeper.add(keyHash) // Refresh in doorkeeper
		atomic.AddUint64(&faf.admissionGrants, 1)
		return true
	}

	// Get frequency estimate for new item
	newItemFrequency := faf.frequencyFilter.increment(keyHash)

	faf.doorkeeper.add(keyHash)

	// Admission decision based on frequency comparison
	var shouldAdmit bool

	if newItemFrequency >= frequencyThreshold {
		// High frequency items get guaranteed admission
		shouldAdmit = true
	} else if newItemFrequency > victimFrequency {
		// Admit if frequency is higher than victim
		shouldAdmit = true
	} else if newItemFrequency == victimFrequency && newItemFrequency > 0 {
		// Tie-breaker: admit 50% of equal frequency items
		shouldAdmit = (keyHash % 2) == 0
	} else {
		// Low frequency new item vs higher frequency victim
		// Give small chance based on base admission rate
		shouldAdmit = (keyHash % 100) < uint64(baseAdmissionRate-int(victimFrequency*10))
	}

	now := time.Now().UnixNano()
	if now-faf.lastReset > faf.resetInterval {
		faf.reset(now)
	}

	if shouldAdmit {
		atomic.AddUint64(&faf.admissionGrants, 1)
	}

	return shouldAdmit
}

// getFrequencyEstimate returns frequency estimate for a key
func (faf *frequencyAdmissionFilter) getFrequencyEstimate(keyHash uint64) uint64 {
	return faf.frequencyFilter.estimateFrequency(keyHash)
}

// reset clears doorkeeper and resets statistics
func (faf *frequencyAdmissionFilter) reset(now int64) {
	faf.doorkeeper.reset()
	faf.lastReset = now
}

// getStats returns admission statistics
func (faf *frequencyAdmissionFilter) getStats() (requests, grants uint64, admissionRate float64) {
	requests = atomic.LoadUint64(&faf.admissionRequests)
	grants = atomic.LoadUint64(&faf.admissionGrants)

	if requests > 0 {
		admissionRate = float64(grants) / float64(requests)
	}

	return requests, grants, admissionRate
}
