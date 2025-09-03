package cluster

import (
	"testing"
	"time"
)

func TestRateLimiterTokensAndRefill(t *testing.T) {
	rl := newRateLimiter(2, 50*time.Millisecond)
	defer rl.Stop()

	if !rl.Allow() || !rl.Allow() {
		t.Fatalf("expected first two allows to pass")
	}

	if rl.Allow() {
		t.Fatalf("expected third allow to be rate-limited")
	}

	time.Sleep(60 * time.Millisecond)
	if !rl.Allow() {
		t.Fatalf("expected allow after refill")
	}
}
