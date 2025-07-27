package cache

import (
	"encoding/binary"
	"math/bits"
)

// xxHash64 algorithm constants
const (
	// Prime constants - large odd primes for multiplication mixing
	prime64_1 = 0x9E3779B185EBCA87 // Primary mixing prime
	prime64_2 = 0xC2B2AE3D27D4EB4F // 2-th prime for different bit patterns
	prime64_3 = 0x165667B19E3779F9 // 3-th prime for avalanche mixing
	prime64_4 = 0x85EBCA77C2B2AE63 // 4-th prime for merge operations
	prime64_5 = 0x27D4EB2F165667C5 // 5-th prime for small input processing

	// Seeds for accumulator registers (derived from primes)
	seed64_1 = 0x60EA27EEADC0B5D6 // v1 initial state
	seed64_4 = 0x61C8864E7A143579 // v4 initial state (v2=prime64_2, v3=0)

	largeInputThreshold = 32 // Switches to 4-accumulator mode for inputs ≥32 bytes

	// Rotation amounts
	roundRotation = 31 // Primary mixing rotation in accumulator rounds
	mergeRotation = 27 // Merge phase rotation for 8-byte chunks
	smallRotation = 23 // Finalization rotation for 4-byte chunks
	tinyRotation  = 11 // Single-byte processing rotation

	// Multi-stage avalanche XOR-shifts eliminate linear deps.
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
// Small inputs skip the accumulator phase and go directly to finalization.
func xxHash64(input string) uint64 {
	data := []byte(input)
	length := len(data)

	var h64 uint64
	if length >= largeInputThreshold {
		h64 = xxHash64Large(data, uint64(length))
	} else {
		h64 = prime64_5 + uint64(length)
		h64 = xxHash64Small(data, h64)
	}

	return xxHash64Avalanche(h64)
}

// xxHash64Large processes input data ≥32 bytes using 4-accumulator.
func xxHash64Large(data []byte, length uint64) uint64 {
	// Initialize 4 accumulators with different seeds to avoid correlation
	v1 := uint64(seed64_1)
	v2 := uint64(prime64_2)
	v3 := uint64(0)
	v4 := uint64(seed64_4)

	// 4 lanes × 8 bytes each
	for len(data) >= largeInputThreshold {
		v1 = xxHash64Round(v1, binary.LittleEndian.Uint64(data[0:8]))
		v2 = xxHash64Round(v2, binary.LittleEndian.Uint64(data[8:16]))
		v3 = xxHash64Round(v3, binary.LittleEndian.Uint64(data[16:24]))
		v4 = xxHash64Round(v4, binary.LittleEndian.Uint64(data[24:32]))
		data = data[largeInputThreshold:]
	}

	// Combine accumulators with different rotations to mix their states
	h64 := bits.RotateLeft64(v1, v1Rotation) + bits.RotateLeft64(v2, v2Rotation) +
		bits.RotateLeft64(v3, v3Rotation) + bits.RotateLeft64(v4, v4Rotation)

	// Merge each accumulator to eliminate state correlation
	h64 = xxHash64MergeRound(h64, v1)
	h64 = xxHash64MergeRound(h64, v2)
	h64 = xxHash64MergeRound(h64, v3)
	h64 = xxHash64MergeRound(h64, v4)

	h64 += length
	return xxHash64Finalize(data, h64)
}

func xxHash64Small(data []byte, h64 uint64) uint64 {
	return xxHash64Finalize(data, h64)
}

// xxHash64Round performs a single round of the xxHash algorithm.
// multiply-add → rotate → multiply creates strong bit mixing.
func xxHash64Round(acc, input uint64) uint64 {
	acc += input * prime64_2
	acc = bits.RotateLeft64(acc, roundRotation)
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
		k1 = xxHash64Round(0, k1)
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
		h64 ^= k1 * prime64_5
		h64 = bits.RotateLeft64(h64, tinyRotation) * prime64_1
		data = data[1:]
	}

	return h64
}

// xxHash64Avalanche applies final mixing to eliminate bias.
// The XOR-shift sequence ensures small input changes cause large output changes.
func xxHash64Avalanche(h64 uint64) uint64 {
	h64 ^= h64 >> avalancheShift1
	h64 *= prime64_2
	h64 ^= h64 >> avalancheShift2
	h64 *= prime64_3
	h64 ^= h64 >> avalancheShift3
	return h64
}
