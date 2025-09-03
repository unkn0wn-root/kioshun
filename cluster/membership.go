package cluster

import (
	"sync"
	"time"
)

type membership struct {
	mu    sync.RWMutex
	peers map[NodeID]*nodeMeta
	seen  map[NodeID]int64
	epoch uint64
}

// newMembership creates an empty membership view with per-node metadata and
// last-seen timestamps used for liveness and ring construction.
func newMembership() *membership {
	return &membership{
		peers: make(map[NodeID]*nodeMeta),
		seen:  make(map[NodeID]int64),
	}
}

// snapshot returns copies of peers and seen maps along with the current epoch
// so callers can take a consistent view without holding locks.
func (m *membership) snapshot() (map[NodeID]*nodeMeta, map[NodeID]int64, uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p := make(map[NodeID]*nodeMeta, len(m.peers))
	for k, v := range m.peers {
		p[k] = v
	}

	s := make(map[NodeID]int64, len(m.seen))
	for k, v := range m.seen {
		s[k] = v
	}
	return p, s, m.epoch
}

// integrate merges gossip from a peer: updates address, seen timestamps,
// and tracks the highest epoch to detect cluster resyncs.
func (m *membership) integrate(from NodeID, addr string, peersAddrs []string, seen map[string]int64, epoch uint64, now int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if epoch > m.epoch {
		m.epoch = epoch
	}

	if _, ok := m.peers[from]; !ok {
		m.peers[from] = newMeta(from, addr)
	} else {
		m.peers[from].Addr = addr
	}

	m.seen[from] = now

	for _, a := range peersAddrs {
		id := NodeID(a)
		if _, ok := m.peers[id]; !ok {
			m.peers[id] = newMeta(id, a)
		}
	}

	// merge remote observations: keep the freshest timestamp per node.
	for k, ts := range seen {
		id := NodeID(k)
		if old, ok := m.seen[id]; !ok || ts > old {
			m.seen[id] = ts
		}
	}
}

// alive returns nodes not suspected within the given timeframe.
func (m *membership) alive(now int64, suspicionAfter time.Duration) []*nodeMeta {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*nodeMeta, 0, len(m.peers))
	threshold := now - suspicionAfter.Nanoseconds()
	for id, meta := range m.peers {
		if m.seen[id] >= threshold {
			out = append(out, meta)
		}
	}
	return out
}

// pruneTombstones removes nodes that have not been seen for tombstoneAfter.
func (m *membership) pruneTombstones(now int64, tombstoneAfter time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	threshold := now - tombstoneAfter.Nanoseconds()
	for id := range m.peers {
		if ts, ok := m.seen[id]; ok && ts < threshold {
			delete(m.peers, id)
			delete(m.seen, id)
		}
	}
}

// ensure ensures a node entry exists and bumps its seen timestamp to now.
func (m *membership) ensure(id NodeID, addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.peers[id]; !ok {
		m.peers[id] = newMeta(id, addr)
	}
	m.seen[id] = time.Now().UnixNano()
}
