package kioshun

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"reflect"
	"unsafe"
)

// xxHash64 seed=0 implementation tuned for cache hot paths.
const (
	prime64_1 = 0x9E3779B185EBCA87
	prime64_2 = 0xC2B2AE3D27D4EB4F
	prime64_3 = 0x165667B19E3779F9
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

	// xor-shifts (order matters).
	avalancheShift1 = 33
	avalancheShift2 = 29
	avalancheShift3 = 32

	// lane-combine rotations
	v1Rotation = 1
	v2Rotation = 7
	v3Rotation = 12
	v4Rotation = 18

	ghostTagSalt = 0x9ddfea08eb382d69
)

// <=8B uses FNV-1a, larger strings use xxHash64.
const (
	stringByteLength = 8
)

// hashableInteger is every key kind we hash by reading its integer
// representation directly. newHasher selects T from reflect.Kind so the
// unsafe read matches K's integer representation.
type hashableInteger interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr
}

type hasher[K comparable] struct {
	sum func(K) uint64
}

func newHasher[K comparable]() hasher[K] {
	var zero K
	if typ := reflect.TypeOf(zero); typ != nil {
		switch typ.Kind() {
		case reflect.String:
			return hasher[K]{sum: hashStringKey[K]}
		case reflect.Int:
			return hasher[K]{sum: hashIntKey[K, int]}
		case reflect.Int8:
			return hasher[K]{sum: hashIntKey[K, int8]}
		case reflect.Int16:
			return hasher[K]{sum: hashIntKey[K, int16]}
		case reflect.Int32:
			return hasher[K]{sum: hashIntKey[K, int32]}
		case reflect.Int64:
			return hasher[K]{sum: hashIntKey[K, int64]}
		case reflect.Uint:
			return hasher[K]{sum: hashIntKey[K, uint]}
		case reflect.Uint8:
			return hasher[K]{sum: hashIntKey[K, uint8]}
		case reflect.Uint16:
			return hasher[K]{sum: hashIntKey[K, uint16]}
		case reflect.Uint32:
			return hasher[K]{sum: hashIntKey[K, uint32]}
		case reflect.Uint64:
			return hasher[K]{sum: hashIntKey[K, uint64]}
		case reflect.Uintptr:
			return hasher[K]{sum: hashIntKey[K, uintptr]}
		}
	}

	return hasher[K]{sum: hashFallbackKey[K]}
}

func (h hasher[K]) hash(key K) uint64 {
	return h.sum(key)
}

func (h hasher[K]) tag(key K) uint16 {
	return tagFromHash(h.hash(key))
}

func tagFromHash(hash uint64) uint16 {
	tag := uint16(xxHash64Avalanche(hash^ghostTagSalt) >> 48)
	if tag == 0 {
		return 1
	}
	return tag
}

func hashStringKey[K comparable](key K) uint64 {
	return hashString(*(*string)(unsafe.Pointer(&key)))
}

func hashIntKey[K comparable, T hashableInteger](key K) uint64 {
	return xxHash64Avalanche(uint64(*(*T)(unsafe.Pointer(&key))))
}

func hashFallbackKey[K comparable](key K) uint64 {
	return hashString(fmt.Sprintf("%v", key))
}

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
		return xxHash64Avalanche(prime64_5)
	}
	data := unsafe.Slice(unsafe.StringData(input), length)

	var h64 uint64
	if length >= largeInputThreshold {
		h64 = xxHash64Large(data, uint64(length))
	} else {
		h64 = prime64_5 + uint64(length) // small-input init
		h64 = xxHash64Small(data, h64)
	}

	return xxHash64Avalanche(h64)
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

func xxHash64Small(data []byte, h64 uint64) uint64 {
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

// do the final xor-shift/multiply chain to enforce avalanche behavior.
func xxHash64Avalanche(h64 uint64) uint64 {
	h64 ^= h64 >> avalancheShift1
	h64 *= prime64_2
	h64 ^= h64 >> avalancheShift2
	h64 *= prime64_3
	h64 ^= h64 >> avalancheShift3
	return h64
}
