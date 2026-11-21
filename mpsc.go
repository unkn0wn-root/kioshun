package kioshun

import (
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/unkn0wn-root/kioshun/internal/mathx"
)

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
	closeCh <-chan struct{}

	_         [cacheLinePadding]byte
	head      atomic.Uint64
	_         [cacheLinePadding]byte
	tail      atomic.Uint64 // written only by the consumer
	_         [cacheLinePadding]byte
	wakeState atomic.Uint32 // 1 while a wake is pending or the consumer is active
	_         [cacheLinePadding]byte

	// One producer at a time waits for space. It sets spaceWanted, and the
	// consumer that clears it sends one wake-up on space.
	spaceMu     sync.Mutex
	spaceWanted atomic.Bool
	space       chan struct{}
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
				// Publish before reading tail so we either send a wake-up or the consumer sees the command itself.
				if q.tail.Load() == pos {
					q.wakeConsumer()
				}
				return nil
			}
		case dif < 0:
			if err := q.waitForSpace(); err != nil {
				return err
			}
		default:
			// Another producer advanced head.
		}
	}
}

// wakeConsumer wakes the consumer unless a wake-up is pending or the consumer
// is running. Only the caller that sets wakeState sends, so the send never
// blocks. Keep it a plain send. A failed non-blocking send gives no ordering.
func (q *mpscQueue[K, V]) wakeConsumer() {
	if q.wakeState.Load() == 0 && q.wakeState.CompareAndSwap(0, 1) {
		q.wake <- struct{}{}
	}
}

// waitForSpace returns when a slot may be free or the cache is closed. It sets
// spaceWanted before checking the ring again, so a consumer that frees a slot
// after the check sends a wake-up.
func (q *mpscQueue[K, V]) waitForSpace() error {
	q.spaceMu.Lock()
	defer q.spaceMu.Unlock()
	q.spaceWanted.Store(true)
	pos := q.head.Load()
	if int64(q.buffer[pos&q.mask].seq.Load())-int64(pos) >= 0 {
		q.withdrawSpaceRequest()
		return nil
	}
	select {
	case <-q.space:
		return nil
	case <-q.closeCh:
		q.withdrawSpaceRequest()
		return ErrCacheClosed
	}
}

// withdrawSpaceRequest clears spaceWanted, or takes the wake-up if a consumer
// already cleared it.
func (q *mpscQueue[K, V]) withdrawSpaceRequest() {
	if !q.spaceWanted.CompareAndSwap(true, false) {
		<-q.space
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

// waitClaimed returns false once the consumer reaches end.
// Otherwise it waits for the producer at tail to publish. Only the consumer may call it.
func (q *mpscQueue[K, V]) waitClaimed(end uint64) bool {
	if int64(end-q.tail.Load()) <= 0 {
		return false
	}
	for !q.ready() {
		runtime.Gosched()
	}
	return true
}

// tryDequeue moves contiguous published commands before end into buf. Only the consumer may call it.
func (q *mpscQueue[K, V]) tryDequeue(buf []writeCommand[K, V], end uint64) int {
	n := 0
	pos := q.tail.Load()
	for n < len(buf) && int64(end-pos) > 0 {
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
	// Free slots before taking the request, so a waiter that rechecks the ring
	// after posting it sees them.
	if n > 0 && q.spaceWanted.Load() && q.spaceWanted.CompareAndSwap(true, false) {
		q.space <- struct{}{}
	}
	return n
}
