# HTTP Middleware

Kioshun provides HTTP middleware for transparent response caching. The middleware integrates with standard `net/http` and popular frameworks like Gin, Echo, Chi, and Gorilla Mux.

The middleware acts as a shared cache and follows the storage and freshness rules of RFC 9111. With the default policy, it stores only responses that say how long they stay fresh, so a handler opts in with `Cache-Control` (`max-age`, `s-maxage` or `public`) or `Expires`. See [What Is Cached](#what-is-cached).

## Quick Start

```go
package main

import (
    "net/http"
    "time"

    "github.com/unkn0wn-root/kioshun/httpcache"
)

func usersHandler(w http.ResponseWriter, r *http.Request) {
    // Without explicit freshness the response is not stored.
    w.Header().Set("Cache-Control", "public, max-age=60")
    w.Header().Set("Content-Type", "application/json")
    w.Write([]byte(`["alice","bob"]`))
}

func main() {
    // Create middleware with defaults (SieveTinyLFU eviction)
    config := httpcache.DefaultConfig()
    config.DefaultTTL = 5 * time.Minute
    config.MaxSize = 100000

    middleware, err := httpcache.New(config)
    if err != nil {
        panic(err)
    }
    defer middleware.Close()

    // Wrap your handlers
    http.Handle("/api/users", middleware.Wrap(http.HandlerFunc(usersHandler)))
    http.ListenAndServe(":8080", nil)
}
```

`DefaultTTL` is the lifetime for responses marked `public` that carry no `max-age`, `s-maxage` or `Expires`. It does not make other responses cacheable. A response with no freshness information and no `public` is not stored. `DefaultTTL` is also used when a custom policy returns a TTL of 0.

## HTTP Caching

### Basic Setup

```go
config := httpcache.DefaultConfig() // default uses SieveTinyLFU
config.DefaultTTL = 5 * time.Minute
config.MaxSize = 100000
config.ShardCount = 16

middleware, err := httpcache.New(config)
if err != nil {
    // handle invalid configuration
}
defer middleware.Close()

// Use with any HTTP framework
http.Handle("/api/users", middleware.Wrap(usersHandler))
```

To invalidate entries by URL pattern, see [Cache Invalidation Setup](#cache-invalidation-setup).

## Framework Compatibility

The middleware works seamlessly with all major Go HTTP frameworks:

```go
// Standard net/http
http.Handle("/api/users", middleware.Wrap(handler))

// Gin Framework
router.Use(gin.WrapH(middleware.Wrap(http.DefaultServeMux)))

// Echo Framework
e.Use(echo.WrapMiddleware(middleware.Wrap))

// Chi Router
r.Use(middleware.Wrap)

// Gorilla Mux
r.Use(middleware.Wrap)
```

## Middleware Configuration

### Advanced Configuration Examples

```go
apiConfig := httpcache.DefaultConfig() // default config uses SieveTinyLFU
apiConfig.MaxSize = 50000
apiConfig.ShardCount = 32
apiConfig.DefaultTTL = 10 * time.Minute
apiConfig.MaxBodySize = 5 * 1024 * 1024 // 5MB

apiMiddleware, err := httpcache.New(apiConfig)
if err != nil {
    // handle invalid configuration
}

// User-specific caching
userMiddleware, err := httpcache.New(config)
if err != nil {
    // handle invalid configuration
}
// X-User-ID must be set by trusted code in front of the cache, such as your
// auth middleware. A client can send any value.
userMiddleware.SetKeyGenerator(httpcache.KeyWithUserID("X-User-ID"))

// Content-type based caching with different TTLs
contentMiddleware, err := httpcache.New(config)
if err != nil {
    // handle invalid configuration
}
contentMiddleware.SetCachePolicy(httpcache.ByContentType(map[string]time.Duration{
    "application/json": 5 * time.Minute,
    "text/html":       10 * time.Minute,
    "image/":          1 * time.Hour,
}, 2*time.Minute))

// Size-based conditional caching
conditionalMiddleware, err := httpcache.New(config)
if err != nil {
    // handle invalid configuration
}
conditionalMiddleware.SetCachePolicy(httpcache.BySize(100, 1024*1024, 3*time.Minute))

// Configure eviction policy
cacheConfig := httpcache.DefaultConfig()
cacheConfig.EvictionPolicy = kioshun.FIFO
cacheConfig.MaxSize = 100000
cacheConfig.DefaultTTL = 5 * time.Minute

middleware, err := httpcache.New(cacheConfig)
if err != nil {
    // handle invalid configuration
}

```

## Built-in Key Generators

Cache keys determine how responses are stored and retrieved. Choose the right key generator based on your caching requirements:

```go
// Default key generator (method + scheme + host + path + query)
middleware.SetKeyGenerator(httpcache.DefaultKeyGenerator)

// User-specific keys. The header must come from trusted code, not the client.
middleware.SetKeyGenerator(httpcache.KeyWithUserID("X-User-ID"))

// Custom vary headers
middleware.SetKeyGenerator(httpcache.KeyWithVaryHeaders([]string{"Accept", "Authorization"}))

// Ignore query parameters. Requests that differ only in the query share one entry.
middleware.SetKeyGenerator(httpcache.KeyWithoutQuery())
```

The default key does not include request headers. A response that depends on a request header should name it in `Vary`, and the cache then checks that header on every hit. See [Vary](#vary).

## Eviction Policies

The middleware supports different eviction algorithms that can be configured based on different access patterns:

```go
config := httpcache.DefaultConfig()

// FIFO (First In, First Out)
config.EvictionPolicy = kioshun.FIFO

// LRU (Least Recently Used)
config.EvictionPolicy = kioshun.LRU

// LFU (Least Frequently Used)
config.EvictionPolicy = kioshun.LFU

// SieveTinyLFU - Probation SIEVE with TinyLFU admission
config.EvictionPolicy = kioshun.SieveTinyLFU
```

See [internals.md](internals.md) for detailed information about eviction policies and their performance characteristics.

## Built-in Cache Policies

Control what gets cached and for how long with flexible cache policies:

```go
// Always cache successful responses
middleware.SetCachePolicy(httpcache.AlwaysCache(5 * time.Minute))

// Never cache
middleware.SetCachePolicy(httpcache.NeverCache())

// Content-type based caching
middleware.SetCachePolicy(httpcache.ByContentType(map[string]time.Duration{
    "application/json": 5 * time.Minute,
    "text/html":       10 * time.Minute,
}, 2*time.Minute))

// Size-based caching
middleware.SetCachePolicy(httpcache.BySize(100, 1024*1024, 3*time.Minute))
```

A policy picks whether to store a response and for how long. It cannot store a response that a shared cache must not store. The [storage rules](#storage-rules) apply whatever the policy returns, so a `206` or a response with `no-store`, `no-cache`, `private`, `Set-Cookie` or `Vary: *` is never stored. A custom policy's TTL replaces `max-age`, `s-maxage`, `Expires` and `Age`, but Wrap's storage restrictions still apply. A TTL of 0 means `DefaultTTL`.

## Monitoring

Real-time observability into cache behavior:

```go
// Cache hit/miss callbacks
middleware.OnHit(func(key string) {
    fmt.Printf("Cache hit: %s\n", key)
})

middleware.OnMiss(func(key string) {
    fmt.Printf("Cache miss: %s\n", key)
})

middleware.OnSet(func(key string, ttl time.Duration) {
    fmt.Printf("Cache set: %s (TTL: %v)\n", key, ttl)
})

// Get cache statistics
stats := middleware.Stats()
fmt.Printf("Hit ratio: %.2f%%\n", stats.HitRatio*100)
```

## Cache Management

### Basic Operations

```go
// Get cache statistics
stats := middleware.Stats()

// Clear all cached responses
middleware.Clear()

// Invalidate by function
middleware.InvalidateByFunc(func(key string) bool {
    return strings.Contains(key, "user")
})

// Close middleware
middleware.Close()
```

### Automatic Invalidation

A request with any method other than GET, HEAD, OPTIONS or TRACE passes through to the handler. If its response status is 2xx or 3xx, the stored GET and HEAD responses for the same URL are removed. A successful `POST /api/users` therefore removes the cached `GET /api/users`. The entries are found with the configured key generator, so with a key that includes request headers only the entry for the same header values is removed. Methods listed in `CacheableMethods` are cached instead and do not invalidate.

## Cache Invalidation Setup

**Cache invalidation by URL pattern requires specific key generator configuration.**

### The Problem

The default key generator uses MD5 hashing which makes pattern-based invalidation impossible:

```go
// DEFAULT SETUP - Invalidation won't work
config := httpcache.DefaultConfig()
middleware, err := httpcache.New(config)
if err != nil {
    // handle invalid configuration
}

// This returns 0 removed entries because keys are hashed
removed := middleware.Invalidate("/api/users/*") // Returns 0
```

**Why it fails:**
- Default keys: `"a1b2c3d4e5f6..."` (MD5 hash)
- Pattern matching needs: `"GET:example.com/api/users/123"` (readable path)
- Hash loses original URL information

### The Solution

Use a path-based key generator **and** give the config a `PathExtractor` so the
pattern index can recover the path from each key. Supplying a `PathExtractor` is
what enables pattern invalidation: it turns on the index and wires the cache's
removal listener that keeps the index in sync as entries are evicted, rejected,
expired or deleted.

```go
// CORRECT SETUP - Invalidation works
config := httpcache.DefaultConfig()
config.PathExtractor = httpcache.PathExtractorFromKey // enables pattern invalidation
middleware, err := httpcache.New(config)
if err != nil {
    // handle invalid configuration
}

// IMPORTANT: use a path-based key generator so keys carry a recoverable path.
// It ignores the query string, so use it only where the query does not change the response.
middleware.SetKeyGenerator(httpcache.KeyWithoutQuery())

// Now invalidation works
removed := middleware.Invalidate("/api/users/*") // Returns actual count
```

### Key Generator Comparison

| Key Generator | Example Key | Host in key | Query in key | Pattern invalidation | Use Case |
|---------------|-------------|-------------|--------------|----------------------|----------|
| `DefaultKeyGenerator` | `"a1b2c3d4..."` | Yes | Yes | No | Default |
| `KeyWithVaryHeaders(names)` | `"a1b2c3d4..."` | Yes | Yes | No | Named request headers in the key |
| `KeyWithUserID(header)` | `"a1b2c3d4..."` | Yes | Yes | No | One entry per user, header set by trusted code |
| `KeyWithoutQueryHashed()` | `"a1b2c3d4..."` | Yes | No | No | Accept, Accept-Encoding and Authorization in the key |
| `KeyWithoutQuery()` + `Config.PathExtractor` | `"GET:example.com/api/users"` | Yes | No | Yes | **Recommended for invalidation** |
| `PathBasedKeyGenerator` + `Config.PathExtractor` | `"GET:example.com/api/users"` | Yes | No | Yes | Same as `KeyWithoutQuery()` |

With a generator that ignores the query string, requests that differ only in the query share one entry. Keys use the escaped path, so `/files/a%2Fb` and `/files/a/b` are different entries.

### Minimal Pattern Invalidation Setup

```go
config := httpcache.DefaultConfig()
config.PathExtractor = httpcache.PathExtractorFromKey            // map keys -> paths (enables invalidation)
middleware, err := httpcache.New(config)
if err != nil {
    // handle invalid configuration
}
middleware.SetKeyGenerator(httpcache.KeyWithoutQuery())          // readable keys, query ignored

// later…
removed := middleware.Invalidate("/api/users/*")
```

### Complete Working Example

```go
package main

import (
    "encoding/json"
    "fmt"
    "net/http"
    "time"

    "github.com/unkn0wn-root/kioshun/httpcache"
)

func main() {
    // Setup middleware
    config := httpcache.DefaultConfig()
    config.DefaultTTL = 10 * time.Minute
    config.PathExtractor = httpcache.PathExtractorFromKey // enables pattern invalidation

    middleware, err := httpcache.New(config)
    if err != nil {
        panic(err)
    }
    defer middleware.Close()

    // Use a path-based key generator so keys carry a recoverable path.
    // These handlers ignore the query string, so the key can too.
    middleware.SetKeyGenerator(httpcache.KeyWithoutQuery())

    // Setup handlers
    http.Handle("/api/users", middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // public without max-age: stored for DefaultTTL
        w.Header().Set("Cache-Control", "public")
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]any{
            "users": []string{"alice", "bob", "charlie"},
            "cached_at": time.Now(),
        })
    })))

    http.Handle("/api/users/", middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Cache-Control", "public, max-age=60")
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]any{
            "user": "user-data",
            "cached_at": time.Now(),
        })
    })))

    // Invalidation endpoint
    http.HandleFunc("/admin/invalidate", func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
            return
        }

        pattern := r.URL.Query().Get("pattern")
        if pattern == "" {
            http.Error(w, "pattern parameter required", http.StatusBadRequest)
            return
        }

        // This now works!
        removed := middleware.Invalidate(pattern)

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]any{
            "message": fmt.Sprintf("Invalidated %d entries", removed),
            "pattern": pattern,
        })
    })

    fmt.Println("Server starting on :8080")
    fmt.Println("\nTest caching:")
    fmt.Println("  curl http://localhost:8080/api/users")
    fmt.Println("  curl http://localhost:8080/api/users/123")
    fmt.Println("\nTest invalidation:")
    fmt.Println("  curl -X POST 'http://localhost:8080/admin/invalidate?pattern=/api/users/*'")

    http.ListenAndServe(":8080", nil)
}
```

### When to Use Each Approach

**Use `KeyWithoutQuery()` when:**
- You need pattern-based invalidation
- Query parameters don't affect response content
- You want readable cache keys for debugging

**Use `DefaultKeyGenerator` when:**
- You don't need pattern invalidation
- Query parameters affect response content
- You only use `Clear()` for cache management


## What Is Cached

The middleware behaves as a shared cache under RFC 9111, with the limits listed under [Not Supported](#not-supported).

### Storage Rules

These rules apply under every policy. A response is not stored when:

- the request has `Range`, `If-Range`, `If-Match`, `If-None-Match`, `If-Modified-Since` or `If-Unmodified-Since`
- the request has `Cache-Control: no-store`
- the status is `206 Partial Content`
- the response `Cache-Control` has `no-store`, `no-cache` or `private`
- the request has `Authorization` and the response `Cache-Control` has none of `public`, `s-maxage` or `must-revalidate`
- the response has `Set-Cookie`
- the response has `Vary: *`
- the handler flushed, hijacked the connection or switched protocols
- the body is larger than `MaxBodySize`, unless `DisableBodySizeLimit` is set

All field lines of `Cache-Control` and `Vary` count, so `Cache-Control: max-age=60` followed by `Cache-Control: private` is not stored. A field counts as present even when its value is empty, so `Authorization:` with no value still prevents storage. Field names match in any letter case. The rules look at the request as the client sent it, so a handler that removes `Authorization` or changes a header named in `Vary` does not change the outcome.

The default policy also requires a method in `CacheableMethods` (GET and HEAD by default), a status in `CacheableStatus` (200, 201, 300, 301, 302, 404 and 410 by default) and explicit freshness.

### Freshness

The default policy takes the lifetime from the first of these that is present:

1. `s-maxage`
2. `max-age`
3. `Expires` minus `Date`, or minus the current time when `Date` is missing
4. `DefaultTTL`, only when `Cache-Control` has `public`

A response with none of these is not stored. An `Expires` value that cannot be parsed, such as `0`, means the response is already stale. `Expires` accepts all three HTTP date formats. Quoted values such as `max-age="60"` are accepted, an invalid value means stale, and values above 2^31 seconds count as 2^31.

The response's age is subtracted from the lifetime. The age is the larger of the time since its `Date` and its `Age` header, at most 2^31 seconds. A list-valued `Age` uses its first member. A response that is already stale when it arrives is not stored.

### Hits

A hit returns the stored status, headers and body, plus `X-Cache: HIT` and `Age` in whole seconds. The origin's `Date` is kept. A stored response without `Date` gets one when it is stored. A miss gets `X-Cache: MISS`. Headers listed in `IgnoreHeaders` (`Server`, `X-Request-Id` and `X-Trace-Id` by default) are not stored. Neither are the hop-by-hop fields `Connection`, `Keep-Alive`, `Proxy-Connection` and every field that `Connection` names.

GET and HEAD responses are stored separately, so a stored HEAD never answers a GET.

### Vary

The cache records the request's values for each header the response names in `Vary`. A later request is a hit only when its values for those headers are the same. A header that was absent must still be absent. The cache keeps one variant per URL, so a request with different values goes to the origin and its response replaces the stored one.

Request headers that are not named in `Vary` are not part of the default key. A response that depends on `Cookie` or another request header must name it in `Vary` or send `Cache-Control: private`.

### Conditional and Range Requests

Requests with `Range`, `If-Range`, `If-Match`, `If-None-Match`, `If-Modified-Since` or `If-Unmodified-Since` go straight to the origin. The cache neither answers them nor stores their responses, so a `304` or `206` is never served to a client that asked for the full response.

### Authorization and Cookies

A response to a request with `Authorization` is stored only when its `Cache-Control` has `public`, `s-maxage` or `must-revalidate`. A response with `Set-Cookie` is never stored, so one client's cookie is never sent to another.

### Outcomes

| Outcome | When | X-Cache | Origin called | Effect |
|---------|------|---------|---------------|--------|
| Bypass | OPTIONS or TRACE, or GET or HEAD removed from `CacheableMethods` | none | yes | none |
| Invalidation | Any other method not in `CacheableMethods` | none | yes | On 2xx or 3xx, removes the stored GET and HEAD responses for the URL |
| Bypass | Conditional or Range request | none | yes | Nothing is looked up or stored |
| Hit | Stored, fresh, and the `Vary` headers match | HIT, plus `Age` | no | `OnHit` |
| Miss, stored | Storable, the policy agrees, and some lifetime is left | MISS | yes | Stored. `OnMiss`, `OnSet` |
| Miss, not stored | A storage rule applies, the policy refuses, or the response is stale | MISS | yes | `OnMiss` |
| Vary mismatch | Stored, but the `Vary` headers differ | MISS | yes | Replaced if storable |

### Not Supported

- Revalidating stale responses with `ETag` or `Last-Modified`. A stale response is dropped.
- Answering a conditional request with `304` from the cache.
- More than one `Vary` variant per URL.
- Request directives other than `no-store`. `no-cache`, `max-age`, `min-fresh`, `max-stale` and `only-if-cached` in a request are ignored.
- `stale-while-revalidate` and `stale-if-error`.
- Invalidating the `Location` and `Content-Location` URLs after an unsafe method.
- Collapsing concurrent misses for the same key into one origin request.
- Replaying trailers on a hit.
- The `response_delay` term of the age calculation (RFC 9111 section 4.2.3). The time the handler took to answer is not added to the response's `Age`.
- Quoted `Cache-Control` values that contain commas. The value is split at each comma, so `ext="a, max-age=600, b"` is read as containing `max-age=600`.
- Full invalidation with the header-keyed generators. With `KeyWithUserID`, `KeyWithVaryHeaders` or `KeyWithoutQueryHashed`, an unsafe method removes only the entry whose key matches the unsafe request's headers. Entries stored for other header values stay until they expire, which falls short of RFC 9111 section 4.4.
- Telling http from https in the path generators. `KeyWithoutQuery`, `PathBasedKeyGenerator` and `KeyWithoutQueryHashed` leave out the scheme, so `http://` and `https://` requests for the same host and path share an entry.
