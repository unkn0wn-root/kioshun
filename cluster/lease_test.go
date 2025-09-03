package cluster

import (
	"context"
	"testing"
	"time"
)

func TestLeaseTableAcquireReleaseWait(t *testing.T) {
	lt := newLeaseTable(200 * time.Millisecond)
	defer lt.Stop()

	f1, acq1 := lt.acquire("k")
	if !acq1 || f1 == nil {
		t.Fatalf("expected first acquire to create inflight")
	}

	f2, acq2 := lt.acquire("k")
	if acq2 || f2 == nil || f2 != f1 {
		t.Fatalf("expected second acquire to wait on same inflight")
	}

	done := make(chan error, 1)
	go func() {
		done <- lt.wait(context.Background(), "k")
	}()

	time.Sleep(20 * time.Millisecond)
	lt.release("k", nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected wait error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("wait timed out")
	}
}

func TestLeaseTableTimeout(t *testing.T) {
	lt := newLeaseTable(50 * time.Millisecond)
	defer lt.Stop()

	_, acq := lt.acquire("x")
	if !acq {
		t.Fatalf("expected acquire")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := lt.wait(ctx, "x")
	if err == nil {
		t.Fatalf("expected timeout error")
	}
}
