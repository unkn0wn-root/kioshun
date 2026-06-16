package kioshun

import (
	"sync"
	"testing"
	"time"
)

func drainAll(q *mpscQueue[int, int], buf []writeCommand[int, int]) []writeCommand[int, int] {
	var out []writeCommand[int, int]
	for {
		n := q.tryDequeue(buf)
		if n == 0 {
			return out
		}
		out = append(out, buf[:n]...)
	}
}

func TestMPSCQueueMinimumRingSize(t *testing.T) {
	// WriteBufferSize 0/1 must still yield a usable ring (>= 2 slots), or the
	// Vyukov sequence math would let an enqueue overwrite an un-dequeued item.
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
	buf := make([]writeCommand[int, int], 3) // small batch to exercise multi-pass drain
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
	// Fill the ring (2 slots).
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

	// Free a slot; the blocked producer must now complete.
	if n := q.tryDequeue(make([]writeCommand[int, int], 1)); n != 1 {
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

	close(closeCh) // shutdown must wake the blocked producer
	select {
	case err := <-blocked:
		if err != ErrCacheClosed {
			t.Fatalf("blocked enqueue err=%v, want ErrCacheClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not wake the blocked producer")
	}
}

func TestMPSCQueueConcurrentProducersNoLoss(t *testing.T) {
	const producers = 8
	const perProducer = 5000
	q := newMPSCQueue[int, int](64, make(chan struct{}, 1), make(chan struct{}))

	want := producers * perProducer
	counts := make([]int, producers) // next expected seq per producer
	got := 0
	done := make(chan struct{})

	go func() {
		buf := make([]writeCommand[int, int], 32)
		defer close(done)
		for got < want {
			n := q.tryDequeue(buf)
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
