package cluster

import "testing"

func TestBufPoolGetPut(t *testing.T) {
	bp := newBufPool([]int{64, 128})

	// small buffer uses 64 bucket
	b := bp.get(50)
	if len(b) != 50 || cap(b) != 64 {
		t.Fatalf("unexpected buf: len=%d cap=%d", len(b), cap(b))
	}
	bp.put(b)

	// large buffer gets exact allocation (> largest bucket)
	big := bp.get(256)
	if len(big) != 256 || cap(big) != 256 {
		t.Fatalf("unexpected big buf: len=%d cap=%d", len(big), cap(big))
	}
}

func TestBufPoolClass(t *testing.T) {
	bp := newBufPool([]int{64, 128})
	if got := bp.class(1); got != 0 {
		t.Fatalf("class(1)=%d", got)
	}
	if got := bp.class(64); got != 0 {
		t.Fatalf("class(64)=%d", got)
	}
	if got := bp.class(65); got != 1 {
		t.Fatalf("class(65)=%d", got)
	}
	if got := bp.class(129); got != -1 {
		t.Fatalf("class(129)=%d", got)
	}
}
