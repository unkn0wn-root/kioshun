package kioshun

// Weigher reports the relative capacity cost for a cache entry. The default
// cost is 1, preserving entry count when MaxCost is unset.
type Weigher[K comparable, V any] func(K, V) int64

// WithWeigher configures typed item weights for MaxCost enforcement and
// cost-aware SieveTinyLFU admission. A nil weigher is ignored.
func WithWeigher[K comparable, V any](weigher Weigher[K, V]) Option[K, V] {
	return func(c *Cache[K, V]) {
		if weigher != nil {
			c.weigher = weigher
			c.trackCost = true
		}
	}
}
