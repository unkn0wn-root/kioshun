package cluster

import (
	"sync"
	"time"
)

// hlc is a 64-bit Hybrid Logical Clock:
// layout: [48 bits physical millis][16 bits logical counter].
// Next() yields strictly monotonic timestamps for local events.
// Observe() integrates remote HLC values to avoid regressions during replication.
type hlc struct {
	mu      sync.Mutex
	physMS  int64  // last physical ms
	logical uint16 // last logical
}

func newHLC() *hlc { return &hlc{} }

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
	v := (uint64(h.physMS) << 16) | uint64(h.logical)
	h.mu.Unlock()
	return v
}

// Observe incorporates a remote HLC into our state to avoid regressions.
func (h *hlc) Observe(remote uint64) {
	rp := int64(remote >> 16)
	rl := uint16(remote & 0xFFFF)
	now := time.Now().UnixMilli()

	h.mu.Lock()
	// move to the max of (local, remote, now) with proper logical bump when ties.
	phys := h.physMS
	if now > phys {
		phys = now
	}
	if rp > phys {
		phys = rp
	}
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
