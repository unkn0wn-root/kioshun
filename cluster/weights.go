package cluster

// computeWeight converts node load into a rendezvous weight (1..1_000_000).
// Higher free memory and lower CPU/evictions/size produce larger weights.
func computeWeight(load NodeLoad) uint64 {
	var fm float64 = 0.5
	if load.FreeMemBytes > 0 {
		fm = clamp01(float64(load.FreeMemBytes) / float64(8<<30))
	}

	cpu := 0.5
	if load.CPUu16 > 0 {
		cpu = 1.0 - clamp01(float64(load.CPUu16)/10000.0)
	}

	ev := 1.0 / (1.0 + float64(load.Evictions))
	sz := 1.0 / (1.0 + clamp01(float64(load.Size)/1_000_000.0))
	s := fm*0.35 + cpu*0.35 + ev*0.2 + sz*0.1
	w := uint64(s * 1_000_000)
	if w < 1 {
		w = 1
	}
	return w
}
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
