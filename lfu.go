package cache

type freqNode[K comparable, V any] struct {
	freq  int64
	items map[*cacheItem[V]]struct{} // Set of items with this frequency
	prev  *freqNode[K, V]
	next  *freqNode[K, V]
}

type lfuList[K comparable, V any] struct {
	head     *freqNode[K, V]                   // Sentinel head (lowest frequency)
	freqMap  map[int64]*freqNode[K, V]         // frequency -> freqNode
	itemFreq map[*cacheItem[V]]*freqNode[K, V] // item -> its frequency node
}

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

// add inserts a new item with frequency 1
func (l *lfuList[K, V]) add(item *cacheItem[V]) {
	freq := int64(1)
	item.frequency = freq

	node := l.getOrCreateFreqNode(freq)
	node.items[item] = struct{}{}
	l.itemFreq[item] = node
}

// increment increases item frequency by 1
func (l *lfuList[K, V]) increment(item *cacheItem[V]) {
	cur := l.itemFreq[item]
	if cur == nil {
		// never seen → freq=1
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
	item.frequency = newFreq

	// drop empty old bucket
	if len(cur.items) == 0 && cur.freq != 0 {
		l.removeFreqNode(cur)
	}
}

// removeLFU removes and returns the least frequently used item
func (l *lfuList[K, V]) removeLFU() *cacheItem[V] {
	node := l.head.next
	if node == l.head {
		return nil
	}
	// since we always unlink empty nodes, head.next is non-empty
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

// remove removes a specific item
func (l *lfuList[K, V]) remove(item *cacheItem[V]) {
	node := l.itemFreq[item]
	if node == nil {
		return
	}

	delete(node.items, item)
	delete(l.itemFreq, item)

	if len(node.items) == 0 && node.freq != 0 {
		l.removeFreqNode(node)
	}
}

// ensureIndex returns the node for exactly `freq`:
//   - if there's already a node at freq, return it
//   - else splice a brand new node right after `prev`
func (l *lfuList[K, V]) ensureIndex(prev *freqNode[K, V], freq int64) *freqNode[K, V] {
	// fast-path: exact hit
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

// get 1-node (for new items) is always head.next==freq=0, so we splice after head
func (l *lfuList[K, V]) getOrCreateFreqNode(freq int64) *freqNode[K, V] {
	if freq == 1 {
		return l.ensureIndex(l.head, 1)
	}
	// otherwise find your current node and insert after it
	prev := l.freqMap[freq-1]
	return l.ensureIndex(prev, freq)
}

// removeFreqNode removes an empty frequency node
func (l *lfuList[K, V]) removeFreqNode(node *freqNode[K, V]) {
	node.prev.next = node.next
	node.next.prev = node.prev
	delete(l.freqMap, node.freq)
}
