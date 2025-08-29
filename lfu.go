package cache

// freqNode is a doubly-linked bucket for one exact frequency.
// non-sentinel empty buckets are removed eagerly so head.next is the current min.
type freqNode[K comparable, V any] struct {
	freq  int64                      // exact frequency (>= 0); head sentinel uses 0
	items map[*cacheItem[V]]struct{} // set of items at this frequency
	prev  *freqNode[K, V]
	next  *freqNode[K, V]
}

// lfuList is an LFU index of ascending-frequency buckets (sentinel head at freq==0).
// O(1) add/increment/remove; used under the shard lock.
type lfuList[K comparable, V any] struct {
	head     *freqNode[K, V]
	freqMap  map[int64]*freqNode[K, V]         // freq → bucket
	itemFreq map[*cacheItem[V]]*freqNode[K, V] // item → bucket
}

// newLFUList creates a circular list with a freq==0 sentinel; sentinel holds no real items.
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

// add inserts item with frequency=1 and indexes it.
func (l *lfuList[K, V]) add(item *cacheItem[V]) {
	freq := int64(1)
	item.frequency = freq

	node := l.getOrCreateFreqNode(freq)
	node.items[item] = struct{}{}
	l.itemFreq[item] = node
}

// increment bumps item's frequency by 1, moves it to the correct bucket, and removes an empty old bucket.
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

// removeLFU removes and returns one item from the minimum-frequency bucket
// unlinks the bucket if it becomes empty.
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

// remove deletes a specific item from its bucket and removes the bucket if it becomes empty (non-sentinel).
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

// ensureIndex returns the bucket for freq, inserting a new bucket immediately after prev to keep ascending order.
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

// getOrCreateFreqNode returns the bucket for freq, inserting after freq-1 (or after head for freq==1).
func (l *lfuList[K, V]) getOrCreateFreqNode(freq int64) *freqNode[K, V] {
	if freq == 1 {
		return l.ensureIndex(l.head, 1)
	}
	// Insert immediately after the previous frequency bucket.
	prev := l.freqMap[freq-1]
	return l.ensureIndex(prev, freq)
}

// removeFreqNode unlinks an empty non-sentinel bucket and drops its freq map entry.
func (l *lfuList[K, V]) removeFreqNode(node *freqNode[K, V]) {
	node.prev.next = node.next
	node.next.prev = node.prev
	delete(l.freqMap, node.freq)
}
