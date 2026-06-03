// Package httpcache provides HTTP response caching middleware backed by a kioshun cache.
package httpcache

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/unkn0wn-root/kioshun"
)

// Response is a cached HTTP response.
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	CachedAt   time.Time
}

// KeyGenerator builds a cache key from a request.
type KeyGenerator func(*http.Request) string

// Policy decides whether a response should be cached and for how long.
type Policy func(*http.Request, int, http.Header, []byte) (shouldCache bool, ttl time.Duration)

// PathExtractor recovers a URL path from a cache key for pattern invalidation.
type PathExtractor func(string) string

// Middleware is an HTTP response caching middleware backed by a kioshun cache.
type Middleware struct {
	cache       *kioshun.Cache[string, *Response]
	keyGen      KeyGenerator
	policy      Policy
	onHit       func(string)
	onMiss      func(string)
	onSet       func(string, time.Duration)
	hitHeader   string
	missHeader  string
	ignore      map[string]struct{}
	methods     map[string]struct{}
	maxBodySize int64
	limitBody   bool
	patternIdx  *patternIndex
	pathExtract func(string) string
}

// New creates a caching Middleware from config, applying DefaultConfig for unset fields.
func New(config Config) (*Middleware, error) {
	config, err := resolveConfig(config)
	if err != nil {
		return nil, err
	}

	cacheConfig := kioshun.Config{
		MaxSize:         config.MaxSize,
		ShardCount:      config.ShardCount,
		CleanupInterval: config.CleanupInterval,
		DefaultTTL:      config.DefaultTTL,
		EvictionPolicy:  config.EvictionPolicy,
		ProbationRatio:  config.ProbationRatio,
		GhostRatio:      config.GhostRatio,
		Adapt:           !config.DisableAdapt,
		StatsEnabled:    !config.DisableStats,
	}
	if config.DisableCleanup {
		cacheConfig.CleanupInterval = 0
	}

	cache, err := kioshun.New[string, *Response](cacheConfig)
	if err != nil {
		return nil, err
	}

	m := &Middleware{
		cache:       cache,
		keyGen:      DefaultKeyGenerator,
		policy:      DefaultCachePolicy(config),
		hitHeader:   config.HitHeader,
		missHeader:  config.MissHeader,
		ignore:      ignoredHeaders(config),
		methods:     cacheableMethodSet(config.CacheableMethods),
		maxBodySize: config.MaxBodySize,
		limitBody:   !config.DisableBodySizeLimit,
		patternIdx:  newPatternIndex(),
		pathExtract: defaultPathExtractor,
	}

	return m, nil
}

// Wrap wraps an HTTP handler with response caching.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.cacheableRequest(r) {
			next.ServeHTTP(w, r)
			return
		}

		key := m.keyGen(r)
		if cached, found := m.cache.Get(key); found {
			m.serveCached(w, cached, key)
			return
		}

		if m.onMiss != nil {
			m.onMiss(key)
		}

		rw := newResponseWriter(w, m.missHeader, m.maxBodySize, m.limitBody)
		next.ServeHTTP(rw, r)
		rw.finish()

		body := rw.buf.Bytes()
		if rw.cacheable() {
			shouldCache, ttl := m.policy(r, rw.statusCode, rw.headers, body)
			if !shouldCache {
				return
			}
			cached := &Response{
				StatusCode: rw.statusCode,
				Headers:    m.cachedHeaders(rw.headers),
				Body:       make([]byte, len(body)),
				CachedAt:   time.Now(),
			}
			copy(cached.Body, body)

			if err := m.cache.Set(key, cached, ttl); err == nil {
				if path := m.pathExtract(key); path != "" {
					m.patternIdx.addKey(path, key)
				}

				if m.onSet != nil {
					m.onSet(key, ttl)
				}
			}
		}
	})
}

// Stats returns cache statistics.
func (m *Middleware) Stats() kioshun.Stats { return m.cache.Stats() }

// Clear removes all cached entries.
func (m *Middleware) Clear() {
	m.cache.Clear()
	m.patternIdx.clear()
}

// Invalidate removes cached entries matching a URL pattern.
func (m *Middleware) Invalidate(urlPattern string) int {
	removed := 0

	matchingKeys := m.patternIdx.getMatchingKeys(urlPattern)
	for _, key := range matchingKeys {
		if path := m.pathExtract(key); path != "" {
			m.patternIdx.removeKey(path, key)
		}
		if m.cache.Delete(key) {
			removed++
		}
	}

	return removed
}

// InvalidateByFunc removes cached entries matching a custom predicate.
func (m *Middleware) InvalidateByFunc(fn func(string) bool) int {
	removed := 0
	keys := m.cache.Keys()

	for _, key := range keys {
		if fn(key) && m.cache.Delete(key) {
			if path := m.pathExtract(key); path != "" {
				m.patternIdx.removeKey(path, key)
			}
			removed++
		}
	}

	return removed
}

// Close releases middleware resources.
func (m *Middleware) Close() error {
	return m.cache.Close()
}

// SetKeyGenerator overrides the cache key generator.
func (m *Middleware) SetKeyGenerator(keyGen KeyGenerator) { m.keyGen = keyGen }

// SetCachePolicy overrides the policy deciding what to cache and for how long.
func (m *Middleware) SetCachePolicy(policy Policy) { m.policy = policy }

// SetPathExtractor overrides the key-to-path extractor used for pattern invalidation.
func (m *Middleware) SetPathExtractor(extractor PathExtractor) { m.pathExtract = extractor }

// OnHit registers a callback invoked on each cache hit.
func (m *Middleware) OnHit(callback func(string)) { m.onHit = callback }

// OnMiss registers a callback invoked on each cache miss.
func (m *Middleware) OnMiss(callback func(string)) { m.onMiss = callback }

// OnSet registers a callback invoked after a response is cached.
func (m *Middleware) OnSet(callback func(string, time.Duration)) { m.onSet = callback }

// serveCached writes a cached response and emits cache-hit metadata.
func (m *Middleware) serveCached(w http.ResponseWriter, cached *Response, key string) {
	if m.onHit != nil {
		m.onHit(key)
	}

	for k, v := range cached.Headers {
		w.Header()[k] = slices.Clone(v)
	}

	w.Header().Set(m.hitHeader, "HIT")
	w.Header().Set("X-Cache-Date", cached.CachedAt.Format(time.RFC3339))
	w.Header().Set("X-Cache-Age", time.Since(cached.CachedAt).String())

	w.WriteHeader(cached.StatusCode)
	_, _ = w.Write(cached.Body)
}

func ignoredHeaders(config Config) map[string]struct{} {
	headers := make(map[string]struct{}, len(config.IgnoreHeaders)+5)
	for _, name := range config.IgnoreHeaders {
		if name != "" {
			headers[http.CanonicalHeaderKey(name)] = struct{}{}
		}
	}
	for _, name := range []string{
		"X-Cache",
		"X-Cache-Date",
		"X-Cache-Age",
		config.HitHeader,
		config.MissHeader,
	} {
		if name != "" {
			headers[http.CanonicalHeaderKey(name)] = struct{}{}
		}
	}
	return headers
}

func (m *Middleware) cachedHeaders(headers http.Header) http.Header {
	cached := make(http.Header, len(headers))
	for name, values := range headers {
		if _, ignored := m.ignore[http.CanonicalHeaderKey(name)]; ignored {
			continue
		}
		cached[name] = slices.Clone(values)
	}
	return cached
}

func (m *Middleware) cacheableRequest(r *http.Request) bool {
	_, ok := m.methods[r.Method]
	return ok
}

// DefaultKeyGenerator hashes the method, full URL, and common vary headers.
func DefaultKeyGenerator(r *http.Request) string {
	h := md5.New()
	h.Write([]byte(r.Method))
	h.Write([]byte(r.URL.String()))

	vary := []string{"Accept", "Accept-Encoding", "Authorization", "Accept-Language"}
	for _, header := range vary {
		if value := r.Header.Get(header); value != "" {
			h.Write([]byte(header + ":" + value))
		}
	}

	return hex.EncodeToString(h.Sum(nil))
}

// DefaultCachePolicy builds a policy that respects HTTP cache headers.
func DefaultCachePolicy(config Config) Policy {
	config = configWithDefaults(config)
	cacheableMethods := cacheableMethodSet(config.CacheableMethods)

	cacheableStatus := make(map[int]bool, len(config.CacheableStatus))
	for _, status := range config.CacheableStatus {
		cacheableStatus[status] = true
	}

	return func(r *http.Request, statusCode int, headers http.Header, body []byte) (bool, time.Duration) {
		if _, ok := cacheableMethods[r.Method]; !ok {
			return false, 0
		}
		if !cacheableStatus[statusCode] {
			return false, 0
		}
		if !config.DisableBodySizeLimit && int64(len(body)) > config.MaxBodySize {
			return false, 0
		}

		cacheControl := headers.Get("Cache-Control")
		if hasCacheControlDirective(cacheControl, "no-cache", "no-store", "private") {
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

func cacheableMethodSet(methods []string) map[string]struct{} {
	set := make(map[string]struct{}, len(methods))
	for _, method := range methods {
		if method != "" {
			set[method] = struct{}{}
		}
	}
	return set
}

// PathBasedKeyGenerator keys responses by request method and path.
func PathBasedKeyGenerator(r *http.Request) string {
	return r.Method + ":" + r.URL.Path
}

// responseWriter captures status, headers, and body while forwarding writes.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	buf        bytes.Buffer
	headers    http.Header
	missHeader string
	maxBody    int64
	limitBody  bool
	wrote      bool
	tooLarge   bool
	streamed   bool
}

func newResponseWriter(w http.ResponseWriter, missHeader string, maxBody int64, limitBody bool) *responseWriter {
	return &responseWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		missHeader:     missHeader,
		maxBody:        maxBody,
		limitBody:      limitBody,
	}
}

// WriteHeader captures status code and headers.
func (rw *responseWriter) WriteHeader(code int) {
	if isInformationalStatus(code) {
		rw.ResponseWriter.WriteHeader(code)
		return
	}
	if rw.wrote {
		return
	}

	rw.statusCode = code
	rw.headers = rw.ResponseWriter.Header().Clone()
	if code == http.StatusSwitchingProtocols {
		rw.streamed = true
	}
	if rw.missHeader != "" {
		rw.ResponseWriter.Header().Set(rw.missHeader, "MISS")
	}
	rw.wrote = true
	rw.ResponseWriter.WriteHeader(code)
}

// Write captures response body while passing through.
func (rw *responseWriter) Write(data []byte) (int, error) {
	if !rw.wrote {
		rw.WriteHeader(http.StatusOK)
	}
	rw.capture(data)
	return rw.ResponseWriter.Write(data)
}

func (rw *responseWriter) capture(data []byte) {
	if rw.streamed || rw.tooLarge || len(data) == 0 {
		return
	}
	if !rw.limitBody {
		rw.buf.Write(data)
		return
	}
	remaining := rw.maxBody - int64(rw.buf.Len())
	if remaining <= 0 {
		rw.tooLarge = true
		return
	}
	if int64(len(data)) > remaining {
		rw.buf.Write(data[:int(remaining)])
		rw.tooLarge = true
		return
	}
	rw.buf.Write(data)
}

func (rw *responseWriter) finish() {
	if !rw.wrote {
		rw.WriteHeader(http.StatusOK)
	}
}

func (rw *responseWriter) cacheable() bool {
	return !rw.streamed && !rw.tooLarge
}

func isInformationalStatus(code int) bool {
	return code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols
}

func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

func (rw *responseWriter) Flush() {
	_ = rw.FlushError()
}

func (rw *responseWriter) FlushError() error {
	rw.streamed = true
	if !rw.wrote {
		rw.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(rw.ResponseWriter).Flush()
}

func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	rw.streamed = true
	conn, brw, err := http.NewResponseController(rw.ResponseWriter).Hijack()
	if err == nil {
		rw.wrote = true
	}
	return conn, brw, err
}

func (rw *responseWriter) Push(target string, opts *http.PushOptions) error {
	pusher, ok := rw.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}

// KeyWithVaryHeaders hashes the method, full URL, and selected vary headers.
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

		return hex.EncodeToString(h.Sum(nil))
	}
}

// KeyWithoutQuery keys responses by method and path, ignoring query parameters.
func KeyWithoutQuery() KeyGenerator {
	return func(r *http.Request) string {
		return r.Method + ":" + r.URL.Path
	}
}

// KeyWithoutQueryHashed hashes method, path, and common vary headers.
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

		return hex.EncodeToString(h.Sum(nil))
	}
}

// PathExtractorFromKey extracts path from "METHOD:PATH" keys.
func PathExtractorFromKey(key string) string {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return key
}

// KeyWithUserID hashes the request with a user identifier header.
func KeyWithUserID(userIDHeader string) KeyGenerator {
	return func(r *http.Request) string {
		h := md5.New()
		h.Write([]byte(r.Method))
		h.Write([]byte(r.URL.String()))

		if userID := r.Header.Get(userIDHeader); userID != "" {
			h.Write([]byte("user:" + userID))
		}

		return hex.EncodeToString(h.Sum(nil))
	}
}

// AlwaysCache caches successful 2xx responses for the supplied TTL.
func AlwaysCache(ttl time.Duration) Policy {
	return func(_ *http.Request, statusCode int, _ http.Header, _ []byte) (bool, time.Duration) {
		return statusCode >= 200 && statusCode < 300, ttl
	}
}

// NeverCache disables response caching.
func NeverCache() Policy {
	return func(_ *http.Request, _ int, _ http.Header, _ []byte) (bool, time.Duration) {
		return false, 0
	}
}

// ByContentType assigns TTLs by Content-Type prefix.
func ByContentType(rules map[string]time.Duration, defaultTTL time.Duration) Policy {
	return func(_ *http.Request, statusCode int, headers http.Header, _ []byte) (bool, time.Duration) {
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

// BySize caches successful 2xx responses within the specified size range.
func BySize(minSize, maxSize int64, ttl time.Duration) Policy {
	return func(_ *http.Request, statusCode int, _ http.Header, body []byte) (bool, time.Duration) {
		if statusCode < 200 || statusCode >= 300 {
			return false, 0
		}

		size := int64(len(body))
		return size >= minSize && size <= maxSize, ttl
	}
}

// extractMaxAge parses a positive max-age directive from Cache-Control.
func extractMaxAge(cacheControl string) time.Duration {
	if cacheControl == "" {
		return 0
	}

	for part := range strings.SplitSeq(cacheControl, ",") {
		part = strings.TrimSpace(part)
		name, value, ok := strings.Cut(part, "=")
		if ok && strings.EqualFold(strings.TrimSpace(name), "max-age") {
			if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
				return time.Duration(seconds) * time.Second
			}
		}
	}
	return 0
}

func hasCacheControlDirective(cacheControl string, directives ...string) bool {
	if cacheControl == "" {
		return false
	}

	for part := range strings.SplitSeq(cacheControl, ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
		name = strings.TrimSpace(name)
		for _, directive := range directives {
			if strings.EqualFold(name, directive) {
				return true
			}
		}
	}
	return false
}
