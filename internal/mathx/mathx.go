package mathx

import "math/bits"

// NextPowerOf2 rounds n up to the nearest power of two (minimum 1).
func NextPowerOf2(n int) int {
	if n <= 1 {
		return 1
	}
	return 1 << bits.Len(uint(n-1))
}

// PrevPowerOf2 rounds n down to the nearest power of two.
func PrevPowerOf2(n int64) int64 {
	return 1 << (bits.Len64(uint64(n)) - 1)
}
