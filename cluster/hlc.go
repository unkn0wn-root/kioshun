package cluster

import (
	"sync"
	"time"
)

const (
	hlcLogicalBits = 16     // total logical bits (low end of the 64-bit HLC)
	hlcNodeBits    = 8      // low bits reserved for nodeID (0..255)
	hlcSeqBits     = 16 - 8 // remaining logical bits for per-ms sequence
	hlcNodeMask    = (1 << hlcNodeBits) - 1
	hlcSeqMask     = (1 << hlcSeqBits) - 1
)

// hlc is a 64-bit Hybrid Logical Clock:
// layout:
// [48 bits physical millis][hlcSeqBits seq][hlcNodeBits nodeID].
type hlc struct {
	mu     sync.Mutex
	physMS int64
	seq    uint16
	nodeID uint16
}

// newHLC constructs an HLC that embeds a per-node ID in the low logical bits.
// nodeID is masked to hlcNodeBits (e.g., 8 bits -> 0..255).
func newHLC(nodeID uint16) *hlc {
	return &hlc{nodeID: nodeID & hlcNodeMask}
}

// Next returns a strictly monotonic timestamp for local events.
// yields strictly monotonic timestamps for local events.
func (h *hlc) Next() uint64 {
	now := time.Now().UnixMilli()

	h.mu.Lock()
	defer h.mu.Unlock()

	if now > h.physMS {
		h.physMS = now
		h.seq = 0
	} else {
		if h.seq < hlcSeqMask {
			h.seq++
		} else {
			h.physMS++
			h.seq = 0
		}
	}
	v := packHLC(h.physMS, h.seq, h.nodeID)
	return v
}

// Observe incorporates a remote HLC into our state to avoid regressions.
// After Observe(remote), a subsequent Next() will be strictly > remote (monotonic).
func (h *hlc) Observe(remote uint64) {
	rp, rlog := unpackHLC(remote)
	rseq, _ := splitLogical(rlog)
	now := time.Now().UnixMilli()

	h.mu.Lock()
	defer h.mu.Unlock()

	phys := maxOf(h.physMS, now, rp)

	switch {
	case phys == rp && phys == h.physMS:
		target := h.seq
		if rseq > target {
			target = rseq
		}

		newSeq := target + 1
		if newSeq > hlcSeqMask {
			h.physMS = phys + 1
			h.seq = 0
		} else {
			h.physMS = phys
			h.seq = newSeq
		}
	case phys == rp && phys > h.physMS:
		newSeq := rseq + 1
		if newSeq > hlcSeqMask {
			h.physMS = phys + 1
			h.seq = 0
		} else {
			h.physMS = phys
			h.seq = newSeq
		}
	case phys == h.physMS && phys > rp:
		if h.seq < hlcSeqMask {
			h.seq++
		} else {
			h.physMS++
			h.seq = 0
		}

	default:
		h.physMS = phys
		h.seq = 0
	}

}

// packHLC encodes physical milliseconds and a combined (seq,nodeID) into a 64-bit HLC.
func packHLC(physMS int64, seq uint16, nodeID uint16) uint64 {
	logical := ((seq & hlcSeqMask) << hlcNodeBits) | (nodeID & hlcNodeMask)
	return (uint64(physMS) << hlcLogicalBits) | uint64(logical)
}

// unpackHLC decodes a 64-bit HLC into physical milliseconds and the 16-bit logical.
func unpackHLC(ts uint64) (physMS int64, logical uint16) {
	return int64(ts >> hlcLogicalBits), uint16(ts & ((1 << hlcLogicalBits) - 1))
}

// splitLogical splits the 16-bit logical field into (seq, nodeID).
func splitLogical(logical uint16) (seq uint16, nodeID uint16) {
	seq = (logical >> hlcNodeBits) & hlcSeqMask
	nodeID = logical & hlcNodeMask
	return
}

func maxOf(a, b, c int64) int64 {
	if b > a {
		a = b
	}
	if c > a {
		a = c
	}
	return a
}
