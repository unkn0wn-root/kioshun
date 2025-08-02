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
	currentNode := l.itemFreq[item]
	if currentNode == nil {
		l.add(item)
		return
	}

	oldFreq := currentNode.freq
	newFreq := oldFreq + 1

	delete(currentNode.items, item)

	newNode := l.getOrCreateFreqNode(newFreq)
	newNode.items[item] = struct{}{}
	l.itemFreq[item] = newNode
	item.frequency = newFreq

	if len(currentNode.items) == 0 && currentNode.freq != 0 {
		l.removeFreqNode(currentNode)
	}
}

// removeLFU removes and returns the least frequently used item
func (l *lfuList[K, V]) removeLFU() *cacheItem[V] {
	node := l.head.next
	for node != l.head && len(node.items) == 0 {
		node = node.next
	}

	if node == l.head {
		return nil // Empty
	}

	var victim *cacheItem[V]
	for item := range node.items {
		victim = item
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

// getOrCreateFreqNode ensures a frequency node exists in the right position
func (l *lfuList[K, V]) getOrCreateFreqNode(freq int64) *freqNode[K, V] {
	if node, exists := l.freqMap[freq]; exists {
		return node
	}

	newNode := &freqNode[K, V]{
		freq:  freq,
		items: make(map[*cacheItem[V]]struct{}),
	}

	prev := l.head
	for prev.next != l.head && prev.next.freq < freq {
		prev = prev.next
	}

	newNode.next = prev.next
	newNode.prev = prev
	prev.next.prev = newNode
	prev.next = newNode

	l.freqMap[freq] = newNode
	return newNode
}

// removeFreqNode removes an empty frequency node
func (l *lfuList[K, V]) removeFreqNode(node *freqNode[K, V]) {
	node.prev.next = node.next
	node.next.prev = node.prev
	delete(l.freqMap, node.freq)
}
