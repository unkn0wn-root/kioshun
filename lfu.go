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
	node := l.ensureIndex(l.head, 1)
	node.items[item] = struct{}{}
	l.itemFreq[item] = node
}

func (l *lfuList[K, V]) increment(item *cacheItem[K, V]) {
	cur := l.itemFreq[item]
	if cur == nil {
		l.add(item)
		return
	}

	newFreq := cur.freq + 1
	delete(cur.items, item)

	target := cur.next
	if target == l.head || target.freq != newFreq {
		target = l.ensureIndex(cur, newFreq)
	}
	target.items[item] = struct{}{}
	l.itemFreq[item] = target

	if len(cur.items) == 0 && cur.freq != 0 {
		l.removeFreqNode(cur)
	}
}

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

func (l *lfuList[K, V]) removeFreqNode(node *freqNode[K, V]) {
	node.prev.next = node.next
	node.next.prev = node.prev
	delete(l.freqMap, node.freq)
}
