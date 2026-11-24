package kioshun

import (
	"testing"
	"time"
)

func TestSyncMutateWaitsForEarlierSetAsync(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Cache[string, string]) error
		wantVal string
		wantOK  bool
	}{
		{
			name: "Delete",
			mutate: func(c *Cache[string, string]) error {
				c.Delete("k")
				return nil
			},
		},
		{
			name: "Set",
			mutate: func(c *Cache[string, string]) error {
				return c.Set("k", "v2", NoExpiration)
			},
			wantVal: "v2",
			wantOK:  true,
		},
		{
			name: "SetWithCallback",
			mutate: func(c *Cache[string, string]) error {
				return c.SetWithCallback("k", "v2", time.Hour, func(string, string) {})
			},
			wantVal: "v2",
			wantOK:  true,
		},
		{
			// A negative TTL other than NoExpiration makes Set remove the key.
			name: "SetExpired",
			mutate: func(c *Cache[string, string]) error {
				return c.Set("k", "v2", -time.Second)
			},
		},
	}
	for _, policy := range []EvictionPolicy{LRU, LFU, FIFO, SieveTinyLFU} {
		for _, tt := range tests {
			t.Run(policyName(policy)+"/"+tt.name, func(t *testing.T) {
				c, err := New[string, string](Config{MaxSize: 1024, ShardCount: 1, EvictionPolicy: policy})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				defer c.Close()

				q := c.shards[0].queue
				publishes := []func(writeCommand[string, string]){claimSlot(t, q), claimSlot(t, q)}
				if err := c.SetAsync("k", "v1", NoExpiration); err != nil {
					t.Fatalf("SetAsync: %v", err)
				}

				done := make(chan error, 1)
				go func() { done <- tt.mutate(c) }()

				// Publish one slot at a time to catch a mutation that skips an unpublished slot.
				finished := false
				for _, publish := range publishes {
					if !finished {
						select {
						case err = <-done:
							finished = true
						case <-time.After(50 * time.Millisecond):
						}
					}
					publish(writeCommand[string, string]{op: writeBarrier})
				}
				if !finished {
					err = <-done
				}
				if err != nil {
					t.Fatalf("%s: %v", tt.name, err)
				}
				if err := c.Sync(); err != nil {
					t.Fatalf("Sync: %v", err)
				}

				got, ok := c.Get("k")
				if ok != tt.wantOK || got != tt.wantVal {
					t.Fatalf("Get(k) = %q, %v after SetAsync(v1) then %s, want %q, %v", got, ok, tt.name, tt.wantVal, tt.wantOK)
				}
			})
		}
	}
}
