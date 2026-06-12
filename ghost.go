package kioshun

import "github.com/unkn0wn-root/kioshun/internal/keyhash"

// ghostQueue is a fixed size FIFO of recently evicted item fingerprints. A hit
// is evidence that the queue the item came from was too small for the current
// workload, so SieveTinyLFU readmits the item directly into main and may grow
// probation.
//
// A fingerprint is the full 64-bit key hash, which identifies an item as
// precisely as the cache can. The ring keeps fingerprints in FIFO order; an
// open-addressed index maps a fingerprint back to its ring slot so contains,
// add and remove are O(1). The index is a flat uint32 slice
// (probe position -> ring slot + 1, 0 meaning empty).
//
// All operations run on the single-consumer maintenance path under the shard
// write lock so the index needs no synchronization. Membership lives in the
// index, not the ring values, so fingerprint 0 (a zero integer key hashes to
// 0) is tracked like any other: a cleared ring slot reads as 0 but is
// distinguished from a tracked 0 by having no index entry point at it.
type ghostQueue struct {
	entries []uint64
	slots   []uint32 // probe pos -> ring index + 1 (0 = empty)
	mask    uint64
	next    int // FIFO cursor
	live    int // tracked fingerprints (index entries)
}

func newGhostQueue(n int) ghostQueue {
	if n <= 0 {
		return ghostQueue{}
	}

	m := max(nextPowerOf2(n*2), 8)
	return ghostQueue{
		entries: make([]uint64, n),
		slots:   make([]uint32, m),
		mask:    uint64(m - 1),
	}
}

// probeStart de-correlates the fingerprint before masking. Within a shard every
// stored hash shares the same low bits (those select the shard) so masking the
// raw hash would cluster every entry into one probe chain
func (g *ghostQueue) probeStart(h uint64) uint64 {
	return keyhash.Avalanche(h) & g.mask
}

// idxFind returns the index slot holding fingerprint h and whether it was found.
// On a miss the returned position is the empty slot that terminated the probe.
func (g *ghostQueue) idxFind(h uint64) (uint64, bool) {
	pos := g.probeStart(h)
	for {
		v := g.slots[pos]
		if v == 0 {
			return pos, false
		}
		if g.entries[v-1] == h {
			return pos, true
		}
		pos = (pos + 1) & g.mask
	}
}

// idxInsert records that fingerprint h lives at ring index ringIdx.
func (g *ghostQueue) idxInsert(h uint64, ringIdx int) {
	pos := g.probeStart(h)
	for g.slots[pos] != 0 {
		pos = (pos + 1) & g.mask
	}
	g.slots[pos] = uint32(ringIdx) + 1
}

// idxDeleteAt empties index slot pos then reinserts the rest of that probe
// cluster so no entry is stranded behind the new hole. Clusters are short at
// this load factor, and the rehash needs no tombstones.
func (g *ghostQueue) idxDeleteAt(pos uint64) {
	g.slots[pos] = 0
	next := (pos + 1) & g.mask
	for g.slots[next] != 0 {
		v := g.slots[next]
		g.slots[next] = 0
		g.idxInsert(g.entries[v-1], int(v-1))
		next = (next + 1) & g.mask
	}
}

func (g *ghostQueue) contains(h uint64) bool {
	if len(g.entries) == 0 {
		return false
	}
	_, ok := g.idxFind(h)
	return ok
}

// add records a fingerprint unless it is already present, overwriting the oldest
// ring slot when the ring is full. The overwritten fingerprint's index entry is
// dropped only when it still points at the slot being reused (it may have been
// removed on a ghost hit and re-added at a newer slot since).
func (g *ghostQueue) add(h uint64) {
	if len(g.entries) == 0 {
		return
	}
	if _, ok := g.idxFind(h); ok {
		return
	}

	old := g.entries[g.next]
	if pos, ok := g.idxFind(old); ok && g.slots[pos] == uint32(g.next)+1 {
		g.idxDeleteAt(pos)
		g.live--
	}

	g.entries[g.next] = h
	g.idxInsert(h, g.next)
	g.live++
	g.next++
	if g.next == len(g.entries) {
		g.next = 0
	}
}

// remove deletes a fingerprint from the index and clears its ring slot.
func (g *ghostQueue) remove(h uint64) bool {
	if len(g.entries) == 0 {
		return false
	}

	pos, ok := g.idxFind(h)
	if !ok {
		return false
	}

	ringIdx := g.slots[pos] - 1
	g.idxDeleteAt(pos)
	g.live--
	g.entries[ringIdx] = 0
	return true
}

func (g *ghostQueue) count() int { return g.live }

// clear preserves the allocated ring and index so policy reset does not force
// the next fill cycle to reallocate ghost storage.
func (g *ghostQueue) clear() {
	clear(g.entries)
	clear(g.slots)
	g.next = 0
	g.live = 0
}
