package kioshun

import (
	"sync/atomic"

	"github.com/unkn0wn-root/kioshun/internal/mathx"
)

// htable is a linear-probing table with one writer and lock-free readers.
//
// A reader first compares the hash tag and loads the item only on a match.
// Published items are immutable because a reader may retain one after eviction.
// Updating a value therefore allocates and publishes a new item.
type htable[K comparable, V any] struct {
	data   atomic.Pointer[htableData[K, V]]
	live   int
	tombs  int
	pinned uint64
}

// htNoPin cannot be a real slot index.
const htNoPin = ^uint64(0)

// htslot uses tag 0 for empty, 1 for deleted, and all other values for item
// hashes. Writers publish the item before its tag and mark it deleted before
// clearing the pointer, so a matching tag always has a readable item.
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

// htNormHash reserves tags 0 and 1 for empty and deleted slots.
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
				s.item.Store(it)
				return cur
			}
		}
		i = (i + 1) & d.mask
	}
}

// htCursor records where a deferred insert belongs. Its table pointer detects a
// rehash between probe and publish.
type htCursor[K comparable, V any] struct {
	d    *htableData[K, V]
	slot uint64
	tomb bool
}

// probe finds an existing item or reserves a slot for a later publish. Deferring
// publication lets SieveTinyLFU reject a candidate before readers can see it.
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
			t.pinned = at
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

// publish completes a deferred insert. A stale cursor falls back to store.
func (t *htable[K, V]) publish(it *cacheItem[K, V], cur htCursor[K, V]) {
	t.pinned = htNoPin
	if cur.d != t.data.Load() {
		t.store(it)
		return
	}
	s := &cur.d.slots[cur.slot]
	// An eviction may have reclaimed this deleted slot after probe.
	wasTomb := cur.tomb && s.tag.Load() == 1
	s.item.Store(it)
	s.tag.Store(htNormHash(it.hash))
	t.live++
	if wasTomb {
		t.tombs--
	}
	t.maybeGrow()
}

// swapAt publishes a replacement for the same key. A racing reader sees either
// complete item because the tag does not change.
func (t *htable[K, V]) swapAt(slot *htslot[K, V], it *cacheItem[K, V]) {
	slot.item.Store(it)
}

// removeExact removes the slot only if it still points to the given item. This
// rejects stale policy pointers after a replacement or earlier removal.
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

// reclaimTombs clears deleted slots from the end of a probe cluster. No live
// lookup crosses these slots because the following empty slot already ends the
// search. pinned protects a slot reserved by probe but not yet published.
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

// clear publishes a new empty table. Existing readers may finish on the old one.
func (t *htable[K, V]) clear() {
	d := t.data.Load()
	n := len(d.slots)
	t.data.Store(&htableData[K, V]{slots: make([]htslot[K, V], n), mask: uint64(n - 1)})
	t.live = 0
	t.tombs = 0
	t.pinned = htNoPin
}

// maybeGrow expands a table full of live items or rebuilds one full of deleted slots.
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

// rehash publishes a rebuilt table without deleted slots. Readers see either the
// complete old table or the complete new one.
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
