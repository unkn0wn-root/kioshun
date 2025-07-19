package cache

import "fmt"

// hasher provides type-specific hash functions for cache keys
//
// The hasher implements hashing strategies for different key types:
// - String keys: FNV-1a hash (fast, good distribution)
// - Integer keys: Multiplicative hashing with golden ratio constants
// - Other types: String conversion fallback for compatibility
type hasher[K comparable] struct{}

// newHasher creates a new hasher instance for the specified key type
//
// The hasher is stateless and thread-safe, so a single instance
// can be shared across multiple goroutines safely.
func newHasher[K comparable]() hasher[K] {
	return hasher[K]{}
}

// hash implements type-specific hashing strategies
//
// Hash function selection by key type:
//
// String keys: FNV-1a hash
//
// Integer keys (int, int32, int64, uint, uint32, uint64): Multiplicative hashing
// - Uses golden ratio constants for excellent distribution
// - gratio32 = (φ - 1) * 2^32 for 32-bit integers
// - gratio64 = (φ - 1) * 2^64 for 64-bit integers
// - φ = (1 + √5) / 2 ≈ 1.618 (golden ratio)
//
// Other comparable types: String conversion fallback
// - Uses fmt.Sprintf to convert to string, then applies FNV-1a
// - Necessary for generics support with arbitrary comparable constraints
// - Handles structs, arrays, and other comparable types
func (h hasher[K]) hash(key K) uint64 {
	switch k := any(key).(type) {
	case string:
		return h.hashString(k) // FNV-1a for strings
	case int:
		return h.hashInteger(uint64(k), gratio32) // Golden ratio for int
	case int32:
		return h.hashInteger(uint64(k), gratio32) // Golden ratio for int32
	case int64:
		return h.hashInteger(uint64(k), gratio64) // Golden ratio for int64
	case uint:
		return h.hashInteger(uint64(k), gratio32) // Golden ratio for uint
	case uint32:
		return h.hashInteger(uint64(k), gratio32) // Golden ratio for uint32
	case uint64:
		return h.hashInteger(k, gratio64) // Golden ratio for uint64
	default:
		return h.hashString(fmt.Sprintf("%v", k)) // Fallback: convert to string
	}
}

// hashString computes FNV-1a hash for string keys
func (h hasher[K]) hashString(s string) uint64 {
	return fnvHash64(s)
}

// hashInteger computes multiplicative hash for integer keys
func (h hasher[K]) hashInteger(value uint64, ratio uint64) uint64 {
	return value * ratio // Multiplicative hashing with golden ratio
}
