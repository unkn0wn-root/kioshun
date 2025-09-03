package cluster

import "testing"

func TestComputeWeightBounds(t *testing.T) {
	w := computeWeight(NodeLoad{})
	if w < 1 || w > weightMax {
		t.Fatalf("weight out of bounds: %d", w)
	}
}

func TestComputeWeightSensitivity(t *testing.T) {
	base := NodeLoad{Size: 1_000_000, Evictions: 0, FreeMemBytes: 4 << 30, CPUu16: 5000}
	w1 := computeWeight(base)

	// more free memory => higher weight
	w2 := computeWeight(NodeLoad{Size: base.Size, Evictions: base.Evictions, FreeMemBytes: 8 << 30, CPUu16: base.CPUu16})
	if w2 <= w1 {
		t.Fatalf("expected weight to increase with free memory: %d -> %d", w1, w2)
	}

	// higher CPU usage => lower weight
	w3 := computeWeight(NodeLoad{Size: base.Size, Evictions: base.Evictions, FreeMemBytes: base.FreeMemBytes, CPUu16: 9000})
	if w3 >= w1 {
		t.Fatalf("expected weight to decrease with CPU load: %d -> %d", w1, w3)
	}

	// more evictions => lower weight
	w4 := computeWeight(NodeLoad{Size: base.Size, Evictions: 100, FreeMemBytes: base.FreeMemBytes, CPUu16: base.CPUu16})
	if w4 >= w1 {
		t.Fatalf("expected weight to decrease with evictions: %d -> %d", w1, w4)
	}
}
