package kioshun

import (
	"runtime"
	"strconv"
	"testing"
)

func TestDrainClearsBatch(t *testing.T) {
	for _, d := range drainers {
		// Three writes give us full batches, a short final batch, and a single short batch.
		for _, batch := range []int{1, 2, 4} {
			t.Run(d.name+"/batch="+strconv.Itoa(batch), func(t *testing.T) {
				c := newTestCache[string, string](t, Config{
					MaxSize: 1024, ShardCount: 1, EvictionPolicy: SieveTinyLFU, WriteBufferSize: 4, WriteBatchSize: batch,
				})
				closeIfPassed(t, c)
				s := c.shards[0]
				parkWorker(t, s)
				for i := range 3 {
					queueSet(t, c, "k"+strconv.Itoa(i))
				}
				d.drain(c, s)
				if got := s.queue.tail.Load(); got != 3 {
					t.Fatalf("%s drained to tail %d, want 3", d.name, got)
				}
				for i, cmd := range s.writeBatch {
					if cmd != (writeCommand[string, string]{}) {
						t.Errorf("after %s, batch entry %d holds the write to %q", d.name, i, cmd.key)
					}
				}
			})
		}
	}
}

func TestDeleteReleasesQueuedValues(t *testing.T) {
	c := newTestCache[string, *weakValue](t, Config{
		MaxSize: 1024, ShardCount: 1, EvictionPolicy: LRU, WriteBufferSize: 4, WriteBatchSize: 4,
	})
	t.Cleanup(func() { c.Close() }) // registered first so cleanup releases the worker before closing
	parkWorker(t, c.shards[0])
	ptrs := queueWeakValues(t, c, 3)
	// The first Delete drains all three writes while the worker is parked.
	for i := range ptrs {
		if !c.Delete("k" + strconv.Itoa(i)) {
			t.Fatalf("Delete(k%d) found nothing", i)
		}
	}
	runtime.GC()
	for i, p := range ptrs {
		if p.Value() != nil {
			t.Errorf("after Delete, the value of write %d is still reachable", i)
		}
	}
	runtime.KeepAlive(c)
}
