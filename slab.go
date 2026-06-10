package kioshun

// Slab allocation for cache items.
//
// Probation inserts are bump-allocated out of per-shard slabs instead of one
// heap allocation each. Three invariants make that safe:
//
//   - Slab slots are never reused. A slab only advances, and the GC frees it
//     once every item in it is dead so the lock-free read invariants
//     (immutable, never-recycled items) are untouched.
//   - Slab lifetimes follow the probation FIFO. Probation evicts in insertion
//     order, which is also slab order so coallocated items die together and
//     slabs reclaim wholesale. Whatever outlives its cohort leaves the slab:
//     promotions rehouse the item (unslabForMain), and updates and B1
//     ghost-hit main admits were never slab-backed.
//   - Only pointer-free K/V slab-allocate. Dead slots cannot be zeroed
//     (readers may still hold their items), and one live item keeps the whole
//     slab's pointer fields as live GC roots, so a pointer carrying type
//     would retain dead neighbors' memory far beyond the slab byte cap.

import (
	"reflect"
	"unsafe"
)

const (
	// itemSlabTargetBytes bounds one bump slab's footprint. Bigger slabs
	// amortize the allocator better, but a straggler that outlives its cohort
	// pins the whole slab, so 2KB caps how much dead memory one can hold.
	itemSlabTargetBytes = 2048
	maxItemSlabLen      = 64
)

// itemSlabLen sizes a shard's bump slab for the cache's concrete item type.
// The fixed byte budget keeps a straggler's pinned slab small however large
// K/V make the item, and pointer-carrying types disable slabs entirely
// (slabLen 1 makes newSlabItem allocate singletons).
func itemSlabLen[K comparable, V any]() int {
	if !typeIsPointerFree(reflect.TypeFor[K]()) || !typeIsPointerFree(reflect.TypeFor[V]()) {
		return 1
	}
	return min(int(itemSlabTargetBytes/unsafe.Sizeof(cacheItem[K, V]{})), maxItemSlabLen)
}

// typeIsPointerFree reports whether values of t reference no heap memory, so
// one left behind in a dead slab slot retains nothing beyond the slot's own
// bytes. Strings count as pointer carrying (they reference backing bytes).
func typeIsPointerFree(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr, reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128:
		return true
	case reflect.Array:
		return typeIsPointerFree(t.Elem())
	case reflect.Struct:
		for i := range t.NumField() {
			if !typeIsPointerFree(t.Field(i).Type) {
				return false
			}
		}
		return true
	default:
		// pointer, slice, map, string, chan, func, interface etc.
		return false
	}
}

// newSlabItem returns the next item from the shard's bump slab, replacing one
// heap allocation per insert with one per slabLen inserts. Caller holds s.mu.
func (c *Cache[K, V]) newSlabItem(s *shard[K, V], cmd *writeCommand[K, V]) *cacheItem[K, V] {
	if c.slabLen <= 1 {
		return c.newItem(cmd)
	}
	if s.slabOff == len(s.slab) {
		s.slab = make([]cacheItem[K, V], c.slabLen)
		s.slabOff = 0
	}
	it := &s.slab[s.slabOff]
	s.slabOff++
	it.value = cmd.value
	it.key = cmd.key
	it.hash = cmd.hash
	it.expireTime = cmd.expireTime
	it.cost = cmd.cost
	it.flags = itemSlabbed
	return it
}

// unslabForMain rehouses a slab resident into its own allocation before it
// enters main. Main residents live arbitrarily long, and one survivor would
// pin its whole slab - every dead neighbor included - so the copy keeps slab
// lifetimes coupled to the probation FIFO that allocated them. The table slot
// and SIEVE queue position transfer to the copy; a reader racing the swap gets
// one item or the other, same as any value update. The exception is the
// in-flight unpublished candidate: it has no table slot yet and admission
// tracks it by pointer identity, so it stays in place.
func (s *shard[K, V]) unslabForMain(it *cacheItem[K, V]) *cacheItem[K, V] {
	if it.flags&itemSlabbed == 0 || it.flags&itemUnpublished != 0 {
		return it
	}
	cp := &cacheItem[K, V]{
		value:      it.value,
		expireTime: it.expireTime,
		cost:       it.cost,
		key:        it.key,
		hash:       it.hash,
	}
	if !s.tab.replaceExact(it, cp) {
		return it
	}
	s.sieve.replaceNode(it, cp)
	return cp
}
