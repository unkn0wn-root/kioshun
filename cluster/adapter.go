package cluster

import (
	"context"
	"time"

	cache "github.com/unkn0wn-root/kioshun"
)

// DistributedCache adapts a running Node to the cache.Cache interface
// so existing code using kioshun's single-node Cache can switch to the
// clustered backend without invasive changes. Methods that cannot be expressed
// cluster-wide (e.g., Clear, Size, Stats) operate on the local shard only.
//
// Note: the adapter does not expose context. It uses Node timeouts from Config
// (ReadTimeout/WriteTimeout) to bound remote calls. Errors on Get are treated
// as cache misses to preserve the original interface contract.
type DistributedCache[K comparable, V any] struct {
	n *Node[K, V]
}

// NewDistributedCache wraps a started Node and returns a cache.Cache
// compatible adapter. Call node.Start() before using the adapter, and Stop()
// (or Close() on the adapter) during shutdown.
func NewDistributedCache[K comparable, V any](n *Node[K, V]) *DistributedCache[K, V] {
	return &DistributedCache[K, V]{n: n}
}

func (a *DistributedCacheAdapter[K, V]) getCtx(write bool) (context.Context, context.CancelFunc) {
	to := a.n.cfg.Sec.ReadTimeout
	if write {
		to = a.n.cfg.Sec.WriteTimeout
	}
	if to <= 0 {
		to = 3 * time.Second
	}
	return context.WithTimeout(context.Background(), to)
}

// Set forwards to Node.Set with the configured write timeout.
func (a *DistributedCache[K, V]) Set(key K, value V, ttl time.Duration) error {
	ctx, cancel := a.getCtx(true)
	defer cancel()
	return a.n.Set(ctx, key, value, ttl)
}

// Get forwards to Node.Get with the configured read timeout. Errors are
// returned as cache misses to match the cache.Cache interface.
func (a *DistributedCache[K, V]) Get(key K) (V, bool) {
	ctx, cancel := a.getCtx(false)
	defer cancel()
	v, ok, err := a.n.Get(ctx, key)
	if err != nil {
		var zero V
		return zero, false
	}
	return v, ok
}

// Delete forwards to Node.Delete; returns true on success.
func (a *DistributedCache[K, V]) Delete(key K) bool {
	ctx, cancel := a.getCtx(true)
	defer cancel()
	return a.n.Delete(ctx, key) == nil
}

// Clear clears only the local in-memory shard.
// This does not broadcast a cluster-wide clear.
// Callers requiring global invalidation should implement an explicit protocol at a higher layer.
func (a *DistributedCache[K, V]) Clear() { a.n.local.Clear() }

// Size returns the size of the local shard only.
func (a *DistributedCache[K, V]) Size() int64 { return a.n.local.Size() }

// Stats returns statistics from the local shard only.
func (a *DistributedCache[K, V]) Stats() cache.Stats { return a.n.local.Stats() }

// Close stops the node and returns nil.
func (a *DistributedCache[K, V]) Close() error { a.n.Stop(); return nil }
