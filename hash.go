package cache

import "fmt"

// threshold for choosing between FNV and xxHash algorithms
const stringByteLength = 8

// hasher provides type-specific hash functions for cache keys.
type hasher[K comparable] struct{}

// newHasher creates a new hasher instance for the specified key type.
// The hasher is stateless and can be reused across multiple operations.
func newHasher[K comparable]() hasher[K] {
	return hasher[K]{}
}

// hash returns a 64-bit hash value for the given key based on its type.
// integers get avalanche mixing for distribution,
// strings use length-based algorithm selection, others fall back to string conversion.
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
		return h.hashString(fmt.Sprintf("%v", k))
	}
}

// hashString computes a 64-bit hash for string keys using length-based selection.
// Short strings (≤8 bytes): FNV-1a is faster due to simpler operations
// Long strings (>8 bytes): xxHash64 provides better collision resistance and parallelism
func (h hasher[K]) hashString(s string) uint64 {
	if len(s) <= stringByteLength {
		return fnvHash64(s)
	}
	return xxHash64(s)
}
