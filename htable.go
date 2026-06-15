package kioshun

import (
	"sync/atomic"

	"github.com/unkn0wn-root/kioshun/internal/mathx"
)

// htable is the per-shard key/value store: a single-writer/multi-reader
// open-addressing hash table with lock-free reads.
//
// Reads (lookup) take no lock - they snapshot the slot array and linear probe a
// dense array of co-located {tag, item} cells, dereferencing an item only
// on a tag match. Safety of lock-free reads rests on item immutability:
// a reader may still hold an item the writer has evicted,
// so reader-visible item fields (key, hash, value, expireTime) are written
// before the item is published and never mutated afterwards;
// a value update allocates a fresh item and swaps it in.
type htable[K comparable, V any] struct {
	data   atomic.Pointer[htableData[K, V]]
	live   int
	tombs  int
	pinned uint64
}

// htNoPin marks no probe cursor in flight; no real slot index reaches 2^64-1.
const htNoPin = ^uint64(0)

// htslot is one colocated cell. tag pre-filters probes without dereferencing
// the item: 0 = empty (a lookup stops), 1 = tombstone (a lookup continues past
// it), any other value = the slot's normalized item hash.
// Publication order makes a matching tag imply a readable item: store writes the
// item pointer before the tag and remove writes the tombstone tag before
// clearing the pointer.
type htslot[K comparable, V any] struct {
	tag  atomic.Uint64
	item atomic.Pointer[cacheItem[K, V]]
}

type htableData[K comparable, V any] struct {
	slots []htslot[K, V]
	mask  uint64
}

const (
	htMinSlots = 8
	htLoadNum  = 3
	htLoadDen  = 4
)

func newHtable[K comparable, V any](capacityHint int) *htable[K, V] {
	n := max(mathx.NextPowerOf2(capacityHint*2), htMinSlots)
	t := &htable[K, V]{pinned: htNoPin}
	t.data.Store(&htableData[K, V]{slots: make([]htslot[K, V], n), mask: uint64(n - 1)})
	return t
}

// htNormHash keeps stored tags out of the 0 (empty) and 1 (tombstone) sentinel
// space. A real avalanche hash hitting {0,1} is unlikely,
// and remapping it only risks an extra key comparison.
func htNormHash(h uint64) uint64 {
	if h < 2 {
		return h + 2
	}
	return h
}

func (t *htable[K, V]) lookup(hash uint64, key K) (*cacheItem[K, V], bool) {
	tag := htNormHash(hash)
	d := t.data.Load()
	i := tag & d.mask
	for {
		s := &d.slots[i]
		switch s.tag.Load() {
		case 0:
			return nil, false
		case tag:
			if it := s.item.Load(); it != nil && it.key == key {
				return it, true
			}
		}
		i = (i + 1) & d.mask
	}
}

func (t *htable[K, V]) store(it *cacheItem[K, V]) (prev *cacheItem[K, V]) {
	tag := htNormHash(it.hash)
	d := t.data.Load()
	i := tag & d.mask
	firstTomb := -1
	for {
		s := &d.slots[i]
		switch s.tag.Load() {
		case 0:
			// key absent (probed to an empty slot): insert, reusing the first
			// tombstone seen on the way if there was one.
			dst := s
			if firstTomb >= 0 {
				dst = &d.slots[firstTomb]
				t.tombs--
			}
			dst.item.Store(it)
			dst.tag.Store(tag)
			t.live++
			t.maybeGrow()
			return nil
		case 1:
			if firstTomb < 0 {
				firstTomb = int(i)
			}
		case tag:
			if cur := s.item.Load(); cur != nil && cur.key == it.key {
				s.item.Store(it) // same tag, swap the value-carrying item
				return cur
			}
		}
		i = (i + 1) & d.mask
	}
}

// htCursor captures where a deferred insert will be published. It is produced by
// probe and consumed by publish under the same shard write lock. The captured
// htableData pointer lets publish detect (and fall back from) a rehash that
// happened in between, though eviction never rehashes today.
type htCursor[K comparable, V any] struct {
	d    *htableData[K, V]
	slot uint64
	tomb bool
}

// probe walks for key in one pass. If the key already exists, it returns the
// resident item and its slot so the caller can build the replacement item and
// swap it in - an update completes without a second walk. If the key is absent,
// it returns prev=nil plus a cursor at the slot a later publish should fill,
// WITHOUT inserting, so a SieveTinyLFU candidate can run admission before it ever
// becomes visible to lock-free readers. Caller holds the shard write lock.
//
// This mirrors store's walk, diverging only at the empty slot (store commits, probe
// defers to publish); keep tombstone/tag handling in sync with store, lookup and
// removeExact.
func (t *htable[K, V]) probe(hash uint64, key K) (prev *cacheItem[K, V], slot *htslot[K, V], cur htCursor[K, V]) {
	tag := htNormHash(hash)
	d := t.data.Load()
	i := tag & d.mask
	firstTomb := -1
	for {
		s := &d.slots[i]
		switch s.tag.Load() {
		case 0:
			at, tomb := i, false
			if firstTomb >= 0 {
				at, tomb = uint64(firstTomb), true
			}
			t.pinned = at // barrier for reclaimTombs until publish or unpin
			return nil, nil, htCursor[K, V]{d: d, slot: at, tomb: tomb}
		case 1:
			if firstTomb < 0 {
				firstTomb = int(i)
			}
		case tag:
			if it := s.item.Load(); it != nil && it.key == key {
				return it, s, htCursor[K, V]{}
			}
		}
		i = (i + 1) & d.mask
	}
}

// publish completes a deferred insert at cur, mirroring store's empty-slot arm.
// If the table was rehashed or cleared since probe (defensive: eviction never
// rehashes), the cursor is stale, so it falls back to a full store.
func (t *htable[K, V]) publish(it *cacheItem[K, V], cur htCursor[K, V]) {
	t.pinned = htNoPin
	if cur.d != t.data.Load() {
		t.store(it)
		return
	}
	s := &cur.d.slots[cur.slot]
	// an eviction between probe and publish may have reclaimed the cursor's
	// tombstone (reclaimTombs), so only credit a tombstone that still exists.
	wasTomb := cur.tomb && s.tag.Load() == 1
	s.item.Store(it)
	s.tag.Store(htNormHash(it.hash))
	t.live++
	if wasTomb {
		t.tombs--
	}
	t.maybeGrow()
}

// swapAt swaps a fresh item into a slot probe already matched for the same
// key. The pointer store alone publishes it: the hash didn't change so the
// tag doesn't either, and a reader racing the swap gets the old item or the
// new one - both are complete snapshots of the key.
func (t *htable[K, V]) swapAt(slot *htslot[K, V], it *cacheItem[K, V]) {
	slot.item.Store(it)
}

// removeExact tombstones the slot only if it still holds exactly it, returning
// whether it did. The identity check rejects stale hand/queue pointers whose
// slot another mutation already replaced or removed.
func (t *htable[K, V]) removeExact(it *cacheItem[K, V]) bool {
	tag := htNormHash(it.hash)
	d := t.data.Load()
	i := tag & d.mask
	for {
		s := &d.slots[i]
		switch s.tag.Load() {
		case 0:
			return false
		case tag:
			if s.item.Load() == it {
				s.tag.Store(1)
				s.item.Store(nil)
				t.live--
				t.tombs++
				t.reclaimTombs(d, i)
				return true
			}
		}
		i = (i + 1) & d.mask
	}
}

// reclaimTombs converts the tombstone at i, and any tombstones immediately before
// it, back to empty when the following slot is empty. A tombstone at the end of its
// probe cluster lies on no live item's probe path (any walk stops at the empty slot
// after it), so clearing is safe under concurrent lock-free lookups: a reader sees
// the new empty and stops one slot earlier with the same result. Trimming cluster
// tails keeps miss probes short and defers same-size rehashes on eviction-heavy shards.
func (t *htable[K, V]) reclaimTombs(d *htableData[K, V], i uint64) {
	next := (i + 1) & d.mask
	if next == t.pinned || d.slots[next].tag.Load() != 0 {
		return
	}
	for i != t.pinned && d.slots[i].tag.Load() == 1 {
		d.slots[i].tag.Store(0)
		t.tombs--
		i = (i - 1) & d.mask
	}
}

// unpin releases a probe cursor that will never be published
// (the candidate was rejected by admission).
func (t *htable[K, V]) unpin() { t.pinned = htNoPin }

func (t *htable[K, V]) length() int { return t.live }

func (t *htable[K, V]) forEach(fn func(*cacheItem[K, V]) bool) {
	d := t.data.Load()
	for i := range d.slots {
		s := &d.slots[i]
		if s.tag.Load() <= 1 {
			continue
		}
		if it := s.item.Load(); it != nil && !fn(it) {
			return
		}
	}
}

// clear publishes a fresh empty table of the same size. In-flight readers keep
// reading their old immutable snapshot until they finish.
func (t *htable[K, V]) clear() {
	d := t.data.Load()
	n := len(d.slots)
	t.data.Store(&htableData[K, V]{slots: make([]htslot[K, V], n), mask: uint64(n - 1)})
	t.live = 0
	t.tombs = 0
	t.pinned = htNoPin
}

// maybeGrow rehashes when the table is too full. A table dense with live items
// grows; one merely full of tombstones is rebuilt at the same size to reclaim
// them.
func (t *htable[K, V]) maybeGrow() {
	d := t.data.Load()
	n := len(d.slots)
	if (t.live+t.tombs)*htLoadDen < n*htLoadNum {
		return
	}

	newN := n
	if t.live*htLoadDen >= n*htLoadNum {
		newN = n * 2
	}
	t.rehash(newN)
}

// rehash rebuilds the slot array at newN slots dropping tombstones, then
// publishes it atomically so a concurrent reader observes either the complete
// old table or the complete new one.
func (t *htable[K, V]) rehash(newN int) {
	d := t.data.Load()
	nd := &htableData[K, V]{slots: make([]htslot[K, V], newN), mask: uint64(newN - 1)}
	live := 0
	for i := range d.slots {
		if d.slots[i].tag.Load() <= 1 {
			continue
		}

		it := d.slots[i].item.Load()
		if it == nil {
			continue
		}

		tag := htNormHash(it.hash)
		j := tag & nd.mask
		for nd.slots[j].tag.Load() != 0 {
			j = (j + 1) & nd.mask
		}

		nd.slots[j].item.Store(it)
		nd.slots[j].tag.Store(tag)
		live++
	}
	t.data.Store(nd)
	t.live = live
	t.tombs = 0
	t.pinned = htNoPin
}
