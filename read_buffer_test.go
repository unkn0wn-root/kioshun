package kioshun

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newReadTestShard builds a minimal SieveTinyLFU shard wired with a read buffer,
// without spinning up the cache's worker goroutine, so drains are deterministic.
func newReadTestShard(t *testing.T, cap int64) *shard[int, int] {
	t.Helper()
	s := &shard[int, int]{
		data: make(map[int]*cacheItem[int]),
		cap:  cap,
		wake: make(chan struct{}, 1),
	}
	s.initLRU()
	s.sieve = newSieveTinyLFU[int](cap, 10, 100)
	s.readBuf = newReadBuffer()
	return s
}

func TestReadBufferSampleThenDrainFeedsSketch(t *testing.T) {
	s := newReadTestShard(t, 64)
	h := uint64(0xABCDEF)

	if got := s.sieve.estimate(h); got != 0 {
		t.Fatalf("initial estimate=%d, want 0", got)
	}

	for i := 0; i < 50; i++ {
		s.sampleRead(h)
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
	for i := 0; i < total; i++ {
		s.sampleRead(h)
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
	for i := 0; i < 30; i++ {
		s.sampleRead(0)
	}
	s.drainReadSamples()
	if got := s.sieve.estimate(1); got == 0 {
		t.Fatal("zero-hash samples were dropped instead of remapped to sentinel 1")
	}
}

// TestReadBufferConcurrentSampleDrain stresses many producers against a single
// draining consumer; correctness here is "no race / no panic / bounded work".
func TestReadBufferConcurrentSampleDrain(t *testing.T) {
	s := newReadTestShard(t, 256)

	var stop atomic.Bool
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			h := uint64(id + 1)
			for !stop.Load() {
				s.sampleRead(h)
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

// End-to-end: many read hits on a live cache must not break the read→worker
// pipeline (Get -> sampleRead -> worker drain). We deliberately do not read the
// sketch from the test goroutine: the worker mutates it lock-free, so any
// external read would race. The deterministic sample->drain->sketch assertion
// lives in TestReadBufferSampleThenDrainFeedsSketch.
func TestReadHitsDrainWithoutBreakingCache(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         64,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
		StatsEnabled:    true,
		Adapt:           false,
	})
	defer c.Close()

	const key = 7
	c.Set(key, key, time.Hour)
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 500; i++ {
		if v, ok := c.Get(key); !ok || v != key {
			t.Fatalf("read %d: got (%d,%v), want (%d,true)", i, v, ok, key)
		}
	}
	// Interleave a write so the worker drains read samples alongside writes.
	c.Set(key, key+1, time.Hour)
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	if v, ok := c.Get(key); !ok || v != key+1 {
		t.Fatalf("after update: got (%d,%v), want (%d,true)", v, ok, key+1)
	}
}

func TestReadMissesFeedAdmissionSketch(t *testing.T) {
	c := newTestCache[int, int](t, Config{
		MaxSize:         64,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
		StatsEnabled:    false,
		Adapt:           false,
	})
	defer c.Close()

	for i := 0; i < 32; i++ {
		if err := c.Set(i, i, time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	const key = 10_000
	h := c.hasher.hash(key)
	for i := 0; i < readStripeSlots; i++ {
		if _, ok := c.Get(key); ok {
			t.Fatal("unexpected hit for miss-sampling key")
		}
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}

	s := c.shards[0]
	s.mu.RLock()
	got := s.sieve.estimate(h)
	s.mu.RUnlock()
	if got == 0 {
		t.Fatal("read misses were not replayed into the admission sketch")
	}
}
