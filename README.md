<div align="center">
  <img src="assets/logo.JPG" alt="Kioshun Logo" width="200"/>

  # Kioshun - In-Memory Cache for Go

  *"kee-oh-shoon" /kiːoʊʃuːn/*

  [![Go Version](https://img.shields.io/badge/Go-1.21+-blue.svg)](https://golang.org)
  [![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
  [![CI](https://github.com/unkn0wn-root/kioshun/actions/workflows/test.yml/badge.svg)](https://github.com/unkn0wn-root/kioshun/actions)


  *Thread-safe, sharded in-memory cache for Go*
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
| **Kioshun** | 100,000,000 | **53.95** | 41 | 3 |
| **Ristretto** | 83,301,469 | 82.08 | 153 | 5 |
| **BigCache** | 38,670,102 | 154.1 | 40 | 2 |
| **go-cache** | 20,022,591 | 353.3 | 57 | 3 |
| **freecache** | 13,592,491 | 523.3 | 903 | 2 |

##### GET Operations
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Ristretto** | 287,087,389 | **21.22** | 31 | 2 |
| **Kioshun** | 281,013,220 | **21.78** | 31 | 2 |
| **BigCache** | 100,000,000 | 79.34 | 1047 | 3 |
| **freecache** | 80,708,305 | 77.29 | 1039 | 2 |
| **go-cache** | 45,681,829 | 136.2 | 15 | 1 |

#### Workload-Specific

##### Mixed Operations (70% reads, 30% writes)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 125,047,722 | **48.47** | 31 | 2 |
| **Ristretto** | 85,749,963 | 60.89 | 69 | 3 |
| **BigCache** | 36,268,677 | 148.9 | 743 | 3 |
| **go-cache** | 30,280,268 | 201.9 | 22 | 2 |
| **freecache** | 21,571,833 | 273.3 | 1039 | 2 |

##### High Contention Scenarios
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 97,568,907 | **69.87** | 34 | 2 |
| **BigCache** | 36,030,003 | 167.2 | 558 | 2 |
| **go-cache** | 28,345,117 | 235.5 | 34 | 1 |
| **Ristretto** | 27,784,916 | 222.6 | 83 | 3 |
| **freecache** | 18,082,710 | 329.7 | 919 | 2 |

##### Read-Heavy Workloads (90% reads, 10% writes)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Ristretto** | 106,750,716 | **33.20** | 45 | 3 |
| **Kioshun** | 96,229,144 | **37.01** | 31 | 2 |
| **BigCache** | 26,457,594 | 130.0 | 946 | 3 |
| **freecache** | 23,158,801 | 157.0 | 1039 | 2 |
| **go-cache** | 19,328,863 | 183.2 | 18 | 2 |

##### Write-Heavy Workloads (90% writes, 10% reads)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 100,000,000 | **33.74** | 31 | 2 |
| **Ristretto** | 25,788,711 | 163.6 | 132 | 5 |
| **BigCache** | 21,301,932 | 168.7 | 133 | 3 |
| **go-cache** | 12,469,190 | 279.9 | 37 | 2 |
| **freecache** | 9,007,708 | 380.0 | 1039 | 2 |

#### Simulate 'Real-World' Workflow

##### Real-World Workload Simulation
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 58,979,691 | **60.30** | 48 | 3 |
| **Ristretto** | 26,206,014 | 120.9 | 97 | 3 |
| **BigCache** | 20,647,864 | 183.7 | 816 | 3 |
| **go-cache** | 16,281,172 | 229.4 | 40 | 2 |
| **freecache** | 9,182,894 | 383.5 | 1167 | 2 |

##### Memory Efficiency
| Cache Library | Ops/sec | ns/op | bytes/op |
|---------------|---------|-------|----------|
| **Kioshun** | 94,229,144 | **38.01** | **31.0** |
| **Ristretto** | 106,750,716 | 33.20 | 45.0 |
| **BigCache** | 26,457,594 | 130.0 | 946.0 |
| **freecache** | 23,158,801 | 157.0 | 1039.0 |
| **go-cache** | 19,328,863 | 183.2 | 18.0 |

### Performance Characteristics

- **21-82ns per operation** for most cache operations
- **58+ million operations/sec** throughput
- **Peak throughput**: 275M+ operations/sec for GET operations

### Stress Test Results

#### High Load Scenarios
| Load Profile | Ops/sec | ns/op | B/op | allocs/op | Description |
|-------------|---------|-------|------|-----------|-------------|
| **Small + High Concurrency** | 59,745,292 | **59.50** | 27 | 2 | Many goroutines, small cache |
| **Medium + Mixed Load** | 56,104,041 | **64.83** | 31 | 2 | Balanced read/write operations |
| **Large + Read Heavy** | 56,693,991 | **65.72** | 38 | 2 | Large cache, mostly reads |
| **XLarge + Write Heavy** | 44,826,436 | **68.11** | 40 | 3 | Very large cache, mostly writes |
| **Extreme + Balanced** | 45,561,304 | **80.95** | 41 | 3 | Maximum scale, balanced ops |

#### Advanced Stress Test Results

##### Contention Stress Test
| Test | Ops/sec | ns/op | B/op | allocs/op |
|------|---------|-------|------|-----------|
| **High Contention** | 49,839,693 | **68.86** | 34 | 2 |

##### Memory Pressure Tests
| Value Size | Ops/sec | ns/op | B/op | allocs/op |
|------------|---------|-------|------|-----------|
| **64KB** | 56,616,940 | **66.20** | 40 | 3 |
| **1KB** | 55,313,698 | **66.37** | 40 | 3 |
| **4KB** | 56,154,799 | **65.81** | 40 | 2 |
| **16KB** | 55,306,689 | **68.36** | 40 | 3 |

##### Sharding Efficiency Analysis
| Shards | Ops/sec | ns/op | B/op | allocs/op |
|--------|---------|-------|------|-----------|
| **1** | 9,378,514 | 426.8 | 43 | 3 |
| **2** | 11,517,030 | 340.5 | 43 | 3 |
| **4** | 16,063,339 | 232.1 | 43 | 3 |
| **8** | 23,789,816 | 154.2 | 45 | 3 |
| **16** | 32,913,006 | 105.1 | 46 | 3 |
| **32** | 42,531,112 | 80.99 | 47 | 3 |
| **64** | 52,462,201 | 67.55 | 49 | 3 |
| **128** | 59,367,160 | **56.80** | 49 | 3 |

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
- **Integer Keys**: xxHash64 avalanche mixing for optimal distribution
- **String Keys**: FNV-1a for short strings (≤8 bytes), xxHash64 for longer strings (>8 bytes)
- **Other Types**: Fallback to string conversion with hybrid string hashing

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

#### FIFO (First In, First Out)
- **Access**: No updates required on access
- **Eviction**: Remove oldest item (at tail) in O(1)
- **Memory**: Minimal overhead per item

#### Random Eviction
- **Access**: No updates required on access
- **Eviction**: Randomly select item using time-based pseudo-randomness in O(n)
- **Memory**: No additional overhead per item
- **Algorithm**: Uses `time.Now().UnixNano() % len(keys)` for selection

#### SampledLFU (Sampled Least Frequently Used)
Uses random sampling with bloom filter admission control to prevent cache pollution:
- **Access**: Update frequency counter in O(1)
- **Eviction**: Sample items (default 5-20) and evict least frequent in O(k) where k = sample size
- **Admission Control**: Tracks recently seen keys with adaptive admission rates (50-90%)
- **Scan Detection**: Automatically detects sequential scan patterns and reduces admission rate
- **Memory**: Bloom filter per shard + frequency counters per item

**SampledLFU Architecture:**
```
┌─────────────────────────────────────────────────────────────┐
│                    SampledLFU Shard                         │
├─────────────────────┬───────────────────────────────────────┤
│   Admission Filter  │           Cache Items                 │
│  ┌───────────────┐  │  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐      │
│  │ Bloom Filter  │  │  │Item │ │Item │ │Item │ │Item │ ...  │
│  │ (Doorkeeper)  │  │  │Freq:│ │Freq:│ │Freq:│ │Freq:│      │
│  │               │  │  │  5  │ │  12 │ │  3  │ │  8  │      │
│  └───────────────┘  │  └─────┘ └─────┘ └─────┘ └─────┘      │
│  Rate: 50-90%       │                                       │
│  Scan: Detected     │  Sample 5 → Evict Item with Freq: 3   │
└─────────────────────┴───────────────────────────────────────┘
```

**Admission Filter:**
- **Bloom Filter**: 3 hash functions with ~1% false positive rate
- **Adaptive Rates**: 90% default, drops to 50% minimum during scans
- **Scan Detection**: 80% new items in 100 requests triggers scan mode
- **Reset Interval**: Periodic reset (default 1 minute)

**Eviction Algorithm Comparison:**
| Policy | Access Time | Eviction Time | Memory Overhead | Best Use Case |
|--------|-------------|---------------|-----------------|---------------|
| LRU    | O(1)        | O(1)          | Low            | General purpose |
| LFU    | O(log n)    | O(log n)      | Medium         | Frequency-based access |
| FIFO   | O(1)        | O(1)          | Low            | Simple time-based |
| Random | O(1)        | O(n)          | Low            | Cache-oblivious workloads |
| SampledLFU | O(1)    | O(k)          | Medium         | Scan-resistant, better LFU |

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
    LRU        EvictionPolicy = iota // Least Recently Used (default)
    LFU                              // Least Frequently Used
    FIFO                             // First In, First Out
    Random                           // Random eviction
    SampledLFU                       // Sampled LFU with admission control
)
```

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

    cache "github.com/unkn0wn-root/kioshun"
)

func main() {
    // Create cache with default configuration
    c := cache.NewWithDefaults[string, string]()
    defer c.Close()

    // Set with default TTL (30 min)
    c.Set("user:123", "David Nice", cache.DefaultExpiration)

    // Set with no expiration
    c.Set("user:123", "David Nice", cache.NoExpiration)

    // Set value with custom TTL
    c.Set("user:123", "David Nice", 5*time.Minute)

    // Get value
    if value, found := c.Get("user:123"); found {
        fmt.Printf("User: %s\n", value)
    }

    // Get cache statistics
    stats := c.Stats()
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

// Exists checks if a key exists without updating access time
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

### Advanced

```go
// Set with expiration callback
cache.SetWithCallback(key, value, ttl, callback func(key, value)) error

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
config := cache.DefaultMiddlewareConfig() // default uses FIFO
config.DefaultTTL = 5 * time.Minute
config.MaxSize = 100000
config.ShardCount = 16

middleware := cache.NewHTTPCacheMiddleware(config)
defer middleware.Close()

// IMPORTANT: Enable invalidation if needed
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
apiConfig := cache.DefaultMiddlewareConfig() // default config uses FIFO
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

// Configure eviction policy
cacheConfig := cache.DefaultMiddlewareConfig()
cacheConfig.EvictionPolicy = cache.FIFO
cacheConfig.MaxSize = 100000
cacheConfig.DefaultTTL = 5 * time.Minute

middleware := cache.NewHTTPCacheMiddleware(cacheConfig)

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

### Eviction Policies

The middleware supports different eviction algorithms that can be configured based on diffrent access patterns:

```go
config := cache.DefaultMiddlewareConfig()

// FIFO (First In, First Out) - Best Performance
config.EvictionPolicy = cache.FIFO

// LRU (Least Recently Used) - General Purpose
config.EvictionPolicy = cache.LRU

// LFU (Least Frequently Used) - Frequency-Based
config.EvictionPolicy = cache.LFU

// Random - Cache-Oblivious
config.EvictionPolicy = cache.Random

// SampledLFU - Approximate LFU with Admission Control
config.EvictionPolicy = cache.SampledLFU
```

**Recommend**: For most HTTP middlewares, **FIFO** offers the best performance and is suitable for most web apps where request patterns follow temporal locality.

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
