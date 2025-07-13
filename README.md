<div align="center">
  <img src="assets/logo.JPG" alt="Kioshun Logo" width="200"/>

  # Kioshun - In-Memory Cache for Go

  *"kee-oh-shoon" /kiːoʊʃuːn/*

  [![Go Version](https://img.shields.io/badge/Go-1.21+-blue.svg)](https://golang.org)
  [![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

  *Fast, thread-safe, sharded in-memory cache for Go*
</div>

## Table of Contents

- [Benchmark Results](#benchmark-results-kioshun-vs-ristretto-go-cache-and-freecache)
  - [Running Benchmarks](#running-benchmarks)
  - [Core Operations](#core-operations)
  - [Workload-Specific](#workload-specific)
  - [Simulate 'Real-World' Workflow](#simulate-real-world-workflow)
  - [Performance Characteristics](#performance-characteristics)
  - [Stress Test Results](#stress-test-results)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [Configuration](#configuration)
  - [Basic Configuration](#basic-configuration)
  - [Predefined Configurations](#predefined-configurations)
- [Architecture](#architecture)
  - [Sharded Design](#sharded-design)
  - [Hash Function Optimization](#hash-function-optimization)
  - [Eviction Policy Implementation](#eviction-policy-implementation)
  - [Concurrent Access Patterns](#concurrent-access-patterns)
  - [Memory Management](#memory-management)
  - [Eviction Policies](#eviction-policies)
- [API Reference](#api-reference)
- [HTTP Middleware](#http-middleware)
- [Cache Invalidation Setup](#cache-invalidation-setup)

### Benchmark Results Kioshun vs. Ristretto, go-cache and freecache

### Running Benchmarks

```bash
# Run comparison benchmarks
make bench-compare

# Run stress tests
make stress-test

# Run all benchmarks with the benchmark runner
make bench-runner

# Run all benchmark tests
make bench
```

**Test Environment:**
- **OS:** macOS (Darwin)
- **Architecture:** ARM64
- **CPU:** Apple M4 Max
- **Go Version:** 1.21+

#### Core Operations

##### SET Operations
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 12,996,515 | **80.66** | 56 | 4 |
| **Ristretto** | 11,247,014 | **85.89** | 153 | 5 |
| **BigCache** | 6,427,754 | 155.6 | 40 | 2 |
| **go-cache** | 2,880,859 | 346.6 | 57 | 3 |
| **freecache** | 1,866,667 | 535.6 | 919 | 2 |

##### GET Operations
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Ristretto** | 34,308,471 | **22.58** | 31 | 2 |
| **Kioshun** | 19,207,689 | **52.07** | 31 | 2 |
| **BigCache** | 12,662,976 | 78.91 | 1047 | 3 |
| **freecache** | 13,076,071 | 76.45 | 1039 | 2 |
| **go-cache** | 7,343,721 | 136.2 | 15 | 1 |

#### Workload-Specific

##### Mixed Operations (70% reads, 30% writes)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Ristretto** | 14,962,564 | **60.01** | 69 | 3 |
| **Kioshun** | 21,152,779 | **64.73** | 36 | 3 |
| **BigCache** | 6,758,445 | 148.0 | 742 | 3 |
| **go-cache** | 4,939,018 | 202.6 | 22 | 2 |
| **freecache** | 3,625,789 | 275.7 | 1039 | 2 |

##### High Contention Scenarios
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 14,890,139 | **71.99** | 40 | 2 |
| **BigCache** | 6,458,463 | 154.8 | 567 | 2 |
| **go-cache** | 4,363,460 | 229.2 | 33 | 1 |
| **Ristretto** | 4,344,312 | 230.3 | 83 | 3 |
| **freecache** | 3,261,829 | 306.5 | 920 | 2 |

##### Read-Heavy Workloads (90% reads, 10% writes)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Ristretto** | 28,720,270 | **32.57** | 45 | 3 |
| **Kioshun** | 17,530,855 | **60.48** | 33 | 3 |
| **BigCache** | 7,595,923 | 131.6 | 946 | 3 |
| **freecache** | 6,422,010 | 155.7 | 1039 | 2 |
| **go-cache** | 5,454,579 | 183.3 | 18 | 2 |

##### Write-Heavy Workloads (90% writes, 10% reads)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 14,920,641 | **71.87** | 46 | 3 |
| **BigCache** | 5,925,754 | 168.7 | 133 | 3 |
| **Ristretto** | 6,327,607 | 158.0 | 132 | 5 |
| **go-cache** | 3,474,501 | 287.7 | 37 | 2 |
| **freecache** | 2,670,829 | 374.5 | 1039 | 2 |

#### Simulate 'Real-World' Workflow

##### Real-World Workload Simulation
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 14,851,725 | **67.34** | 53 | 3 |
| **Ristretto** | 8,403,505 | 119.0 | 97 | 3 |
| **BigCache** | 5,467,201 | 182.9 | 818 | 3 |
| **go-cache** | 4,358,015 | 229.5 | 40 | 2 |
| **freecache** | 2,664,825 | 375.3 | 1164 | 2 |

##### Memory Efficiency
| Cache Library | Ops/sec | ns/op | bytes/op |
|---------------|---------|-------|----------|
| **Kioshun** | 11,154,840 | 89.73 | **50.0** |
| **BigCache** | 6,758,445 | 148.0 | 742.0 |
| **Ristretto** | 8,403,505 | 119.0 | 97.0 |
| **go-cache** | 4,358,015 | 229.5 | 40.0 |
| **freecache** | 2,664,825 | 375.3 | 1164.0 |

### Performance Characteristics

- **18-79ns per operation** for most cache operations
- **10+ million operations/sec** throughput
- **Timestamp Caching**: expiration checks use cached timestamps updated every 100ms (default) instead of expensive `time.Now()` calls

### Stress Test Results

#### Eviction Policy Performance
| Policy | Ops/sec | ns/op | B/op | allocs/op |
|--------|---------|-------|------|-----------|
| **LRU** | 17,086,760 | **146.2** | 57 | 4 |
| **FIFO** | 17,229,789 | **148.8** | 57 | 4 |
| **LFU** | 14,558,103 | 173.3 | 56 | 4 |
| **Random** | 13,789,909 | 183.0 | 132 | 4 |

#### High Load Scenarios
| Load Profile | Ops/sec | ns/op | B/op | allocs/op | Description |
|-------------|---------|-------|------|-----------|-------------|
| **Small + High Concurrency** | 32,651,302 | **71.60** | 31 | 2 | Many goroutines, small cache |
| **Medium + Mixed Load** | 29,994,172 | **80.39** | 36 | 3 | Balanced read/write operations |
| **Large + Read Heavy** | 34,459,508 | **70.79** | 40 | 3 | Large cache, mostly reads |
| **XLarge + Write Heavy** | 26,019,538 | **87.39** | 51 | 3 | Very large cache, mostly writes |
| **Extreme + Balanced** | 26,771,521 | **96.33** | 49 | 3 | Maximum scale, balanced ops |


## Installation

```bash
go get github.com/unkn0wn-root/kioshun
```

## Quick Start

```go
package main

import (
    "fmt"
    "time"

    "github.com/unkn0wn-root/kioshun"
)

func main() {
    // Create cache with default configuration
    cache := cache.NewWithDefaults[string, string]()
    defer cache.Close()

    // Set with default TTL (30 min)
    cache.Set("user:123", "David Nice", kioshun.DefaultExpiration)

    // Set with no expiration
    cache.Set("user:123", "David Nice", kioshun.NoExpiration)

    // Set value with custom TTL
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

cache := cache.New[string, any](config)
```

### Predefined Configurations

```go
// For session storage
sessionCache := cache.New[string, Session](cache.SessionCacheConfig())

// For API response caching
apiCache := cache.New[string, APIResponse](cache.APICacheConfig())

// For temporary data
tempCache := cache.New[string, any](cache.TemporaryCacheConfig())

// For persistent data
persistentCache := cache.New[string, any](cache.PersistentCacheConfig())
```

## Architecture

### Sharded Design

Kioshun uses a sharded architecture to minimize lock contention:

```
┌─────────────────────────────────────────────────────────────┐
│                      Kioshun Cache                          │
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

// ⚠IMPORTANT: Enable invalidation if needed
// middleware.SetKeyGenerator(cache.KeyWithoutQuery())

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

apiMiddleware := cache.NewHTTPCacheMiddleware(apiConfig)
// Enable invalidation for API endpoints
apiMiddleware.SetKeyGenerator(cache.KeyWithoutQuery())

// User-specific caching
userMiddleware := cache.NewHTTPCacheMiddleware(config)
userMiddleware.SetKeyGenerator(cache.KeyWithUserID("X-User-ID"))
// Note: User-specific caching uses different key format - invalidation works differently

// Content-type based caching with different TTLs
contentMiddleware := cache.NewHTTPCacheMiddleware(config)
contentMiddleware.SetKeyGenerator(cache.KeyWithoutQuery()) // Enable invalidation
contentMiddleware.SetCachePolicy(cache.CacheByContentType(map[string]time.Duration{
    "application/json": 5 * time.Minute,
    "text/html":       10 * time.Minute,
    "image/":          1 * time.Hour,
}, 2*time.Minute))

// Size-based conditional caching
conditionalMiddleware := cache.NewHTTPCacheMiddleware(config)
conditionalMiddleware.SetKeyGenerator(cache.KeyWithoutQuery()) // Enable invalidation
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

## Cache Invalidation Setup

**Cache invalidation by URL pattern requires specific key generator configuration.**

### The Problem

The default key generator uses MD5 hashing which makes pattern-based invalidation impossible:

```go
// DEFAULT SETUP - Invalidation won't work
config := cache.DefaultMiddlewareConfig()
middleware := cache.NewHTTPCacheMiddleware(config)

// This returns 0 removed entries because keys are hashed
removed := middleware.Invalidate("/api/users/*") // Returns 0
```

**Why it fails:**
- Default keys: `"a1b2c3d4e5f6..."` (MD5 hash)
- Pattern matching needs: `"GET:/api/users/123"` (readable path)
- Hash loses original URL information

### The Solution

Use path-based key generators for invalidation to work:

```go
// CORRECT SETUP - Invalidation works
config := cache.DefaultMiddlewareConfig()
middleware := cache.NewHTTPCacheMiddleware(config)

// CRITICAL: Set path-based key generator
middleware.SetKeyGenerator(cache.KeyWithoutQuery())

// Now invalidation works
removed := middleware.Invalidate("/api/users/*") // Returns actual count
```

### Key Generator Comparison

| Key Generator | Example Key | Invalidation | Use Case |
|---------------|-------------|--------------|----------|
| `DefaultKeyGenerator` | `"a1b2c3d4..."` | ❌ **Broken** | No invalidation needed |
| `KeyWithoutQuery()` | `"GET:/api/users"` | ✅ **Works** | **Recommended for invalidation** |
| `PathBasedKeyGenerator` | `"GET:/api/users"` | ✅ **Works** | Simple path-based caching |
| `KeyWithVaryHeaders()` | `"a1b2c3d4..."` | ❌ **Broken** | Custom headers + security |

### Complete Working Example

```go
package main

import (
    "encoding/json"
    "fmt"
    "net/http"
    "time"

    "github.com/unkn0wn-root/kioshun"
)

func main() {
    // Setup middleware
    config := cache.DefaultMiddlewareConfig()
    config.DefaultTTL = 10 * time.Minute

    middleware := cache.NewHTTPCacheMiddleware(config)
    defer middleware.Close()


    // Enable invalidation
    middleware.SetKeyGenerator(cache.KeyWithoutQuery())

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


### HTTP Compliance

The middleware automatically handles:
- **Cache-Control** headers (max-age, no-cache, no-store, private)
- **Expires** headers (RFC1123 format)
- **ETag** generation and validation
- **Vary** headers for content negotiation
- **X-Cache** headers (HIT/MISS status)
- **X-Cache-Date** and **X-Cache-Age** headers
