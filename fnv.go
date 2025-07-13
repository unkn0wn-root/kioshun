package cache

// fnvHash64 implements a custom FNV-1a hash algorithm with additional XOR-folding
//
// Standard FNV-1a algorithm:
// 1. Initialize hash with FNV offset basis (14695981039346656037 for 64-bit)
// 2. For each byte in input: XOR byte with current hash, then multiply by FNV prime (1099511628211)
// 3. The XOR-before-multiply order distinguishes FNV-1a from FNV-1 (multiply-before-XOR)
//
// Our is custom with XOR-folding:
// - Final step: h ^ (h >> 32) combines upper and lower 32 bits
// - Improves distribution when hash is used in smaller address spaces
// - Reduces clustering with power-of-2 sizes
// - Quite nice for cache shard selection (uses bitmask on lower bits)
func fnvHash64(s string) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	return h ^ (h >> 32)
}
