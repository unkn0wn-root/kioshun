<div align="center">
  <img src="assets/logo.JPG" alt="CaGo Logo" width="200"/>

  # CaGo - In-Memory Cache for Go

  [![Go Version](https://img.shields.io/badge/Go-1.21+-blue.svg)](https://golang.org)
  [![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

  *High-performance, thread-safe, sharded in-memory cache for Go*
</div>

## Features

- **Performance**: 280-370ns per operation with 10M+ ops/sec throughput
- **Sharded Architecture**: Automatic sharding to minimize lock contention
- **Multiple Eviction Policies**: LRU, LFU, FIFO, Random, and TinyLFU algorithms
- **Thread Safe**: Optimized for high-concurrency workloads
- **HTTP Middleware**: HTTP response caching middleware
- **Global Cache Manager**: Centralized management of multiple cache instances

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

### Benchmark Results vs. Popular Go Caches

Comprehensive benchmarks comparing CaGo against leading Go cache libraries:

```
goos: darwin
goarch: arm64
cpu: Apple M4 Max

=== SET OPERATIONS ===
BenchmarkCacheComparison_Set/cago-14             	52,143,810	  74.79 ns/op	56 B/op	4 allocs/op
BenchmarkCacheComparison_Set/ristretto-14        	55,905,141	  73.19 ns/op	150 B/op	5 allocs/op
BenchmarkCacheComparison_Set/go-cache-14         	11,950,642	  339.8 ns/op	56 B/op	3 allocs/op
BenchmarkCacheComparison_Set/freecache-14        	 8,166,033	  517.7 ns/op	956 B/op	2 allocs/op

=== GET OPERATIONS ===
BenchmarkCacheComparison_Get/ristretto-14        	182,913,994	  19.54 ns/op	31 B/op	2 allocs/op
BenchmarkCacheComparison_Get/cago-14             	 79,889,264	  51.42 ns/op	31 B/op	2 allocs/op
BenchmarkCacheComparison_Get/freecache-14        	 43,043,793	  83.97 ns/op	1039 B/op	2 allocs/op
BenchmarkCacheComparison_Get/go-cache-14         	 27,013,480	  134.9 ns/op	15 B/op	1 allocs/op

=== MIXED OPERATIONS (70% reads, 30% writes) ===
BenchmarkCacheComparison_Mixed/ristretto-14      	50,880,874	  60.69 ns/op	69 B/op	3 allocs/op
BenchmarkCacheComparison_Mixed/cago-14           	57,670,704	  63.19 ns/op	36 B/op	3 allocs/op
BenchmarkCacheComparison_Mixed/go-cache-14       	17,468,449	  203.8 ns/op	22 B/op	2 allocs/op
BenchmarkCacheComparison_Mixed/freecache-14      	13,021,879	  274.9 ns/op	1039 B/op	2 allocs/op

=== HIGH CONTENTION SCENARIOS ===
BenchmarkCacheComparison_HighContention/cago-14          	49,625,168	  73.67 ns/op	40 B/op	2 allocs/op
BenchmarkCacheComparison_HighContention/ristretto-14     	15,899,757	  229.7 ns/op	83 B/op	3 allocs/op
BenchmarkCacheComparison_HighContention/go-cache-14      	16,982,012	  234.8 ns/op	32 B/op	1 allocs/op
BenchmarkCacheComparison_HighContention/freecache-14     	11,682,682	  318.5 ns/op	922 B/op	2 allocs/op

=== READ-HEAVY WORKLOADS (90% reads, 10% writes) ===
BenchmarkCacheComparison_ReadHeavy/ristretto-14  	68,613,873	  34.45 ns/op	45 B/op	3 allocs/op
BenchmarkCacheComparison_ReadHeavy/cago-14       	41,003,152	  57.62 ns/op	33 B/op	3 allocs/op
BenchmarkCacheComparison_ReadHeavy/freecache-14  	14,973,655	  159.6 ns/op	1039 B/op	2 allocs/op
BenchmarkCacheComparison_ReadHeavy/go-cache-14   	12,914,514	  186.6 ns/op	18 B/op	2 allocs/op

=== WRITE-HEAVY WORKLOADS (90% writes, 10% reads) ===
BenchmarkCacheComparison_WriteHeavy/cago-14      	34,513,875	  69.29 ns/op	46 B/op	3 allocs/op
BenchmarkCacheComparison_WriteHeavy/ristretto-14 	17,119,744	  151.3 ns/op	132 B/op	5 allocs/op
BenchmarkCacheComparison_WriteHeavy/go-cache-14  	 9,193,803	  287.6 ns/op	37 B/op	2 allocs/op
BenchmarkCacheComparison_WriteHeavy/freecache-14 	 6,132,829	  377.0 ns/op	1039 B/op	2 allocs/op

=== REAL-WORLD WORKLOAD ===
BenchmarkCacheComparison_RealWorldWorkload/cago-14       	39,203,995	  63.04 ns/op	53 B/op	3 allocs/op
BenchmarkCacheComparison_RealWorldWorkload/ristretto-14  	17,528,142	  125.8 ns/op	97 B/op	3 allocs/op
BenchmarkCacheComparison_RealWorldWorkload/go-cache-14   	10,621,634	  227.8 ns/op	40 B/op	2 allocs/op
BenchmarkCacheComparison_RealWorldWorkload/freecache-14  	 6,302,244	  382.0 ns/op	1168 B/op	2 allocs/op

=== MEMORY EFFICIENCY ===
BenchmarkCacheComparison_MemoryEfficiency/freecache-14   	 8,404,188	  286.3 ns/op	0.1641 bytes/op
BenchmarkCacheComparison_MemoryEfficiency/ristretto-14   	16,212,883	  150.3 ns/op	0.6158 bytes/op
BenchmarkCacheComparison_MemoryEfficiency/cago-14        	 9,914,840	  225.0 ns/op	0.7055 bytes/op
BenchmarkCacheComparison_MemoryEfficiency/go-cache-14    	10,193,031	  318.3 ns/op	105.3 bytes/op
```

### Performance Characteristics

- **19-75ns per operation** for most cache operations
- **10+ million operations/sec** sustained throughput
- **Superior performance** in high contention scenarios
- **Excellent memory efficiency** with low allocation overhead
- **Linear scalability** with CPU cores due to sharding
- **Consistent performance** across all workload patterns

### Stress Test Results

```
=== EVICTION POLICY PERFORMANCE ===
BenchmarkCacheEvictionStress/Eviction_LRU-14     	17,086,760	  146.2 ns/op	57 B/op	4 allocs/op
BenchmarkCacheEvictionStress/Eviction_FIFO-14    	17,229,789	  148.8 ns/op	57 B/op	4 allocs/op
BenchmarkCacheEvictionStress/Eviction_LFU-14     	14,558,103	  173.3 ns/op	56 B/op	4 allocs/op
BenchmarkCacheEvictionStress/Eviction_Random-14  	13,789,909	  183.0 ns/op	132 B/op	4 allocs/op

=== EXTREME LOAD SCENARIOS ===
BenchmarkCacheHeavyLoad/Small_HighConcurrency-14 	32,651,302	  71.60 ns/op	31 B/op	2 allocs/op
BenchmarkCacheHeavyLoad/Medium_MixedLoad-14       	29,994,172	  80.39 ns/op	36 B/op	3 allocs/op
BenchmarkCacheHeavyLoad/Large_ReadHeavy-14        	34,459,508	  70.79 ns/op	40 B/op	3 allocs/op
BenchmarkCacheHeavyLoad/XLarge_WriteHeavy-14      	26,019,538	  87.39 ns/op	51 B/op	3 allocs/op
BenchmarkCacheHeavyLoad/Extreme_Balanced-14       	26,771,521	  96.33 ns/op	49 B/op	3 allocs/op
```

### Running Benchmarks

```bash
# Run comparison benchmarks
go test -bench=BenchmarkCacheComparison -benchmem -benchtime=3s ./benchmark/

# Run stress tests
go test -bench=BenchmarkCacheHeavyLoad -benchmem ./benchmark/
go test -bench=BenchmarkCacheEvictionStress -benchmem ./benchmark/

# Run all benchmarks with the benchmark runner
go run benchmark/benchmark_runner.go
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
