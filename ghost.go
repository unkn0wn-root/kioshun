package kioshun

// ghostQueue is a fixed-size FIFO of recently evicted probation fingerprints
// (hash+tag). A hit against the ghost queue is evidence an item was evicted too
// early, so SieveTinyLFU readmits it straight into the main queue.
type ghostEntry struct {
	hash uint64
	tag  uint16
}

type ghostQueue struct {
	entries []ghostEntry
	index   map[ghostEntry]int
	next    int
}

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

func (g *ghostQueue) clear() {
	clear(g.entries)
	clear(g.index)
	g.next = 0
}
