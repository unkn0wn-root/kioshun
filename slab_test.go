package kioshun

import (
	"sync/atomic"
	"testing"
)

func TestNewSlabItemRolloverAndFallback(t *testing.T) {
	c := &Cache[int, int]{slabLen: 4}
	s := &shard[int, int]{}
	cmd := &writeCommand[int, int]{key: 7, value: 9, hash: 42, expireTime: 5, cost: 3}

	seen := make(map[*cacheItem[int, int]]bool)
	for i := range 9 {
		it := c.newSlabItem(s, cmd)
		if seen[it] {
			t.Fatalf("allocation %d reused a slab slot", i)
		}
		seen[it] = true
		if it.flags != itemSlabbed {
			t.Fatalf("allocation %d flags=%b, want itemSlabbed", i, it.flags)
		}
		if it.key != 7 || it.value != 9 || it.hash != 42 || it.expireTime != 5 || it.cost != 3 {
			t.Fatalf("allocation %d not populated: %+v", i, it)
		}
		if it.prev != nil || it.next != nil || it.queue != queueNone || it.visited != 0 {
			t.Fatalf("allocation %d has dirty policy state: %+v", i, it)
		}
	}
	// 9 allocations at slabLen 4: two full slabs consumed, one slot into the third.
	if len(s.slab) != 4 || s.slabOff != 1 {
		t.Fatalf("slab len=%d off=%d after 9 allocations, want 4/1", len(s.slab), s.slabOff)
	}

	c.slabLen = 1
	it := c.newSlabItem(s, cmd)
	if it.flags&itemSlabbed != 0 {
		t.Fatal("slabLen<=1 must allocate a singleton without the slab flag")
	}
	if s.slabOff != 1 {
		t.Fatal("singleton fallback must not advance the slab cursor")
	}
}

func TestSlabLenDisabledForHugeItems(t *testing.T) {
	type huge struct{ buf [4096]byte }
	c := newTestCache[int, huge](t, Config{
		MaxSize:        8,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
	})
	defer c.Close()
	if c.slabLen > 1 {
		t.Fatalf("slabLen=%d for a >2KiB item, want <=1 (slabs disabled)", c.slabLen)
	}

	small := newTestCache[int, int](t, Config{
		MaxSize:        8,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
	})
	defer small.Close()
	if small.slabLen <= 1 || small.slabLen > maxItemSlabLen {
		t.Fatalf("slabLen=%d for a small item, want in (1, %d]", small.slabLen, maxItemSlabLen)
	}
}

func TestSlabDisabledForPointerCarryingTypes(t *testing.T) {
	cfg := Config{MaxSize: 8, ShardCount: 1, EvictionPolicy: SieveTinyLFU}

	stringVal := newTestCache[int, string](t, cfg)
	defer stringVal.Close()
	if stringVal.slabLen > 1 {
		t.Fatalf("slabLen=%d for string values, want <=1", stringVal.slabLen)
	}

	stringKey := newTestCache[string, int](t, cfg)
	defer stringKey.Close()
	if stringKey.slabLen > 1 {
		t.Fatalf("slabLen=%d for string keys, want <=1", stringKey.slabLen)
	}

	sliceVal := newTestCache[int, []byte](t, cfg)
	defer sliceVal.Close()
	if sliceVal.slabLen > 1 {
		t.Fatalf("slabLen=%d for slice values, want <=1", sliceVal.slabLen)
	}

	type nested struct {
		id   uint64
		name string
	}
	nestedVal := newTestCache[int, nested](t, cfg)
	defer nestedVal.Close()
	if nestedVal.slabLen > 1 {
		t.Fatalf("slabLen=%d for a struct embedding a string, want <=1", nestedVal.slabLen)
	}

	type flat struct {
		id  uint64
		buf [16]byte
		ts  int64
	}
	flatVal := newTestCache[uint64, flat](t, cfg)
	defer flatVal.Close()
	if flatVal.slabLen <= 1 {
		t.Fatalf("slabLen=%d for a pointer-free struct, want >1 (slabs enabled)", flatVal.slabLen)
	}
}

func TestUnslabForMainRehousesSlabItem(t *testing.T) {
	s := &shard[int, int]{tab: newHtable[int, int](8), cap: 8, stats: newStats(1)}
	s.sieve = newSieveTinyLFU[int, int](8, 0, 25, 100)

	it := &cacheItem[int, int]{key: 1, value: 11, hash: 1, flags: itemSlabbed}
	s.tab.store(it)
	s.sieve.insert(it, false)
	atomic.AddInt64(&s.size, 1)
	markSieveItemVisited(it) // visited => evictProbation promotes instead of evicting

	got := s.evictProbation(false)
	if got == nil || got == it {
		t.Fatalf("promotion did not rehouse the slab item: got %p, original %p", got, it)
	}
	if got.flags&itemSlabbed != 0 {
		t.Fatal("rehoused copy must not carry the slab flag")
	}
	if !s.sieve.main.holds(got) {
		t.Fatal("rehoused copy is not in the main queue")
	}
	if s.sieve.owns(it) {
		t.Fatal("original slab item is still policy-linked")
	}
	if res, ok := s.tab.lookup(1, 1); !ok || res != got || res.value != 11 {
		t.Fatal("table does not resolve the key to the rehoused copy")
	}

	// An unpublished candidate has no table slot and must keep its identity.
	cand := &cacheItem[int, int]{key: 2, value: 22, hash: 2, flags: itemSlabbed | itemUnpublished}
	if s.unslabForMain(cand) != cand {
		t.Fatal("unpublished candidate must not be rehoused")
	}
}

func TestClearDropsActiveSlab(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:        64,
		ShardCount:     1,
		EvictionPolicy: SieveTinyLFU,
	})
	defer c.Close()

	for i := range 8 {
		if err := c.Set(i, i, 0); err != nil {
			t.Fatal(err)
		}
	}
	waitForWrites(t, c)
	s := c.shards[0]
	if len(s.slab) == 0 || s.slabOff == 0 {
		t.Fatalf("expected an active slab after inserts (len=%d off=%d)", len(s.slab), s.slabOff)
	}

	c.Clear()
	if s.slab != nil || s.slabOff != 0 {
		t.Fatalf("Clear left the active slab (len=%d off=%d), want released", len(s.slab), s.slabOff)
	}

	if err := c.Set(100, 100, 0); err != nil {
		t.Fatal(err)
	}
	waitForWrites(t, c)
	if v, ok := c.Get(100); !ok || v != 100 {
		t.Fatalf("post-clear insert not readable: (%d,%v)", v, ok)
	}
	if len(s.slab) == 0 {
		t.Fatal("post-clear insert did not start a fresh slab")
	}
}
