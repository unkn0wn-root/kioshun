package cluster

import (
	"testing"
	"time"
)

func TestMembershipIntegrateAlivePrune(t *testing.T) {
	m := newMembership()
	now := time.Now().UnixNano()

	// integrate gossip from A, referencing B as known peer.
	m.integrate(NodeID("A"), "A", []PeerInfo{{ID: "B", Addr: "B"}}, map[string]int64{"B": now}, 10, now)

	if m.epoch != 10 {
		t.Fatalf("epoch not updated: %d", m.epoch)
	}

	al := m.alive(now, 1*time.Second)
	got := make(map[NodeID]bool)
	for _, nm := range al {
		got[nm.ID] = true
	}

	if !got[NodeID("A")] || !got[NodeID("B")] {
		t.Fatalf("alive missing A or B: %+v", got)
	}

	// lower epoch should not decrease stored epoch.
	m.integrate(NodeID("A"), "A", nil, nil, 5, now)
	if m.epoch != 10 {
		t.Fatalf("epoch regressed: %d", m.epoch)
	}

	// make B stale and prune tombstones.
	m.mu.Lock()
	m.seen[NodeID("B")] = now - int64(10*time.Second)
	m.mu.Unlock()
	m.pruneTombstones(now, 5*time.Second)

	m.mu.RLock()
	_, okB := m.peers[NodeID("B")]
	m.mu.RUnlock()
	if okB {
		t.Fatalf("expected B to be pruned")
	}
}
