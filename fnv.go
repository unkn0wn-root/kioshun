package cache

const (
	// FNV-1a
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// fnvHash64 implements FNV-1a hash algorithm with XOR-folding.
//
// Standard FNV-1a algorithm:
// 1. Initialize hash with FNV offset basis
// 2. For each byte: XOR byte with current hash, then multiply by FNV prime
// 3. XOR-before-multiply order distinguishes FNV-1a from FNV-1
//
// Kioshun XOR-folding:
// - Combines upper and lower 32 bits via h ^ (h >> 32)
// - Better hash distribution for shard selection
// - Reduces clustering when using power-of-2 table sizes
func fnvHash64(s string) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	return h ^ (h >> 32)
}
