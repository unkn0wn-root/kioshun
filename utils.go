package cache

import "math/bits"

// nextPowerOf2 returns the next power of 2 greater than or equal to n.
// find the position of the highest set bit in (n-1), then shift 1 left.
// This works because:
// - For exact powers of 2: (n-1) has all lower bits set, bits.Len gives log2(n)
// - For non-powers: (n-1) has highest bit at floor(log2(n)), result is next power up
func nextPowerOf2(n int) int {
	if n <= 1 {
		return 1 // Handle edge cases: 0 → 1, 1 → 1
	}
	// 1 << bits.Len(n-1) gives next power of 2
	return 1 << bits.Len(uint(n-1))
}

// defaultPathExtractor is a placeholder that returns empty string.
// This default implementation assumes keys are hashed or don't contain URL paths.
// Users should provide custom extractors for pattern matching.
func defaultPathExtractor(key string) string {
	// Default: return empty string (no path extraction)
	// Custom extractors should parse key format to extract URL path component
	return ""
}
