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

## Benchmark Configuration

The benchmarks compare **Kioshun** with **AdmissionLFU** eviction policy against other popular Go cache libraries:

### Cache Configurations Used

| Cache Library | Configuration | Notes |
|---------------|---------------|-------|
| **Kioshun** | MaxSize: 100,000<br>ShardCount: CPU cores × 4<br>EvictionPolicy: **AdmissionLFU**<br>DefaultTTL: 1 hour<br>CleanupInterval: 5 min | AdmissionLFU eviction policy with admission control |
| **Ristretto** | NumCounters: 1,000,000<br>MaxCost: 100,000<br>BufferItems: 64 | TinyLFU-based admission policy |
| **BigCache** | MaxEntriesInWindow: 100,000<br>Shards: CPU cores (power of 2)<br>MaxEntrySize: 64KB<br>HardMaxCacheSize: 256MB | No eviction policy, size-based |
| **FreeCache** | Size: 128MB | Segmented LRU |
| **go-cache** | DefaultExpiration: 1 hour<br>CleanupInterval: 5 min | Simple map-based with cleanup |

**Test Environment:**
- **CPU:** Apple M4 Max (14 cores)
- **OS:** macOS (Darwin ARM64)
- **Kioshun Shards:** 56 (14 × 4)
- **Go Version:** 1.21+

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

#### Core Operations

##### SET Operations
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 100,000,000 | **61.19** | 42 | 3 |
| **Ristretto** | 82,496,528 | 81.48 | 151 | 5 |
| **BigCache** | 37,729,303 | 153.9 | 40 | 2 |
| **go-cache** | 19,802,497 | 352.3 | 57 | 3 |
| **freecache** | 12,485,425 | 540.2 | 920 | 2 |

##### GET Operations
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Ristretto** | 310,158,865 | **19.35** | 31 | 2 |
| **Kioshun** | 258,173,943 | **23.15** | 31 | 2 |
| **BigCache** | 92,523,937 | 82.81 | 1047 | 3 |
| **freecache** | 81,388,165 | 76.40 | 1039 | 2 |
| **go-cache** | 45,881,512 | 132.9 | 15 | 1 |

#### Workload-Specific

##### Mixed Operations (70% reads, 30% writes)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 100,000,000 | **49.68** | 31 | 2 |
| **Ristretto** | 85,088,882 | 60.14 | 69 | 3 |
| **BigCache** | 40,550,335 | 149.4 | 742 | 3 |
| **go-cache** | 30,110,391 | 200.5 | 22 | 2 |
| **freecache** | 22,155,008 | 266.9 | 1039 | 2 |

##### High Contention Scenarios
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 83,795,762 | **73.72** | 35 | 2 |
| **BigCache** | 35,587,495 | 155.3 | 570 | 2 |
| **go-cache** | 29,478,771 | 233.1 | 33 | 1 |
| **Ristretto** | 27,075,663 | 228.9 | 83 | 3 |
| **freecache** | 19,292,937 | 306.2 | 918 | 2 |

##### Read-Heavy Workloads (90% reads, 10% writes)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Ristretto** | 109,256,152 | **32.95** | 45 | 3 |
| **Kioshun** | 87,788,641 | **36.73** | 31 | 2 |
| **BigCache** | 26,907,688 | 130.0 | 946 | 3 |
| **freecache** | 22,986,230 | 156.4 | 1039 | 2 |
| **go-cache** | 19,272,679 | 185.3 | 18 | 2 |

##### Write-Heavy Workloads (90% writes, 10% reads)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 91,341,868 | **38.24** | 31 | 2 |
| **Ristretto** | 25,319,823 | 167.6 | 132 | 5 |
| **BigCache** | 20,838,819 | 169.9 | 133 | 3 |
| **go-cache** | 12,607,954 | 275.5 | 37 | 2 |
| **freecache** | 8,449,123 | 376.2 | 1039 | 2 |

#### Simulate 'Real-World' Workflow

##### Real-World Workload Simulation
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 56,552,866 | **64.99** | 48 | 3 |
| **Ristretto** | 27,220,159 | 121.7 | 97 | 3 |
| **BigCache** | 20,555,629 | 180.3 | 812 | 3 |
| **go-cache** | 16,119,222 | 229.1 | 40 | 2 |
| **freecache** | 8,877,139 | 375.4 | 1163 | 2 |

##### Memory Efficiency
| Cache Library | Ops/sec | ns/op | bytes/op |
|---------------|---------|-------|----------|
| **Kioshun** | 89,788,641 | **36.73** | **31.0** |
| **Ristretto** | 109,256,152 | 32.95 | 45.0 |
| **BigCache** | 26,907,688 | 130.0 | 946.0 |
| **freecache** | 22,986,230 | 156.4 | 1039.0 |
| **go-cache** | 19,272,679 | 185.3 | 18.0 |

### Performance Characteristics

- **~19-87ns per operation** for most cache operations using AdmissionLFU
- **56+ million operations/sec** throughput in real-world scenarios
- **Peak throughput**: 310M+ operations/sec for GET operations

### Stress Test Results

#### High Load Scenarios
| Load Profile | Ops/sec | ns/op | B/op | allocs/op | Description |
|-------------|---------|-------|------|-----------|-------------|
| **Small + High Concurrency** | 56,066,578 | **59.72** | 27 | 2 | Many goroutines, small cache |
| **Medium + Mixed Load** | 55,117,999 | **62.48** | 31 | 2 | Balanced read/write operations |
| **Large + Read Heavy** | 63,296,053 | **51.89** | 38 | 2 | Large cache, mostly reads |
| **XLarge + Write Heavy** | 43,971,300 | **67.03** | 40 | 3 | Very large cache, mostly writes |
| **Extreme + Balanced** | 43,159,774 | **81.41** | 41 | 3 | Maximum scale, balanced ops |

#### Advanced Stress Test Results

##### Contention Stress Test
| Test | Ops/sec | ns/op | B/op | allocs/op |
|------|---------|-------|------|-----------|
| **High Contention** | 47,936,589 | **73.58** | 34 | 2 |

##### Eviction Policy Performance
| Eviction Policy | Ops/sec | ns/op | B/op | allocs/op |
|-----------------|---------|-------|------|-----------|
| **AdmissionLFU** | 35,685,565 | **124.7** | 53 | 3 |
| **FIFO** | 42,240,423 | 146.4 | 56 | 3 |
| **LRU** | 36,104,913 | 165.0 | 59 | 3 |
| **LFU** | 24,205,524 | 195.0 | 58 | 3 |

##### Memory Pressure Tests
| Value Size | Ops/sec | ns/op | B/op | allocs/op |
|------------|---------|-------|------|-----------|
| **1KB** | 53,634,813 | **71.20** | 40 | 3 |
| **4KB** | 52,830,013 | **71.18** | 40 | 3 |
| **16KB** | 53,889,962 | **70.94** | 40 | 3 |
| **64KB** | 54,438,056 | **71.44** | 40 | 3 |

##### Sharding Efficiency Analysis
| Shards | Ops/sec | ns/op | B/op | allocs/op |
|--------|---------|-------|------|-----------|
| **1** | 15,326,596 | 352.5 | 49 | 3 |
| **2** | 15,610,903 | 310.2 | 48 | 3 |
| **4** | 19,235,764 | 212.7 | 46 | 3 |
| **8** | 25,790,181 | 157.0 | 47 | 3 |
| **16** | 33,085,953 | 119.4 | 47 | 3 |
| **32** | 41,616,372 | 95.76 | 48 | 3 |
| **64** | 50,870,149 | 78.81 | 48 | 3 |
| **128** | 63,269,726 | **62.85** | 49 | 3 |

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

#### AdmissionLFU (Admission-controlled Least Frequently Used)
- **Access**: Update frequency counter in O(1), no heap maintenance
- **Eviction**: Random sampling (default 5 items, configurable up to 20) with LFU selection in O(k) where k = sample size
- **Admission Control**: Multi-layer frequency-based admission with Count-Min Sketch and doorkeeper bloom filter
- **Memory**: Frequency bloom filter (10x shard size counters) + doorkeeper bloom filter (1/8 size) per shard

**AdmissionLFU Architecture:**
```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                              AdmissionLFU Shard                                 │
├─────────────────────────────┬───────────────────────────────────────────────────┤
│   Frequency Admission Filter │                Cache Items                       │
│  ┌─────────────────────────┐ │  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐                │
│  │  Frequency Bloom Filter  │ │  │Item │ │Item │ │Item │ │Item │ ...            │
│  │  Count-Min Sketch        │ │  │Freq:│ │Freq:│ │Freq:│ │Freq:│                │
│  │  4-bit counters x10*cap  │ │  │  5  │ │  12 │ │  3  │ │  8  │                │
│  │  Hash: 4 functions       │ │  └─────┘ └─────┘ └─────┘ └─────┘                │
│  └─────────────────────────┘ │                                                  │
│  ┌─────────────────────────┐ │  Random Sample 5 → Compare Frequencies           │
│  │    Doorkeeper Filter     │ │  Victim Selection: Min(freq, lastAccess)        │
│  │    Bloom Filter          │ │  ↓                                               │
│  │    Size: cap/8           │ │  Admission Decision vs Victim Frequency          │
│  │    Hash: 3 functions     │ │                                                  │
│  │    Reset: 1min interval  │ │  Admit Rules:                                    │
│  └─────────────────────────┘ │  • freq ≥ 3: Always admit                       │
│                              │  • freq > victim: Always admit                   │
│                              │  • freq = victim & freq > 0: 50% chance          │
│                              │  • else: 70% - (victim_freq * 10)%               │
└─────────────────────────────┴───────────────────────────────────────────────────┘
```

**Admission Control Components:**

1. **Frequency Estimation (Count-Min Sketch)**:
   - 4-bit counters packed in uint64 arrays (16 counters per word)
   - 4 hash functions with xxHash64-based avalanche mixing
   - Automatic aging: counters halved when total increments ≥ size × 10
   - Size: 10× shard capacity counters (min 1024 per shard)

2. **Doorkeeper Bloom Filter**:
   - 3 hash functions for recent access tracking
   - Size: 1/8 of frequency filter size for memory efficiency
   - Periodic reset every 1 minute (configurable via AdmissionResetInterval)
   - Immediate admission for items in doorkeeper (bypass frequency check)

3. **Admission Algorithm**:
   ```
   Phase 1: Doorkeeper Check
   if in_doorkeeper(key):
       refresh_doorkeeper(key)
       return ADMIT

   Phase 2: Update Frequency
   new_freq = frequency_filter.increment(key)
   doorkeeper.add(key)

   Phase 3: Admission Decision
   if new_freq >= 3:                    // High frequency guarantee
       return ADMIT
   elif new_freq > victim_frequency:    // Better than victim
       return ADMIT
   elif new_freq == victim_frequency && new_freq > 0:  // Tie-breaking
       return ADMIT if hash(key) % 2 == 0
   else:                                // Probabilistic admission
       threshold = 70 - (victim_frequency * 10)
       return ADMIT if hash(key) % 100 < threshold
   ```

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
    AdmissionLFU                     // Sampled LFU with admission control
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

// AdmissionLFU - Approximate LFU with Admission Control
config.EvictionPolicy = cache.AdmissionLFU
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
