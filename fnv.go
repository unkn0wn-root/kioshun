package cache

const (
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// fnvHash64 computes FNV-1a then XOR-folds the high and low 32-bit halves so
// short keys spread across 2^n shard masks.
func fnvHash64(s string) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	return h ^ (h >> 32)
}
