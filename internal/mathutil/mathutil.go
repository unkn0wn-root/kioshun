package mathutil

import "math/bits"

// NextPowerOf2 returns the next power of 2 greater than or equal to n.
// For n <= 1, returns 1.
func NextPowerOf2(n int) int {
	if n <= 1 {
		return 1
	}
	return 1 << bits.Len(uint(n-1))
}
