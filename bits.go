package kioshun

import "math/bits"

func nextPowerOf2(n int) int {
	if n <= 1 {
		return 1
	}
	return 1 << bits.Len(uint(n-1))
}

func prevPowerOf2(n int64) int64 {
	return 1 << (bits.Len64(uint64(n)) - 1)
}
