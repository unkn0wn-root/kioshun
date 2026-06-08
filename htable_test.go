package kioshun

import "testing"

func htItem(key, hash int) *cacheItem[int, int] {
	return &cacheItem[int, int]{key: key, hash: uint64(hash), value: key}
}

func TestHtableProbeDefersInsert(t *testing.T) {
	tab := newHtable[int, int](8)
	it := htItem(1, 100)

	prev, cur := tab.probe(it)
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
	prev, cur := tab.probe(nw)
	if prev != old {
		t.Fatalf("probe update returned prev=%p, want old=%p", prev, old)
	}
	if cur.d != nil {
		t.Fatal("update path must not return an insert cursor")
	}
	if got, _ := tab.lookup(nw.hash, nw.key); got != nw {
		t.Fatal("probe did not swap to the new item")
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
	prev, cur := tab.probe(c)
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

	_, cur := tab.probe(it)
	tab.clear() // publishes a fresh slot array; cur.d is now stale

	tab.publish(it, cur)
	if got, ok := tab.lookup(it.hash, it.key); !ok || got != it {
		t.Fatal("publish did not fall back to store after the table was replaced")
	}
	if tab.live != 1 {
		t.Fatalf("live=%d after fallback publish, want 1", tab.live)
	}
}
