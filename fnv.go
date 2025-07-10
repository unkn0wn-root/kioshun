package cache

// FNV-1a hash
func fnvHash64(s string) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i]) // XOR with byte
		h *= fnvPrime64   // Multiply by prime
	}
	return h ^ (h >> 32) // XOR-fold upper 32 bits with lower 32
}
