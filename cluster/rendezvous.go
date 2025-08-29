package cluster

import (
	"sort"
	"sync/atomic"
)

type nodeMeta struct {
	ID     NodeID
	Addr   string
	weight uint64 // scaled 0..1_000_000
	salt   uint64 // per-node salt (pre-hashed ID)
}

func (n *nodeMeta) Weight() float64 {
	return float64(atomic.LoadUint64(&n.weight)) / 1_000_000.0
}

type ring struct {
	nodes []*nodeMeta
	rf    int
}

func newRing(rf int) *ring { return &ring{rf: rf} }

// mix64: fast 64-bit mixer (SplitMix64 finalizer).
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

func (r *ring) ownersFromKeyHash(keyHash uint64) []*nodeMeta {
	// compute rendezvous scores per node using a per-node salt and mix64.
	// weighted ordering is achieved by scaling scores by node weight.
	type pair struct {
		s uint64
		n *nodeMeta
	}

	arr := make([]pair, 0, len(r.nodes))
	for _, nm := range r.nodes {
		score := mix64(keyHash ^ nm.salt)
		arr = append(arr, pair{s: score, n: nm})
	}

	sort.Slice(arr, func(i, j int) bool {
		wi := arr[i].n.Weight()
		wj := arr[j].n.Weight()
		return float64(arr[i].s)*wi > float64(arr[j].s)*wj
	})

	n := r.rf
	if n > len(arr) {
		n = len(arr)
	}

	out := make([]*nodeMeta, n)
	for i := 0; i < n; i++ {
		out[i] = arr[i].n
	}
	return out
}

func (r *ring) ownersTopNFromKeyHash(keyHash uint64, n int) []*nodeMeta {
	// variant that returns the top-N candidates for hot-key shadowing.
	type pair struct {
		s uint64
		n *nodeMeta
	}

	arr := make([]pair, 0, len(r.nodes))
	for _, nm := range r.nodes {
		score := mix64(keyHash ^ nm.salt)
		arr = append(arr, pair{s: score, n: nm})
	}

	sort.Slice(arr, func(i, j int) bool {
		wi := arr[i].n.Weight()
		wj := arr[j].n.Weight()
		return float64(arr[i].s)*wi > float64(arr[j].s)*wj
	})

	if n > len(arr) {
		n = len(arr)
	}

	out := make([]*nodeMeta, n)
	for i := 0; i < n; i++ {
		out[i] = arr[i].n
	}
	return out
}
