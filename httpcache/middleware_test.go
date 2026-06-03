package httpcache

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unkn0wn-root/kioshun"
)

func newTestMiddleware(t testing.TB, config Config) *Middleware {
	t.Helper()
	middleware, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return middleware
}

func waitForMiddlewareWrites(t testing.TB, m *Middleware) {
	t.Helper()
	if err := m.cache.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
}

// newPatternMiddleware builds a middleware wired for URL-pattern invalidation:
// path-preserving keys plus a PathExtractor, which enables the index and the
// backing cache's eviction listener.
func newPatternMiddleware(t testing.TB) *Middleware {
	t.Helper()
	config := DefaultConfig()
	config.PathExtractor = PathExtractorFromKey
	m := newTestMiddleware(t, config)
	m.SetKeyGenerator(KeyWithoutQuery())
	return m
}

// waitFor polls cond until it holds or a deadline passes. Eviction notifications
// (which reconcile the pattern index) are delivered asynchronously, so tests of
// index state cannot assert immediately.
func waitFor(t testing.TB, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

type committingResponseWriter struct {
	header    http.Header
	committed http.Header
	body      bytes.Buffer
	status    int
}

type informationalResponseWriter struct {
	header    http.Header
	committed http.Header
	body      bytes.Buffer
	statuses  []int
}

func newInformationalResponseWriter() *informationalResponseWriter {
	return &informationalResponseWriter{header: make(http.Header)}
}

func (w *informationalResponseWriter) Header() http.Header {
	return w.header
}

func (w *informationalResponseWriter) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
	if status >= 100 && status <= 199 && status != http.StatusSwitchingProtocols {
		return
	}
	if w.committed == nil {
		w.committed = w.header.Clone()
	}
}

func (w *informationalResponseWriter) Write(body []byte) (int, error) {
	if w.committed == nil {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(body)
}

func newCommittingResponseWriter() *committingResponseWriter {
	return &committingResponseWriter{header: make(http.Header)}
}

func (w *committingResponseWriter) Header() http.Header {
	return w.header
}

func (w *committingResponseWriter) WriteHeader(status int) {
	if w.committed != nil {
		return
	}
	w.status = status
	w.committed = w.header.Clone()
}

func (w *committingResponseWriter) Write(body []byte) (int, error) {
	if w.committed == nil {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(body)
}

func (w *committingResponseWriter) HeaderValue(name string) string {
	if w.committed != nil {
		return w.committed.Get(name)
	}
	return w.header.Get(name)
}

func TestNewDefaultsPartialConfigPerField(t *testing.T) {
	middleware := newTestMiddleware(t, Config{
		ShardCount: 32,
		DefaultTTL: time.Hour,
	})
	defer middleware.Close()

	stats := middleware.Stats()
	if stats.Capacity != DefaultConfig().MaxSize {
		t.Fatalf("capacity=%d, want default %d", stats.Capacity, DefaultConfig().MaxSize)
	}
	if stats.Shards != 32 {
		t.Fatalf("shards=%d, want 32", stats.Shards)
	}
	if middleware.maxBodySize != DefaultConfig().MaxBodySize {
		t.Fatalf("maxBodySize=%d, want default %d", middleware.maxBodySize, DefaultConfig().MaxBodySize)
	}

	req := httptest.NewRequest("GET", "/partial", nil)
	shouldCache, ttl := middleware.policy(req, http.StatusOK, http.Header{}, []byte("body"))
	if !shouldCache {
		t.Fatal("policy rejected default-size body")
	}
	if ttl != time.Hour {
		t.Fatalf("ttl=%v, want %v", ttl, time.Hour)
	}
}

func TestNewCanDisableCacheAndBodySizeLimits(t *testing.T) {
	config := DefaultConfig()
	config.DisableCacheSizeLimit = true
	config.DisableBodySizeLimit = true
	config.MaxBodySize = 0

	middleware := newTestMiddleware(t, config)
	defer middleware.Close()

	if capacity := middleware.Stats().Capacity; capacity != 0 {
		t.Fatalf("capacity=%d, want unlimited capacity 0", capacity)
	}
	if middleware.limitBody {
		t.Fatal("limitBody=true, want false")
	}

	largeBody := bytes.Repeat([]byte("x"), int(DefaultConfig().MaxBodySize)+1)
	req := httptest.NewRequest("GET", "/unlimited", nil)
	shouldCache, ttl := middleware.policy(req, http.StatusOK, http.Header{}, largeBody)
	if !shouldCache {
		t.Fatal("policy rejected large body with body size limit disabled")
	}
	if ttl != DefaultConfig().DefaultTTL {
		t.Fatalf("ttl=%v, want %v", ttl, DefaultConfig().DefaultTTL)
	}
}

func TestNewRejectsNegativeLimitsEvenWhenDisabled(t *testing.T) {
	if _, err := New(Config{MaxSize: -1, DisableCacheSizeLimit: true}); err == nil {
		t.Fatal("New() accepted negative MaxSize with cache size limit disabled")
	}
	if _, err := New(Config{MaxBodySize: -1, DisableBodySizeLimit: true}); err == nil {
		t.Fatal("New() accepted negative MaxBodySize with body size limit disabled")
	}
}

func TestDefaultCachePolicyAllowsNoExpirationDefaultTTL(t *testing.T) {
	policy := DefaultCachePolicy(Config{DefaultTTL: kioshun.NoExpiration})
	req := httptest.NewRequest("GET", "/no-expiration", nil)

	shouldCache, ttl := policy(req, http.StatusOK, http.Header{}, []byte("body"))
	if !shouldCache {
		t.Fatal("DefaultCachePolicy rejected cacheable response")
	}
	if ttl != kioshun.NoExpiration {
		t.Fatalf("ttl=%v, want NoExpiration", ttl)
	}
}

func TestDefaultCachePolicyDefaultsZeroConfig(t *testing.T) {
	policy := DefaultCachePolicy(Config{})
	req := httptest.NewRequest("GET", "/zero", nil)

	shouldCache, ttl := policy(req, http.StatusOK, http.Header{}, []byte("body"))
	if !shouldCache {
		t.Fatal("DefaultCachePolicy(Config{}) rejected non-empty body")
	}
	if ttl != DefaultConfig().DefaultTTL {
		t.Fatalf("ttl=%v, want %v", ttl, DefaultConfig().DefaultTTL)
	}
}

func TestHTTPCacheMiddleware_MissHeaderBeforeCommittedResponse(t *testing.T) {
	middleware := newTestMiddleware(t, DefaultConfig())
	defer middleware.Close()

	wrappedHandler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("response"))
	}))

	req := httptest.NewRequest("GET", "/miss", nil)
	rec := newCommittingResponseWriter()
	wrappedHandler.ServeHTTP(rec, req)
	waitForMiddlewareWrites(t, middleware)

	if rec.HeaderValue("X-Cache") != "MISS" {
		t.Fatalf("X-Cache=%q, want MISS", rec.HeaderValue("X-Cache"))
	}
}

func TestHTTPCacheMiddleware_CapturesImplicitStatusHeaders(t *testing.T) {
	middleware := newTestMiddleware(t, DefaultConfig())
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("response"))
	})
	wrappedHandler := middleware.Wrap(handler)

	req1 := httptest.NewRequest("GET", "/implicit", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)
	waitForMiddlewareWrites(t, middleware)

	req2 := httptest.NewRequest("GET", "/implicit", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec2.Code)
	}
	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("X-Cache=%q, want HIT", rec2.Header().Get("X-Cache"))
	}
	if ct := rec2.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("Content-Type=%q, want text/plain", ct)
	}
}

func TestHTTPCacheMiddleware_IgnoresConfiguredHeaders(t *testing.T) {
	middleware := newTestMiddleware(t, DefaultConfig())
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().Format(time.RFC1123))
		w.Header().Set("X-Request-Id", "request-1")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("response"))
	})
	wrappedHandler := middleware.Wrap(handler)

	req1 := httptest.NewRequest("GET", "/ignored", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)
	waitForMiddlewareWrites(t, middleware)

	req2 := httptest.NewRequest("GET", "/ignored", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)

	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("X-Cache=%q, want HIT", rec2.Header().Get("X-Cache"))
	}
	if rec2.Header().Get("X-Request-Id") != "" {
		t.Fatalf("X-Request-Id=%q, want empty", rec2.Header().Get("X-Request-Id"))
	}
	if rec2.Header().Get("Date") != "" {
		t.Fatalf("Date=%q, want empty", rec2.Header().Get("Date"))
	}
	if rec2.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("Content-Type=%q, want text/plain", rec2.Header().Get("Content-Type"))
	}
}

func TestHTTPCacheMiddleware_LargeBodyIsNotCached(t *testing.T) {
	config := DefaultConfig()
	config.MaxBodySize = 4
	middleware := newTestMiddleware(t, config)
	defer middleware.Close()

	var setCount int
	middleware.OnSet(func(string, time.Duration) { setCount++ })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("12345"))
	})
	wrappedHandler := middleware.Wrap(handler)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/large", nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)
		waitForMiddlewareWrites(t, middleware)
		if rec.Header().Get("X-Cache") != "MISS" {
			t.Fatalf("request %d X-Cache=%q, want MISS", i+1, rec.Header().Get("X-Cache"))
		}
	}
	if setCount != 0 {
		t.Fatalf("setCount=%d, want 0", setCount)
	}
}

func TestHTTPCacheMiddleware_FlushDisablesCaching(t *testing.T) {
	middleware := newTestMiddleware(t, DefaultConfig())
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("part"))
		w.(http.Flusher).Flush()
		w.Write([]byte("tail"))
	})
	wrappedHandler := middleware.Wrap(handler)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/stream", nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)
		waitForMiddlewareWrites(t, middleware)
		if rec.Header().Get("X-Cache") != "MISS" {
			t.Fatalf("request %d X-Cache=%q, want MISS", i+1, rec.Header().Get("X-Cache"))
		}
	}
}

func TestHTTPCacheMiddleware_InformationalStatusDoesNotCommitFinalStatus(t *testing.T) {
	middleware := newTestMiddleware(t, DefaultConfig())
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("created"))
	})
	wrappedHandler := middleware.Wrap(handler)

	req1 := httptest.NewRequest("GET", "/early-hints", nil)
	rec1 := newInformationalResponseWriter()
	wrappedHandler.ServeHTTP(rec1, req1)
	waitForMiddlewareWrites(t, middleware)

	if len(rec1.statuses) != 2 || rec1.statuses[0] != http.StatusEarlyHints || rec1.statuses[1] != http.StatusCreated {
		t.Fatalf("statuses=%v, want [103 201]", rec1.statuses)
	}

	req2 := httptest.NewRequest("GET", "/early-hints", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusCreated {
		t.Fatalf("status=%d, want 201", rec2.Code)
	}
	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("X-Cache=%q, want HIT", rec2.Header().Get("X-Cache"))
	}
	if body := rec2.Body.String(); body != "created" {
		t.Fatalf("body=%q, want created", body)
	}
}

func TestHTTPCacheMiddleware_SwitchingProtocolsIsNotCached(t *testing.T) {
	middleware := newTestMiddleware(t, DefaultConfig())
	defer middleware.Close()

	var setCount int
	middleware.OnSet(func(string, time.Duration) { setCount++ })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusSwitchingProtocols)
	})
	wrappedHandler := middleware.Wrap(handler)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/switch", nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)
		waitForMiddlewareWrites(t, middleware)
		if rec.Header().Get("X-Cache") != "MISS" {
			t.Fatalf("request %d X-Cache=%q, want MISS", i+1, rec.Header().Get("X-Cache"))
		}
	}
	if setCount != 0 {
		t.Fatalf("setCount=%d, want 0", setCount)
	}
}

func TestHTTPCacheMiddleware_NonCacheableMethodBypassesCachedKey(t *testing.T) {
	middleware := newTestMiddleware(t, DefaultConfig())
	defer middleware.Close()

	middleware.SetKeyGenerator(func(*http.Request) string { return "shared-key" })

	var calls int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt32(&calls, 1)
		w.Write([]byte(fmt.Sprintf("%s:%d", r.Method, call)))
	})
	wrappedHandler := middleware.Wrap(handler)

	req1 := httptest.NewRequest("GET", "/shared", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)
	waitForMiddlewareWrites(t, middleware)

	req2 := httptest.NewRequest("POST", "/shared", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)
	waitForMiddlewareWrites(t, middleware)

	if rec2.Header().Get("X-Cache") == "HIT" {
		t.Fatal("POST received cached GET response")
	}
	if body := rec2.Body.String(); body != "POST:2" {
		t.Fatalf("POST body=%q, want POST:2", body)
	}
}

// Capacity eviction (including SIEVE admission rejection) must keep the pattern
// index converged on the cache's resident key set rather than every key ever
// cached.
func TestHTTPCacheMiddleware_EvictionCleansPatternIndex(t *testing.T) {
	config := DefaultConfig()
	config.PathExtractor = PathExtractorFromKey
	config.MaxSize = 100
	config.ShardCount = 4
	middleware := newTestMiddleware(t, config)
	defer middleware.Close()
	middleware.SetKeyGenerator(KeyWithoutQuery())

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	wrappedHandler := middleware.Wrap(handler)

	const total = 2000
	for i := 0; i < total; i++ {
		req := httptest.NewRequest("GET", fmt.Sprintf("/evict/%d", i), nil)
		wrappedHandler.ServeHTTP(httptest.NewRecorder(), req)
	}
	waitForMiddlewareWrites(t, middleware)

	// The eviction listener reconciles the index, so it converges to exactly the
	// resident set — never the 2000 keys inserted.
	waitFor(t, func() bool {
		return int64(len(middleware.patternIdx.getMatchingKeys("/evict/*"))) == middleware.cache.Size()
	})
	if n := int64(len(middleware.patternIdx.getMatchingKeys("/evict/*"))); n > config.MaxSize {
		t.Fatalf("index holds %d keys, want <= cache capacity %d", n, config.MaxSize)
	}
}

// Invalidate deletes the matched entries; the eviction listener then reconciles
// the index for exactly those keys, leaving unrelated entries in place.
func TestHTTPCacheMiddleware_InvalidateCleansPatternIndex(t *testing.T) {
	middleware := newPatternMiddleware(t)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	wrappedHandler := middleware.Wrap(handler)

	for _, path := range []string{"/api/users/1", "/api/users/2", "/api/posts/1"} {
		wrappedHandler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	waitForMiddlewareWrites(t, middleware)

	if removed := middleware.Invalidate("/api/users/*"); removed != 2 {
		t.Fatalf("removed=%d, want 2", removed)
	}

	waitFor(t, func() bool {
		return len(middleware.patternIdx.getMatchingKeys("/api/users/*")) == 0
	})
	if keys := middleware.patternIdx.getMatchingKeys("/api/posts/*"); len(keys) != 1 {
		t.Fatalf("posts keys=%v, want one survivor", keys)
	}
}

func TestHTTPCacheMiddleware_InvalidateByFunc(t *testing.T) {
	middleware := newPatternMiddleware(t)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	wrappedHandler := middleware.Wrap(handler)

	for _, path := range []string{"/func/a", "/func/b", "/other/c"} {
		wrappedHandler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	waitForMiddlewareWrites(t, middleware)

	removed := middleware.InvalidateByFunc(func(key string) bool {
		return strings.Contains(key, "/func/")
	})
	if removed != 2 {
		t.Fatalf("removed=%d, want 2", removed)
	}

	waitFor(t, func() bool {
		return len(middleware.patternIdx.getMatchingKeys("/func/*")) == 0
	})
	if keys := middleware.patternIdx.getMatchingKeys("/other/c"); len(keys) != 1 {
		t.Fatalf("/other/c keys=%v, want one survivor", keys)
	}
}

// TTL expiry swept by the cleanup worker must reconcile the index too.
func TestHTTPCacheMiddleware_TTLExpiryCleansPatternIndex(t *testing.T) {
	config := DefaultConfig()
	config.PathExtractor = PathExtractorFromKey
	config.DefaultTTL = 40 * time.Millisecond
	config.CleanupInterval = 5 * time.Millisecond
	middleware := newTestMiddleware(t, config)
	defer middleware.Close()
	middleware.SetKeyGenerator(KeyWithoutQuery())

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	wrappedHandler := middleware.Wrap(handler)

	wrappedHandler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/ttl/1", nil))
	waitForMiddlewareWrites(t, middleware)
	if keys := middleware.patternIdx.getMatchingKeys("/ttl/1"); len(keys) != 1 {
		t.Fatalf("keys before expiry=%v, want one entry", keys)
	}

	waitFor(t, func() bool {
		return len(middleware.patternIdx.getMatchingKeys("/ttl/*")) == 0
	})
}

func TestHTTPCacheMiddleware_BasicCaching(t *testing.T) {
	config := DefaultConfig()
	config.DefaultTTL = 1 * time.Second

	middleware := newTestMiddleware(t, config)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"message": "test"}`))
	})

	wrappedHandler := middleware.Wrap(handler)

	// First request - should miss cache
	req1 := httptest.NewRequest("GET", "/test", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)
	waitForMiddlewareWrites(t, middleware)

	if rec1.Header().Get("X-Cache") != "MISS" {
		t.Error("Expected cache miss on first request")
	}

	// Second request - should hit cache
	req2 := httptest.NewRequest("GET", "/test", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)
	waitForMiddlewareWrites(t, middleware)

	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Error("Expected cache hit on second request")
	}

	if rec2.Body.String() != `{"message": "test"}` {
		t.Errorf("Expected cached response body, got %s", rec2.Body.String())
	}
}

func TestHTTPCacheMiddleware_TTLExpiration(t *testing.T) {
	config := DefaultConfig()
	config.DefaultTTL = 50 * time.Millisecond

	middleware := newTestMiddleware(t, config)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Wrap(handler)

	// First request
	req1 := httptest.NewRequest("GET", "/test", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)
	waitForMiddlewareWrites(t, middleware)

	// Second request - should hit cache
	req2 := httptest.NewRequest("GET", "/test", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)
	waitForMiddlewareWrites(t, middleware)

	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Error("Expected cache hit")
	}

	// Wait for TTL to expire
	time.Sleep(100 * time.Millisecond)

	// Third request - should miss cache due to expiration
	req3 := httptest.NewRequest("GET", "/test", nil)
	rec3 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec3, req3)
	waitForMiddlewareWrites(t, middleware)

	if rec3.Header().Get("X-Cache") != "MISS" {
		t.Error("Expected cache miss after TTL expiration")
	}
}

func TestHTTPCacheMiddleware_CachePolicy(t *testing.T) {
	config := DefaultConfig()
	config.CacheableMethods = []string{"GET", "POST"}
	middleware := newTestMiddleware(t, config)
	defer middleware.Close()

	// Custom policy that only caches POST requests
	middleware.SetCachePolicy(func(r *http.Request, statusCode int, headers http.Header, body []byte) (bool, time.Duration) {
		return r.Method == "POST" && statusCode == 200, 1 * time.Hour
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Wrap(handler)

	// GET request - should not be cached
	req1 := httptest.NewRequest("GET", "/test", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)
	waitForMiddlewareWrites(t, middleware)

	req2 := httptest.NewRequest("GET", "/test", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)
	waitForMiddlewareWrites(t, middleware)

	if rec2.Header().Get("X-Cache") != "MISS" {
		t.Error("GET request should not be cached")
	}

	// POST request - should be cached
	req3 := httptest.NewRequest("POST", "/test", nil)
	rec3 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec3, req3)
	waitForMiddlewareWrites(t, middleware)

	req4 := httptest.NewRequest("POST", "/test", nil)
	rec4 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec4, req4)
	waitForMiddlewareWrites(t, middleware)

	if rec4.Header().Get("X-Cache") != "HIT" {
		t.Error("POST request should be cached")
	}
}

func TestHTTPCacheMiddleware_KeyGenerator(t *testing.T) {
	config := DefaultConfig()
	middleware := newTestMiddleware(t, config)
	defer middleware.Close()

	// Custom key generator that includes user ID
	middleware.SetKeyGenerator(KeyWithUserID("X-User-ID"))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Wrap(handler)

	// Request with User-ID: 1
	req1 := httptest.NewRequest("GET", "/test", nil)
	req1.Header.Set("X-User-ID", "1")
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)
	waitForMiddlewareWrites(t, middleware)

	// Same request with User-ID: 1 - should hit cache
	req2 := httptest.NewRequest("GET", "/test", nil)
	req2.Header.Set("X-User-ID", "1")
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)
	waitForMiddlewareWrites(t, middleware)

	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Error("Expected cache hit for same user")
	}

	// Request with User-ID: 2 - should miss cache
	req3 := httptest.NewRequest("GET", "/test", nil)
	req3.Header.Set("X-User-ID", "2")
	rec3 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec3, req3)
	waitForMiddlewareWrites(t, middleware)

	if rec3.Header().Get("X-Cache") != "MISS" {
		t.Error("Expected cache miss for different user")
	}
}

func TestHTTPCacheMiddleware_Callbacks(t *testing.T) {
	config := DefaultConfig()
	middleware := newTestMiddleware(t, config)
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

	wrappedHandler := middleware.Wrap(handler)

	// First request
	req1 := httptest.NewRequest("GET", "/test", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)
	waitForMiddlewareWrites(t, middleware)

	if missCount != 1 || setCount != 1 || hitCount != 0 {
		t.Errorf("Expected 1 miss, 1 set, 0 hits, got %d miss, %d set, %d hits", missCount, setCount, hitCount)
	}

	// Second request
	req2 := httptest.NewRequest("GET", "/test", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)
	waitForMiddlewareWrites(t, middleware)

	if missCount != 1 || setCount != 1 || hitCount != 1 {
		t.Errorf("Expected 1 miss, 1 set, 1 hit, got %d miss, %d set, %d hits", missCount, setCount, hitCount)
	}
}

func TestHTTPCacheMiddleware_CacheControlHeaders(t *testing.T) {
	config := DefaultConfig()
	middleware := newTestMiddleware(t, config)
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

			wrappedHandler := middleware.Wrap(handler)

			// First request
			req1 := httptest.NewRequest("GET", "/test-"+tt.name, nil)
			rec1 := httptest.NewRecorder()
			wrappedHandler.ServeHTTP(rec1, req1)
			waitForMiddlewareWrites(t, middleware)

			// Second request
			req2 := httptest.NewRequest("GET", "/test-"+tt.name, nil)
			rec2 := httptest.NewRecorder()
			wrappedHandler.ServeHTTP(rec2, req2)
			waitForMiddlewareWrites(t, middleware)

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
	config := DefaultConfig()
	middleware := newTestMiddleware(t, config)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Wrap(handler)

	methods := []string{"POST", "PUT", "DELETE", "PATCH"}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			// First request
			req1 := httptest.NewRequest(method, "/test", nil)
			rec1 := httptest.NewRecorder()
			wrappedHandler.ServeHTTP(rec1, req1)
			waitForMiddlewareWrites(t, middleware)

			// Second request
			req2 := httptest.NewRequest(method, "/test", nil)
			rec2 := httptest.NewRecorder()
			wrappedHandler.ServeHTTP(rec2, req2)
			waitForMiddlewareWrites(t, middleware)

			if rec2.Header().Get("X-Cache") == "HIT" {
				t.Errorf("%s request should not be served from cache", method)
			}
		})
	}
}

func TestHTTPCacheMiddleware_Stats(t *testing.T) {
	config := DefaultConfig()
	middleware := newTestMiddleware(t, config)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Wrap(handler)

	// First request (miss)
	req1 := httptest.NewRequest("GET", "/test", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)
	waitForMiddlewareWrites(t, middleware)

	// Second request (hit)
	req2 := httptest.NewRequest("GET", "/test", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)
	waitForMiddlewareWrites(t, middleware)

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
	config := DefaultConfig()
	middleware := newTestMiddleware(t, config)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Wrap(handler)

	// Cache an item
	req1 := httptest.NewRequest("GET", "/test", nil)
	rec1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec1, req1)
	waitForMiddlewareWrites(t, middleware)

	// Verify it's cached
	req2 := httptest.NewRequest("GET", "/test", nil)
	rec2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec2, req2)
	waitForMiddlewareWrites(t, middleware)

	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Error("Expected cache hit before clear")
	}

	// Clear cache
	middleware.Clear()

	// Verify cache is cleared
	req3 := httptest.NewRequest("GET", "/test", nil)
	rec3 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec3, req3)
	waitForMiddlewareWrites(t, middleware)

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

	t.Run("ByContentType", func(t *testing.T) {
		rules := map[string]time.Duration{
			"application/json": 10 * time.Minute,
			"text/html":        5 * time.Minute,
		}
		policy := ByContentType(rules, 1*time.Minute)

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

	t.Run("BySize", func(t *testing.T) {
		policy := BySize(5, 20, 1*time.Hour)

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
	// Path-based keys plus a PathExtractor enable pattern matching.
	middleware := newPatternMiddleware(t)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response for " + r.URL.Path))
	})

	wrappedHandler := middleware.Wrap(handler)

	// Cache multiple paths
	paths := []string{"/api/users/", "/api/users/1", "/api/users/2/profile", "/api/posts/1", "/api/posts/2"}
	for _, path := range paths {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)
		waitForMiddlewareWrites(t, middleware)

		if rec.Header().Get("X-Cache") != "MISS" {
			t.Errorf("Expected MISS for first request to %s", path)
		}
	}

	// Verify all paths are cached
	for _, path := range paths {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)
		waitForMiddlewareWrites(t, middleware)

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
		waitForMiddlewareWrites(t, middleware)

		// Verify it's cached
		req2 := httptest.NewRequest("GET", "/api/users/1", nil)
		rec2 := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec2, req2)
		waitForMiddlewareWrites(t, middleware)
		if rec2.Header().Get("X-Cache") != "HIT" {
			t.Error("Expected cache hit before invalidation")
		}

		// Invalidate
		middleware.Invalidate("/api/users/1")

		// Verify HTTP cache miss
		req3 := httptest.NewRequest("GET", "/api/users/1", nil)
		rec3 := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec3, req3)
		waitForMiddlewareWrites(t, middleware)

		if rec3.Header().Get("X-Cache") != "MISS" {
			t.Error("Expected cache miss after invalidation")
		}
	})
}

func TestHTTPCacheMiddleware_InvalidationEdgeCases(t *testing.T) {
	middleware := newPatternMiddleware(t)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Wrap(handler)

	t.Run("DoubleInvalidation", func(t *testing.T) {
		// Cache an item
		req := httptest.NewRequest("GET", "/test/double", nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)
		waitForMiddlewareWrites(t, middleware)

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
		waitForMiddlewareWrites(t, middleware)

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
		waitForMiddlewareWrites(t, middleware)

		// Should invalidate based on path only (query ignored)
		removed := middleware.Invalidate("/test")
		if removed != 1 {
			t.Errorf("Expected 1 item removed ignoring query params, got %d", removed)
		}
	})
}

func TestHTTPCacheMiddleware_CacheHitMissVerification(t *testing.T) {
	config := DefaultConfig()
	middleware := newTestMiddleware(t, config)
	defer middleware.Close()

	var hitCount, missCount int
	middleware.OnHit(func(key string) { hitCount++ })
	middleware.OnMiss(func(key string) { missCount++ })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Wrap(handler)

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
				waitForMiddlewareWrites(t, middleware)

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
	middleware := newPatternMiddleware(t)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Wrap(handler)

	// Cache multiple items
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest("GET", fmt.Sprintf("/test/%d", i), nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)
		waitForMiddlewareWrites(t, middleware)
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
		waitForMiddlewareWrites(t, middleware)

		if rec.Header().Get("X-Cache") != "MISS" {
			t.Errorf("Expected MISS after concurrent invalidation for /test/%d", i)
		}
	}
}

func TestHTTPCacheMiddleware_BasicPatternInvalidation(t *testing.T) {
	middleware := newPatternMiddleware(t)
	defer middleware.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("response"))
	})

	wrappedHandler := middleware.Wrap(handler)

	// Cache a few simple paths
	paths := []string{"/api/users", "/api/posts"}
	for _, path := range paths {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		wrappedHandler.ServeHTTP(rec, req)
		waitForMiddlewareWrites(t, middleware)
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
		waitForMiddlewareWrites(t, middleware)

		if rec.Header().Get("X-Cache") != "MISS" {
			t.Errorf("Expected MISS after invalidation for %s", path)
		}
	}
}
