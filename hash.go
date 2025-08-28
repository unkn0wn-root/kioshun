package cache

import (
	"encoding/binary"
	"fmt"
	"math/bits"
)

// xxHash64 (seed=0) partial reimplementation focused on hot-path characteristics.
// Notes on design choices:
//   - We keep the exact xxHash64 mixing/avalanche structure so the output
//     distribution matches stock xxHash64 for the processed lanes.
//   - Little-endian loads (binary.LittleEndian) are used per spec.
//   - For short strings we bypass xxHash64 entirely (see hasher.hashString) because
//     FNV-1a has fewer multiplies/rotates, which wins on tiny payloads.
//   - Integers are passed through a reduced "avalanche-only" path to decorrelate
//     low-entropy values without the cost of lane processing.
const (
	// Standard xxHash64 primes
	prime64_1 = 0x9E3779B185EBCA87 // main mixing prime
	prime64_2 = 0xC2B2AE3D27D4EB4F // secondary prime (used in rounds/avalanche)
	prime64_3 = 0x165667B19E3779F9 // avalanche prime
	prime64_4 = 0x85EBCA77C2B2AE63 // merge/add prime
	prime64_5 = 0x27D4EB2F165667C5 // small-input prime

	// Precomputed seeds for seed=0 (exact xxHash64 initialization)
	// v1 = seed + prime64_1 + prime64_2
	// v2 = seed + prime64_2
	// v3 = seed + 0
	// v4 = seed - prime64_1
	seed64_1 = 0x60EA27EEADC0B5D6 // v1 initial (prime64_1 + prime64_2)
	seed64_4 = 0x61C8864E7A143579 // v4 initial (two's complement of prime64_1)

	// Size and rotation parameters per xxHash64 spec
	largeInputThreshold = 32 // switch to 4-lane (32B) processing when len ≥ 32

	roundRotation = 31 // rotation used inside per-lane round
	mergeRotation = 27 // rotation when folding 8B chunks in finalization
	smallRotation = 23 // rotation when folding 4B chunks
	tinyRotation  = 11 // rotation when folding trailing bytes

	// Final avalanche XOR-shifts; order matters (destroys patterns progressively).
	avalancheShift1 = 33
	avalancheShift2 = 29
	avalancheShift3 = 32

	// Rotations for combining v1..v4 before merge rounds (xxHash64 combine step).
	v1Rotation = 1
	v2Rotation = 7
	v3Rotation = 12
	v4Rotation = 18
)

// Heuristic for choosing hashing strategy for strings.
// For very short strings, FNV-1a tends to be faster on modern CPUs due to avoiding
// 64-bit multiplies/rotates in loops (and because payload fits in registers).
const (
	// Threshold for choosing between FNV and xxHash for strings.
	// ≤8 bytes → FNV-1a; >8 bytes → xxHash64
	stringByteLength = 8
)

// hasher provides type-specialized hashing without reflection.
// The instance is stateless and may be reused across goroutines.
type hasher[K comparable] struct{}

// newHasher creates a new hasher instance for the specified key type.
// No state is captured; returned value is a tiny value type.
func newHasher[K comparable]() hasher[K] {
	return hasher[K]{}
}

// hash returns a 64-bit hash for the given key.
// Policy:
//   - Built-in integer types go through xxHash64Avalanche to cheaply
//     decorrelate small integers (common in sharding/IDs).
//   - Strings choose FNV-1a vs xxHash64 based on length (see hashString).
//   - All other key types fall back to fmt.Sprintf("%v", k) then string hashing.
//     (This introduces an allocation; callers who care about zero-alloc should
//     prefer K=string/int/uint... or provide their own adapter.)
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
		// Fallback path: formatting incurs allocation; kept for generality.
		return h.hashString(fmt.Sprintf("%v", k))
	}
}

// hashString chooses the hashing backend based on length.
// Rationale:
//   - ≤8B: FNV-1a usually beats xxHash on tiny payloads due to less arithmetic.
//   - >8B: xxHash64 provides better distribution and throughput on longer inputs.
//
// Endianness not relevant here; FNV and xxHash treat the byte slice as-is.
func (h hasher[K]) hashString(s string) uint64 {
	if len(s) <= stringByteLength {
		return fnvHash64(s) // assumed provided elsewhere in the package
	}
	return xxHash64(s)
}

// xxHash64 computes xxHash64(seed=0) for an input string.
// Implementation detail: converting string → []byte allocates; acceptable for
// long strings where the hashing work dwarfs the allocation cost. If callers
// need zero-alloc hashing for large strings, they should pass []byte directly
// via a specialized path (not provided here by design).
//
// Branching:
//   - len ≥ 32: 4-lane processing (v1..v4) for bulk throughput.
//   - len <  32: small-input path that feeds directly into finalization.
func xxHash64(input string) uint64 {
	data := []byte(input)
	length := len(data)

	var h64 uint64
	if length >= largeInputThreshold {
		h64 = xxHash64Large(data, uint64(length))
	} else {
		// Per spec: initialize small-input state with prime64_5 + len.
		h64 = prime64_5 + uint64(length)
		h64 = xxHash64Small(data, h64)
	}

	return xxHash64Avalanche(h64)
}

// xxHash64Large processes data in 32-byte stripes using four accumulators.
// After block processing it combines accumulators with distinct rotations and
// merge rounds to decorrelate lanes, then hands remaining tail to finalization.
func xxHash64Large(data []byte, length uint64) uint64 {
	// Initialize accumulators for seed=0.
	v1 := uint64(seed64_1)  // prime64_1 + prime64_2
	v2 := uint64(prime64_2) // seed + prime64_2
	v3 := uint64(0)         // seed
	v4 := uint64(seed64_4)  // seed - prime64_1

	// Main loop: 32B per iteration, 8B per lane.
	for len(data) >= largeInputThreshold {
		v1 = xxHash64Round(v1, binary.LittleEndian.Uint64(data[0:8]))
		v2 = xxHash64Round(v2, binary.LittleEndian.Uint64(data[8:16]))
		v3 = xxHash64Round(v3, binary.LittleEndian.Uint64(data[16:24]))
		v4 = xxHash64Round(v4, binary.LittleEndian.Uint64(data[24:32]))
		data = data[largeInputThreshold:]
	}

	// Combine accumulators (distinct rotations reduce symmetry).
	h64 := bits.RotateLeft64(v1, v1Rotation) +
		bits.RotateLeft64(v2, v2Rotation) +
		bits.RotateLeft64(v3, v3Rotation) +
		bits.RotateLeft64(v4, v4Rotation)

	// Merge rounds fold each lane into the hash state to mix high/low bits.
	h64 = xxHash64MergeRound(h64, v1)
	h64 = xxHash64MergeRound(h64, v2)
	h64 = xxHash64MergeRound(h64, v3)
	h64 = xxHash64MergeRound(h64, v4)

	h64 += length
	return xxHash64Finalize(data, h64)
}

// xxHash64Small feeds the small remainder directly into finalization.
func xxHash64Small(data []byte, h64 uint64) uint64 {
	return xxHash64Finalize(data, h64)
}

// xxHash64Round is the per-lane step: (acc + input*prime2) → rot(31) → *prime1.
// The multiply-rotate-multiply pattern provides fast diffusion across bits.
func xxHash64Round(acc, input uint64) uint64 {
	acc += input * prime64_2
	acc = bits.RotateLeft64(acc, roundRotation)
	acc *= prime64_1
	return acc
}

// xxHash64MergeRound mixes one lane into the main hash during the combine phase.
// It runs the lane value through a round and then xors/adds with primes to avoid
// carry/rotation artifacts.
func xxHash64MergeRound(h64, val uint64) uint64 {
	val = xxHash64Round(0, val)
	h64 ^= val
	h64 = h64*prime64_1 + prime64_4
	return h64
}

// xxHash64Finalize folds the remaining tail bytes and applies final avalanche.
// Tail processing order: 8-byte chunks → 4-byte chunk → single bytes.
// Each step uses its own rotation/prime mix per spec to avoid structural bias.
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

	// Trailing bytes (len(data) ∈ [0..3])
	for len(data) > 0 {
		k1 := uint64(data[0])
		h64 ^= k1 * prime64_5
		h64 = bits.RotateLeft64(h64, tinyRotation) * prime64_1
		data = data[1:]
	}

	return h64
}

// xxHash64Avalanche is the final mixing that destroys regularities.
// The xor-shift/multiply/xor-shift/multiply/xor-shift sequence ensures that
// small input deltas cause widespread output changes (avalanche property).
func xxHash64Avalanche(h64 uint64) uint64 {
	h64 ^= h64 >> avalancheShift1
	h64 *= prime64_2
	h64 ^= h64 >> avalancheShift2
	h64 *= prime64_3
	h64 ^= h64 >> avalancheShift3
	return h64
}
