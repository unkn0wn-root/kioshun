package cache

import "testing"

func waitForWrites[K comparable, V any](t testing.TB, c *InMemoryCache[K, V]) {
	t.Helper()
	if err := c.Wait(); err != nil {
		t.Fatalf("wait for async writes: %v", err)
	}
}

func waitForMiddlewareWrites(t testing.TB, m *HTTPCacheMiddleware) {
	t.Helper()
	waitForWrites(t, m.cache)
}
