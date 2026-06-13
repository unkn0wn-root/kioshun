package kioshun

import (
	"sync/atomic"

	"github.com/unkn0wn-root/kioshun/internal/mathx"
)

// cacheLinePadding isolates contended atomics onto their own cache lines.
const cacheLinePadding = 64

// signal performs wake on a size-1 channel:
// if a token is already pending the wake is a no-op.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// mpscCell is one ring slot. seq sequences ownership between producers and the consumer
type mpscCell[K comparable, V any] struct {
	seq atomic.Uint64
	cmd writeCommand[K, V]
}

// mpscQueue connects cache producers to a shard's single write worker. It is a
// bounded Vyukov MPSC ring: sequence numbers distinguish free, published and
// stale slots across laps without a producer-side lock. The queue applies
// back-pressure instead of dropping writes; on shutdown it wakes blocked
// producers.
type mpscQueue[K comparable, V any] struct {
	mask    uint64
	buffer  []mpscCell[K, V]
	wake    chan struct{}   // consumer wakeup
	space   chan struct{}   // producer wakeup when the consumer frees a slot
	closeCh <-chan struct{} // cache shutdown broadcast

	_         [cacheLinePadding]byte
	head      atomic.Uint64 // hot
	_         [cacheLinePadding]byte
	tail      atomic.Uint64 // single writer (the consumer)
	_         [cacheLinePadding]byte
	wakeState atomic.Uint32 // 1 when a wake is pending or the consumer is active
	_         [cacheLinePadding]byte
}

func newMPSCQueue[K comparable, V any](size int, wake chan struct{}, closeCh <-chan struct{}) *mpscQueue[K, V] {
	// Vyukov ring needs >= 2 slots: at size 1 a cell's "published" sequence
	// is indistinguishable from its "freed" sequence so the next enqueue would
	// overwrite an un-dequeued item.
	n := max(mathx.NextPowerOf2(size), 2)
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
				// Signal only when this publish makes an otherwise idle queue
				// non-empty: tail == pos means the slot just filled is the one the
				// consumer reads next and the wakeState CAS claims the single
				// outstanding wake. When an earlier slot is still unconsumed
				// (tail != pos) or a wake is already outstanding (wakeState == 1),
				// the consumer reaches this item without another signal. The skip is
				// safe because the consumer decides there is work from the ring
				// itself (ready), never from wakeState - see writeWorker.
				if q.tail.Load() == pos && q.wakeState.CompareAndSwap(0, 1) {
					signal(q.wake)
				}
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

// quiescent reports whether the queue holds no in-flight writes: head == tail
// means every reserved slot has been consumed so there is neither a published
// command waiting nor a slot a producer has reserved (advanced head) but not yet
// published. Both ends are read atomically, so this is safe to call without the
// drain token (e.g. on the lock-free read miss path) - it is only a hint: a
// producer may reserve a slot, or the consumer may advance tail, right after it
// returns, which the caller re-checks under the drain token before acting.
func (q *mpscQueue[K, V]) quiescent() bool {
	return q.head.Load() == q.tail.Load()
}

// ready reports whether a command is published at the consumer's tail, so the
// next tryDequeue would return it. It is the wake protocol's source of truth for
// "is there work". Only the consumer (writeWorker) may call it: it reads tail
// unsynchronized, which is stable solely for the single consumer.
func (q *mpscQueue[K, V]) ready() bool {
	pos := q.tail.Load()
	cell := &q.buffer[pos&q.mask]
	return cell.seq.Load() == pos+1
}

func (q *mpscQueue[K, V]) tryDequeue(buf []writeCommand[K, V]) int {
	n := 0
	pos := q.tail.Load()
	for n < len(buf) {
		cell := &q.buffer[pos&q.mask]
		if cell.seq.Load() != pos+1 {
			break // not yet published (empty)
		}
		buf[n] = cell.cmd
		cell.cmd = writeCommand[K, V]{}  // drop references
		cell.seq.Store(pos + q.mask + 1) // free the slot for the next lap
		pos++
		q.tail.Store(pos) // publish progress so quiescent() sees it
		n++
	}
	if n > 0 {
		signal(q.space) // a producer waiting for room can proceed
	}
	return n
}
