package cache

import (
	"encoding/binary"
	"fmt"
	"math/bits"
)

// xxHash64 (seed=0) tuned for cache hot paths:
// preserves spec mixing, uses FNV-1a for tiny strings, and applies avalanche-only for ints.
const (
	// xxHash64 primes (spec).
	prime64_1 = 0x9E3779B185EBCA87
	prime64_2 = 0xC2B2AE3D27D4EB4F
	prime64_3 = 0x165667B19E3779F9
	prime64_4 = 0x85EBCA77C2B2AE63
	prime64_5 = 0x27D4EB2F165667C5

	// Precomputed seeds for seed=0 (v1 and v4 initial values per spec).
	seed64_1 = 0x60EA27EEADC0B5D6 // prime64_1 + prime64_2
	seed64_4 = 0x61C8864E7A143579 // -prime64_1 (two's complement)

	// Size/rotation params (spec).
	largeInputThreshold = 32

	roundRotation = 31
	mergeRotation = 27
	smallRotation = 23
	tinyRotation  = 11

	// Avalanche xor-shifts (order matters).
	avalancheShift1 = 33
	avalancheShift2 = 29
	avalancheShift3 = 32

	// Lane-combine rotations (spec).
	v1Rotation = 1
	v2Rotation = 7
	v3Rotation = 12
	v4Rotation = 18
)

// Strategy heuristic for string keys: ≤8B → FNV-1a, >8B → xxHash64.
const (
	stringByteLength = 8
)

// hasher provides type-specialized hashing without reflection (stateless, goroutine-safe).
type hasher[K comparable] struct{}

// newHasher returns a tiny value-type hasher for K (no captured state).
func newHasher[K comparable]() hasher[K] {
	return hasher[K]{}
}

// hash routes by key type:
// ints → avalanche-only, strings → FNV/xxHash by length,
// others → formatted string then string hashing.
func (h hasher[K]) hash(key K) uint64 {
	switch k := any(key).(type) {
	case string:
		return h.hashString(k)
	case int:
		return xxHash64Avalanche(uint64(k))
	case int32:
		return xxHash64Avalanche(uint64(k))
	case int64:
		return xxHash64Avalanche(uint64(k))
	case uint:
		return xxHash64Avalanche(uint64(k))
	case uint32:
		return xxHash64Avalanche(uint64(k))
	case uint64:
		return xxHash64Avalanche(k)
	default:
		// Fallback allocates; prefer native K types to avoid it.
		return h.hashString(fmt.Sprintf("%v", k))
	}
}

// hashString selects FNV-1a for very short strings, xxHash64 otherwise.
func (h hasher[K]) hashString(s string) uint64 {
	if len(s) <= stringByteLength {
		return fnvHash64(s)
	}
	return xxHash64(s)
}

// xxHash64 computes xxHash64(seed=0) for a string;
// ≥32B uses 4-lane path, smaller uses small-input path, then avalanche.
func xxHash64(input string) uint64 {
	data := []byte(input)
	length := len(data)

	var h64 uint64
	if length >= largeInputThreshold {
		h64 = xxHash64Large(data, uint64(length))
	} else {
		h64 = prime64_5 + uint64(length) // small-input init
		h64 = xxHash64Small(data, h64)
	}

	return xxHash64Avalanche(h64)
}

// xxHash64Large processes 32B blocks with four accumulators, combines lanes, then finalizes the tail.
func xxHash64Large(data []byte, length uint64) uint64 {
	// Seed accumulators for seed=0.
	v1 := uint64(seed64_1)
	v2 := uint64(prime64_2)
	v3 := uint64(0)
	v4 := uint64(seed64_4)

	// 32B per iteration (8B per lane).
	for len(data) >= largeInputThreshold {
		v1 = xxHash64Round(v1, binary.LittleEndian.Uint64(data[0:8]))
		v2 = xxHash64Round(v2, binary.LittleEndian.Uint64(data[8:16]))
		v3 = xxHash64Round(v3, binary.LittleEndian.Uint64(data[16:24]))
		v4 = xxHash64Round(v4, binary.LittleEndian.Uint64(data[24:32]))
		data = data[largeInputThreshold:]
	}

	// Combine lanes with distinct rotations, then merge rounds.
	h64 := bits.RotateLeft64(v1, v1Rotation) +
		bits.RotateLeft64(v2, v2Rotation) +
		bits.RotateLeft64(v3, v3Rotation) +
		bits.RotateLeft64(v4, v4Rotation)

	h64 = xxHash64MergeRound(h64, v1)
	h64 = xxHash64MergeRound(h64, v2)
	h64 = xxHash64MergeRound(h64, v3)
	h64 = xxHash64MergeRound(h64, v4)

	h64 += length
	return xxHash64Finalize(data, h64)
}

// xxHash64Small forwards small inputs directly to finalization.
func xxHash64Small(data []byte, h64 uint64) uint64 {
	return xxHash64Finalize(data, h64)
}

// xxHash64Round is one per-lane round: (acc + input*prime2) → rot(31) → *prime1.
func xxHash64Round(acc, input uint64) uint64 {
	acc += input * prime64_2
	acc = bits.RotateLeft64(acc, roundRotation)
	acc *= prime64_1
	return acc
}

// xxHash64MergeRound folds a lane into the main hash during lane combination.
func xxHash64MergeRound(h64, val uint64) uint64 {
	val = xxHash64Round(0, val)
	h64 ^= val
	h64 = h64*prime64_1 + prime64_4
	return h64
}

// xxHash64Finalize folds tail bytes (8B → 4B → 1B) and applies the final avalanche.
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

	// Final 0..3 bytes.
	for len(data) > 0 {
		k1 := uint64(data[0])
		h64 ^= k1 * prime64_5
		h64 = bits.RotateLeft64(h64, tinyRotation) * prime64_1
		data = data[1:]
	}

	return h64
}

// xxHash64Avalanche performs the final xor-shift/multiply chain to enforce avalanche behavior.
func xxHash64Avalanche(h64 uint64) uint64 {
	h64 ^= h64 >> avalancheShift1
	h64 *= prime64_2
	h64 ^= h64 >> avalancheShift2
	h64 *= prime64_3
	h64 ^= h64 >> avalancheShift3
	return h64
}
