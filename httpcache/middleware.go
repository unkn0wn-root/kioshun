// Package httpcache provides HTTP response caching middleware backed by a kioshun cache.
package httpcache

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
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

	vary http.Header   // request values of the fields named in the response's Vary
	age  time.Duration // corrected initial age at CachedAt
}

// KeyGenerator builds a cache key from a request.
type KeyGenerator func(*http.Request) string

// Policy decides whether a response should be cached and for how long.
// Wrap never stores a response that a shared cache must not store, whatever
// the Policy returns.
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

	// tracks cached keys by URL path for Invalidate. It is populated
	// and queried only when patternEnabled is set (a PathExtractor was configured),
	// and kept in sync by the backing cache's removal listener (onCacheRemove).
	patternEnabled bool
	patternIdx     *patternIndex
	pathExtract    PathExtractor
}

// New creates a caching Middleware from config, applying DefaultConfig for unset fields.
func New(config Config) (*Middleware, error) {
	config, err := resolveConfig(config)
	if err != nil {
		return nil, err
	}

	// resolveConfig already zeroed CleanupInterval/MaxSize/MaxBodySize for the
	// corresponding Disable* flags.
	cacheConfig := kioshun.Config{
		MaxSize:         config.MaxSize,
		ShardCount:      config.ShardCount,
		CleanupInterval: config.CleanupInterval,
		DefaultTTL:      config.DefaultTTL,
		EvictionPolicy:  config.EvictionPolicy,
		ProbationRatio:  config.ProbationRatio,
		GhostRatio:      config.GhostRatio,
		StatsEnabled:    !config.DisableStats,
	}

	m := &Middleware{
		keyGen:      DefaultKeyGenerator,
		policy:      DefaultCachePolicy(config),
		hitHeader:   config.HitHeader,
		missHeader:  config.MissHeader,
		ignore:      ignoredHeaders(config),
		methods:     cacheableMethodSet(config.CacheableMethods),
		maxBodySize: config.MaxBodySize,
		limitBody:   !config.DisableBodySizeLimit,
		patternIdx:  newPatternIndex(),
	}

	// pattern invalidation is opt-in: supplying a PathExtractor enables the index
	// and wires the cache's removal listener. Callers that
	// don't use Invalidate pay nothing for the index or the listener.
	var opts []kioshun.Option[string, *Response]
	if config.PathExtractor != nil {
		m.patternEnabled = true
		m.pathExtract = config.PathExtractor
		opts = append(opts, kioshun.WithOnRemove(m.onCacheRemove))
	}

	cache, err := kioshun.New[string, *Response](cacheConfig, opts...)
	if err != nil {
		return nil, err
	}
	m.cache = cache

	return m, nil
}

// Wrap wraps an HTTP handler with response caching.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := m.methods[r.Method]; !ok {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
				next.ServeHTTP(w, r)
				return
			}
			// A successful unsafe method invalidates the target URI (RFC 9111
			// section 4.4). Take the keys before the handler can change the
			// request. A body limit of 0 buffers nothing.
			req := *r
			req.Method = http.MethodGet
			getKey := m.keyGen(&req)
			req.Method = http.MethodHead
			headKey := m.keyGen(&req)
			rw := newResponseWriter(w, "", 0, true)
			next.ServeHTTP(rw, r)
			if rw.statusCode >= 200 && rw.statusCode < 400 {
				m.cache.Delete(getKey)
				m.cache.Delete(headKey)
			}
			return
		}
		if !cacheableRequest(r) {
			next.ServeHTTP(w, r)
			return
		}

		key := m.keyGen(r)
		if cached, found := m.cache.Get(key); found {
			match := true
			for name, values := range cached.vary {
				if !slices.Equal(r.Header.Values(name), values) {
					match = false
					break
				}
			}
			if match {
				m.serveCached(w, cached, key)
				return
			}
		}

		if m.onMiss != nil {
			m.onMiss(key)
		}

		// Judge the request as received, not as the handler left it.
		reqHeader := r.Header.Clone()
		rw := newResponseWriter(w, m.missHeader, m.maxBodySize, m.limitBody)
		next.ServeHTTP(rw, r)
		rw.finish()

		if !rw.cacheable() {
			return
		}

		// Storage restrictions for a shared cache (RFC 9111 section 3) apply
		// whatever the policy decides. A 206 is incomplete (section 3.3).
		cc := strings.Join(rw.headers.Values("Cache-Control"), ",")
		switch {
		case rw.statusCode == http.StatusPartialContent,
			hasCacheControlDirective(cc, "no-store", "no-cache", "private"),
			len(reqHeader.Values("Authorization")) > 0 && !hasCacheControlDirective(cc, "public", "s-maxage", "must-revalidate"),
			len(rw.headers.Values("Set-Cookie")) > 0,
			hasCacheControlDirective(strings.Join(reqHeader.Values("Cache-Control"), ","), "no-store"):
			return
		}

		body := rw.buf.Bytes()
		shouldCache, ttl := m.policy(r, rw.statusCode, rw.headers, body)
		if !shouldCache {
			return
		}
		var vary http.Header
		for name := range strings.SplitSeq(strings.Join(rw.headers.Values("Vary"), ","), ",") {
			name = http.CanonicalHeaderKey(strings.TrimSpace(name))
			switch name {
			case "":
				continue
			case "*":
				return
			}
			if vary == nil {
				vary = make(http.Header)
			}
			// Clone so the entry does not keep the whole request header alive.
			vary[name] = slices.Clone(reqHeader.Values(name))
		}
		now := time.Now()
		headers := m.cachedHeaders(rw.headers)
		if headers.Get("Date") == "" {
			headers.Set("Date", now.UTC().Format(http.TimeFormat))
		}
		cached := &Response{
			StatusCode: rw.statusCode,
			Headers:    headers,
			Body:       bytes.Clone(body),
			CachedAt:   now,
			vary:       vary,
			age:        initialAge(rw.headers, now),
		}
		if err := m.store(key, cached, ttl); err == nil && m.onSet != nil {
			m.onSet(key, ttl)
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

// Invalidate removes cached entries whose path matches urlPattern and returns
// the number removed. urlPattern is either an exact path ("/api/users") or a
// prefix ending in "*" ("/api/*"), matched against the path recovered by the
// configured PathExtractor. Without a PathExtractor (see Config) the index is
// empty and this returns 0.
func (m *Middleware) Invalidate(urlPattern string) int {
	if !m.patternEnabled {
		return 0
	}

	removed := 0
	for _, key := range m.patternIdx.getMatchingKeys(urlPattern) {
		if m.cache.Delete(key) {
			removed++
		}
	}
	return removed
}

// InvalidateByFunc removes cached entries whose key matches fn and returns the
// number removed. When pattern invalidation is enabled, the index is reconciled
// by the cache's removal listener as the matched entries are deleted.
func (m *Middleware) InvalidateByFunc(fn func(string) bool) int {
	removed := 0
	for _, key := range m.cache.Keys() {
		if fn(key) && m.cache.Delete(key) {
			removed++
		}
	}
	return removed
}

// store writes resp to the cache. When pattern invalidation is enabled it
// indexes the key before the write, so a removal during Set - including a
// SIEVE admission rejection that drops the entry immediately - is reported to
// onCacheRemove and cleaned up.
func (m *Middleware) store(key string, resp *Response, ttl time.Duration) error {
	if !m.patternEnabled {
		return m.cache.Set(key, resp, ttl)
	}

	path := m.pathExtract(key)
	if path == "" {
		return m.cache.Set(key, resp, ttl)
	}

	m.patternIdx.addKey(path, key, resp)
	if err := m.cache.Set(key, resp, ttl); err != nil {
		m.patternIdx.removeKeyByIdentity(path, key, resp)
		return err
	}
	return nil
}

// onCacheRemove drops a key from the pattern index when the backing cache removes
// it. It is wired as the cache's removal listener only when pattern invalidation
// is enabled. The reason is irrelevant here: any removal (including an admission
// rejection) means the key should leave the index. Removal is by response
// identity, so a notification that arrives after the key was recached leaves the
// live entry in place (see patternIndex).
func (m *Middleware) onCacheRemove(key string, resp *Response, _ kioshun.RemovalReason) {
	path := m.pathExtract(key)
	if path == "" {
		return
	}
	m.patternIdx.removeKeyByIdentity(path, key, resp)
}

// Close releases middleware resources.
func (m *Middleware) Close() error {
	return m.cache.Close()
}

// SetKeyGenerator overrides the cache key generator.
func (m *Middleware) SetKeyGenerator(keyGen KeyGenerator) { m.keyGen = keyGen }

// SetCachePolicy overrides the policy deciding what to cache and for how long.
func (m *Middleware) SetCachePolicy(policy Policy) { m.policy = policy }

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

	age := (cached.age + time.Since(cached.CachedAt)) / time.Second
	w.Header().Set(m.hitHeader, "HIT")
	w.Header().Set("Age", strconv.FormatInt(int64(age), 10))

	w.WriteHeader(cached.StatusCode)
	_, _ = w.Write(cached.Body)
}

func ignoredHeaders(config Config) map[string]struct{} {
	headers := make(map[string]struct{}, len(config.IgnoreHeaders)+3)
	for _, name := range config.IgnoreHeaders {
		if name != "" {
			headers[http.CanonicalHeaderKey(name)] = struct{}{}
		}
	}
	for _, name := range []string{
		"X-Cache",
		config.HitHeader,
		config.MissHeader,
	} {
		if name != "" {
			headers[http.CanonicalHeaderKey(name)] = struct{}{}
		}
	}
	return headers
}

// cachedHeaders copies the headers to store. It leaves out ignored headers and
// the hop-by-hop fields, which describe one connection (RFC 9110 section 7.6.1).
func (m *Middleware) cachedHeaders(headers http.Header) http.Header {
	hop := []string{"Connection", "Keep-Alive", "Proxy-Connection"}
	for name := range strings.SplitSeq(strings.Join(headers.Values("Connection"), ","), ",") {
		if name = strings.TrimSpace(name); name != "" {
			hop = append(hop, http.CanonicalHeaderKey(name))
		}
	}
	cached := make(http.Header, len(headers))
	for name, values := range headers {
		if _, ignored := m.ignore[name]; ignored || slices.Contains(hop, name) {
			continue
		}
		cached[name] = slices.Clone(values)
	}
	return cached
}

// cacheableRequest reports whether r may be served from or stored in the
// cache. Conditional and Range requests go to the origin.
func cacheableRequest(r *http.Request) bool {
	for _, name := range []string{"Range", "If-Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		if len(r.Header.Values(name)) > 0 {
			return false
		}
	}
	return true
}

// DefaultKeyGenerator hashes the method and target URI. Responses that vary
// on request headers are told apart by their Vary field instead.
func DefaultKeyGenerator(r *http.Request) string {
	sum := md5.Sum([]byte(r.Method + " " + targetURI(r)))
	return hex.EncodeToString(sum[:])
}

// targetURI returns the absolute target URI of a server request, whose
// authority is in r.Host while r.URL holds only the path and query.
func targetURI(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.URL.RequestURI()
}

// DefaultCachePolicy builds a policy that stores responses with explicit
// freshness. The lifetime is s-maxage, then max-age, then Expires minus Date.
// A response marked public without any of these gets DefaultTTL. The result
// is the lifetime minus the response's current age.
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

		now := time.Now()
		cc := strings.Join(headers.Values("Cache-Control"), ",")
		var lifetime time.Duration
		if sMaxAge, ok := cacheControlSeconds(cc, "s-maxage"); ok {
			lifetime = sMaxAge
		} else if maxAge, ok := cacheControlSeconds(cc, "max-age"); ok {
			lifetime = maxAge
		} else if expires := headers.Values("Expires"); len(expires) > 0 {
			// An invalid Expires means already expired (RFC 9111 section 5.3).
			t, err := http.ParseTime(expires[0])
			if err != nil {
				return false, 0
			}
			date, err := http.ParseTime(headers.Get("Date"))
			if err != nil {
				date = now
			}
			lifetime = t.Sub(date)
		} else if hasCacheControlDirective(cc, "public") {
			if config.DefaultTTL == kioshun.NoExpiration {
				return true, kioshun.NoExpiration
			}
			lifetime = config.DefaultTTL
		} else {
			return false, 0
		}

		ttl := lifetime - initialAge(headers, now)
		return ttl > 0, ttl
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

// PathBasedKeyGenerator keys responses as "METHOD:host/path", using the
// escaped path. It ignores the query string, so it suits only resources whose
// response does not depend on the query.
func PathBasedKeyGenerator(r *http.Request) string {
	return r.Method + ":" + r.Host + r.URL.EscapedPath()
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
	// Canonicalize, merging keys in sorted order as net/http writes them, so the
	// rules see fields the handler set under non-canonical keys.
	h := rw.Header()
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	slices.Sort(names)
	rw.headers = make(http.Header, len(h))
	for _, name := range names {
		canonical := http.CanonicalHeaderKey(name)
		rw.headers[canonical] = append(rw.headers[canonical], h[name]...)
	}
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

// KeyWithVaryHeaders hashes the method, target URI, and the named request headers.
func KeyWithVaryHeaders(varyHeaders []string) KeyGenerator {
	return func(r *http.Request) string {
		h := md5.New()
		h.Write([]byte(r.Method + " " + targetURI(r)))
		for _, name := range varyHeaders {
			h.Write([]byte("\n" + name + ": " + strings.Join(r.Header.Values(name), ",")))
		}
		return hex.EncodeToString(h.Sum(nil))
	}
}

// KeyWithoutQuery keys responses as "METHOD:host/path". It ignores the query
// string, so requests that differ only in the query share one entry. Use it
// only for resources whose response does not depend on the query.
// It is PathBasedKeyGenerator in the form New-style call sites expect.
func KeyWithoutQuery() KeyGenerator {
	return PathBasedKeyGenerator
}

// KeyWithoutQueryHashed hashes the method, host, escaped path, and the Accept,
// Accept-Encoding and Authorization request headers. It ignores the query string.
func KeyWithoutQueryHashed() KeyGenerator {
	return func(r *http.Request) string {
		h := md5.New()
		h.Write([]byte(r.Method + " " + r.Host + r.URL.EscapedPath()))
		for _, name := range []string{"Accept", "Accept-Encoding", "Authorization"} {
			h.Write([]byte("\n" + name + ": " + strings.Join(r.Header.Values(name), ",")))
		}
		return hex.EncodeToString(h.Sum(nil))
	}
}

// PathExtractorFromKey extracts the path from "METHOD:host/path" keys. It also
// accepts "METHOD:/path" keys.
func PathExtractorFromKey(key string) string {
	_, rest, ok := strings.Cut(key, ":")
	if !ok {
		return key
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i:]
	}
	return rest
}

// KeyWithUserID hashes the method, target URI, and a user identifier header.
// The header must be set by trusted code in front of the cache, since a
// client can send any value.
func KeyWithUserID(userIDHeader string) KeyGenerator {
	return func(r *http.Request) string {
		h := md5.New()
		h.Write([]byte(r.Method + " " + targetURI(r)))
		h.Write([]byte("\n" + userIDHeader + ": " + strings.Join(r.Header.Values(userIDHeader), ",")))
		return hex.EncodeToString(h.Sum(nil))
	}
}

// AlwaysCache caches successful 2xx responses for the supplied TTL. Wrap still
// refuses responses that a shared cache must not store.
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

// ByContentType assigns TTLs by Content-Type prefix. Wrap still refuses
// responses that a shared cache must not store.
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
// Wrap still refuses responses that a shared cache must not store.
func BySize(minSize, maxSize int64, ttl time.Duration) Policy {
	return func(_ *http.Request, statusCode int, _ http.Header, body []byte) (bool, time.Duration) {
		if statusCode < 200 || statusCode >= 300 {
			return false, 0
		}

		size := int64(len(body))
		return size >= minSize && size <= maxSize, ttl
	}
}

// cacheControlSeconds reports the delta-seconds value of the first directive
// named in cc. An invalid value reads as 0, which makes the response stale.
func cacheControlSeconds(cc, directive string) (time.Duration, bool) {
	for part := range strings.SplitSeq(cc, ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		if !strings.EqualFold(strings.TrimSpace(name), directive) {
			continue
		}
		seconds, _ := deltaSeconds(value)
		return seconds, true
	}
	return 0, false
}

// initialAge returns the corrected initial age of a response received at now:
// the larger of its apparent age from Date and its Age field, at most 2^31
// seconds (RFC 9111 section 4.2.3).
func initialAge(h http.Header, now time.Time) time.Duration {
	var age time.Duration
	if date, err := http.ParseTime(h.Get("Date")); err == nil {
		age = now.Sub(date)
	}
	if seconds, ok := deltaSeconds(h.Get("Age")); ok {
		age = max(age, seconds)
	}
	return min(max(age, 0), (1<<31)*time.Second)
}

// deltaSeconds parses a delta-seconds value (RFC 9111 section 1.2.2). It reads
// the first member of a list, ignores surrounding spaces and one surrounding
// pair of quotes, and clamps values above 2^31.
func deltaSeconds(s string) (time.Duration, bool) {
	s, _, _ = strings.Cut(s, ",")
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	seconds, err := strconv.ParseUint(s, 10, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return time.Duration(min(seconds, 1<<31)) * time.Second, true
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
