package cache

import "fmt"

// hasher provides type-specific hash functions for cache keys.
type hasher[K comparable] struct{}

// newHasher creates a new hasher instance for the specified key type.
// The hasher is stateless and can be reused across multiple operations.
func newHasher[K comparable]() hasher[K] {
	return hasher[K]{}
}

// hash returns a 64-bit hash value for the given key based on its type.
// String keys are processed using xxHash64, integer types use avalanche mixing,
// and other comparable types are converted to strings before hashing.
func (h hasher[K]) hash(key K) uint64 {
	switch k := any(key).(type) {
	case string:
		return h.hashString(k)
	case int:
		return h.hashInteger(uint64(k))
	case int32:
		return h.hashInteger(uint64(k))
	case int64:
		return h.hashInteger(uint64(k))
	case uint:
		return h.hashInteger(uint64(k))
	case uint32:
		return h.hashInteger(uint64(k))
	case uint64:
		return h.hashInteger(k)
	default:
		return h.hashString(fmt.Sprintf("%v", k))
	}
}

// hashString computes a 64-bit hash for string keys.
// Short strings (≤8 bytes) use FNV-1a, while longer use xxHash64
func (h hasher[K]) hashString(s string) uint64 {
	if len(s) <= 8 {
		return fnvHash64(s)
	}
	return xxHash64(s)
}

// hashInteger computes a 64-bit hash for integer keys using xxHash avalanche mixing.
func (h hasher[K]) hashInteger(value uint64) uint64 {
	value ^= value >> 33
	value *= prime64_2
	value ^= value >> 29
	value *= prime64_3
	value ^= value >> 32
	return value
}
