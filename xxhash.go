package cache

import (
	"encoding/binary"
	"math/bits"
)

// xxHash64 algorithm constants
const (
	prime64_1 = 0x9E3779B185EBCA87
	prime64_2 = 0xC2B2AE3D27D4EB4F
	prime64_3 = 0x165667B19E3779F9
	prime64_4 = 0x85EBCA77C2B2AE63
	prime64_5 = 0x27D4EB2F165667C5

	// init seeds for accumulator processing
	seed64_1 = 0x60EA27EEADC0B5D6
	seed64_4 = 0x61C8864E7A143579

	// minimum bytes for multi-accumulator
	largeInputThreshold = 32

	// Rotation amounts for hash mixing
	roundRotation = 31 // rotation in xxHash64Round
	mergeRotation = 27 // rotation in finalization 8-byte processing
	smallRotation = 23 // rotation 4-byte processing
	tinyRotation  = 11 // rotation 1-byte processing

	// Avalanche bit shifts for final mixing
	avalancheShift1 = 33 // first avalanche shift...
	avalancheShift2 = 29
	avalancheShift3 = 32

	// Accumulator rotation amounts in large input processing
	v1Rotation = 1 // v1 left rotation..
	v2Rotation = 7
	v3Rotation = 12
	v4Rotation = 18
)

// xxHash64 computes a 64-bit hash of the input string.
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

// xxHash64Large processes input data >= 32 bytes.
func xxHash64Large(data []byte, length uint64) uint64 {
	v1 := uint64(seed64_1)
	v2 := uint64(prime64_2)
	v3 := uint64(0)
	v4 := uint64(seed64_4)

	for len(data) >= largeInputThreshold {
		v1 = xxHash64Round(v1, binary.LittleEndian.Uint64(data[0:8]))
		v2 = xxHash64Round(v2, binary.LittleEndian.Uint64(data[8:16]))
		v3 = xxHash64Round(v3, binary.LittleEndian.Uint64(data[16:24]))
		v4 = xxHash64Round(v4, binary.LittleEndian.Uint64(data[24:32]))
		data = data[largeInputThreshold:]
	}

	h64 := bits.RotateLeft64(v1, v1Rotation) + bits.RotateLeft64(v2, v2Rotation) +
		bits.RotateLeft64(v3, v3Rotation) + bits.RotateLeft64(v4, v4Rotation)

	h64 = xxHash64MergeRound(h64, v1)
	h64 = xxHash64MergeRound(h64, v2)
	h64 = xxHash64MergeRound(h64, v3)
	h64 = xxHash64MergeRound(h64, v4)

	h64 += length

	return xxHash64Finalize(data, h64)
}

// xxHash64Small processes input data < 32 bytes directly using finalization.
func xxHash64Small(data []byte, h64 uint64) uint64 {
	return xxHash64Finalize(data, h64)
}

// xxHash64Round performs a single round of the xxHash algorithm.
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
// Handles 8-byte, 4-byte, and single-byte chunks sequentially,
// ensuring all input data contributes to the final hash value.
func xxHash64Finalize(data []byte, h64 uint64) uint64 {
	for len(data) >= 8 {
		k1 := binary.LittleEndian.Uint64(data[0:8])
		k1 = xxHash64Round(0, k1)
		h64 ^= k1
		h64 = bits.RotateLeft64(h64, mergeRotation)*prime64_1 + prime64_4
		data = data[8:]
	}

	if len(data) >= 4 {
		k1 := uint64(binary.LittleEndian.Uint32(data[0:4]))
		h64 ^= k1 * prime64_1
		h64 = bits.RotateLeft64(h64, smallRotation)*prime64_2 + prime64_3
		data = data[4:]
	}

	for len(data) > 0 {
		k1 := uint64(data[0])
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
