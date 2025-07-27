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

	bloomHashFunctions = 3  // 3 hashes with 10 bits/item gives ~1% false positive rate
	bitsPerWord        = 64 // Bits per uint64 for bit array indexing

)

const (
	defaultAdmissionRate = 80 // Start optimistic
	minAdmissionRate     = 50 // Floor to prevent total rejection
	admissionRateStep    = 10 // Adjustment step size

	scanDetectionWindow    = 100 // Requests before checking pattern
	scanThreshold          = 0.8 // 80% new items indicates scan
	normalPatternThreshold = 0.5 // 50% new items indicates normal access
)

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

// hash1 computes the first hash using xxHash64 mixing
func (bf *bloomFilter) hash1(hash uint64) uint64 {
	h := hash
	h ^= h >> 33
	h *= bloomMixPrime1
	h ^= h >> 29
	h *= bloomMixPrime2
	h ^= h >> 32
	return h & bf.mask
}

// hash2 computes the second hash with bit rotation
func (bf *bloomFilter) hash2(hash uint64) uint64 {
	h := bits.RotateLeft64(hash, 17)
	h ^= h >> 33
	h *= bloomMixPrime1
	h ^= h >> 29
	h *= bloomMixPrime2
	h ^= h >> 32
	return h & bf.mask
}

// hash3 computes the third hash with different rotation
func (bf *bloomFilter) hash3(hash uint64) uint64 {
	h := bits.RotateLeft64(hash, 31)
	h ^= h >> 33
	h *= bloomMixPrime1
	h ^= h >> 29
	h *= bloomMixPrime2
	h ^= h >> 32
	return h & bf.mask
}

// add marks a key as present in the bloom filter
func (bf *bloomFilter) add(keyHash uint64) {
	h1 := bf.hash1(keyHash)
	h2 := bf.hash2(keyHash)
	h3 := bf.hash3(keyHash)

	// Set bits at all three hash positions
	bf.bits[h1/bitsPerWord] |= 1 << (h1 % bitsPerWord)
	bf.bits[h2/bitsPerWord] |= 1 << (h2 % bitsPerWord)
	bf.bits[h3/bitsPerWord] |= 1 << (h3 % bitsPerWord)
}

// contains checks if a key might be in the set
func (bf *bloomFilter) contains(keyHash uint64) bool {
	h1 := bf.hash1(keyHash)
	h2 := bf.hash2(keyHash)
	h3 := bf.hash3(keyHash)

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

// admissionFilter controls cache admission to prevent pollution from scans.
// Uses a bloom filter to track recently seen keys and adaptive admission rates.
type admissionFilter struct {
	doorkeeper    *bloomFilter // Tracks recently seen keys
	resetInterval int64        // Nanoseconds between resets
	lastReset     int64

	admissionRate  uint64 // Current admission percentage (50-80)
	recentRequests uint64 // Total requests in current window
	recentNewItems uint64 // New items in current window

	scanDetected bool // Currently in scan mode
}

// newAdmissionFilter creates an admission filter with specified capacity
func newAdmissionFilter(size uint64, resetInterval time.Duration) *admissionFilter {
	return &admissionFilter{
		doorkeeper:    newBloomFilter(size),
		resetInterval: int64(resetInterval),
		lastReset:     time.Now().UnixNano(),
		admissionRate: defaultAdmissionRate,
	}
}

// shouldAdmit decides whether to admit a new cache entry.
// Always admits previously seen items, probabilistically admits new items.
func (af *admissionFilter) shouldAdmit(keyHash uint64) bool {
	// Fast path: always admit recently seen items
	if af.doorkeeper.contains(keyHash) {
		af.recentRequests++
		af.checkScanPattern()
		return true
	}

	// Track new item
	af.doorkeeper.add(keyHash)
	af.recentRequests++
	af.recentNewItems++

	// Check for scan patterns periodically
	af.checkScanPattern()

	// Reset periodically to prevent saturation
	now := time.Now().UnixNano()
	if now-af.lastReset > af.resetInterval {
		af.reset(now)
	}

	return (keyHash % 100) < af.admissionRate
}

// checkScanPattern detects sequential scan patterns and adjusts admission rate
func (af *admissionFilter) checkScanPattern() {
	if af.recentRequests < scanDetectionWindow {
		return
	}

	// Calculate ratio of new vs seen items
	newItemRatio := float64(af.recentNewItems) / float64(af.recentRequests)

	if newItemRatio > scanThreshold && !af.scanDetected {
		// High ratio of new items indicates scan
		af.scanDetected = true
		af.decreaseAdmissionRate()
	} else if newItemRatio < normalPatternThreshold && af.scanDetected {
		// Low ratio indicates normal access pattern
		af.scanDetected = false
		af.increaseAdmissionRate()
	}

	// Reset counters for next window
	af.recentRequests = 0
	af.recentNewItems = 0
}

// decreaseAdmissionRate reduces admission probability during scans
func (af *admissionFilter) decreaseAdmissionRate() {
	newRate := af.admissionRate - admissionRateStep
	if newRate < minAdmissionRate {
		newRate = minAdmissionRate
	}
	atomic.StoreUint64(&af.admissionRate, newRate)
}

// increaseAdmissionRate restores admission probability after scans
func (af *admissionFilter) increaseAdmissionRate() {
	newRate := af.admissionRate + admissionRateStep
	if newRate > defaultAdmissionRate {
		newRate = defaultAdmissionRate
	}
	atomic.StoreUint64(&af.admissionRate, newRate)
}

// reset clears the bloom filter and updates state
func (af *admissionFilter) reset(now int64) {
	af.doorkeeper.reset()
	af.lastReset = now

	// Gradually restore admission rate after reset
	if af.admissionRate < defaultAdmissionRate {
		af.increaseAdmissionRate()
	}

	af.scanDetected = false
	af.recentRequests = 0
	af.recentNewItems = 0
}

// GetStats returns current admission filter statistics
func (af *admissionFilter) GetStats() (admissionRate uint64, scanDetected bool) {
	return atomic.LoadUint64(&af.admissionRate), af.scanDetected
}
