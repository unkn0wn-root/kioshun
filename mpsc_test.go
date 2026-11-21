package kioshun

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func drainAll(q *mpscQueue[int, int], buf []writeCommand[int, int]) []writeCommand[int, int] {
	var out []writeCommand[int, int]
	for {
		n := q.tryDequeue(buf, q.head.Load())
		if n == 0 {
			return out
		}
		out = append(out, buf[:n]...)
	}
}

// claimSlot pauses a producer between claiming and publishing a slot.
// publish fills the slot without sending a wake-up.
func claimSlot[K comparable, V any](t *testing.T, q *mpscQueue[K, V]) (publish func(cmd writeCommand[K, V])) {
	t.Helper()
	pos := q.head.Load()
	cell := &q.buffer[pos&q.mask]
	if cell.seq.Load() != pos || !q.head.CompareAndSwap(pos, pos+1) {
		t.Fatal("could not claim the next ring slot")
	}
	return func(cmd writeCommand[K, V]) {
		cell.cmd = cmd
		cell.seq.Store(pos + 1)
	}
}

func TestMPSCQueueMinimumRingSize(t *testing.T) {
	// Sizes 0 and 1 must still allocate the algorithm's minimum two slots.
	for _, size := range []int{0, 1} {
		q := newMPSCQueue[int, int](size, make(chan struct{}, 1), make(chan struct{}))
		if len(q.buffer) < 2 {
			t.Fatalf("size=%d ring=%d, want >= 2", size, len(q.buffer))
		}
		if err := q.enqueue(writeCommand[int, int]{hash: 1}); err != nil {
			t.Fatalf("enqueue 1: %v", err)
		}
		if err := q.enqueue(writeCommand[int, int]{hash: 2}); err != nil {
			t.Fatalf("enqueue 2: %v", err)
		}
		got := drainAll(q, make([]writeCommand[int, int], 4))
		if len(got) != 2 || got[0].hash != 1 || got[1].hash != 2 {
			t.Fatalf("size=%d drained=%v, want [1 2]", size, got)
		}
	}
}

func TestMPSCQueueFIFOSingleProducer(t *testing.T) {
	q := newMPSCQueue[int, int](8, make(chan struct{}, 1), make(chan struct{}))
	buf := make([]writeCommand[int, int], 3)
	for i := range 6 {
		if err := q.enqueue(writeCommand[int, int]{hash: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	got := drainAll(q, buf)
	if len(got) != 6 {
		t.Fatalf("drained %d, want 6", len(got))
	}
	for i, cmd := range got {
		if cmd.hash != uint64(i) {
			t.Fatalf("position %d hash=%d, want %d (FIFO violated)", i, cmd.hash, i)
		}
	}
}

func TestMPSCQueueBackpressureBlocksUntilDrain(t *testing.T) {
	q := newMPSCQueue[int, int](2, make(chan struct{}, 1), make(chan struct{}))
	for i := range 2 {
		if err := q.enqueue(writeCommand[int, int]{hash: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}

	blocked := make(chan error, 1)
	go func() { blocked <- q.enqueue(writeCommand[int, int]{hash: 99}) }()

	select {
	case <-blocked:
		t.Fatal("enqueue returned while ring was full; expected back-pressure")
	case <-time.After(50 * time.Millisecond):
	}

	if n := q.tryDequeue(make([]writeCommand[int, int], 1), q.head.Load()); n != 1 {
		t.Fatalf("dequeued %d, want 1", n)
	}
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("unblocked enqueue err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("enqueue stayed blocked after a slot was freed")
	}
}

func TestMPSCQueueCloseWakesBlockedProducer(t *testing.T) {
	closeCh := make(chan struct{})
	q := newMPSCQueue[int, int](2, make(chan struct{}, 1), closeCh)
	for i := range 2 {
		if err := q.enqueue(writeCommand[int, int]{hash: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}

	blocked := make(chan error, 1)
	go func() { blocked <- q.enqueue(writeCommand[int, int]{hash: 99}) }()

	select {
	case <-blocked:
		t.Fatal("enqueue returned before close while ring was full")
	case <-time.After(50 * time.Millisecond):
	}

	close(closeCh)
	select {
	case err := <-blocked:
		if err != ErrCacheClosed {
			t.Fatalf("blocked enqueue err=%v, want ErrCacheClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not wake the blocked producer")
	}
	if q.spaceWanted.Load() || len(q.space) != 0 {
		t.Fatalf("left behind after close: space wanted %v, %d wake-ups queued", q.spaceWanted.Load(), len(q.space))
	}
}

func TestMPSCQueueConcurrentProducersNoLoss(t *testing.T) {
	const producers = 8
	const perProducer = 5000
	q := newMPSCQueue[int, int](64, make(chan struct{}, 1), make(chan struct{}))

	want := producers * perProducer
	counts := make([]int, producers)
	got := 0
	done := make(chan struct{})

	go func() {
		buf := make([]writeCommand[int, int], 32)
		defer close(done)
		for got < want {
			n := q.tryDequeue(buf, q.head.Load())
			if n == 0 {
				continue
			}
			for _, cmd := range buf[:n] {
				p := int(cmd.hash >> 32)
				seq := int(cmd.hash & 0xffffffff)
				if seq != counts[p] {
					t.Errorf("producer %d: got seq %d, want %d (per-producer FIFO)", p, seq, counts[p])
					return
				}
				counts[p]++
				got++
			}
		}
	}()

	var wg sync.WaitGroup
	for p := range producers {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for s := range perProducer {
				cmd := writeCommand[int, int]{hash: uint64(p)<<32 | uint64(s)}
				if err := q.enqueue(cmd); err != nil {
					t.Errorf("producer %d enqueue: %v", p, err)
					return
				}
			}
		}(p)
	}
	wg.Wait()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("consumer stalled at %d/%d (possible lost command or wakeup)", got, want)
	}
	if got != want {
		t.Fatalf("delivered %d, want %d", got, want)
	}
}

func TestWriteQueueWakeSignaled(t *testing.T) {
	wake := make(chan struct{}, 1)
	q := newMPSCQueue[int, int](8, wake, make(chan struct{}))
	if err := q.enqueue(writeCommand[int, int]{hash: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("enqueue did not signal the wake channel")
	}
}

// One dequeue that frees two slots must release two blocked producers.
func TestMPSCQueueFreedSlotsWakeEveryWaiter(t *testing.T) {
	closeCh := make(chan struct{})
	defer close(closeCh) // releases the producer that stays blocked
	q := newMPSCQueue[int, int](2, make(chan struct{}, 1), closeCh)
	for i := range 2 {
		if err := q.enqueue(writeCommand[int, int]{hash: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 3)
	for i := range 3 {
		go func() { done <- q.enqueue(writeCommand[int, int]{hash: uint64(10 + i)}) }()
	}
	select {
	case <-done:
		t.Fatal("enqueue returned while the ring was full")
	case <-time.After(50 * time.Millisecond):
	}

	if n := q.tryDequeue(make([]writeCommand[int, int], 2), q.head.Load()); n != 2 {
		t.Fatalf("dequeued %d, want 2", n)
	}
	for i := range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatalf("only %d of 2 blocked producers got a freed slot", i)
		}
	}
	select {
	case <-done:
		t.Fatal("a third producer returned while the ring was full again")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMPSCQueueWakeConsumerQueuesOneWakeUp(t *testing.T) {
	wake := make(chan struct{}, 1)
	q := newMPSCQueue[int, int](8, wake, make(chan struct{}))
	done := make(chan struct{})
	go func() {
		q.wakeConsumer()
		q.wakeConsumer()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a second wake-up blocked while one was queued")
	}
	if n := len(wake); n != 1 {
		t.Fatalf("%d wake-ups queued, want 1", n)
	}
}

// Concurrent callers with no consumer must queue one wake-up and never block.
func TestMPSCQueueWakeConsumerConcurrentCallers(t *testing.T) {
	for round := range 500 {
		wake := make(chan struct{}, 1)
		q := newMPSCQueue[int, int](8, wake, make(chan struct{}))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				q.wakeConsumer()
			}()
		}
		close(start)
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("round %d: a wake-up blocked while one was queued", round)
		}
		if n := len(wake); n != 1 {
			t.Fatalf("round %d: %d wake-ups queued, want 1", round, n)
		}
	}
}

// Slots freed while the producer is about to wait must leave no request or
// wake-up behind.
func TestMPSCQueueSpaceLeavesNothingBehind(t *testing.T) {
	for round := range 40 {
		q := newMPSCQueue[int, int](2, make(chan struct{}, 1), make(chan struct{}))
		var quit atomic.Bool
		consumed := make(chan struct{})
		go func() {
			defer close(consumed)
			buf := make([]writeCommand[int, int], 2)
			for !quit.Load() {
				if q.tryDequeue(buf, q.head.Load()) == 0 {
					runtime.Gosched() // lets the producer run when GOMAXPROCS is 1
				}
			}
		}()
		produced := make(chan struct{})
		go func() {
			defer close(produced)
			for i := range 20000 {
				_ = q.enqueue(writeCommand[int, int]{hash: uint64(i)})
			}
		}()
		select {
		case <-produced:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: the producer is stuck: head %d, tail %d, space wanted %v, %d wake-ups queued",
				round, q.head.Load(), q.tail.Load(), q.spaceWanted.Load(), len(q.space))
		}
		quit.Store(true)
		select {
		case <-consumed:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: the consumer is blocked on a space wake-up that nobody takes", round)
		}
		if q.spaceWanted.Load() || len(q.space) != 0 {
			t.Fatalf("round %d: left behind: space wanted %v, %d wake-ups queued", round, q.spaceWanted.Load(), len(q.space))
		}
	}
}

func TestMPSCQueueWaitClaimedIgnoresLaterClaims(t *testing.T) {
	q := newMPSCQueue[int, int](8, make(chan struct{}, 1), make(chan struct{}))
	if err := q.enqueue(writeCommand[int, int]{hash: 1}); err != nil {
		t.Fatal(err)
	}
	end := q.head.Load()
	claimSlot(t, q) // claimed after end and never published

	if got := drainAll(q, make([]writeCommand[int, int], 8)); len(got) != 1 || got[0].hash != 1 {
		t.Fatalf("drained %v, want [1]", got)
	}
	done := make(chan bool, 1)
	go func() { done <- q.waitClaimed(end) }()
	select {
	case more := <-done:
		if more {
			t.Fatal("waitClaimed reported an unconsumed slot claimed before end")
		}
	case <-time.After(time.Second):
		t.Fatal("waitClaimed waited for a slot claimed after end")
	}
}

func TestMPSCQueueTryDequeueStopsAtEnd(t *testing.T) {
	tests := []struct {
		name     string
		consumed int
		end      uint64
		want     int
	}{
		{"end inside published run", 0, 2, 2},
		{"end at tail", 2, 2, 0},
		{"end behind tail", 2, 1, 0},
		{"end past unpublished slot", 0, 5, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := newMPSCQueue[int, int](8, make(chan struct{}, 1), make(chan struct{}))
			for i := range 3 {
				if err := q.enqueue(writeCommand[int, int]{hash: uint64(i)}); err != nil {
					t.Fatal(err)
				}
			}
			claimSlot(t, q) // leave an unpublished slot before the write at 4
			if err := q.enqueue(writeCommand[int, int]{hash: 4}); err != nil {
				t.Fatal(err)
			}
			if tt.consumed > 0 {
				if n := q.tryDequeue(make([]writeCommand[int, int], tt.consumed), q.head.Load()); n != tt.consumed {
					t.Fatalf("setup dequeued %d, want %d", n, tt.consumed)
				}
			}

			buf := make([]writeCommand[int, int], 8)
			n := q.tryDequeue(buf, tt.end)
			if n != tt.want {
				t.Fatalf("tryDequeue(end=%d) at tail %d = %d, want %d", tt.end, tt.consumed, n, tt.want)
			}
			for i, cmd := range buf[:n] {
				if want := uint64(tt.consumed + i); cmd.hash != want {
					t.Fatalf("command %d has hash %d, want %d", i, cmd.hash, want)
				}
			}
			if got, want := q.tail.Load(), uint64(tt.consumed+n); got != want {
				t.Fatalf("tail = %d, want %d", got, want)
			}
		})
	}
}
