package kioshun

import "testing"

func htItem(key, hash int) *cacheItem[int, int] {
	return &cacheItem[int, int]{key: key, hash: uint64(hash), value: key}
}

func TestHtableProbeDefersInsert(t *testing.T) {
	tab := newHtable[int, int](8)
	it := htItem(1, 100)

	prev, _, cur := tab.probe(it.hash, it.key)
	if prev != nil {
		t.Fatalf("probe of absent key returned prev=%p", prev)
	}
	if _, ok := tab.lookup(it.hash, it.key); ok {
		t.Fatal("probe published the item; it must defer until publish")
	}
	if tab.live != 0 || tab.tombs != 0 {
		t.Fatalf("probe mutated table: live=%d tombs=%d", tab.live, tab.tombs)
	}

	tab.publish(it, cur)
	got, ok := tab.lookup(it.hash, it.key)
	if !ok || got != it {
		t.Fatal("publish did not store the probed item")
	}
	if tab.live != 1 {
		t.Fatalf("live=%d after publish, want 1", tab.live)
	}
}

func TestHtableProbeUpdatesInPlace(t *testing.T) {
	tab := newHtable[int, int](8)
	old := htItem(1, 100)
	tab.store(old)

	nw := htItem(1, 100)
	prev, slot, cur := tab.probe(nw.hash, nw.key)
	if prev != old {
		t.Fatalf("probe update returned prev=%p, want old=%p", prev, old)
	}
	if cur.d != nil {
		t.Fatal("update path must not return an insert cursor")
	}
	tab.swapAt(slot, nw)
	if got, _ := tab.lookup(nw.hash, nw.key); got != nw {
		t.Fatal("slot swap did not publish the new item")
	}
	if tab.live != 1 {
		t.Fatalf("live=%d after in-place update, want 1", tab.live)
	}
}

func TestHtablePublishReclaimsTombstone(t *testing.T) {
	tab := newHtable[int, int](8) // 16 slots, mask 15
	a := htItem(1, 16)            // slot 0
	b := htItem(2, 32)            // collides to slot 0, lands at slot 1
	tab.store(a)
	tab.store(b)
	if !tab.removeExact(a) {
		t.Fatal("removeExact(a) failed")
	}
	if tab.tombs != 1 {
		t.Fatalf("tombs=%d after removing a, want 1", tab.tombs)
	}

	c := htItem(3, 48) // collides to slot 0; probe should target the tombstone
	prev, _, cur := tab.probe(c.hash, c.key)
	if prev != nil {
		t.Fatal("c must be absent")
	}
	if !cur.tomb {
		t.Fatal("cursor should target the reclaimable tombstone")
	}
	tab.publish(c, cur)
	if tab.tombs != 0 {
		t.Fatalf("tombs=%d after reclaim, want 0", tab.tombs)
	}
	if got, ok := tab.lookup(c.hash, c.key); !ok || got != c {
		t.Fatal("c not findable after publish")
	}
	if _, ok := tab.lookup(b.hash, b.key); !ok {
		t.Fatal("b became unreachable after reclaiming the tombstone before it")
	}
}

func TestHtablePublishGuardFallsBackAfterRehash(t *testing.T) {
	tab := newHtable[int, int](8)
	it := htItem(1, 100)

	_, _, cur := tab.probe(it.hash, it.key)
	tab.clear() // publishes a fresh slot array; cur.d is now stale

	tab.publish(it, cur)
	if got, ok := tab.lookup(it.hash, it.key); !ok || got != it {
		t.Fatal("publish did not fall back to store after the table was replaced")
	}
	if tab.live != 1 {
		t.Fatalf("live=%d after fallback publish, want 1", tab.live)
	}
}

func TestHtableReclaimTombsAtClusterTail(t *testing.T) {
	tab := newHtable[int, int](8) // 16 slots, mask 15
	a := htItem(1, 16)            // slot 0
	b := htItem(2, 32)            // slot 1
	c := htItem(3, 48)            // slot 2
	tab.store(a)
	tab.store(b)
	tab.store(c)

	// c is the cluster tail (slot 3 is empty): its tombstone reclaims at once.
	if !tab.removeExact(c) {
		t.Fatal("removeExact(c) failed")
	}
	if tab.tombs != 0 {
		t.Fatalf("tail tombstone not reclaimed: tombs=%d", tab.tombs)
	}

	// a is mid-cluster (b is live after it): its tombstone must survive.
	if !tab.removeExact(a) {
		t.Fatal("removeExact(a) failed")
	}
	if tab.tombs != 1 {
		t.Fatalf("mid-cluster tombstone reclaimed early: tombs=%d", tab.tombs)
	}

	// removing b makes b's slot the new tail; the sweep must also clear the
	// chained tombstone at a's slot.
	if !tab.removeExact(b) {
		t.Fatal("removeExact(b) failed")
	}
	if tab.tombs != 0 {
		t.Fatalf("backward chain not reclaimed: tombs=%d", tab.tombs)
	}
	d := tab.data.Load()
	for i := range 3 {
		if got := d.slots[i].tag.Load(); got != 0 {
			t.Fatalf("slot %d tag=%d after full reclaim, want 0", i, got)
		}
	}
}

func TestHtableReclaimTombsRespectsPin(t *testing.T) {
	tab := newHtable[int, int](8) // 16 slots
	a := htItem(1, 16)            // slot 0
	b := htItem(2, 32)            // slot 1
	tab.store(a)
	tab.store(b)
	if !tab.removeExact(a) {
		t.Fatal("removeExact(a) failed")
	}

	// probe for an absent colliding key: the cursor takes a's tombstone and
	// pins slot 0.
	c := htItem(3, 48)
	prev, _, cur := tab.probe(c.hash, c.key)
	if prev != nil || !cur.tomb || cur.slot != 0 {
		t.Fatalf("cursor (tomb=%v slot=%d), want tombstone cursor at slot 0", cur.tomb, cur.slot)
	}

	// an eviction between probe and publish: b's removal makes slot 1 a cluster
	// tail, and the backward sweep must stop at the pinned slot 0.
	if !tab.removeExact(b) {
		t.Fatal("removeExact(b) failed")
	}
	d := tab.data.Load()
	if got := d.slots[0].tag.Load(); got != 1 {
		t.Fatalf("pinned tombstone was reclaimed (tag=%d)", got)
	}
	if got := d.slots[1].tag.Load(); got != 0 {
		t.Fatalf("unpinned tail tombstone not reclaimed (tag=%d)", got)
	}
	if tab.tombs != 1 {
		t.Fatalf("tombs=%d after pinned sweep, want 1", tab.tombs)
	}

	tab.publish(c, cur)
	if tab.pinned != htNoPin {
		t.Fatal("publish did not release the pin")
	}
	if tab.tombs != 0 {
		t.Fatalf("tombs=%d after publish reclaimed the cursor tombstone, want 0", tab.tombs)
	}
	if got, ok := tab.lookup(c.hash, c.key); !ok || got != c {
		t.Fatal("published item is unreachable")
	}
}

func TestHtableReclaimTombsPinnedEmptyTrigger(t *testing.T) {
	tab := newHtable[int, int](8) // 16 slots
	a := htItem(1, 16)            // slot 0
	b := htItem(2, 32)            // slot 1
	tab.store(a)
	tab.store(b)

	// probe for an absent colliding key: the cursor takes the first empty slot
	// (2) and pins it.
	c := htItem(3, 48)
	prev, _, cur := tab.probe(c.hash, c.key)
	if prev != nil || cur.tomb || cur.slot != 2 {
		t.Fatalf("cursor (tomb=%v slot=%d), want empty cursor at slot 2", cur.tomb, cur.slot)
	}

	// b's removal sees the pinned empty at slot 2 next to it: no sweep.
	if !tab.removeExact(b) {
		t.Fatal("removeExact(b) failed")
	}
	if got := tab.data.Load().slots[1].tag.Load(); got != 1 {
		t.Fatalf("tombstone before the pinned cursor was reclaimed (tag=%d)", got)
	}

	tab.publish(c, cur)
	if got, ok := tab.lookup(c.hash, c.key); !ok || got != c {
		t.Fatal("published item is unreachable")
	}
	if _, ok := tab.lookup(a.hash, a.key); !ok {
		t.Fatal("a became unreachable")
	}
}

func TestHtablePublishTombAccountingDefensive(t *testing.T) {
	tab := newHtable[int, int](8) // 16 slots
	a := htItem(1, 16)            // slot 0
	b := htItem(2, 32)            // slot 1 keeps a's tombstone from being a cluster tail
	tab.store(a)
	tab.store(b)
	if !tab.removeExact(a) {
		t.Fatal("removeExact(a) failed")
	}

	c := htItem(3, 48)
	_, _, cur := tab.probe(c.hash, c.key)
	if !cur.tomb {
		t.Fatal("cursor should target the tombstone")
	}

	// simulate the tombstone being reclaimed out from under the cursor.
	cur.d.slots[cur.slot].tag.Store(0)
	tab.tombs--

	tab.publish(c, cur)
	if tab.tombs != 0 {
		t.Fatalf("tombs=%d after defensive publish, want 0 (no double decrement)", tab.tombs)
	}
	if got, ok := tab.lookup(c.hash, c.key); !ok || got != c {
		t.Fatal("published item is unreachable")
	}
}
