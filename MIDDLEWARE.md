# HTTP Middleware

Kioshun provides HTTP middleware for transparent response caching. The middleware integrates with standard `net/http` and popular frameworks like Gin, Echo, Chi, and Gorilla Mux.

## Quick Start

```go
package main

import (
    "net/http"
    "time"

    "github.com/unkn0wn-root/kioshun/httpcache"
)

func main() {
    // Create middleware with defaults (FIFO eviction)
    config := httpcache.DefaultConfig()
    config.DefaultTTL = 5 * time.Minute
    config.MaxSize = 100000

    middleware, err := httpcache.New(config)
    if err != nil {
        panic(err)
    }
    defer middleware.Close()

    // Wrap your handlers
    http.Handle("/api/users", middleware.Middleware(usersHandler))
    http.ListenAndServe(":8080", nil)
}
```

## HTTP Caching

### Basic Setup

```go
config := httpcache.DefaultConfig() // default uses FIFO
config.DefaultTTL = 5 * time.Minute
config.MaxSize = 100000
config.ShardCount = 16

middleware, err := httpcache.New(config)
if err != nil {
    // handle invalid configuration
}
defer middleware.Close()

// IMPORTANT: Enable invalidation if needed
// middleware.SetKeyGenerator(httpcache.KeyWithoutQuery())

// Use with any HTTP framework
http.Handle("/api/users", middleware.Middleware(usersHandler))
```

## Framework Compatibility

The middleware works seamlessly with all major Go HTTP frameworks:

```go
// Standard net/http
http.Handle("/api/users", middleware.Middleware(handler))

// Gin Framework
router.Use(gin.WrapH(middleware.Middleware(http.DefaultServeMux)))

// Echo Framework
e.Use(echo.WrapMiddleware(middleware.Middleware))

// Chi Router
r.Use(middleware.Middleware)

// Gorilla Mux
r.Use(middleware.Middleware)
```

## Middleware Configuration

### Advanced Configuration Examples

```go
apiConfig := httpcache.DefaultConfig() // default config uses FIFO
apiConfig.MaxSize = 50000
apiConfig.ShardCount = 32
apiConfig.DefaultTTL = 10 * time.Minute
apiConfig.MaxBodySize = 5 * 1024 * 1024 // 5MB

apiMiddleware, err := httpcache.New(apiConfig)
if err != nil {
    // handle invalid configuration
}
// Enable invalidation for API endpoints
apiMiddleware.SetKeyGenerator(httpcache.KeyWithoutQuery())

// User-specific caching
userMiddleware, err := httpcache.New(config)
if err != nil {
    // handle invalid configuration
}
userMiddleware.SetKeyGenerator(httpcache.KeyWithUserID("X-User-ID"))
// Note: User-specific caching uses different key format - invalidation works differently

// Content-type based caching with different TTLs
contentMiddleware, err := httpcache.New(config)
if err != nil {
    // handle invalid configuration
}
contentMiddleware.SetKeyGenerator(httpcache.KeyWithoutQuery()) // Enable invalidation
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
conditionalMiddleware.SetKeyGenerator(httpcache.KeyWithoutQuery()) // Enable invalidation
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
// Default key generator (method + URL + vary headers)
middleware.SetKeyGenerator(httpcache.DefaultKeyGenerator)

// User-specific keys
middleware.SetKeyGenerator(httpcache.KeyWithUserID("X-User-ID"))

// Custom vary headers
middleware.SetKeyGenerator(httpcache.KeyWithVaryHeaders([]string{"Accept", "Authorization"}))

// Ignore query parameters
middleware.SetKeyGenerator(httpcache.KeyWithoutQuery())
```

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

See [ARCHITECTURE.md](ARCHITECTURE.md) for detailed information about eviction policies and their performance characteristics.

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
- Pattern matching needs: `"GET:/api/users/123"` (readable path)
- Hash loses original URL information

### The Solution

Use a path-based key generator **and** teach the pattern index how to recover the path from each key:

```go
// CORRECT SETUP - Invalidation works
config := httpcache.DefaultConfig()
middleware, err := httpcache.New(config)
if err != nil {
    // handle invalid configuration
}

// IMPORTANT: Set path-based key generator and path extractor
middleware.SetKeyGenerator(httpcache.KeyWithoutQuery())
middleware.SetPathExtractor(httpcache.PathExtractorFromKey)

// Now invalidation works
removed := middleware.Invalidate("/api/users/*") // Returns actual count
```

### Key Generator Comparison

| Key Generator | Example Key | Invalidation | Use Case |
|---------------|-------------|--------------|----------|
| `DefaultKeyGenerator` | `"a1b2c3d4..."` | ❌ **Won't work** | No invalidation needed |
| `KeyWithoutQuery()` + `SetPathExtractor` | `"GET:/api/users"` | ✅ **Works** | **Recommended for invalidation** |
| `PathBasedKeyGenerator` + `SetPathExtractor` | `"GET:/api/users"` | ✅ **Works** | Simple path-based caching |
| `KeyWithVaryHeaders()` | `"a1b2c3d4..."` | ❌ **Won't work** | Custom headers + security |

### Minimal Pattern Invalidation Setup

```go
middleware, err := httpcache.New(httpcache.DefaultConfig())
if err != nil {
    // handle invalid configuration
}
middleware.SetKeyGenerator(httpcache.KeyWithoutQuery())          // readable keys
middleware.SetPathExtractor(httpcache.PathExtractorFromKey)      // map keys -> paths

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

    middleware, err := httpcache.New(config)
    if err != nil {
        panic(err)
    }
    defer middleware.Close()

    // Enable invalidation
    middleware.SetKeyGenerator(httpcache.KeyWithoutQuery())
    middleware.SetPathExtractor(httpcache.PathExtractorFromKey)

    // Setup handlers
    http.Handle("/api/users", middleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]any{
            "users": []string{"alice", "bob", "charlie"},
            "cached_at": time.Now(),
        })
    })))

    http.Handle("/api/users/", middleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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


## HTTP Compliance

The middleware automatically handles:
- **Cache-Control** headers (max-age, no-cache, no-store, private)
- **Expires** headers (RFC1123 format)
- **ETag** generation and validation
- **Vary** headers for content negotiation
- **X-Cache** headers (HIT/MISS status)
- **X-Cache-Date** and **X-Cache-Age** headers
