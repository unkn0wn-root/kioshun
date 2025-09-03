package cluster

import (
	"sync/atomic"
	"testing"
)

func TestRingOwnersWeightedFirstIsHeaviest(t *testing.T) {
	r := newRing(2)
	a := newMeta(NodeID("A"), "a")
	b := newMeta(NodeID("B"), "b")
	c := newMeta(NodeID("C"), "c")

	// make A overwhelmingly heavy so it always wins top rank.
	atomic.StoreUint64(&a.weight, 1_000_000)
	atomic.StoreUint64(&b.weight, 1)
	atomic.StoreUint64(&c.weight, 1)
	r.nodes = []*nodeMeta{a, b, c}

	owners := r.ownersFromKeyHash(12345)
	if len(owners) == 0 || owners[0] != a {
		t.Fatalf("expected A as first owner, got %#v", owners)
	}

	top := r.ownersTopNFromKeyHash(12345, 3)
	if len(top) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(top))
	}
	if top[0] != a {
		t.Fatalf("expected A as first candidate, got %#v", top)
	}
}
