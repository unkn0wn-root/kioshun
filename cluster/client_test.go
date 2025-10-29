package cluster

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	cache "github.com/unkn0wn-root/kioshun"
)

func TestNodeGetServesReplicaLocalHits(t *testing.T) {
	cfg := Default()
	cfg.ID = NodeID("node-2")
	cfg.BindAddr = ":0"
	cfg.PublicURL = "node-2"
	cfg.ReplicationFactor = 3
	cfg.WriteConcern = 2

	local := cache.NewWithDefaults[string, []byte]()
	t.Cleanup(func() { _ = local.Close() })

	n := NewNode[string, []byte](cfg, StringKeyCodec[string]{}, local, BytesCodec{})

	r := newRing(cfg.ReplicationFactor)
	r.nodes = []*nodeMeta{
		newMeta(NodeID("node-1"), "node-1"),
		newMeta(NodeID("node-2"), "node-2"),
		newMeta(NodeID("node-3"), "node-3"),
	}
	n.ring.Store(r)

	var key string
	for i := 0; i < 10000; i++ {
		candidate := fmt.Sprintf("key-%d", i)
		owners := r.ownersFromKeyHash(n.hash64Of(candidate))
		if len(owners) == 0 {
			continue
		}
		hasSelf := false
		for _, o := range owners {
			if o.ID == cfg.ID {
				hasSelf = true
				break
			}
		}
		if hasSelf && owners[0].ID != cfg.ID {
			key = candidate
			break
		}
	}
	if key == "" {
		t.Fatal("unable to find key where node is replica but not primary owner")
	}

	if err := n.local.Set(key, []byte("value"), cache.NoExpiration); err != nil {
		t.Fatalf("set local value: %v", err)
	}

	got, ok, err := n.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if !ok {
		t.Fatalf("expected local hit for key %q", key)
	}
	if !bytes.Equal(got, []byte("value")) {
		t.Fatalf("unexpected value %q", string(got))
	}
}
