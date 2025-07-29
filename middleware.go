package cache

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type CachedResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	CachedAt   time.Time
}

type KeyGenerator func(*http.Request) string
type CachePolicy func(*http.Request, int, http.Header, []byte) (shouldCache bool, ttl time.Duration)
type PathExtractor func(string) string

type HTTPCacheMiddleware struct {
	cache       *InMemoryCache[string, *CachedResponse]
	keyGen      KeyGenerator
	policy      CachePolicy
	onHit       func(string)
	onMiss      func(string)
	onSet       func(string, time.Duration)
	hitHeader   string
	missHeader  string
	patternIdx  *patternIndex
	pathExtract func(string) string
}

type MiddlewareConfig struct {
	// Cache configuration
	MaxSize                int64
	ShardCount             int
	CleanupInterval        time.Duration
	DefaultTTL             time.Duration
	EvictionPolicy         EvictionPolicy
	AdmissionResetInterval time.Duration
	StatsEnabled           bool

	// HTTP-specific options
	CacheableMethods []string
	CacheableStatus  []int
	IgnoreHeaders    []string
	MaxBodySize      int64

	// Headers
	HitHeader  string // Default: "X-Cache"
	MissHeader string // Default: "X-Cache"
}

func DefaultMiddlewareConfig() MiddlewareConfig {
	return MiddlewareConfig{
		MaxSize:          100000,
		ShardCount:       16,
		CleanupInterval:  5 * time.Minute,
		DefaultTTL:       5 * time.Minute,
		EvictionPolicy:   FIFO,
		StatsEnabled:     true,
		CacheableMethods: []string{"GET", "HEAD"},
		CacheableStatus:  []int{200, 201, 300, 301, 302, 304, 404, 410},
		IgnoreHeaders:    []string{"Date", "Server", "X-Request-Id", "X-Trace-Id"},
		MaxBodySize:      10 * 1024 * 1024, // 10MB
		HitHeader:        "X-Cache",
		MissHeader:       "X-Cache",
	}
}

func NewHTTPCacheMiddleware(config MiddlewareConfig) *HTTPCacheMiddleware {
	if config.MaxSize == 0 {
		config = DefaultMiddlewareConfig()
	}

	cacheConfig := Config{
		MaxSize:                config.MaxSize,
		ShardCount:             config.ShardCount,
		CleanupInterval:        config.CleanupInterval,
		DefaultTTL:             config.DefaultTTL,
		EvictionPolicy:         config.EvictionPolicy,
		AdmissionResetInterval: config.AdmissionResetInterval,
		StatsEnabled:           config.StatsEnabled,
	}

	m := &HTTPCacheMiddleware{
		cache:       New[string, *CachedResponse](cacheConfig),
		keyGen:      DefaultKeyGenerator,
		policy:      DefaultCachePolicy(config),
		hitHeader:   config.HitHeader,
		missHeader:  config.MissHeader,
		patternIdx:  newPatternIndex(),
		pathExtract: defaultPathExtractor,
	}

	if m.hitHeader == "" {
		m.hitHeader = "X-Cache"
	}
	if m.missHeader == "" {
		m.missHeader = "X-Cache"
	}

	return m
}

// Middleware wraps HTTP handlers for response caching
func (m *HTTPCacheMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := m.keyGen(r)
		if cached, found := m.cache.Get(key); found {
			m.serveCached(w, cached, key)
			return
		}

		// cache miss - call callback
		if m.onMiss != nil {
			m.onMiss(key)
		}

		// wrap response writer to capture data
		rw := newResponseWriter(w)
		next.ServeHTTP(rw, r)

		body := rw.buf.Bytes()
		if shouldCache, ttl := m.policy(r, rw.statusCode, rw.headers, body); shouldCache {
			cached := &CachedResponse{
				StatusCode: rw.statusCode,
				Headers:    rw.headers.Clone(),
				Body:       make([]byte, len(body)),
				CachedAt:   time.Now(),
			}
			copy(cached.Body, body)

			m.cache.Set(key, cached, ttl)

			// add to pattern index
			if path := m.pathExtract(key); path != "" {
				m.patternIdx.addKey(path, key)
			}

			if m.onSet != nil {
				m.onSet(key, ttl)
			}
		}

		w.Header().Set(m.missHeader, "MISS")
	})
}

// serveCached writes a cached response
func (m *HTTPCacheMiddleware) serveCached(w http.ResponseWriter, cached *CachedResponse, key string) {
	if m.onHit != nil {
		m.onHit(key)
	}

	for k, v := range cached.Headers {
		w.Header()[k] = v
	}

	w.Header().Set(m.hitHeader, "HIT")
	w.Header().Set("X-Cache-Date", cached.CachedAt.Format(time.RFC3339))
	w.Header().Set("X-Cache-Age", time.Since(cached.CachedAt).String())

	w.WriteHeader(cached.StatusCode)
	w.Write(cached.Body)
}

// Stats returns cache statistics
func (m *HTTPCacheMiddleware) Stats() Stats { return m.cache.Stats() }

// Clear removes all cached entries
func (m *HTTPCacheMiddleware) Clear() {
	m.cache.Clear()
	m.patternIdx.clear()
}

// Invalidate removes cached entries matching a URL pattern
func (m *HTTPCacheMiddleware) Invalidate(urlPattern string) int {
	removed := 0

	matchingKeys := m.patternIdx.getMatchingKeys(urlPattern)
	for _, key := range matchingKeys {
		if m.cache.Delete(key) {
			if path := m.pathExtract(key); path != "" {
				m.patternIdx.removeKey(path, key)
			}
			removed++
		}
	}

	return removed
}

// InvalidateByFunc removes cached entries matching a custom function
func (m *HTTPCacheMiddleware) InvalidateByFunc(fn func(string) bool) int {
	removed := 0
	keys := m.cache.Keys()

	for _, key := range keys {
		if fn(key) && m.cache.Delete(key) {
			removed++
		}
	}

	return removed
}

// Close releases middleware resources
func (m *HTTPCacheMiddleware) Close() error {
	return m.cache.Close()
}

func (m *HTTPCacheMiddleware) SetKeyGenerator(keyGen KeyGenerator)        { m.keyGen = keyGen }
func (m *HTTPCacheMiddleware) SetCachePolicy(policy CachePolicy)          { m.policy = policy }
func (m *HTTPCacheMiddleware) SetPathExtractor(extractor PathExtractor)   { m.pathExtract = extractor }
func (m *HTTPCacheMiddleware) OnHit(callback func(string))                { m.onHit = callback }
func (m *HTTPCacheMiddleware) OnMiss(callback func(string))               { m.onMiss = callback }
func (m *HTTPCacheMiddleware) OnSet(callback func(string, time.Duration)) { m.onSet = callback }

// DefaultKeyGenerator creates MD5 hash from request method, URL, and vary headers
func DefaultKeyGenerator(r *http.Request) string {
	h := md5.New()
	h.Write([]byte(r.Method))
	h.Write([]byte(r.URL.String()))

	// include common vary headers
	vary := []string{"Accept", "Accept-Encoding", "Authorization", "Accept-Language"}
	for _, header := range vary {
		if value := r.Header.Get(header); value != "" {
			h.Write([]byte(header + ":" + value))
		}
	}

	return fmt.Sprintf("%x", h.Sum(nil))
}

// DefaultCachePolicy creates policy respecting HTTP cache headers
func DefaultCachePolicy(config MiddlewareConfig) CachePolicy {
	cacheableMethods := make(map[string]bool, len(config.CacheableMethods))
	for _, method := range config.CacheableMethods {
		cacheableMethods[method] = true
	}

	cacheableStatus := make(map[int]bool, len(config.CacheableStatus))
	for _, status := range config.CacheableStatus {
		cacheableStatus[status] = true
	}

	return func(r *http.Request, statusCode int, headers http.Header, body []byte) (bool, time.Duration) {
		if !cacheableMethods[r.Method] {
			return false, 0
		}
		if !cacheableStatus[statusCode] {
			return false, 0
		}
		if int64(len(body)) > config.MaxBodySize {
			return false, 0
		}

		// cache-control headers
		cacheControl := headers.Get("Cache-Control")
		if strings.Contains(cacheControl, "no-cache") ||
			strings.Contains(cacheControl, "no-store") ||
			strings.Contains(cacheControl, "private") {
			return false, 0
		}

		if maxAge := extractMaxAge(cacheControl); maxAge > 0 {
			return true, maxAge
		}

		if expires := headers.Get("Expires"); expires != "" {
			if expireTime, err := time.Parse(time.RFC1123, expires); err == nil {
				ttl := time.Until(expireTime)
				if ttl > 0 {
					return true, ttl
				}
			}
		}

		return true, config.DefaultTTL
	}
}

func PathBasedKeyGenerator(r *http.Request) string {
	return r.Method + ":" + r.URL.Path
}

// responseWriter wraps http.ResponseWriter to capture response data
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	buf        *bytes.Buffer
	headers    http.Header
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{
		ResponseWriter: w,
		statusCode:     200,
		buf:            new(bytes.Buffer),
		headers:        make(http.Header),
	}
}

// WriteHeader captures status code and headers
func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code

	// copy headers before writing
	for k, v := range rw.ResponseWriter.Header() {
		rw.headers[k] = v
	}

	rw.ResponseWriter.WriteHeader(code)
}

// Write captures response body while passing through
func (rw *responseWriter) Write(data []byte) (int, error) {
	rw.buf.Write(data)
	return rw.ResponseWriter.Write(data)
}

// KeyWithVaryHeaders creates MD5 hash including custom vary headers
func KeyWithVaryHeaders(varyHeaders []string) KeyGenerator {
	return func(r *http.Request) string {
		h := md5.New()
		h.Write([]byte(r.Method))
		h.Write([]byte(r.URL.String()))

		for _, header := range varyHeaders {
			if value := r.Header.Get(header); value != "" {
				h.Write([]byte(header + ":" + value))
			}
		}

		return fmt.Sprintf("%x", h.Sum(nil))
	}
}

// KeyWithoutQuery creates simple keys ignoring query parameters
func KeyWithoutQuery() KeyGenerator {
	return func(r *http.Request) string {
		return r.Method + ":" + r.URL.Path
	}
}

// KeyWithoutQueryHashed creates MD5 hash from path and headers only
func KeyWithoutQueryHashed() KeyGenerator {
	return func(r *http.Request) string {
		h := md5.New()
		h.Write([]byte(r.Method))
		h.Write([]byte(r.URL.Path))

		vary := []string{"Accept", "Accept-Encoding", "Authorization"}
		for _, header := range vary {
			if value := r.Header.Get(header); value != "" {
				h.Write([]byte(header + ":" + value))
			}
		}

		return fmt.Sprintf("%x", h.Sum(nil))
	}
}

// PathExtractorFromKey extracts path from "METHOD:PATH" format keys
func PathExtractorFromKey(key string) string {
	// Extract path from "METHOD:PATH" format
	parts := strings.SplitN(key, ":", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return key
}

// KeyWithUserID creates MD5 hash including user ID from header
func KeyWithUserID(userIDHeader string) KeyGenerator {
	return func(r *http.Request) string {
		h := md5.New()
		h.Write([]byte(r.Method))
		h.Write([]byte(r.URL.String()))

		if userID := r.Header.Get(userIDHeader); userID != "" {
			h.Write([]byte("user:" + userID))
		}

		return fmt.Sprintf("%x", h.Sum(nil))
	}
}

func AlwaysCache(ttl time.Duration) CachePolicy {
	return func(r *http.Request, statusCode int, headers http.Header, body []byte) (bool, time.Duration) {
		return statusCode >= 200 && statusCode < 300, ttl
	}
}

func NeverCache() CachePolicy {
	return func(r *http.Request, statusCode int, headers http.Header, body []byte) (bool, time.Duration) {
		return false, 0
	}
}

func CacheByContentType(rules map[string]time.Duration, defaultTTL time.Duration) CachePolicy {
	return func(r *http.Request, statusCode int, headers http.Header, body []byte) (bool, time.Duration) {
		if statusCode < 200 || statusCode >= 300 {
			return false, 0
		}

		contentType := headers.Get("Content-Type")
		for prefix, ttl := range rules {
			if strings.HasPrefix(contentType, prefix) {
				return true, ttl
			}
		}

		return true, defaultTTL
	}
}

// CacheBySize caches responses within specified size range
func CacheBySize(minSize, maxSize int64, ttl time.Duration) CachePolicy {
	return func(r *http.Request, statusCode int, headers http.Header, body []byte) (bool, time.Duration) {
		if statusCode < 200 || statusCode >= 300 {
			return false, 0
		}

		size := int64(len(body))
		return size >= minSize && size <= maxSize, ttl
	}
}

// extractMaxAge parses max-age directive from Cache-Control header
func extractMaxAge(cacheControl string) time.Duration {
	if cacheControl == "" {
		return 0
	}

	parts := strings.Split(cacheControl, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "max-age=") {
			if seconds, err := strconv.Atoi(part[8:]); err == nil && seconds > 0 {
				return time.Duration(seconds) * time.Second
			}
		}
	}
	return 0
}
