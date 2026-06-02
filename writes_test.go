package kioshun

import "testing"

func waitForWrites[K comparable, V any](t testing.TB, c *Cache[K, V]) {
	t.Helper()
	if err := c.Wait(); err != nil {
		t.Fatalf("wait for async writes: %v", err)
	}
}
