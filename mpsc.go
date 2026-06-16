package kioshun

import (
	"sync/atomic"

	"github.com/unkn0wn-root/kioshun/internal/mathx"
)

// signal wakes a size-1 channel; a no-op if a token is already pending.
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

// mpscQueue is a bounded Vyukov MPSC ring linking cache producers to a shard's
// single write worker. Per-cell sequence numbers order free/published/stale slots
// across laps without a producer lock; full producers block (back-pressure, not
// drop) until the consumer frees a slot or the cache closes.
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
	// Vyukov ring needs >= 2 slots: at size 1 a cell's published and freed
	// sequences coincide, so the next enqueue could overwrite an un-dequeued item.
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
				// Wake only when this publish fills the consumer's next slot
				// (tail == pos) and claims the lone outstanding wake (wakeState CAS).
				// Skipping is safe: the consumer detects work from the ring (ready),
				// not from wakeState.
				if q.tail.Load() == pos && q.wakeState.CompareAndSwap(0, 1) {
					signal(q.wake)
				}
				return nil
			}
		case dif < 0:
			// full: slot still holds an unfreed item from the previous lap.
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

// quiescent reports whether no writes are in flight (head == tail: every reserved
// slot consumed). Lock-free and safe off the drain token, but only a hint - a
// producer or the consumer may move either end right after it returns, so callers
// re-check under the token before acting.
func (q *mpscQueue[K, V]) quiescent() bool {
	return q.head.Load() == q.tail.Load()
}

// ready reports whether a command is published at tail (the next tryDequeue would
// return it) - the wake protocol's source of truth for "is there work". Consumer
// only: it reads tail unsynchronized.
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
