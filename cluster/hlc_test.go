package cluster

import (
	"testing"
	"time"
)

func TestHLCNextMonotonic(t *testing.T) {
	h := newHLC()
	a := h.Next()
	b := h.Next()
	if b <= a {
		t.Fatalf("Next not monotonic: a=%d b=%d", a, b)
	}
}

func TestHLCObserveRemoteAhead(t *testing.T) {
	h := newHLC()
	_ = h.Next() // initialize
	rp := time.Now().UnixMilli() + 5
	remote := packHLC(rp, 3)
	h.Observe(remote)
	after := h.Next()
	ap, _ := unpackHLC(after)
	if ap < rp { // should catch up to remote physical time
		t.Fatalf("did not catch up: ap=%d rp=%d", ap, rp)
	}
}

func TestHLCObserveRemoteBehindNoRegression(t *testing.T) {
	h := newHLC()
	a := h.Next()
	ap, _ := unpackHLC(a)
	// remote behind current physical time
	remote := packHLC(ap-10, 0)
	h.Observe(remote)
	b := h.Next()
	if b <= a {
		t.Fatalf("regressed: a=%d b=%d", a, b)
	}
}
