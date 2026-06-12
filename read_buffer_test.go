package kioshun

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newReadTestShard(t *testing.T, cap int64) *shard[int, int] {
	t.Helper()
	s := &shard[int, int]{
		tab:  newHtable[int, int](int(cap)),
		cap:  cap,
		wake: make(chan struct{}, 1),
	}
	s.sieve = newSieveTinyLFU[int, int](cap, 0, 10, 100, CostAdmissionFrequency)
	s.readBuf = newReadBuffer()
	return s
}

func TestReadBufferSampleThenDrainFeedsSketch(t *testing.T) {
	s := newReadTestShard(t, 64)
	h := uint64(0xABCDEF)

	if got := s.sieve.estimate(h); got != 0 {
		t.Fatalf("initial estimate=%d, want 0", got)
	}

	for range 50 {
		s.readBuf.sample(h, stripeID())
	}
	s.drainReadSamples()

	if got := s.sieve.estimate(h); got == 0 {
		t.Fatal("drain did not replay samples into the sketch")
	}
}

func TestReadBufferDrainEmptyIsNoop(t *testing.T) {
	s := newReadTestShard(t, 64)
	// Draining with nothing buffered must not panic or advance the sketch.
	s.drainReadSamples()
	if got := s.sieve.estimate(123); got != 0 {
		t.Fatalf("estimate=%d after empty drain, want 0", got)
	}
}

func TestReadBufferLossyOverflowDrainsRecentWindow(t *testing.T) {
	s := newReadTestShard(t, 64)
	h := uint64(99)

	// Push far more than total ring capacity; producers overwrite older slots.
	// The drain must stay bounded and still replay the most recent window.
	total := readStripeSlots * (len(s.readBuf.stripes) + 4) * 8
	for range total {
		s.readBuf.sample(h, stripeID())
	}
	s.drainReadSamples()

	if got := s.sieve.estimate(h); got == 0 {
		t.Fatal("expected the recent-sample window to reach the sketch")
	}
	// A second drain should be a clean no-op (slots were cleared).
	s.drainReadSamples()
}

func TestReadBufferZeroHashMappedToSentinel(t *testing.T) {
	s := newReadTestShard(t, 64)
	// hash 0 collides with the empty-slot sentinel; sample() remaps it to 1 so
	// it is not silently dropped by the drain.
	for range 30 {
		s.readBuf.sample(0, stripeID())
	}
	s.drainReadSamples()
	if got := s.sieve.estimate(1); got == 0 {
		t.Fatal("zero-hash samples were dropped instead of remapped to sentinel 1")
	}
}

func TestReadBufferSignalsAtHeadRelativeFullWindow(t *testing.T) {
	rb := readBuffer{
		stripes: make([]readStripe, 1),
		mask:    0,
	}
	st := &rb.stripes[0]
	st.tail.Store(10)
	st.head.Store(10)

	for i := range readStripeSlots - 1 {
		_, needDrain := rb.sample(uint64(i+1), 0)
		if needDrain {
			t.Fatalf("sample %d signaled before head-relative window was full", i)
		}
	}

	_, needDrain := rb.sample(123, 0)
	if !needDrain {
		t.Fatal("sample did not signal when tail-head reached readStripeSlots")
	}
	if got := st.tail.Load() - st.head.Load(); got != readStripeSlots {
		t.Fatalf("backlog=%d, want %d", got, readStripeSlots)
	}
}

func TestReadBufferConcurrentSampleDrain(t *testing.T) {
	s := newReadTestShard(t, 256)

	var stop atomic.Bool
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			h := uint64(id + 1)
			for !stop.Load() {
				s.readBuf.sample(h, stripeID())
			}
		}(g)
	}

	// Single consumer drains repeatedly, mirroring the write worker.
	deadline := time.After(150 * time.Millisecond)
	for {
		select {
		case <-deadline:
			stop.Store(true)
			wg.Wait()
			s.drainReadSamples()
			return
		default:
			s.drainReadSamples()
		}
	}
}

func TestReadHitsDrainWithoutBreakingCache(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         64,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
		StatsEnabled:    true,
	})
	defer c.Close()

	const key = 7
	c.Set(key, key, time.Hour)
	if err := c.Sync(); err != nil {
		t.Fatal(err)
	}

	for i := range 500 {
		if v, ok := c.Get(key); !ok || v != key {
			t.Fatalf("read %d: got (%d,%v), want (%d,true)", i, v, ok, key)
		}
	}
	// Interleave a write so the worker drains read samples alongside writes.
	c.Set(key, key+1, time.Hour)
	if err := c.Sync(); err != nil {
		t.Fatal(err)
	}
	if v, ok := c.Get(key); !ok || v != key+1 {
		t.Fatalf("after update: got (%d,%v), want (%d,true)", v, ok, key+1)
	}
}

func TestMissesCountAtInsertNotOnReadPath(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         64,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
		StatsEnabled:    false,
	})
	defer c.Close()

	for i := range 32 {
		if err := c.Set(i, i, time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	const key = 10_000
	h := c.hasher.Sum(key)
	for range readStripeSlots {
		if _, ok := c.Get(key); ok {
			t.Fatal("unexpected hit for miss-sampling key")
		}
	}
	if err := c.Sync(); err != nil {
		t.Fatal(err)
	}

	s := c.shards[0]
	s.mu.RLock()
	got := s.sieve.estimate(h)
	s.mu.RUnlock()
	if got != 0 {
		t.Fatalf("read misses fed the sketch (estimate=%d), want no read-path admission work", got)
	}

	if err := c.Set(key, key, time.Hour); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	got = s.sieve.estimate(h)
	s.mu.RUnlock()
	if got < 2 {
		t.Fatalf("insert estimate=%d, want >= 2 (miss and Set both counted at insert)", got)
	}
}
