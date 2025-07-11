package cache

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPCacheMiddleware_BasicCaching(t *testing.T) {
	config := DefaultMiddlewareConfig()
	config.DefaultTTL = 1 * time.Second

	middleware := NewHTTPCacheMiddleware(config)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"message": "test"}`))
	})

	wrappedHandler := middleware.Middleware(handler)

	// First request - should miss cache
	req1 := httptest.NewRequest("GET", "/test", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)

	if rec1.Header().Get("X-Cache") != "MISS" {
		t.Error("Expected cache miss on first request")
	}

	// Second request - should hit cache
	req2 := httptest.NewRequest("GET", "/test", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)

	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Error("Expected cache hit on second request")
	}

	if rec2.Body.String() != `{"message": "test"}` {
		t.Errorf("Expected cached response body, got %s", rec2.Body.String())
	}
}

func TestHTTPCacheMiddleware_TTLExpiration(t *testing.T) {
	config := DefaultMiddlewareConfig()
	config.DefaultTTL = 50 * time.Millisecond

	middleware := NewHTTPCacheMiddleware(config)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Middleware(handler)

	// First request
	req1 := httptest.NewRequest("GET", "/test", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)

	// Second request - should hit cache
	req2 := httptest.NewRequest("GET", "/test", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)

	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Error("Expected cache hit")
	}

	// Wait for TTL to expire
	time.Sleep(100 * time.Millisecond)

	// Third request - should miss cache due to expiration
	req3 := httptest.NewRequest("GET", "/test", nil)
	rec3 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec3, req3)

	if rec3.Header().Get("X-Cache") != "MISS" {
		t.Error("Expected cache miss after TTL expiration")
	}
}

func TestHTTPCacheMiddleware_CachePolicy(t *testing.T) {
	config := DefaultMiddlewareConfig()
	middleware := NewHTTPCacheMiddleware(config)
	defer middleware.Close()

	// Custom policy that only caches POST requests
	middleware.SetCachePolicy(func(r *http.Request, statusCode int, headers http.Header, body []byte) (bool, time.Duration) {
		return r.Method == "POST" && statusCode == 200, 1 * time.Hour
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Middleware(handler)

	// GET request - should not be cached
	req1 := httptest.NewRequest("GET", "/test", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)

	req2 := httptest.NewRequest("GET", "/test", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)

	if rec2.Header().Get("X-Cache") != "MISS" {
		t.Error("GET request should not be cached")
	}

	// POST request - should be cached
	req3 := httptest.NewRequest("POST", "/test", nil)
	rec3 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec3, req3)

	req4 := httptest.NewRequest("POST", "/test", nil)
	rec4 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec4, req4)

	if rec4.Header().Get("X-Cache") != "HIT" {
		t.Error("POST request should be cached")
	}
}

func TestHTTPCacheMiddleware_KeyGenerator(t *testing.T) {
	config := DefaultMiddlewareConfig()
	middleware := NewHTTPCacheMiddleware(config)
	defer middleware.Close()

	// Custom key generator that includes user ID
	middleware.SetKeyGenerator(KeyWithUserID("X-User-ID"))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Middleware(handler)

	// Request with User-ID: 1
	req1 := httptest.NewRequest("GET", "/test", nil)
	req1.Header.Set("X-User-ID", "1")
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)

	// Same request with User-ID: 1 - should hit cache
	req2 := httptest.NewRequest("GET", "/test", nil)
	req2.Header.Set("X-User-ID", "1")
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)

	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Error("Expected cache hit for same user")
	}

	// Request with User-ID: 2 - should miss cache
	req3 := httptest.NewRequest("GET", "/test", nil)
	req3.Header.Set("X-User-ID", "2")
	rec3 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec3, req3)

	if rec3.Header().Get("X-Cache") != "MISS" {
		t.Error("Expected cache miss for different user")
	}
}

func TestHTTPCacheMiddleware_Callbacks(t *testing.T) {
	config := DefaultMiddlewareConfig()
	middleware := NewHTTPCacheMiddleware(config)
	defer middleware.Close()

	var hitCount, missCount, setCount int

	middleware.OnHit(func(key string) {
		hitCount++
	})

	middleware.OnMiss(func(key string) {
		missCount++
	})

	middleware.OnSet(func(key string, ttl time.Duration) {
		setCount++
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Middleware(handler)

	// First request
	req1 := httptest.NewRequest("GET", "/test", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)

	if missCount != 1 || setCount != 1 || hitCount != 0 {
		t.Errorf("Expected 1 miss, 1 set, 0 hits, got %d miss, %d set, %d hits", missCount, setCount, hitCount)
	}

	// Second request
	req2 := httptest.NewRequest("GET", "/test", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)

	if missCount != 1 || setCount != 1 || hitCount != 1 {
		t.Errorf("Expected 1 miss, 1 set, 1 hit, got %d miss, %d set, %d hits", missCount, setCount, hitCount)
	}
}

func TestHTTPCacheMiddleware_CacheControlHeaders(t *testing.T) {
	config := DefaultMiddlewareConfig()
	middleware := NewHTTPCacheMiddleware(config)
	defer middleware.Close()

	tests := []struct {
		name         string
		cacheControl string
		expectCached bool
	}{
		{"no-cache-control", "", true},
		{"max-age-300", "max-age=300", true},
		{"no-cache", "no-cache", false},
		{"no-store", "no-store", false},
		{"private", "private", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.cacheControl != "" {
					w.Header().Set("Cache-Control", tt.cacheControl)
				}
				w.WriteHeader(200)
				w.Write([]byte("response"))
			})

			wrappedHandler := middleware.Middleware(handler)

			// First request
			req1 := httptest.NewRequest("GET", "/test-"+tt.name, nil)
			rec1 := httptest.NewRecorder()
			wrappedHandler.ServeHTTP(rec1, req1)

			// Second request
			req2 := httptest.NewRequest("GET", "/test-"+tt.name, nil)
			rec2 := httptest.NewRecorder()
			wrappedHandler.ServeHTTP(rec2, req2)

			cacheHeader := rec2.Header().Get("X-Cache")
			if tt.expectCached && cacheHeader != "HIT" {
				t.Errorf("Expected cache hit, got %s", cacheHeader)
			}
			if !tt.expectCached && cacheHeader == "HIT" {
				t.Error("Expected cache miss, got hit")
			}
		})
	}
}

func TestHTTPCacheMiddleware_NonCacheableMethods(t *testing.T) {
	config := DefaultMiddlewareConfig()
	middleware := NewHTTPCacheMiddleware(config)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Middleware(handler)

	methods := []string{"POST", "PUT", "DELETE", "PATCH"}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			// First request
			req1 := httptest.NewRequest(method, "/test", nil)
			rec1 := httptest.NewRecorder()
			wrappedHandler.ServeHTTP(rec1, req1)

			// Second request
			req2 := httptest.NewRequest(method, "/test", nil)
			rec2 := httptest.NewRecorder()
			wrappedHandler.ServeHTTP(rec2, req2)

			if rec2.Header().Get("X-Cache") != "MISS" {
				t.Errorf("%s request should not be cached", method)
			}
		})
	}
}

func TestHTTPCacheMiddleware_Stats(t *testing.T) {
	config := DefaultMiddlewareConfig()
	middleware := NewHTTPCacheMiddleware(config)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Middleware(handler)

	// First request (miss)
	req1 := httptest.NewRequest("GET", "/test", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)

	// Second request (hit)
	req2 := httptest.NewRequest("GET", "/test", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)

	stats := middleware.Stats()
	if stats.Hits != 1 || stats.Misses != 1 {
		t.Errorf("Expected 1 hit and 1 miss, got %d hits and %d misses", stats.Hits, stats.Misses)
	}

	if stats.HitRatio != 0.5 {
		t.Errorf("Expected hit ratio of 0.5, got %f", stats.HitRatio)
	}

	if stats.Size != 1 {
		t.Errorf("Expected cache size of 1, got %d", stats.Size)
	}
}

func TestHTTPCacheMiddleware_Clear(t *testing.T) {
	config := DefaultMiddlewareConfig()
	middleware := NewHTTPCacheMiddleware(config)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Middleware(handler)

	// Cache an item
	req1 := httptest.NewRequest("GET", "/test", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)

	// Verify it's cached
	req2 := httptest.NewRequest("GET", "/test", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)

	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Error("Expected cache hit before clear")
	}

	// Clear cache
	middleware.Clear()

	// Verify cache is cleared
	req3 := httptest.NewRequest("GET", "/test", nil)
	rec3 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec3, req3)

	if rec3.Header().Get("X-Cache") != "MISS" {
		t.Error("Expected cache miss after clear")
	}
}

func TestKeyGenerators(t *testing.T) {
	req1 := httptest.NewRequest("GET", "/test?a=1&b=2", nil)
	req1.Header.Set("Accept", "application/json")
	req1.Header.Set("X-User-ID", "123")

	req2 := httptest.NewRequest("GET", "/test?a=1&b=2", nil)
	req2.Header.Set("Accept", "application/xml")
	req2.Header.Set("X-User-ID", "456")

	t.Run("DefaultKeyGenerator", func(t *testing.T) {
		key1 := DefaultKeyGenerator(req1)
		key2 := DefaultKeyGenerator(req2)

		if key1 == key2 {
			t.Error("Different Accept headers should generate different keys")
		}

		if len(key1) != 32 || len(key2) != 32 {
			t.Error("Generated keys should be 32 characters long (MD5 hash)")
		}
	})

	t.Run("KeyWithVaryHeaders", func(t *testing.T) {
		keyGen := KeyWithVaryHeaders([]string{"X-User-ID"})
		key1 := keyGen(req1)
		key2 := keyGen(req2)

		if key1 == key2 {
			t.Error("Different user IDs should generate different keys")
		}
	})

	t.Run("KeyWithoutQuery", func(t *testing.T) {
		keyGen := KeyWithoutQuery()

		req3 := httptest.NewRequest("GET", "/test", nil)
		req3.Header.Set("Accept", "application/json")

		key1 := keyGen(req1)
		key3 := keyGen(req3)

		if key1 != key3 {
			t.Error("Same path with different query should generate same key")
		}
	})

	t.Run("KeyWithUserID", func(t *testing.T) {
		keyGen := KeyWithUserID("X-User-ID")
		key1 := keyGen(req1)
		key2 := keyGen(req2)

		if key1 == key2 {
			t.Error("Different user IDs should generate different keys")
		}
	})
}

func TestCachePolicies(t *testing.T) {
	headers := make(http.Header)
	body := []byte("test response")

	t.Run("AlwaysCache", func(t *testing.T) {
		policy := AlwaysCache(5 * time.Minute)

		shouldCache, ttl := policy(nil, 200, headers, body)
		if !shouldCache || ttl != 5*time.Minute {
			t.Error("AlwaysCache should cache successful responses")
		}

		shouldCache, _ = policy(nil, 500, headers, body)
		if shouldCache {
			t.Error("AlwaysCache should not cache error responses")
		}
	})

	t.Run("NeverCache", func(t *testing.T) {
		policy := NeverCache()

		shouldCache, _ := policy(nil, 200, headers, body)
		if shouldCache {
			t.Error("NeverCache should never cache")
		}
	})

	t.Run("CacheByContentType", func(t *testing.T) {
		rules := map[string]time.Duration{
			"application/json": 10 * time.Minute,
			"text/html":        5 * time.Minute,
		}
		policy := CacheByContentType(rules, 1*time.Minute)

		headers.Set("Content-Type", "application/json")
		shouldCache, ttl := policy(nil, 200, headers, body)
		if !shouldCache || ttl != 10*time.Minute {
			t.Error("Should cache JSON with specific TTL")
		}

		headers.Set("Content-Type", "text/plain")
		shouldCache, ttl = policy(nil, 200, headers, body)
		if !shouldCache || ttl != 1*time.Minute {
			t.Error("Should cache unknown types with default TTL")
		}
	})

	t.Run("CacheBySize", func(t *testing.T) {
		policy := CacheBySize(5, 20, 1*time.Hour)

		// Too small
		shouldCache, _ := policy(nil, 200, headers, []byte("hi"))
		if shouldCache {
			t.Error("Should not cache responses too small")
		}

		// Just right
		shouldCache, ttl := policy(nil, 200, headers, []byte("hello world"))
		if !shouldCache || ttl != 1*time.Hour {
			t.Error("Should cache responses of correct size")
		}

		// Too large
		shouldCache, _ = policy(nil, 200, headers, []byte("this is a very long response that exceeds the limit"))
		if shouldCache {
			t.Error("Should not cache responses too large")
		}
	})
}

func TestExtractMaxAge(t *testing.T) {
	tests := []struct {
		cacheControl string
		expected     time.Duration
	}{
		{"", 0},
		{"max-age=300", 300 * time.Second},
		{"public, max-age=600", 600 * time.Second},
		{"no-cache, max-age=300", 300 * time.Second},
		{"max-age=invalid", 0},
		{"no-cache", 0},
	}

	for _, tt := range tests {
		t.Run(tt.cacheControl, func(t *testing.T) {
			result := extractMaxAge(tt.cacheControl)
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestHTTPCacheMiddleware_PatternInvalidation(t *testing.T) {
	config := DefaultMiddlewareConfig()
	middleware := NewHTTPCacheMiddleware(config)
	defer middleware.Close()

	// Use path-based key generator for pattern matching
	middleware.SetKeyGenerator(KeyWithoutQuery())
	middleware.SetPathExtractor(PathExtractorFromKey)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response for " + r.URL.Path))
	})

	wrappedHandler := middleware.Middleware(handler)

	// Cache multiple paths
	paths := []string{"/api/users/", "/api/users/1", "/api/users/2/profile", "/api/posts/1", "/api/posts/2"}
	for _, path := range paths {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)

		if rec.Header().Get("X-Cache") != "MISS" {
			t.Errorf("Expected MISS for first request to %s", path)
		}
	}

	// Verify all paths are cached
	for _, path := range paths {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)

		if rec.Header().Get("X-Cache") != "HIT" {
			t.Errorf("Expected HIT for cached request to %s", path)
		}
	}

	// Test that invalidation properly triggers cache misses (HTTP behavior)
	t.Run("InvalidationTriggersHTTPMiss", func(t *testing.T) {
		// Cache an item
		req := httptest.NewRequest("GET", "/api/users/1", nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)

		// Verify it's cached
		req2 := httptest.NewRequest("GET", "/api/users/1", nil)
		rec2 := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec2, req2)
		if rec2.Header().Get("X-Cache") != "HIT" {
			t.Error("Expected cache hit before invalidation")
		}

		// Invalidate
		middleware.Invalidate("/api/users/1")

		// Verify HTTP cache miss
		req3 := httptest.NewRequest("GET", "/api/users/1", nil)
		rec3 := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec3, req3)

		if rec3.Header().Get("X-Cache") != "MISS" {
			t.Error("Expected cache miss after invalidation")
		}
	})
}

func TestHTTPCacheMiddleware_InvalidationEdgeCases(t *testing.T) {
	config := DefaultMiddlewareConfig()
	middleware := NewHTTPCacheMiddleware(config)
	defer middleware.Close()

	middleware.SetKeyGenerator(KeyWithoutQuery())
	middleware.SetPathExtractor(PathExtractorFromKey)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Middleware(handler)

	t.Run("DoubleInvalidation", func(t *testing.T) {
		// Cache an item
		req := httptest.NewRequest("GET", "/test/double", nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)

		// First invalidation
		removed1 := middleware.Invalidate("/test/double")
		if removed1 != 1 {
			t.Errorf("Expected 1 item removed on first invalidation, got %d", removed1)
		}

		// Second invalidation should remove 0 items
		removed2 := middleware.Invalidate("/test/double")
		if removed2 != 0 {
			t.Errorf("Expected 0 items removed on second invalidation, got %d", removed2)
		}
	})

	t.Run("InvalidateRootPath", func(t *testing.T) {
		// Cache root path
		req := httptest.NewRequest("GET", "/", nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)

		removed := middleware.Invalidate("/")
		if removed != 1 {
			t.Errorf("Expected 1 item removed for root path, got %d", removed)
		}
	})

	t.Run("InvalidateWithQuery", func(t *testing.T) {
		// Cache with query params
		req := httptest.NewRequest("GET", "/test?param=value", nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)

		// Should invalidate based on path only (query ignored)
		removed := middleware.Invalidate("/test")
		if removed != 1 {
			t.Errorf("Expected 1 item removed ignoring query params, got %d", removed)
		}
	})
}

func TestHTTPCacheMiddleware_CacheHitMissVerification(t *testing.T) {
	config := DefaultMiddlewareConfig()
	middleware := NewHTTPCacheMiddleware(config)
	defer middleware.Close()

	var hitCount, missCount int
	middleware.OnHit(func(key string) { hitCount++ })
	middleware.OnMiss(func(key string) { missCount++ })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Middleware(handler)

	tests := []struct {
		name           string
		requests       int
		expectedHits   int
		expectedMisses int
	}{
		{"SingleRequest", 1, 0, 1},
		{"TwoRequests", 2, 1, 1},
		{"MultipleRequests", 5, 4, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset counters
			hitCount, missCount = 0, 0
			middleware.Clear()

			path := "/test/" + tt.name
			for i := 0; i < tt.requests; i++ {
				req := httptest.NewRequest("GET", path, nil)
				rec := httptest.NewRecorder()
				wrappedHandler.ServeHTTP(rec, req)

				// Verify header on each request
				expectedHeader := "MISS"
				if i > 0 {
					expectedHeader = "HIT"
				}

				if rec.Header().Get("X-Cache") != expectedHeader {
					t.Errorf("Request %d: Expected %s, got %s", i+1, expectedHeader, rec.Header().Get("X-Cache"))
				}
			}

			if hitCount != tt.expectedHits {
				t.Errorf("Expected %d hits, got %d", tt.expectedHits, hitCount)
			}

			if missCount != tt.expectedMisses {
				t.Errorf("Expected %d misses, got %d", tt.expectedMisses, missCount)
			}
		})
	}
}

func TestHTTPCacheMiddleware_ConcurrentInvalidation(t *testing.T) {
	config := DefaultMiddlewareConfig()
	middleware := NewHTTPCacheMiddleware(config)
	defer middleware.Close()

	middleware.SetKeyGenerator(KeyWithoutQuery())
	middleware.SetPathExtractor(PathExtractorFromKey)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Middleware(handler)

	// Cache multiple items
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest("GET", fmt.Sprintf("/test/%d", i), nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)
	}

	// Concurrent invalidation
	var wg sync.WaitGroup
	totalRemoved := int64(0)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(start int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				removed := middleware.Invalidate(fmt.Sprintf("/test/%d", start*10+j))
				atomic.AddInt64(&totalRemoved, int64(removed))
			}
		}(i)
	}

	wg.Wait()

	if totalRemoved != 100 {
		t.Errorf("Expected 100 items removed total, got %d", totalRemoved)
	}

	// Verify all items are invalidated
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest("GET", fmt.Sprintf("/test/%d", i), nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)

		if rec.Header().Get("X-Cache") != "MISS" {
			t.Errorf("Expected MISS after concurrent invalidation for /test/%d", i)
		}
	}
}

func TestHTTPCacheMiddleware_BasicPatternInvalidation(t *testing.T) {
	config := DefaultMiddlewareConfig()
	middleware := NewHTTPCacheMiddleware(config)
	defer middleware.Close()

	middleware.SetKeyGenerator(KeyWithoutQuery())
	middleware.SetPathExtractor(PathExtractorFromKey)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Middleware(handler)

	// Cache a few simple paths
	paths := []string{"/api/users", "/api/posts"}
	for _, path := range paths {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)
	}

	// Test basic wildcard invalidation
	removed := middleware.Invalidate("/api/*")
	if removed != 2 {
		t.Errorf("Expected 2 items removed, got %d", removed)
	}

	// Verify invalidation worked at HTTP level
	for _, path := range paths {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)

		if rec.Header().Get("X-Cache") != "MISS" {
			t.Errorf("Expected MISS after invalidation for %s", path)
		}
	}
}
