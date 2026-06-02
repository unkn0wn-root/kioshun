package kioshun

// freqNode buckets items by exact frequency.
// Empty non-sentinel buckets are removed eagerly,
// so head.next is always the current minimum.
type freqNode[K comparable, V any] struct {
	freq  int64
	items map[*cacheItem[K, V]]struct{}
	prev  *freqNode[K, V]
	next  *freqNode[K, V]
}

type lfuList[K comparable, V any] struct {
	head     *freqNode[K, V]
	freqMap  map[int64]*freqNode[K, V]            // freq -> bucket
	itemFreq map[*cacheItem[K, V]]*freqNode[K, V] // item -> bucket
}

// newLFUList creates a circular list with a freq==0 sentinel.
func newLFUList[K comparable, V any]() *lfuList[K, V] {
	list := &lfuList[K, V]{
		head:     &freqNode[K, V]{freq: 0, items: make(map[*cacheItem[K, V]]struct{})},
		freqMap:  make(map[int64]*freqNode[K, V]),
		itemFreq: make(map[*cacheItem[K, V]]*freqNode[K, V]),
	}
	list.head.next = list.head
	list.head.prev = list.head
	return list
}

func (l *lfuList[K, V]) add(item *cacheItem[K, V]) {
	freq := int64(1)
	item.lfuFreq = freq

	node := l.getOrCreateFreqNode(freq)
	node.items[item] = struct{}{}
	l.itemFreq[item] = node
}

// increment moves item to freq+1, preserving the ascending-bucket invariant.
func (l *lfuList[K, V]) increment(item *cacheItem[K, V]) {
	cur := l.itemFreq[item]
	if cur == nil {
		l.add(item)
		return
	}

	newFreq := cur.freq + 1
	delete(cur.items, item)

	nxt := cur.next
	var target *freqNode[K, V]
	if nxt != l.head && nxt.freq == newFreq {
		target = nxt
	} else {
		target = l.ensureIndex(cur, newFreq)
	}
	target.items[item] = struct{}{}
	l.itemFreq[item] = target
	item.lfuFreq = newFreq

	if len(cur.items) == 0 && cur.freq != 0 {
		l.removeFreqNode(cur)
	}
}

// removeLFU returns one item from the minimum frequency bucket.
func (l *lfuList[K, V]) removeLFU() *cacheItem[K, V] {
	node := l.head.next
	if node == l.head {
		return nil // list is empty
	}
	// non-sentinel buckets are never empty.
	var victim *cacheItem[K, V]
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

func (l *lfuList[K, V]) remove(item *cacheItem[K, V]) {
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

// ensureIndex inserts a missing bucket immediately after prev.
func (l *lfuList[K, V]) ensureIndex(prev *freqNode[K, V], freq int64) *freqNode[K, V] {
	if node, ok := l.freqMap[freq]; ok {
		return node
	}

	newNode := &freqNode[K, V]{
		freq:  freq,
		items: make(map[*cacheItem[K, V]]struct{}),
	}

	nxt := prev.next
	prev.next = newNode
	newNode.prev = prev
	newNode.next = nxt
	nxt.prev = newNode

	l.freqMap[freq] = newNode
	return newNode
}

func (l *lfuList[K, V]) getOrCreateFreqNode(freq int64) *freqNode[K, V] {
	if freq == 1 {
		return l.ensureIndex(l.head, 1)
	}
	prev := l.freqMap[freq-1]
	return l.ensureIndex(prev, freq)
}

// removeFreqNode unlinks an empty non-sentinel bucket and drops its freq map entry.
func (l *lfuList[K, V]) removeFreqNode(node *freqNode[K, V]) {
	node.prev.next = node.next
	node.next.prev = node.prev
	delete(l.freqMap, node.freq)
}
