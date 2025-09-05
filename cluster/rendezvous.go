package cluster

import (
	"math/bits"
	"sort"
	"sync/atomic"

	"github.com/cespare/xxhash/v2"
)

type nodeMeta struct {
	ID     NodeID
	Addr   string
	weight uint64 // scaled 0..1_000_000
	salt   uint64 // per-node salt (pre-hashed ID)
}

// newMeta initializes per-node rendezvous metadata with a default weight and
// a precomputed salt derived from the node ID.
func newMeta(id NodeID, addr string) *nodeMeta {
	return &nodeMeta{
		ID: id, Addr: addr,
		weight: 500_000,
		salt:   xxhash.Sum64String(string(id)),
	}
}

// Weight returns the current scaled weight as a [0,1] float.
func (n *nodeMeta) Weight() float64 {
	return float64(atomic.LoadUint64(&n.weight)) / 1_000_000.0
}

type ring struct {
	nodes []*nodeMeta
	rf    int
}

func newRing(rf int) *ring { return &ring{rf: rf} }

// ownersFromKeyHash returns the top rf owners for a 64-bit key hash using
// weighted rendezvous hashing. Node salt keeps per-node independence.
func (r *ring) ownersFromKeyHash(keyHash uint64) []*nodeMeta {
	type pair struct {
		s uint64 // rendezvous score
		w uint64 // scaled weight (0..1_000_000)
		n *nodeMeta
	}
	arr := make([]pair, 0, len(r.nodes))
	for _, nm := range r.nodes {
		arr = append(arr, pair{
			s: mix64(keyHash ^ nm.salt),
			w: atomic.LoadUint64(&nm.weight), // snapshot once
			n: nm,
		})
	}

	less := func(i, j int) bool {
		hi1, lo1 := bits.Mul64(arr[i].s, arr[i].w)
		hi2, lo2 := bits.Mul64(arr[j].s, arr[j].w)
		if hi1 != hi2 {
			return hi1 > hi2 // higher product first
		}
		if lo1 != lo2 {
			return lo1 > lo2
		}
		return arr[i].n.ID < arr[j].n.ID // tie-break
	}
	sort.Slice(arr, less)

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

// ownersTopNFromKeyHash returns the top N candidates by weighted rendezvous
// score. Used for hot-key shadowing beyond rf.
func (r *ring) ownersTopNFromKeyHash(keyHash uint64, n int) []*nodeMeta {
	// variant that returns the top-N candidates for hot-key shadowing.
	// Use the same integer 128-bit ranking as ownersFromKeyHash for
	// consistent ordering and tie-breaking.
	type pair struct {
		s uint64 // rendezvous score
		w uint64 // scaled weight (0..1_000_000)
		n *nodeMeta
	}

	arr := make([]pair, 0, len(r.nodes))
	for _, nm := range r.nodes {
		arr = append(arr, pair{
			s: mix64(keyHash ^ nm.salt),
			w: atomic.LoadUint64(&nm.weight),
			n: nm,
		})
	}

	less := func(i, j int) bool {
		hi1, lo1 := bits.Mul64(arr[i].s, arr[i].w)
		hi2, lo2 := bits.Mul64(arr[j].s, arr[j].w)
		if hi1 != hi2 {
			return hi1 > hi2
		}
		if lo1 != lo2 {
			return lo1 > lo2
		}
		return arr[i].n.ID < arr[j].n.ID
	}
	sort.Slice(arr, less)

	if n > len(arr) {
		n = len(arr)
	}

	out := make([]*nodeMeta, n)
	for i := 0; i < n; i++ {
		out[i] = arr[i].n
	}
	return out
}

// ownsHash reports whether selfID is among the top-rf owners for keyHash.
func (r *ring) ownsHash(selfID NodeID, keyHash uint64) bool {
	if len(r.nodes) == 0 || r.rf <= 0 {
		return false
	}
	top := r.rf
	if top > len(r.nodes) {
		top = len(r.nodes)
	}

	type slot struct {
		hi, lo uint64 // 128-bit product of (score * weight)
		n      *nodeMeta
	}
	best := make([]slot, 0, top)
	worst := 0

	worse := func(a slot, b slot) bool {
		if a.hi != b.hi {
			return a.hi < b.hi
		}
		if a.lo != b.lo {
			return a.lo < b.lo
		}
		return a.n.ID > b.n.ID
	}

	for _, nm := range r.nodes {
		s := mix64(keyHash ^ nm.salt)
		w := atomic.LoadUint64(&nm.weight)
		hi, lo := bits.Mul64(s, w)

		if len(best) < top {
			best = append(best, slot{hi: hi, lo: lo, n: nm})
			if len(best) == 1 || worse(best[len(best)-1], best[worst]) {
				worst = len(best) - 1
			}
			continue
		}

		if !worse(slot{hi, lo, nm}, best[worst]) {
			best[worst] = slot{hi: hi, lo: lo, n: nm}
			// recompute worst
			worst = 0
			for i := 1; i < len(best); i++ {
				if worse(best[i], best[worst]) {
					worst = i
				}
			}
		}
	}

	for _, sl := range best {
		if sl.n.ID == selfID {
			return true
		}
	}
	return false
}

// mix64: fast 64-bit mixer (SplitMix64 finalizer).
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

func (r *ring) idByAddr(addr string) (NodeID, bool) {
	for _, nm := range r.nodes {
		if nm.Addr == addr {
			return nm.ID, true
		}
	}
	return "", false
}
