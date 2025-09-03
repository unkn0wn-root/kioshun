package cluster

import (
	"sync"
	"time"
)

const (
	hlcLogicalBits = 16
	hlcLogicalMask = (1 << hlcLogicalBits) - 1
)

// packHLC encodes physical milliseconds and logical counter into a 64-bit HLC.
func packHLC(physMS int64, logical uint16) uint64 {
	return (uint64(physMS) << hlcLogicalBits) | uint64(logical)
}

// unpackHLC decodes a 64-bit HLC into physical milliseconds and logical.
func unpackHLC(ts uint64) (physMS int64, logical uint16) {
	return int64(ts >> hlcLogicalBits), uint16(ts & hlcLogicalMask)
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

// hlc is a 64-bit Hybrid Logical Clock:
// layout: [48 bits physical millis][16 bits logical counter].
// Next() yields strictly monotonic timestamps for local events.
// Observe() integrates remote HLC values to avoid regressions during replication.
type hlc struct {
	mu      sync.Mutex
	physMS  int64  // last physical ms
	logical uint16 // last logical
}

// newHLC constructs an HLC initialized to zero state.
func newHLC() *hlc {
	return &hlc{}
}

// Next returns a strictly monotonic timestamp for local events.
func (h *hlc) Next() uint64 {
	now := time.Now().UnixMilli()
	h.mu.Lock()
	if now > h.physMS {
		h.physMS = now
		h.logical = 0
	} else {
		h.logical++
	}
	v := packHLC(h.physMS, h.logical)
	h.mu.Unlock()
	return v
}

// Observe incorporates a remote HLC into our state to avoid regressions.
func (h *hlc) Observe(remote uint64) {
	rp, rl := unpackHLC(remote)
	now := time.Now().UnixMilli()

	h.mu.Lock()
	phys := maxOf(h.physMS, now, rp)
	switch {
	case phys == rp && phys == h.physMS:
		if rl >= h.logical {
			h.logical = rl + 1
		} else {
			h.logical++ // both equal phys; bump past local logical too
		}
		h.physMS = phys
	case phys == rp && phys > h.physMS:
		h.physMS = phys
		h.logical = rl + 1
	case phys == h.physMS && phys > rp:
		h.logical++ // local phys dominates; ensure monotone logical
	default:
		// phys == now and > both; reset logical
		h.physMS = phys
		h.logical = 0
	}
	h.mu.Unlock()
}
