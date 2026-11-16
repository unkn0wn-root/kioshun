//go:build go1.25

package kioshun

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// The bubble's clock does not move, so a worker that waits on a timer fails.
func TestOnRemove_DeliveryWithoutWaiting(t *testing.T) {
	t.Run("removals are delivered without the clock moving", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			const burst = 300 // enough to make a batching worker wait
			var log removalLog
			hold := make(chan struct{})
			c, err := New(Config{MaxSize: 1, ShardCount: 1, EvictionPolicy: LRU},
				WithOnRemove(func(k int, v string, reason RemovalReason) {
					if k == 0 {
						<-hold
					}
					log.onRemove(k, v, reason)
				}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer c.Close()

			var want []removalEvent
			deliver := func(k int) {
				want = append(want, removalEvent{key: k, value: valFor(k), reason: RemovedCapacity})
			}
			start := time.Now()
			mustSet(t, c, 0)
			mustSet(t, c, 1) // evicts 0 and holds the worker
			synctest.Wait()
			for k := 2; k <= burst+1; k++ {
				mustSet(t, c, k) // evicts k-1 while the worker is held
			}
			close(hold)
			synctest.Wait()
			for k := range burst + 1 {
				deliver(k)
			}
			if got := log.snapshot(); !slices.Equal(got, want) {
				t.Fatalf("got %d listener calls after the burst, want %d", len(got), len(want))
			}
			for k := burst + 2; k < burst+6; k++ {
				mustSet(t, c, k) // evicts k-1
				synctest.Wait()
				deliver(k - 1)
				if got := log.snapshot(); !slices.Equal(got, want) {
					t.Fatalf("after evicting %d: got %d listener calls, want %d", k-1, len(got), len(want))
				}
			}
			if d := time.Since(start); d != 0 {
				t.Fatalf("clock moved by %v", d)
			}
		})
	})
}
