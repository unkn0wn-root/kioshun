package kioshun

import (
	"github.com/unkn0wn-root/kioshun/internal/keyhash"
	"github.com/unkn0wn-root/kioshun/internal/mathx"
)

// ghostQueue is a fixed-size FIFO of recently evicted key hashes. A hit tells
// SieveTinyLFU that an item may have been evicted too soon.
//
// A hash table maps each key hash to its ring position. Value 0 means an empty
// table slot, so positions are stored plus one. Membership comes from this table,
// which allows the ring itself to store key hash 0.
type ghostQueue struct {
	entries []uint64
	slots   []uint32 // ring index plus one; zero means empty
	mask    uint64
	next    int
	live    int
}

func newGhostQueue(n int) ghostQueue {
	if n <= 0 {
		return ghostQueue{}
	}

	m := max(mathx.NextPowerOf2(n*2), 8)
	return ghostQueue{
		entries: make([]uint64, n),
		slots:   make([]uint32, m),
		mask:    uint64(m - 1),
	}
}

// probeStart mixes the hash because keys in one shard share their low bits.
func (g *ghostQueue) probeStart(h uint64) uint64 {
	return keyhash.Avalanche(h) & g.mask
}

// idxFind returns the matching table slot, or the first empty slot on a miss.
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

func (g *ghostQueue) idxInsert(h uint64, ringIdx int) {
	pos := g.probeStart(h)
	for g.slots[pos] != 0 {
		pos = (pos + 1) & g.mask
	}
	g.slots[pos] = uint32(ringIdx) + 1
}

// idxDeleteAt rebuilds the rest of the probe cluster so the new hole does not
// make later entries unreachable.
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

// add records a key hash once and overwrites the oldest ring slot when full.
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

func (g *ghostQueue) clear() {
	clear(g.entries)
	clear(g.slots)
	g.next = 0
	g.live = 0
}
