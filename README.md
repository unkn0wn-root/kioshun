<div align="center">
  <img src="assets/logo.JPG" alt="CaGo Logo" width="200"/>

  # CaGo - In-Memory Cache for Go

  [![Go Version](https://img.shields.io/badge/Go-1.21+-blue.svg)](https://golang.org)
  [![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

  *High-performance, thread-safe, sharded in-memory cache for Go*
</div>

## Installation

```bash
go get github.com/unkn0wn-root/cago
```

## Quick Start

```go
package main

import (
    "fmt"
    "time"

    "github.com/unkn0wn-root/cago"
)

func main() {
    // Create cache with default configuration
    cache := cache.NewWithDefaults[string, string]()
    defer cache.Close()

    // Set value with TTL
    cache.Set("user:123", "David Nice", 5*time.Minute)

    // Get value
    if value, found := cache.Get("user:123"); found {
        fmt.Printf("User: %s\n", value)
    }

    // Get cache statistics
    stats := cache.Stats()
    fmt.Printf("Hit ratio: %.2f%%\n", stats.HitRatio*100)
}
```

## Configuration

### Basic Configuration

```go
config := cache.Config{
    MaxSize:         100000,           // Maximum number of items
    ShardCount:      16,               // Number of shards (0 = auto-detect)
    CleanupInterval: 5 * time.Minute,  // Cleanup frequency
    DefaultTTL:      30 * time.Minute, // Default expiration time
    EvictionPolicy:  cache.LRU,        // Eviction algorithm
    StatsEnabled:    true,             // Enable statistics collection
}

cache := cache.New[string, interface{}](config)
```

### Predefined Configurations

```go
// For session storage
sessionCache := cache.New[string, Session](cache.SessionCacheConfig())

// For API response caching
apiCache := cache.New[string, APIResponse](cache.APICacheConfig())

// For temporary data
tempCache := cache.New[string, interface{}](cache.TemporaryCacheConfig())

// For persistent data
persistentCache := cache.New[string, interface{}](cache.PersistentCacheConfig())
```

## Architecture

### Sharded Design

CaGo uses a sharded architecture to minimize lock contention:

```
┌─────────────────────────────────────────────────────────────┐
│                      CaGo Cache                             │
├─────────────┬─────────────┬─────────────┬───────────────────┤
│   Shard 0   │   Shard 1   │   Shard 2   │   ...   │ Shard N │
│  RWMutex    │  RWMutex    │  RWMutex    │         │ RWMutex │
│  LRU List   │  LRU List   │  LRU List   │         │ LRU List│
│  LFU Heap   │  LFU Heap   │  LFU Heap   │         │ LFU Heap│
│  Hash Map   │  Hash Map   │  Hash Map   │         │ Hash Map│
│  Stats      │  Stats      │  Stats      │         │ Stats   │
└─────────────┴─────────────┴─────────────┴───────────────────┘
```

**Key Design Principles:**
- **Automatic Sharding**: Keys distributed across shards using optimized hash functions
- **Minimal Lock Contention**: Each shard maintains independent read-write mutex
- **Optimal Shard Count**: Auto-detection based on CPU cores
- **Memory Efficiency**: Object pooling to reduce GC pressure

### Hash Function Optimization
- **Integer Keys**: Multiplicative hashing with golden ratio constants
- **String Keys**: FNV-1a algorithm for fast, lock-free hashing
- **Other Types**: Fallback to string conversion with FNV-1a

### Eviction Policy Implementation

#### LRU (Least Recently Used)
- **Access**: Move to head in O(1)
- **Eviction**: Remove tail in O(1)
- **Memory**: Minimal overhead per item

#### LFU (Least Frequently Used) - **Optimized**
Uses min-heap for efficient frequency-based eviction:
- **Access**: Update frequency and rebalance heap in O(log n)
- **Eviction**: Remove minimum frequency item in O(log n)
- **Memory**: Additional heap index per item

**LFU Heap Structure:**
```
       Min Frequency Item (Root)
      /                        \
   Higher Freq              Higher Freq
  /          \              /          \
Items      Items        Items        Items
```

**Eviction Algorithm Comparison:**
| Policy | Access Time | Eviction Time | Memory Overhead |
|--------|-------------|---------------|-----------------|
| LRU    | O(1)        | O(1)          | Low            |
| LFU    | O(log n)    | O(log n)      | Medium         |
| FIFO   | O(1)        | O(1)          | Low            |
| Random | O(1)        | O(1)          | Low            |

### Concurrent Access Patterns
The sharded design enables high-throughput concurrent access:
1. **Read Operations**: Multiple goroutines can read from different shards simultaneously
2. **Write Operations**: Writers only block other operations on the same shard
3. **Cross-Shard Operations**: Statistics aggregation uses atomic operations to avoid blocking

### Memory Management
- **Object Pooling**: Reuses `cacheItem` objects to reduce GC pressure
- **Atomic Statistics**: Per-shard metrics tracked with atomic operations
- **Lazy Initialization**: LFU heaps only created when LFU policy is selected
- **Efficient Cleanup**: Background goroutine removes expired items periodically

### Eviction Policies

```go
const (
    LRU     EvictionPolicy = iota // Least Recently Used (default)
    LFU                           // Least Frequently Used
    FIFO                          // First In, First Out
    Random                        // Random eviction
    TinyLFU                       // Tiny Least Frequently Used
)
```

## Performance

### Benchmark Results

```
goos: darwin
goarch: arm64
pkg: github.com/unkn0wn-root/cago
cpu: Apple M4 Max

BenchmarkCacheSet-14            3,529,033    370.2 ns/op    77 B/op    6 allocs/op
BenchmarkCacheGet-14            4,338,128    279.3 ns/op    31 B/op    2 allocs/op
BenchmarkCacheGetMiss-14        5,375,492    211.2 ns/op    45 B/op    2 allocs/op
BenchmarkCacheHeavyRead-14      4,349,043    255.0 ns/op    33 B/op    3 allocs/op
BenchmarkCacheHeavyWrite-14     3,624,556    330.3 ns/op    65 B/op    5 allocs/op
BenchmarkCacheSize-14          57,196,422     21.6 ns/op     0 B/op    0 allocs/op
BenchmarkCacheStats-14         11,465,524    104.6 ns/op     0 B/op    0 allocs/op
```

### Performance Characteristics

- **280-370ns per operation** for most cache operations
- **10+ million operations/sec**
- **Linear scalability** with CPU cores due to sharding

### Running Benchmarks

```bash
# Run all benchmarks
make bench

or

make bench-full
```

## API Reference

### Core

```go
// Set stores a value with TTL
cache.Set(key, value, ttl time.Duration) error

// Get retrieves a value
cache.Get(key) (value, found bool)

// GetWithTTL retrieves a value with remaining TTL
cache.GetWithTTL(key) (value, ttl time.Duration, found bool)

// Delete removes a key
cache.Delete(key) bool

// Exists checks if a key exists
cache.Exists(key) bool

// Clear removes all items
cache.Clear()

// Size returns current item count
cache.Size() int64

// Stats returns performance statistics
cache.Stats() Stats

// Close gracefully shuts down the cache
cache.Close() error
```

### Other

```go
// Set with expiration callback
cache.SetWithCallback(key, value, ttl, callback func(key, value))

// Get all keys (expensive operation)
cache.Keys() []K

// Trigger manual cleanup
cache.TriggerCleanup()
```

### Statistics

```go
type Stats struct {
    Hits        int64   // Cache hits
    Misses      int64   // Cache misses
    Evictions   int64   // LRU evictions
    Expirations int64   // TTL expirations
    Size        int64   // Current items
    Capacity    int64   // Maximum items
    HitRatio    float64 // Hit ratio (0.0-1.0)
    Shards      int     // Number of shards
}
```

## HTTP Middleware

### HTTP Caching

```go
config := cache.DefaultMiddlewareConfig()
config.DefaultTTL = 5 * time.Minute
config.MaxSize = 100000
config.ShardCount = 16

middleware := cache.NewHTTPCacheMiddleware(config)
defer middleware.Close()

// Use with any HTTP framework
http.Handle("/api/users", middleware.Middleware(usersHandler))
```

### Framework Compatibility

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

### Middleware Configuration

```go
// High-performance API caching
apiConfig := cache.DefaultMiddlewareConfig()
apiConfig.MaxSize = 50000
apiConfig.ShardCount = 32
apiConfig.DefaultTTL = 10 * time.Minute
apiConfig.MaxBodySize = 5 * 1024 * 1024 // 5MB

// User-specific caching
userMiddleware := cache.NewHTTPCacheMiddleware(config)
userMiddleware.SetKeyGenerator(cache.KeyWithUserID("X-User-ID"))

// Content-type based caching with different TTLs
contentMiddleware := cache.NewHTTPCacheMiddleware(config)
contentMiddleware.SetCachePolicy(cache.CacheByContentType(map[string]time.Duration{
    "application/json": 5 * time.Minute,
    "text/html":       10 * time.Minute,
    "image/":          1 * time.Hour,
}, 2*time.Minute))

// Size-based conditional caching
conditionalMiddleware := cache.NewHTTPCacheMiddleware(config)
conditionalMiddleware.SetCachePolicy(cache.CacheBySize(100, 1024*1024, 3*time.Minute))
```

### Built-in Key Generators

```go
// Default key generator (method + URL + vary headers)
middleware.SetKeyGenerator(cache.DefaultKeyGenerator)

// User-specific keys
middleware.SetKeyGenerator(cache.KeyWithUserID("X-User-ID"))

// Custom vary headers
middleware.SetKeyGenerator(cache.KeyWithVaryHeaders([]string{"Accept", "Authorization"}))

// Ignore query parameters
middleware.SetKeyGenerator(cache.KeyWithoutQuery())
```

### Built-in Cache Policies

```go
// Always cache successful responses
middleware.SetCachePolicy(cache.AlwaysCache(5 * time.Minute))

// Never cache
middleware.SetCachePolicy(cache.NeverCache())

// Content-type based caching
middleware.SetCachePolicy(cache.CacheByContentType(map[string]time.Duration{
    "application/json": 5 * time.Minute,
    "text/html":       10 * time.Minute,
}, 2*time.Minute))

// Size-based caching
middleware.SetCachePolicy(cache.CacheBySize(100, 1024*1024, 3*time.Minute))
```

### Monitoring

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

### Cache Management

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

### HTTP Compliance

The middleware automatically handles:
- **Cache-Control** headers (max-age, no-cache, no-store, private)
- **Expires** headers (RFC1123 format)
- **ETag** generation and validation
- **Vary** headers for content negotiation
- **X-Cache** headers (HIT/MISS status)
- **X-Cache-Date** and **X-Cache-Age** headers
