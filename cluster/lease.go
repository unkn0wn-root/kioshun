package cluster

import (
	"context"
	"sync"
	"time"
)

type inflight struct {
	ch  chan struct{}
	err error
	exp int64
}

// leaseTable provides per-key single-flight semantics with a TTL. The first
// goroutine acquires a lease and performs the work, others wait on the channel
// until the lease is released or times out.
type leaseTable struct {
	mu     sync.Mutex
	m      map[string]*inflight
	ttl    time.Duration
	stopCh chan struct{}
}

func newLeaseTable(ttl time.Duration) *leaseTable {
	t := &leaseTable{
		m:      make(map[string]*inflight),
		ttl:    ttl,
		stopCh: make(chan struct{}),
	}
	if ttl > 0 {
		go t.sweeper()
	}
	return t
}

func (t *leaseTable) acquire(key string) (*inflight, bool) {
	t.mu.Lock()
	if f, ok := t.m[key]; ok {
		t.mu.Unlock()
		return f, false
	}

	f := &inflight{ch: make(chan struct{}), exp: time.Now().Add(t.ttl).UnixNano()}
	t.m[key] = f
	t.mu.Unlock()
	return f, true
}

func (t *leaseTable) release(key string, err error) {
	t.mu.Lock()
	f, ok := t.m[key]
	if ok {
		delete(t.m, key)
	}
	t.mu.Unlock()

	if ok {
		f.err = err
		close(f.ch)
	}
}

func (t *leaseTable) wait(ctx context.Context, key string) error {
	t.mu.Lock()
	f := t.m[key]
	t.mu.Unlock()
	if f == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.ch:
		return f.err
	}
}

func (t *leaseTable) sweeper() {
	tick := time.NewTicker(t.ttl / 2)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			now := time.Now().UnixNano()
			t.mu.Lock()
			for k, f := range t.m {
				if f.exp > 0 && now >= f.exp {
					delete(t.m, k)
					f.err = ErrLeaseTimeout
					close(f.ch)
				}
			}
			t.mu.Unlock()
		case <-t.stopCh:
			return
		}
	}
}

func (t *leaseTable) Stop() {
	select {
	case <-t.stopCh:
		return
	default:
		close(t.stopCh)
	}
}
