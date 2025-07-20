package cache

import "fmt"

// hasher provides type-specific hash functions for cache keys.
// String keys use FNV-1a, integers use multiplicative hashing,
// and other types fall back to string conversion.
type hasher[K comparable] struct{}

// newHasher creates a new hasher instance for the specified key type.
func newHasher[K comparable]() hasher[K] {
	return hasher[K]{}
}

// hash returns a hash value for the given key based on its type.
func (h hasher[K]) hash(key K) uint64 {
	switch k := any(key).(type) {
	case string:
		return h.hashString(k) // FNV-1a for strings
	case int:
		return h.hashInteger(uint64(k), gratio32)
	case int32:
		return h.hashInteger(uint64(k), gratio32)
	case int64:
		return h.hashInteger(uint64(k), gratio64)
	case uint:
		return h.hashInteger(uint64(k), gratio32)
	case uint32:
		return h.hashInteger(uint64(k), gratio32)
	case uint64:
		return h.hashInteger(k, gratio64)
	default:
		return h.hashString(fmt.Sprintf("%v", k))
	}
}

// hashString computes FNV-1a hash for string keys.
func (h hasher[K]) hashString(s string) uint64 {
	return fnvHash64(s)
}

// hashInteger computes multiplicative hash for integer keys.
func (h hasher[K]) hashInteger(value uint64, ratio uint64) uint64 {
	return value * ratio
}
