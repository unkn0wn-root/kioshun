//go:build kioshun_purego

package keyhash

import (
	"encoding/binary"
	"math/bits"
	"unsafe"
)

// This file is the pure Go string hasher used when the cache
// is built with -tags kioshun_purego. The default build instead links the Go
// runtime's memhash backend (see stringhash_runtime.go) so if you're
// not comfortable with using runtime memhash - use this instead.

const (
	prime64_1 = 0x9E3779B185EBCA87
	prime64_4 = 0x85EBCA77C2B2AE63
	prime64_5 = 0x27D4EB2F165667C5

	// precomputed seeds for seed=0
	seed64_1 = 0x60EA27EEADC0B5D6 // prime64_1 + prime64_2
	seed64_4 = 0x61C8864E7A143579 // -prime64_1 (two's complement)

	// size/rotation params
	largeInputThreshold = 32

	roundRotation = 31
	mergeRotation = 27
	smallRotation = 23
	tinyRotation  = 11

	// lane-combine rotations
	v1Rotation = 1
	v2Rotation = 7
	v3Rotation = 12
	v4Rotation = 18

	// <=8B uses FNV-1a, larger strings use xxHash64.
	stringByteLength = 8
)

// hashString is the pure Go counterpart to the memhash-based hashString in
// stringhash_runtime.go: FNV-1a for short keys, xxHash64 for longer ones.
func hashString(s string) uint64 {
	if len(s) <= stringByteLength {
		return fnvHash64(s)
	}
	return xxHash64(s)
}

// xxHash64 computes xxHash64(seed=0) for a string. Inputs >=32B use the 4-lane
// path; smaller inputs go straight to tail finalization.
// The string is viewed as a read-only byte slice without copying. Hashing only
// reads the bytes and never retains the slice past this call so aliasing the
// string's backing array is safe and avoids a per-key allocation on hot paths.
func xxHash64(input string) uint64 {
	length := len(input)
	if length == 0 {
		return Avalanche(prime64_5)
	}
	data := unsafe.Slice(unsafe.StringData(input), length)

	var h64 uint64
	if length >= largeInputThreshold {
		h64 = xxHash64Large(data, uint64(length))
	} else {
		h64 = xxHash64Finalize(data, prime64_5+uint64(length)) // small input init
	}

	return Avalanche(h64)
}

// xxHash64Large processes 32B blocks with four accumulators before finalizing the tail.
func xxHash64Large(data []byte, length uint64) uint64 {
	// seed accumulators for seed=0.
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

	// combine lanes with distinct rotations, then merge rounds.
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

// one per-lane round: (acc + input*prime2) -> rot(31) -> *prime1.
func xxHash64Round(acc, input uint64) uint64 {
	acc += input * prime64_2
	acc = bits.RotateLeft64(acc, roundRotation)
	acc *= prime64_1
	return acc
}

// folds a lane into the main hash during lane combination.
func xxHash64MergeRound(h64, val uint64) uint64 {
	val = xxHash64Round(0, val)
	h64 ^= val
	h64 = h64*prime64_1 + prime64_4
	return h64
}

// folds tail bytes and leaves avalanche to the caller.
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
