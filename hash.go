package cache

import "fmt"

const (
	// gratio32 is the golden ratio multiplier for 32-bit integers
	// (φ - 1) * 2^32, where φ = (1 + √5) / 2 (golden ratio)
	gratio32 = 0x9e3779b9

	// gratio64 is the same but for 64-bit integers
	// (φ - 1) * 2^64
	gratio64 = 0x9e3779b97f4a7c15

	// FNV-1a constants
	fnvOffset32 = 2166136261
	fnvPrime32  = 16777619
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// - Strings: FNV-1A
// - Integers: Uses multiplicative hashing with golden ratio constants
// - Other types: Converts to string then uses maphash - compatible with all comparable types
func (c *InMemoryCache[K, V]) hash(key K) uint64 {
	switch k := any(key).(type) {
	case string:
		return fnvHash64(k)
	case int:
		return uint64(k) * gratio32
	case int32:
		return uint64(k) * gratio32
	case int64:
		return uint64(k) * gratio64
	case uint:
		return uint64(k) * gratio32
	case uint32:
		return uint64(k) * gratio32
	case uint64:
		return k * gratio64
	default:
		// For other types, convert to string and hash
		// this is unavoidable for arbitrary comparable types
		return fnvHash64(fmt.Sprintf("%v", k))
	}
}

// getShard returns the appropriate shard for a given key
func (c *InMemoryCache[K, V]) getShard(key K) *shard[K, V] {
	hash := c.hash(key)
	return c.shards[hash&c.shardMask] // Use bitmask for fast modulo
}

// FNV-1a hash
func fnvHash64(s string) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i]) // XOR with byte
		h *= fnvPrime64   // Multiply by prime
	}
	return h ^ (h >> 32) // XOR-fold upper 32 bits with lower 32
}
