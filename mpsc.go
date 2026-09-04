package kioshun

import (
	"sync/atomic"

	"github.com/unkn0wn-root/kioshun/internal/mathx"
)

// signal sends a wake-up unless one is already pending.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

type mpscCell[K comparable, V any] struct {
	seq atomic.Uint64
	cmd writeCommand[K, V]
}

// mpscQueue is a bounded Vyukov ring with many producers and one consumer.
// Sequence numbers distinguish free and published slots on each pass. Producers
// wait when the ring is full instead of dropping writes.
type mpscQueue[K comparable, V any] struct {
	mask    uint64
	buffer  []mpscCell[K, V]
	wake    chan struct{}
	space   chan struct{}
	closeCh <-chan struct{}

	_         [cacheLinePadding]byte
	head      atomic.Uint64
	_         [cacheLinePadding]byte
	tail      atomic.Uint64 // written only by the consumer
	_         [cacheLinePadding]byte
	wakeState atomic.Uint32 // 1 while a wake is pending or the consumer is active
	_         [cacheLinePadding]byte
}

func newMPSCQueue[K comparable, V any](size int, wake chan struct{}, closeCh <-chan struct{}) *mpscQueue[K, V] {
	// The ring needs at least two slots. With one slot, its published and freed
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
			if q.head.CompareAndSwap(pos, pos+1) {
				cell.cmd = cmd
				cell.seq.Store(pos + 1)
				// Wake only for the consumer's next slot and only when no wake is
				// pending. The consumer checks the ring itself before sleeping.
				if q.tail.Load() == pos && q.wakeState.CompareAndSwap(0, 1) {
					signal(q.wake)
				}
				return nil
			}
		case dif < 0:
			select {
			case <-q.space:
			case <-q.closeCh:
				return ErrCacheClosed
			}
		default:
			// Another producer advanced head.
		}
	}
}

// quiescent reports whether every reserved slot has been consumed. The result is
// only a hint unless the caller holds the drain lock.
func (q *mpscQueue[K, V]) quiescent() bool {
	return q.head.Load() == q.tail.Load()
}

// ready reports whether the next command has been published. Only the consumer
// may call it.
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
			break
		}
		buf[n] = cell.cmd
		cell.cmd = writeCommand[K, V]{}
		cell.seq.Store(pos + q.mask + 1)
		pos++
		q.tail.Store(pos)
		n++
	}
	if n > 0 {
		signal(q.space)
	}
	return n
}
