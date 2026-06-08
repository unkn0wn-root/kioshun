package kioshun

import "sync/atomic"

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
	data  atomic.Pointer[htableData[K, V]]
	live  int // writer-only
	tombs int // writer-only
}

// htslot is one co-located cell. tag pre-filters probes without dereferencing
// the item: 0 = empty (a lookup stops), 1 = tombstone (a lookup continues past
// it), any other value = the slot's normalized item hash.
// Publication order makes a matching tag imply a readable item: store writes the
// item pointer before the tag, and remove writes the tombstone tag before
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
	// rehash/grow when (live+tombs)/slots reaches 3/4. Open addressing degrades
	// sharply past this load and probes walk tombstones until then.
	htLoadNum = 3
	htLoadDen = 4
)

func newHtable[K comparable, V any](capacityHint int) *htable[K, V] {
	n := max(nextPowerOf2(capacityHint*2), htMinSlots)
	t := &htable[K, V]{}
	t.data.Store(&htableData[K, V]{slots: make([]htslot[K, V], n), mask: uint64(n - 1)})
	return t
}

// htNormHash keeps stored tags out of the 0 (empty) and 1 (tombstone) sentinel
// space. A real avalanche hash hitting {0,1} is vanishingly unlikely; remapping
// it only risks an extra key comparison, never incorrectness.
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
// probe and consumed by publish under the same shard write lock; the captured
// htableData pointer lets publish detect (and fall back from) a rehash that
// happened in between, though eviction never rehashes today.
type htCursor[K comparable, V any] struct {
	d    *htableData[K, V]
	slot uint64
	tomb bool // the slot was a tombstone at probe time (publish reclaims it)
}

// probe walks for it.key in one pass. If the key already exists, it swaps the
// value-carrying item in place (publishing the new immutable item) and returns
// prev != nil - an update completed without a second walk. If the key is absent,
// it returns prev=nil plus a cursor at the slot a later publish should fill,
// WITHOUT inserting, so a SieveTinyLFU candidate can run admission before it ever
// becomes visible to lock-free readers. Caller holds the shard write lock.
//
// This is store's probe walk - the same tag sentinels and first-tombstone reuse -
// diverging only at the empty slot, where store commits in place and probe defers
// to publish. The two walks must stay in sync: a change to tombstone reuse or tag
// handling here belongs in store (and the read-only walks in lookup/removeExact)
// too.
func (t *htable[K, V]) probe(it *cacheItem[K, V]) (prev *cacheItem[K, V], cur htCursor[K, V]) {
	tag := htNormHash(it.hash)
	d := t.data.Load()
	i := tag & d.mask
	firstTomb := -1
	for {
		s := &d.slots[i]
		switch s.tag.Load() {
		case 0:
			slot, tomb := i, false
			if firstTomb >= 0 {
				slot, tomb = uint64(firstTomb), true
			}
			return nil, htCursor[K, V]{d: d, slot: slot, tomb: tomb}
		case 1:
			if firstTomb < 0 {
				firstTomb = int(i)
			}
		case tag:
			if cur := s.item.Load(); cur != nil && cur.key == it.key {
				s.item.Store(it) // same tag, swap the value-carrying item
				return cur, htCursor[K, V]{}
			}
		}
		i = (i + 1) & d.mask
	}
}

// publish completes a deferred insert at cur, mirroring store's empty-slot arm.
// If the table was rehashed or cleared since probe (defensive: eviction never
// rehashes), the cursor is stale, so it falls back to a full store.
func (t *htable[K, V]) publish(it *cacheItem[K, V], cur htCursor[K, V]) {
	if cur.d != t.data.Load() {
		t.store(it)
		return
	}
	s := &cur.d.slots[cur.slot]
	s.item.Store(it)
	s.tag.Store(htNormHash(it.hash))
	t.live++
	if cur.tomb {
		t.tombs--
	}
	t.maybeGrow()
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
				return true
			}
		}
		i = (i + 1) & d.mask
	}
}

func (t *htable[K, V]) length() int { return t.live }

func (t *htable[K, V]) forEach(fn func(*cacheItem[K, V]) bool) {
	d := t.data.Load()
	for i := range d.slots {
		if d.slots[i].tag.Load() <= 1 {
			continue
		}
		if it := d.slots[i].item.Load(); it != nil {
			if !fn(it) {
				return
			}
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
}
