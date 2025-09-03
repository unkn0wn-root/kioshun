package cluster

import "math"

const (
	weightMax = 1_000_000
	memRef    = 8 << 30 // 8 GiB reference for normalization
)

// computeWeight converts node load into a rendezvous weight (1..1_000_000).
// Higher free memory and lower CPU/evictions/size produce larger weights.
func computeWeight(load NodeLoad) uint64 {
	var fm float64 = 0.5
	if load.FreeMemBytes > 0 {
		fm = clamp01(float64(load.FreeMemBytes) / float64(memRef))
	}

	cpu := 0.5
	if load.CPUu16 > 0 {
		cpu = 1.0 - clamp(float64(load.CPUu16)/10000.0)
	}

	ev := 1.0 / (1.0 + float64(load.Evictions))
	sz := 1.0 / (1.0 + clamp(float64(load.Size)/1_000_000.0))
	s := fm*0.35 + cpu*0.35 + ev*0.2 + sz*0.1
	w := uint64(s * weightMax)
	if w < 1 {
		w = 1
	}
	return w
}
func clamp(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}
