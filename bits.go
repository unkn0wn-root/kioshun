package kioshun

import "math/bits"

func nextPowerOf2(n int) int {
	if n <= 1 {
		return 1
	}
	return 1 << bits.Len(uint(n-1))
}

// prevPowerOf2 returns the largest power of two not exceeding n; n must be >= 1.
func prevPowerOf2(n int64) int64 {
	return 1 << (bits.Len64(uint64(n)) - 1)
}
