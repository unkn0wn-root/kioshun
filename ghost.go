package kioshun

// ghostQueue is a fixed size FIFO of recently evicted item fingerprints. A hit
// is evidence that the queue the item came from was too small for the current
// workload, so SieveTinyLFU readmits the item directly into main and may grow
// probation.
//
// A fingerprint is the full 64-bit key hash, which identifies an item as
// precisely as the cache can. contains and add touch the ghost on every
// admission along the SIEVE eviction path so each entry is kept minimal
// a bare uint64 mapped to the ring slot that holds it.
type ghostQueue struct {
	entries []uint64       // FIFO ring of fingerprints; the index map owns membership
	index   map[uint64]int // fingerprint -> ring slot for membership
	next    int
}

// newGhostQueue returns a disabled zero value when n <= 0, letting callers use
// contains/add/remove without separate nil-state branches.
func newGhostQueue(n int) ghostQueue {
	if n <= 0 {
		return ghostQueue{}
	}

	return ghostQueue{
		entries: make([]uint64, n),
		index:   make(map[uint64]int, n),
	}
}

func (g *ghostQueue) contains(h uint64) bool {
	if g.index == nil {
		return false
	}

	_, ok := g.index[h]
	return ok
}

// add records a fingerprint unless it is already present.
// The ring and index are kept in sync by deleting the overwritten entry only
// when the index still points at the slot being reused (the entry may have been
// removed on a ghost hit and re-added at a newer slot since).
func (g *ghostQueue) add(h uint64) {
	if len(g.entries) == 0 {
		return
	}

	if _, ok := g.index[h]; ok {
		return
	}

	old := g.entries[g.next]
	if i, ok := g.index[old]; ok && i == g.next {
		delete(g.index, old)
	}

	g.entries[g.next] = h
	g.index[h] = g.next
	g.next = (g.next + 1) % len(g.entries)
}

// remove deletes a fingerprint from the index and clears its ring slot if the
// slot still holds it. The FIFO cursor is not rewound.
func (g *ghostQueue) remove(h uint64) bool {
	if g.index == nil {
		return false
	}

	i, ok := g.index[h]
	if !ok {
		return false
	}

	delete(g.index, h)
	if i >= 0 && i < len(g.entries) && g.entries[i] == h {
		g.entries[i] = 0
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
