package kioshun

import "testing"

// With no consumer, a full read ring queues one wake-up through wakeState.
func TestSampleReadWakesThroughWakeState(t *testing.T) {
	wake := make(chan struct{}, 1)
	s := &shard[int, int]{wake: wake, readBuf: newReadBuffer()}
	s.queue = newMPSCQueue[int, int](2, wake, make(chan struct{}))
	s.drainMu.Lock() // the reader cannot drain its ring itself
	defer s.drainMu.Unlock()
	runWithin(t, "read samples", func() {
		for i := range 2 * readStripeSlots {
			s.sampleRead(uint64(i+1), 0)
		}
	})
	if n := len(wake); n != 1 {
		t.Fatalf("read samples queued %d wake-ups, want 1", n)
	}
	if s.queue.wakeState.Load() != 1 {
		t.Fatal("a read sample queued a wake-up without setting wakeState")
	}
	runWithin(t, "a producer's wake-up after the reader's", s.queue.wakeConsumer)
	if n := len(wake); n != 1 {
		t.Fatalf("%d wake-ups queued, want 1", n)
	}
}
