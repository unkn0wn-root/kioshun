package cache

// freqNode represents one frequency bucket in an intrusive, doubly-linked list.
// Invariants:
//   - Nodes are ordered by freq in strictly increasing order from head → ...,
//     with a sentinel head at freq==0.
//   - Each node holds a set of items at exactly that frequency.
//   - Empty non-sentinel buckets are removed immediately to keep head.next
//     pointing at the current minimum frequency.
//
// Concurrency: all mutations are performed under the owning shard's write lock.
type freqNode[K comparable, V any] struct {
	freq  int64                      // exact frequency (>= 0); head sentinel uses 0
	items map[*cacheItem[V]]struct{} // open-addressed set of items at this frequency
	prev  *freqNode[K, V]            // previous bucket in the list
	next  *freqNode[K, V]            // next bucket in the list
}

// lfuList is an O(1) LFU index implemented as a linked set of frequency buckets.
// Data structures:
//   - head:     sentinel node at freq==0 (never removed); head.next is the min freq bucket.
//   - freqMap:  freq → *freqNode (fast bucket lookup/creation).
//   - itemFreq: item → *freqNode (fast item relocation between buckets).
//
// Complexity:
//   - add/increment/remove/removeLFU: O(1) average-case.
//   - removeLFU: picks an arbitrary item from the minimum-frequency bucket
//     (iteration over a Go map yields an arbitrary element).
type lfuList[K comparable, V any] struct {
	head     *freqNode[K, V]
	freqMap  map[int64]*freqNode[K, V]
	itemFreq map[*cacheItem[V]]*freqNode[K, V]
}

// newLFUList initializes the LFU structure with a circular sentinel head.
//
// Sentinel semantics:
//   - head.freq == 0
//   - head.next == head && head.prev == head when empty
//   - real buckets are always spliced between head and head (circular)
//     maintaining ascending freq order starting at 1.
//
// Note: the sentinel "items" set exists only to avoid nil guards; no real items
// are stored at freq==0.
func newLFUList[K comparable, V any]() *lfuList[K, V] {
	list := &lfuList[K, V]{
		head:     &freqNode[K, V]{freq: 0, items: make(map[*cacheItem[V]]struct{})},
		freqMap:  make(map[int64]*freqNode[K, V]),
		itemFreq: make(map[*cacheItem[V]]*freqNode[K, V]),
	}
	list.head.next = list.head
	list.head.prev = list.head
	return list
}

// add inserts a new item with frequency = 1.
//
// Contract:
//   - Caller has ensured the item is not already present in this LFU list.
//   - We set item.frequency for coherence with external observers.
//
// Complexity: O(1).
func (l *lfuList[K, V]) add(item *cacheItem[V]) {
	freq := int64(1)
	item.frequency = freq

	node := l.getOrCreateFreqNode(freq)
	node.items[item] = struct{}{}
	l.itemFreq[item] = node
}

// increment bumps an item's frequency by 1 and relocates it to the appropriate bucket.
//
// Steps:
//  1. Look up current bucket via itemFreq.
//  2. Compute newFreq = cur.freq + 1.
//  3. Move item into existing next bucket if it already matches newFreq,
//     otherwise splice a new bucket at the correct position (right after cur).
//  4. If the old bucket becomes empty (and is not the sentinel), remove it.
//
// Complexity: O(1).
func (l *lfuList[K, V]) increment(item *cacheItem[V]) {
	cur := l.itemFreq[item]
	if cur == nil {
		// Item not indexed yet (defensive); treat as new with freq=1.
		l.add(item)
		return
	}

	newFreq := cur.freq + 1
	delete(cur.items, item)

	nxt := cur.next
	var target *freqNode[K, V]
	if nxt != l.head && nxt.freq == newFreq {
		// Fast path: the next bucket already has the desired frequency.
		target = nxt
	} else {
		// Create or find the exact bucket at newFreq right after 'cur'.
		target = l.ensureIndex(cur, newFreq)
	}
	target.items[item] = struct{}{}
	l.itemFreq[item] = target
	item.frequency = newFreq

	// Drop the old bucket if it is now empty (sentinel node is never removed).
	if len(cur.items) == 0 && cur.freq != 0 {
		l.removeFreqNode(cur)
	}
}

// removeLFU removes and returns one item from the minimum-frequency bucket.
// Tie-breaking within the bucket is arbitrary (map iteration order).
// If the bucket becomes empty, we unlink it to keep head.next pointing at the
// current minimum.
//
// Complexity: O(1) expected.
func (l *lfuList[K, V]) removeLFU() *cacheItem[V] {
	node := l.head.next
	if node == l.head {
		return nil // list is empty
	}
	// By invariant, non-sentinel buckets are never empty.
	var victim *cacheItem[V]
	for it := range node.items {
		victim = it
		break
	}

	delete(node.items, victim)
	delete(l.itemFreq, victim)
	if len(node.items) == 0 {
		l.removeFreqNode(node)
	}
	return victim
}

// remove deletes a specific item from whichever bucket it currently resides in.
// If that bucket becomes empty (and is not the sentinel), it is unlinked.
//
// Complexity: O(1).
func (l *lfuList[K, V]) remove(item *cacheItem[V]) {
	node := l.itemFreq[item]
	if node == nil {
		return // item not tracked
	}

	delete(node.items, item)
	delete(l.itemFreq, item)

	if len(node.items) == 0 && node.freq != 0 {
		l.removeFreqNode(node)
	}
}

// ensureIndex returns the node for exactly 'freq', inserting a new bucket
// right after 'prev' if it doesn't already exist.
//
// Ordering invariant:
//   - The list remains sorted ascending by frequency.
//   - New buckets are always inserted immediately after the previous frequency,
//     so traversal from head is monotonic.
//
// Complexity: O(1).
func (l *lfuList[K, V]) ensureIndex(prev *freqNode[K, V], freq int64) *freqNode[K, V] {
	// Exact-hit fast path via freqMap.
	if node, ok := l.freqMap[freq]; ok {
		return node
	}

	newNode := &freqNode[K, V]{
		freq:  freq,
		items: make(map[*cacheItem[V]]struct{}),
	}

	nxt := prev.next
	prev.next = newNode
	newNode.prev = prev
	newNode.next = nxt
	nxt.prev = newNode

	l.freqMap[freq] = newNode
	return newNode
}

// getOrCreateFreqNode returns the bucket for 'freq' with the correct splice point.
// Common case:
//   - freq==1 → insert right after the sentinel head (min frequency).
//
// Otherwise we place it after the freq-1 bucket to maintain ordering.
//
// Complexity: O(1).
func (l *lfuList[K, V]) getOrCreateFreqNode(freq int64) *freqNode[K, V] {
	if freq == 1 {
		return l.ensureIndex(l.head, 1)
	}
	// Insert immediately after the previous frequency bucket.
	prev := l.freqMap[freq-1]
	return l.ensureIndex(prev, freq)
}

// removeFreqNode unlinks an empty non-sentinel bucket from the list and
// deletes its mapping. Caller must ensure node.freq != 0 and len(node.items) == 0.
//
// Complexity: O(1).
func (l *lfuList[K, V]) removeFreqNode(node *freqNode[K, V]) {
	node.prev.next = node.next
	node.next.prev = node.prev
	delete(l.freqMap, node.freq)
}
