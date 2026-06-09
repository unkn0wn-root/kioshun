package kioshun

import (
	"math/rand"
	"testing"
)

type refGhost struct {
	entries []uint64
	index   map[uint64]int
	next    int
}

func newRefGhost(n int) refGhost {
	if n <= 0 {
		return refGhost{}
	}
	return refGhost{entries: make([]uint64, n), index: make(map[uint64]int, n)}
}

func (g *refGhost) contains(h uint64) bool {
	if g.index == nil {
		return false
	}
	_, ok := g.index[h]
	return ok
}

func (g *refGhost) add(h uint64) {
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

func (g *refGhost) remove(h uint64) bool {
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

func TestGhostBasic(t *testing.T) {
	g := newGhostQueue(4)

	if g.contains(10) {
		t.Fatal("empty ghost should not contain 10")
	}
	g.add(10)
	g.add(20)
	g.add(30)
	g.add(40)
	if !g.contains(10) || !g.contains(40) {
		t.Fatal("expected 10..40 present in a full cap-4 ring")
	}
	// dedup: re-adding a present fingerprint is a no-op (does not refresh recency).
	g.add(10)
	// fifth distinct insert evicts the oldest live fingerprint (10), not the
	// re-added one - the dedup did not advance the FIFO cursor.
	g.add(50)
	if g.contains(10) {
		t.Fatal("10 should have been FIFO-evicted")
	}
	if !g.contains(50) || !g.contains(40) || !g.contains(30) || !g.contains(20) {
		t.Fatal("20,30,40,50 expected present")
	}
	// remove frees membership.
	if !g.remove(20) || g.contains(20) {
		t.Fatal("remove(20) should drop it")
	}
	if g.remove(99) {
		t.Fatal("remove of absent returns false")
	}
	g.clear()
	if g.contains(30) || g.contains(50) {
		t.Fatal("clear should empty membership")
	}
}

func TestGhostDisabled(t *testing.T) {
	g := newGhostQueue(0)
	g.add(1)
	if g.contains(1) || g.remove(1) {
		t.Fatal("disabled ghost must stay empty")
	}
	g.clear()
}

func TestGhostDifferentialFuzz(t *testing.T) {
	for _, n := range []int{1, 2, 3, 7, 8, 16, 64} {
		for seed := range int64(40) {
			r := rand.New(rand.NewSource(seed*1000 + int64(n)))
			g := newGhostQueue(n)
			ref := newRefGhost(n)
			// Small fingerprint domain forces frequent collisions, dedups and
			// re-adds of evicted keys - the corner cases of the ring/index dance.
			dom := uint64(n*3 + 2)
			for step := range 2000 {
				h := uint64(r.Intn(int(dom))) // includes 0: a real, trackable fingerprint
				switch r.Intn(3) {
				case 0, 1:
					g.add(h)
					ref.add(h)
				case 2:
					if g.remove(h) != ref.remove(h) {
						t.Fatalf("n=%d seed=%d step=%d remove(%d) mismatch", n, seed, step, h)
					}
				}
				// Full-domain membership cross-check every few steps.
				if step%7 == 0 {
					for q := uint64(0); q <= dom+1; q++ {
						if g.contains(q) != ref.contains(q) {
							t.Fatalf("n=%d seed=%d step=%d contains(%d): got %v want %v",
								n, seed, step, q, g.contains(q), ref.contains(q))
						}
					}
				}
			}
		}
	}
}
