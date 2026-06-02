package kioshun

import "sync/atomic"

// cacheLinePadding isolates contended atomics onto their own cache lines.
const cacheLinePadding = 64

// writeQueue connects cache producers to a shard's single write worker.
// It's multi-producer/single-consumer, applies back-pressure instead of dropping
// write must wake blocked producers on shutdown.
type writeQueue[K comparable, V any] interface {
	enqueue(cmd writeCommand[K, V]) error
	tryDequeue(buf []writeCommand[K, V]) int
}

func newWriteQueue[K comparable, V any](size int, wake chan struct{}, closeCh <-chan struct{}) writeQueue[K, V] {
	return newMPSCQueue[K, V](size, wake, closeCh)
}

// signal performs wake on a size-1 channel:
// if a token is already pending the wake is a no-op.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// mpscCell is one ring slot. seq sequences ownership between producers and the
// consumer (Vyukov bounded queue); cmd carries the payload in place.
type mpscCell[K comparable, V any] struct {
	seq atomic.Uint64
	cmd writeCommand[K, V]
}

// mpscQueue is a bounded Vyukov MPSC ring. Sequence numbers distinguish free,
// published and stale slots across laps without a producer-side lock.
type mpscQueue[K comparable, V any] struct {
	mask    uint64
	buffer  []mpscCell[K, V]
	wake    chan struct{}   // consumer wakeup
	space   chan struct{}   // producer wakeup when the consumer frees a slot
	closeCh <-chan struct{} // cache shutdown broadcast

	_    [cacheLinePadding]byte
	head atomic.Uint64 // hot
	_    [cacheLinePadding]byte
	tail uint64 // single consumer
	_    [cacheLinePadding]byte
}

func newMPSCQueue[K comparable, V any](size int, wake chan struct{}, closeCh <-chan struct{}) *mpscQueue[K, V] {
	// Vyukov ring needs >= 2 slots: at size 1 a cell's "published" sequence
	// is indistinguishable from its "freed" sequence so the next enqueue would
	// overwrite an un-dequeued item.
	n := max(nextPowerOf2(size), 2)
	q := &mpscQueue[K, V]{
		mask:    uint64(n - 1),
		buffer:  make([]mpscCell[K, V], n),
		wake:    wake,
		space:   make(chan struct{}, 1),
		closeCh: closeCh,
	}
	for i := range q.buffer {
		q.buffer[i].seq.Store(uint64(i))
	}
	return q
}

func (q *mpscQueue[K, V]) enqueue(cmd writeCommand[K, V]) error {
	for {
		pos := q.head.Load()
		cell := &q.buffer[pos&q.mask]
		seq := cell.seq.Load()
		switch dif := int64(seq) - int64(pos); {
		case dif == 0:
			// free for this lap.
			if q.head.CompareAndSwap(pos, pos+1) {
				cell.cmd = cmd
				cell.seq.Store(pos + 1) // publish (release) for the consumer
				signal(q.wake)
				return nil
			}
		case dif < 0:
			// full: this slot still holds an item one lap behind that the
			// consumer has not freed. Wait for room or shutdown.
			select {
			case <-q.space:
			case <-q.closeCh:
				return ErrCacheClosed
			}
		default:
			// another producer advanced head; retry with a fresh position.
		}
	}
}

func (q *mpscQueue[K, V]) tryDequeue(buf []writeCommand[K, V]) int {
	n := 0
	for n < len(buf) {
		pos := q.tail
		cell := &q.buffer[pos&q.mask]
		seq := cell.seq.Load() // acquire; pairs with the producer's publish
		if int64(seq)-int64(pos+1) != 0 {
			break // not yet published (empty)
		}
		q.tail = pos + 1
		buf[n] = cell.cmd
		cell.cmd = writeCommand[K, V]{}  // drop references so the GC can reclaim
		cell.seq.Store(pos + q.mask + 1) // free the slot for the next lap
		n++
	}
	if n > 0 {
		signal(q.space) // a producer waiting for room can proceed
	}
	return n
}
