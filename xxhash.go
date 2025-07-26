package cache

import (
	"encoding/binary"
	"math/bits"
)

// xxHash64 algorithm constants
const (
	// Prime constants
	prime64_1 = 0x9E3779B185EBCA87 // Large odd prime for multiplication mixing
	prime64_2 = 0xC2B2AE3D27D4EB4F // 2-th prime for different bit patterns
	prime64_3 = 0x165667B19E3779F9 // 3-th prime for avalanche mixing
	prime64_4 = 0x85EBCA77C2B2AE63 // 4-th prime for merge operations
	prime64_5 = 0x27D4EB2F165667C5 // 5-th prime for small input processing

	// Seeds for accumulator registers (derived from primes)
	seed64_1 = 0x60EA27EEADC0B5D6 // v1 initial state
	seed64_4 = 0x61C8864E7A143579 // v4 initial state (v2=prime64_2, v3=0)

	// Switches to 4-accumulator mode for inputs ≥32 bytes
	largeInputThreshold = 32

	// Rotation amounts
	roundRotation = 31 // Primary mixing rotation in accumulator rounds
	mergeRotation = 27 // Merge phase rotation for 8-byte chunks
	smallRotation = 23 // Finalization rotation for 4-byte chunks
	tinyRotation  = 11 // Single-byte processing rotation

	// Multi-stage avalanche shifts eliminate linear deps.
	avalancheShift1 = 33 // 1 XOR-shift destroys high-bit patterns
	avalancheShift2 = 29 // 2 shift affects middle bits
	avalancheShift3 = 32 // Final shift ensures low-bit mixing

	// Accumulator rotation offsets for parallel processing lanes
	v1Rotation = 1  // Minimal rotation for v1 accumulator
	v2Rotation = 7  // Small prime rotation for v2
	v3Rotation = 12 // Medium rotation for v3
	v4Rotation = 18 // Larger rotation for v4
)

// xxHash64 computes a 64-bit hash of the input string using algo branching.
// Large inputs (≥32 bytes) use 4-accumulator.
// Small inputs use direct finalization.
func xxHash64(input string) uint64 {
	data := []byte(input)
	length := len(data)

	var h64 uint64
	if length >= largeInputThreshold {
		// Multi-accumulator path: 32-byte blocks
		h64 = xxHash64Large(data, uint64(length))
	} else {
		// Direct: seed with length and prime
		h64 = prime64_5 + uint64(length)
		h64 = xxHash64Small(data, h64)
	}

	return xxHash64Avalanche(h64)
}

// xxHash64Large processes input data ≥32 bytes using 4-accumulator.
func xxHash64Large(data []byte, length uint64) uint64 {
	v1 := uint64(seed64_1)  // Custom seed
	v2 := uint64(prime64_2) // Prime constant
	v3 := uint64(0)         // Zero initialization
	v4 := uint64(seed64_4)  // Second custom seed

	// 4 lanes × 8 bytes each
	for len(data) >= largeInputThreshold {
		v1 = xxHash64Round(v1, binary.LittleEndian.Uint64(data[0:8]))
		v2 = xxHash64Round(v2, binary.LittleEndian.Uint64(data[8:16]))
		v3 = xxHash64Round(v3, binary.LittleEndian.Uint64(data[16:24]))
		v4 = xxHash64Round(v4, binary.LittleEndian.Uint64(data[24:32]))
		data = data[largeInputThreshold:]
	}

	// Combine 4 accumulators using rotations to mix state
	h64 := bits.RotateLeft64(v1, v1Rotation) + bits.RotateLeft64(v2, v2Rotation) +
		bits.RotateLeft64(v3, v3Rotation) + bits.RotateLeft64(v4, v4Rotation)

	// Merge each accumulator to eliminate state correlation
	h64 = xxHash64MergeRound(h64, v1)
	h64 = xxHash64MergeRound(h64, v2)
	h64 = xxHash64MergeRound(h64, v3)
	h64 = xxHash64MergeRound(h64, v4)

	// Include original length in final hash
	h64 += length

	// Process remaining bytes (< 32)
	return xxHash64Finalize(data, h64)
}

// xxHash64Small processes input data < 32 bytes directly using finalization.
func xxHash64Small(data []byte, h64 uint64) uint64 {
	return xxHash64Finalize(data, h64)
}

// xxHash64Round performs a single round of the xxHash algorithm.
// multiply-add → rotate → multiply creates strong bit mixing.
func xxHash64Round(acc, input uint64) uint64 {
	// Step 1: incorporate input with prime multiplication
	acc += input * prime64_2
	// Step 2: rotate for bit position mixing
	acc = bits.RotateLeft64(acc, roundRotation)
	// Step 3: final prime multiplication for avalanche
	acc *= prime64_1
	return acc
}

// xxHash64MergeRound merges an accumulator into the hash state during finalization.
// Ensures that all accumulated entropy is properly distributed in the final hash.
func xxHash64MergeRound(h64, val uint64) uint64 {
	val = xxHash64Round(0, val)
	h64 ^= val
	h64 = h64*prime64_1 + prime64_4
	return h64
}

// xxHash64Finalize processes any remaining bytes and applies final mixing.
// Uses different processing patterns for 8-byte, 4-byte, and 1-byte chunks
// to ensure all input bits contribute to the final hash value.
func xxHash64Finalize(data []byte, h64 uint64) uint64 {
	// 8-byte chunks
	for len(data) >= 8 {
		k1 := binary.LittleEndian.Uint64(data[0:8])
		// Apply round function to isolate chunk
		k1 = xxHash64Round(0, k1)
		// XOR and rotate-multiply for integration
		h64 ^= k1
		h64 = bits.RotateLeft64(h64, mergeRotation)*prime64_1 + prime64_4
		data = data[8:]
	}

	// 4-byte chunk
	if len(data) >= 4 {
		k1 := uint64(binary.LittleEndian.Uint32(data[0:4]))
		h64 ^= k1 * prime64_1
		h64 = bits.RotateLeft64(h64, smallRotation)*prime64_2 + prime64_3
		data = data[4:]
	}

	// Remaining individual bytes
	for len(data) > 0 {
		k1 := uint64(data[0])
		// Minimal mixing for single bytes
		h64 ^= k1 * prime64_5
		h64 = bits.RotateLeft64(h64, tinyRotation) * prime64_1
		data = data[1:]
	}

	return h64
}

// xxHash64Avalanche applies final mixing for avalanche properties.
// The sequence of XOR-shift and multiply operations eliminates any remaining
// bias and ensures that small input changes produce large output differences.
func xxHash64Avalanche(h64 uint64) uint64 {
	h64 ^= h64 >> avalancheShift1
	h64 *= prime64_2
	h64 ^= h64 >> avalancheShift2
	h64 *= prime64_3
	h64 ^= h64 >> avalancheShift3
	return h64
}
