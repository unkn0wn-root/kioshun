package kioshun

// ghostEntry stores a compact item identity. The tag is kept with the hash to
// reduce false ghost hits without retaining full keys after eviction.
type ghostEntry struct {
	hash uint64
	tag  uint16
}

// ghostQueue is a fixed size FIFO of recently evicted probation fingerprints.
// A hit is evidence that probation was too small for the current workload, so
// SieveTinyLFU readmits the item directly into main and may grow probation.
type ghostQueue struct {
	entries []ghostEntry
	index   map[ghostEntry]int
	next    int
}

// newGhostQueue returns a disabled zero value when n <= 0, letting callers use
// contains/add/remove without separate nil-state branches.
func newGhostQueue(n int) ghostQueue {
	if n <= 0 {
		return ghostQueue{}
	}

	return ghostQueue{
		entries: make([]ghostEntry, n),
		index:   make(map[ghostEntry]int, n),
	}
}

func (g *ghostQueue) contains(h uint64, t uint16) bool {
	if g.index == nil {
		return false
	}

	_, ok := g.index[ghostEntry{hash: h, tag: t}]
	return ok
}

// add records a fingerprint unless it is already present.
// The ring and index are kept in sync by deleting the overwritten entry only
// when the index still points at the slot being reused.
func (g *ghostQueue) add(h uint64, t uint16) {
	if len(g.entries) == 0 {
		return
	}

	e := ghostEntry{hash: h, tag: t}
	if g.index == nil {
		g.index = make(map[ghostEntry]int, len(g.entries))
	}
	if _, ok := g.index[e]; ok {
		return
	}

	old := g.entries[g.next]
	if i, ok := g.index[old]; ok && i == g.next {
		delete(g.index, old)
	}

	g.entries[g.next] = e
	g.index[e] = g.next
	g.next = (g.next + 1) % len(g.entries)
}

// remove deletes a fingerprint from the index and clears its slot if the slot
// still contains that exact entry. The FIFO cursor is not rewound.
func (g *ghostQueue) remove(h uint64, t uint16) bool {
	if g.index == nil {
		return false
	}

	e := ghostEntry{hash: h, tag: t}
	i, ok := g.index[e]
	if !ok {
		return false
	}

	delete(g.index, e)
	if i >= 0 && i < len(g.entries) && g.entries[i] == e {
		g.entries[i] = ghostEntry{}
	}
	return true
}

// clear preserves the allocated ring and index so policy reset does not force
// the next fill cycle to reallocate ghost storage.
func (g *ghostQueue) clear() {
	clear(g.entries)
	clear(g.index)
	g.next = 0
}
