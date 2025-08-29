package cluster

import (
	"hash/maphash"
	"sync"
	"sync/atomic"
)

type cmSketch struct {
	rows  [][]uint32
	seeds []maphash.Seed
	width int
}

// newCMS constructs a Count-Min sketch with the given rows and width.
// Collisions are acceptable. It serves as a lightweight frequency estimator.
func newCMS(rows, width int) *cmSketch {
	s := &cmSketch{
		rows:  make([][]uint32, rows),
		seeds: make([]maphash.Seed, rows),
		width: width,
	}

	for i := 0; i < rows; i++ {
		s.rows[i] = make([]uint32, width)
		s.seeds[i] = maphash.MakeSeed()
	}
	return s
}

// add increments counters for the given key across all rows.
func (c *cmSketch) add(key []byte, n uint32) {
	for i := range c.rows {
		var h maphash.Hash
		h.SetSeed(c.seeds[i])
		h.Write(key)
		idx := h.Sum64() % uint64(c.width)
		c.rows[i][idx] += n
	}
}

type ssEntry struct {
	K string
	C uint64
}

type spaceSaving struct {
	mu  sync.Mutex
	cap int            // capacity k
	h   []*ssEntry     // min-heap by C
	idx map[string]int // key -> index in heap
}

// newSpaceSaving builds a Space-Saving top-k structure storing at most k keys.
func newSpaceSaving(k int) *spaceSaving {
	if k < 1 {
		k = 1
	}
	return &spaceSaving{
		cap: k,
		idx: make(map[string]int, k),
	}
}

// add updates the estimated frequency of key using Space-Saving rules. When
// full, it replaces the current minimum counter with the new key.
func (s *spaceSaving) add(k []byte, inc uint64) {
	key := string(k)
	s.mu.Lock()
	defer s.mu.Unlock()

	// existing key: increment and fix heap.
	if i, ok := s.idx[key]; ok {
		e := s.h[i]
		e.C = addSat64(e.C, inc)
		s.siftDown(i) // count increased; min-heap needs siftDown
		return
	}

	// room available: insert new node.
	if len(s.h) < s.cap {
		e := &ssEntry{K: key, C: inc}
		s.h = append(s.h, e)
		i := len(s.h) - 1
		s.idx[key] = i
		s.siftUp(i)
		return
	}

	// full: replace current min with (key, minC + inc). Reuse the min node (no alloc).
	minIdx := 0
	minNode := s.h[minIdx]
	oldKey := minNode.K
	delete(s.idx, oldKey)

	minNode.K = key
	minNode.C = addSat64(minNode.C, inc) // space-Saving rule: newC = minC + inc
	s.idx[key] = minIdx
	s.siftDown(minIdx)
}

// export returns the current top-k keys and approximate counts.
func (s *spaceSaving) export() []HotKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]HotKey, 0, len(s.h))
	for _, e := range s.h {
		out = append(out, HotKey{K: []byte(e.K), C: e.C})
	}
	return out
}

func (s *spaceSaving) siftUp(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if s.h[p].C <= s.h[i].C {
			break
		}
		s.swap(i, p)
		i = p
	}
}

func (s *spaceSaving) siftDown(i int) {
	n := len(s.h)
	for {
		l := 2*i + 1
		if l >= n {
			return
		}

		small := l
		r := l + 1
		if r < n && s.h[r].C < s.h[l].C {
			small = r
		}

		if s.h[i].C <= s.h[small].C {
			return
		}
		s.swap(i, small)
		i = small
	}
}

func (s *spaceSaving) swap(i, j int) {
	s.h[i], s.h[j] = s.h[j], s.h[i]
	s.idx[s.h[i].K] = i
	s.idx[s.h[j].K] = j
}

func addSat64(a, b uint64) uint64 {
	c := a + b
	if c < a {
		return ^uint64(0)
	}
	return c
}

type heat struct {
	cms     *cmSketch
	ss      *spaceSaving
	sampleN uint32
	ctr     uint32
}

// newHeat ties CM-sketch and Space-Saving together and supports sampling to
// reduce overhead under very high request rates.
func newHeat(rows, width, k, sampleN int) *heat {
	return &heat{
		cms:     newCMS(rows, width),
		ss:      newSpaceSaving(k),
		sampleN: uint32(sampleN),
	}
}

// sample records a key access at a reduced rate (1/sampleN). When sampleN=1,
// every key is recorded.
func (h *heat) sample(key []byte) {
	if h.sampleN <= 1 {
		h.cms.add(key, 1)
		h.ss.add(key, 1)
		return
	}
	if atomic.AddUint32(&h.ctr, 1)%h.sampleN == 0 {
		h.cms.add(key, 1)
		h.ss.add(key, 1)
	}
}

func (h *heat) exportTopK() []HotKey { return h.ss.export() }
