//go:build !race

package kioshun

import (
	"runtime"
	"testing"
	"time"
)

// Allocation counts are not stable under -race.

// The producer waits for each round, so the count does not depend on scheduling.
func TestOnRemove_ChurnAllocs(t *testing.T) {
	const warmup, rounds, perRound = 100, 2000, 64
	v := &churnValue{}

	bytesPerSet := func(listen bool) float64 {
		delivered := make(chan struct{}, 1)
		var opts []Option[int, *churnValue]
		if listen {
			opts = append(opts, WithOnRemove(func(k int, _ *churnValue, _ RemovalReason) {
				if k%perRound == perRound-1 {
					delivered <- struct{}{}
				}
			}))
		}
		c, err := New(Config{MaxSize: 1, ShardCount: 1, EvictionPolicy: LRU}, opts...)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer c.Close()
		if err := c.Set(0, v, NoExpiration); err != nil {
			t.Fatalf("Set: %v", err)
		}
		k := 1
		deadline := time.After(10 * time.Second)
		run := func(rounds int) {
			for range rounds {
				// Each Set evicts the previous key.
				for range perRound {
					if err := c.Set(k, v, NoExpiration); err != nil {
						t.Fatalf("Set(%d): %v", k, err)
					}
					k++
				}
				if listen {
					select {
					case <-delivered:
					case <-deadline:
						t.Fatalf("round ending at key %d not delivered within 10s", k-1)
					}
				}
			}
		}
		run(warmup)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		run(rounds)
		runtime.ReadMemStats(&after)
		return float64(after.TotalAlloc-before.TotalAlloc) / (rounds * perRound)
	}

	extra := bytesPerSet(true) - bytesPerSet(false)
	t.Logf("a noop listener adds %.2f B per Set", extra)
	if extra > 8 {
		t.Fatalf("a noop listener adds %.2f B per Set, want <= 8", extra)
	}
}
