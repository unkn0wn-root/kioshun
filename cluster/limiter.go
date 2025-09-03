package cluster

import (
	"sync"
	"time"
)

type rateLimiter struct {
	mu       sync.Mutex
	max      int
	tokens   int
	interval time.Duration
	stopCh   chan struct{}
}

// newRateLimiter implements a simple token bucket with fixed window refill.
func newRateLimiter(max int, interval time.Duration) *rateLimiter {
	rl := &rateLimiter{
		max:      max,
		tokens:   max,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
	go rl.refill()
	return rl
}

// refill resets the available tokens to max at fixed intervals.
func (r *rateLimiter) refill() {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.mu.Lock()
			r.tokens = r.max
			r.mu.Unlock()
		case <-r.stopCh:
			return
		}
	}
}

// Allow consumes a token if available; returns false when rate-limited.
func (r *rateLimiter) Allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tokens <= 0 {
		return false
	}
	r.tokens--
	return true
}

// Stop terminates the refill goroutine.
func (r *rateLimiter) Stop() { close(r.stopCh) }
