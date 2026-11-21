package kioshun

import (
	"runtime"
	"testing"
	"time"
)

// gateWrite uses an unbuffered ack to keep drainMu held until the test receives it.
func gateWrite[K comparable, V any](t *testing.T, s *shard[K, V]) chan struct{} {
	t.Helper()
	ack := make(chan struct{})
	if err := s.queue.enqueue(writeCommand[K, V]{op: writeBarrier, result: ack}); err != nil {
		t.Fatal(err)
	}
	return ack
}

// Skip Close on failure because a stuck drainer would make cleanup hang.
func closeIfPassed(t *testing.T, c *Cache[string, string]) {
	t.Cleanup(func() {
		if !t.Failed() {
			c.Close()
		}
	})
}

func queueSet(t *testing.T, c *Cache[string, string], key string) {
	t.Helper()
	_, cmd, err := c.setCommand(key, "v", NoExpiration, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.shards[0].queue.enqueue(cmd); err != nil {
		t.Fatal(err)
	}
}

// queueGateAndSet fills two slots without waking the worker.
// The Set is behind the gate, and publishing the gate by hand skips its wake-up.
func queueGateAndSet(t *testing.T, c *Cache[string, string]) chan struct{} {
	t.Helper()
	publish := claimSlot(t, c.shards[0].queue)
	queueSet(t, c, "first")
	ack := make(chan struct{})
	publish(writeCommand[string, string]{op: writeBarrier, result: ack})
	return ack
}

func workerAwake(s *shard[string, string]) bool {
	return s.queue.wakeState.Load() != 0 || len(s.wake) != 0
}

var drainers = []struct {
	name  string
	drain func(c *Cache[string, string], s *shard[string, string])
}{
	{"drainShard", func(c *Cache[string, string], s *shard[string, string]) { c.drainShard(s) }},
	{"tryDrainShard", func(c *Cache[string, string], s *shard[string, string]) { c.tryDrainShard(s) }},
	{"syncMutate", func(c *Cache[string, string], s *shard[string, string]) { c.Delete("k") }},
	{"drainMissAndLookup", func(c *Cache[string, string], s *shard[string, string]) { c.Get("missing") }},
}

func TestWorkerDrainsWritesQueuedDuringItsDrain(t *testing.T) {
	c := newTestCache[string, string](t, Config{MaxSize: 1024, ShardCount: 1, EvictionPolicy: LRU})
	closeIfPassed(t, c)
	s := c.shards[0]

	gate := gateWrite(t, s)
	waitFor(t, func() bool { return s.queue.tail.Load() != 0 })
	queueSet(t, c, "later") // skips the wake-up because the worker is already draining
	<-gate

	// LRU reads leave the write queue alone.
	waitFor(t, func() bool {
		_, ok := c.Get("later")
		return ok
	})
}

func TestDrainStopsWhileRingRefills(t *testing.T) {
	const refills = 1000
	for _, d := range drainers {
		t.Run(d.name, func(t *testing.T) {
			c := newTestCache[string, string](t, Config{MaxSize: 1024, ShardCount: 1, EvictionPolicy: SieveTinyLFU})
			closeIfPassed(t, c)
			s := c.shards[0]
			s.queue.wakeState.Store(1) // keep the worker parked

			pending := []chan struct{}{gateWrite(t, s)}
			done := make(chan struct{})
			go func() { d.drain(c, s); close(done) }()

			acked, returned := 0, false
			for !returned && acked < refills {
				// Refill before each ack so the ring stays nonempty until the drain returns.
				pending = append(pending, gateWrite(t, s))
				select {
				case <-pending[0]:
					pending = pending[1:]
					acked++
				case <-done:
					returned = true
				}
			}
			if returned && acked == 0 {
				t.Fatalf("%s returned without applying the write queued before it started", d.name)
			}
			if !returned {
				t.Errorf("%s still held drainMu after %d refills", d.name, refills)
				for len(pending) > 0 { // let the drainer reach an empty ring
					<-pending[0]
					pending = pending[1:]
				}
				<-done
				return
			}
			s.queue.wakeState.Store(0)
			s.queue.wakeConsumer()
			for _, ack := range pending {
				<-ack
			}
		})
	}
}

func TestBoundedDrainWakesWorker(t *testing.T) {
	for _, d := range drainers[1:] { // the worker rechecks the ring itself
		t.Run(d.name, func(t *testing.T) {
			c := newTestCache[string, string](t, Config{
				MaxSize: 1024, ShardCount: 1, EvictionPolicy: SieveTinyLFU, WriteBatchSize: 1,
			})
			closeIfPassed(t, c)
			s := c.shards[0]

			gate := queueGateAndSet(t, c)
			done := make(chan struct{})
			go func() { d.drain(c, s); close(done) }()
			waitFor(t, func() bool { return s.queue.tail.Load() == 1 })
			// Publish past the drain limit while tail still points to the first Set.
			queueSet(t, c, "later")
			awake := workerAwake(s)

			select {
			case <-gate:
			case <-done:
				t.Fatalf("%s returned without applying the gate", d.name)
			}
			<-done
			if awake {
				t.Fatal("the worker was woken during setup")
			}
			// Check the wake-up before Get has a chance to drain the write itself.
			if s.queue.wakeState.Load() == 0 && !s.queue.quiescent() {
				t.Fatalf("%s left a queued write with the worker asleep", d.name)
			}
			waitFor(t, func() bool {
				_, ok := c.Get("later")
				return ok
			})
		})
	}
}

func TestSyncMutateWakesWorkerAfterLaterDrain(t *testing.T) {
	c := newTestCache[string, string](t, Config{MaxSize: 1024, ShardCount: 1, EvictionPolicy: LRU})
	closeIfPassed(t, c)
	s := c.shards[0]

	publishA := claimSlot(t, s.queue)
	done := make(chan struct{})
	go func() { c.Delete("k"); close(done) }()
	// Let Delete start draining before queuing the write that should need a wake-up.
	waitFor(t, func() bool {
		if s.drainMu.TryLock() {
			s.drainMu.Unlock()
			return false
		}
		return true
	})
	queueSet(t, c, "later") // slot 1, published while tail is 0
	awake := workerAwake(s)
	publishA(writeCommand[string, string]{op: writeBarrier})
	<-done
	if awake {
		t.Fatal("the worker was woken during setup")
	}
	// LRU reads leave the write queue alone.
	waitFor(t, func() bool {
		_, ok := c.Get("later")
		return ok
	})
}

func TestBarrierBehindSyncMutateDrain(t *testing.T) {
	barriers := []struct {
		name string
		call func(c *Cache[string, string])
	}{
		{"Sync", func(c *Cache[string, string]) { _ = c.Sync() }},
		{"Clear", func(c *Cache[string, string]) { c.Clear() }},
		{"Close", func(c *Cache[string, string]) { _ = c.Close() }},
	}
	for _, b := range barriers {
		t.Run(b.name, func(t *testing.T) {
			c := newTestCache[string, string](t, Config{
				MaxSize: 1024, ShardCount: 1, EvictionPolicy: LRU, WriteBatchSize: 1,
			})
			closeIfPassed(t, c)
			s := c.shards[0]

			gate := queueGateAndSet(t, c)
			deleted := make(chan struct{})
			go func() { c.Delete("k"); close(deleted) }()
			waitFor(t, func() bool { return s.queue.tail.Load() == 1 })

			acked := make(chan struct{})
			go func() { b.call(c); close(acked) }()
			// Put the barrier past Delete's drain limit while Delete still holds the gate.
			waitFor(t, func() bool { return s.queue.head.Load() == 3 })
			awake := workerAwake(s)

			<-gate
			<-deleted
			if awake {
				t.Fatal("the worker was woken during setup")
			}
			select {
			case <-acked:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s waited 5s for its barrier after Delete returned", b.name)
			}
		})
	}
}

func TestDeleteGetsDrainLockWhileRingRefills(t *testing.T) {
	c := newTestCache[string, string](t, Config{MaxSize: 1024, ShardCount: 1, EvictionPolicy: SieveTinyLFU})
	closeIfPassed(t, c)
	s := c.shards[0]

	pending := []chan struct{}{gateWrite(t, s)}
	waitFor(t, func() bool { return s.queue.tail.Load() != 0 })
	deleted := make(chan struct{})
	go func() { c.Delete("k"); close(deleted) }()

	timeout := time.After(10 * time.Second)
refill:
	for {
		pending = append(pending, gateWrite(t, s))
		select {
		case <-pending[0]:
			pending = pending[1:]
		case <-deleted:
			break refill
		case <-timeout:
			t.Error("Delete waited 10s for drainMu while the worker drained")
			break refill
		}
		// On one P, yield so the ack handoff between this goroutine and the worker doesn't starve Delete.
		runtime.Gosched()
	}
	for _, ack := range pending {
		<-ack
	}
	<-deleted
}
