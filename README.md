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

- [Architecture](#architecture)
  - [Sharded Design](#sharded-design)
  - [Evictions Implementation](#eviction-policy-implementation)
  - [Eviction Policies](#eviction-policies)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [Configuration](#configuration)
- [API Reference](#api-reference)
- [HTTP Middleware](#http-middleware)
- [Cache Invalidation Setup](#cache-invalidation-setup)
- [Benchmark Results](_benchmarks/benchmarks.md)


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

- Keys distributed across shards using optimized hash functions
- Each shard maintains independent read-write mutex
- Auto-detection based on CPU cores
- Object pooling to reduce GC pressure

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

#### AdmissionLFU (Adaptive Admission-controlled Least Frequently Used)
- **Access**: Update frequency counter in O(1), no heap maintenance
- **Eviction**: Random sampling (default 5 items, configurable up to 20) with LFU selection in O(k) where k = sample size
- **Admission Control**: Adaptive multi-layer frequency-based admission with Count-Min Sketch, doorkeeper bloom filter, and scan resistance
- **Scan Resistance**: Detects and adapts to scanning workloads (high admission rates, sequential access patterns, consecutive misses)
- **Memory**: Frequency bloom filter (10x shard size counters) + doorkeeper bloom filter (1/8 size) + workload detector per shard

**AdmissionLFU Architecture:**
```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                          Adaptive AdmissionLFU Shard                            │
├─────────────────────────────┬───────────────────────────────────────────────────┤
│ Adaptive Admission Filter   │                Cache Items                        │
│  ┌─────────────────────────┐│  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐                  │
│  │  Frequency Bloom Filter ││  │Item │ │Item │ │Item │ │Item │ ...              │
│  │  Count-Min Sketch       ││  │Freq:│ │Freq:│ │Freq:│ │Freq:│                  │
│  │  4-bit counters x10*cap ││  │  5  │ │  12 │ │  3  │ │  8  │                  │
│  │  Hash: 4 functions      ││  └─────┘ └─────┘ └─────┘ └─────┘                  │
│  │  Auto-aging threshold   ││                                                   │
│  └─────────────────────────┘│  Random Sample 5 → Compare Frequencies            │
│  ┌─────────────────────────┐│  Victim Selection: Min(freq, lastAccess)          │
│  │    Doorkeeper Filter    ││  ↓                                                │
│  │    Bloom Filter         ││  Adaptive Admission Decision                      │
│  │    Size: cap/8          ││                                                   │
│  │    Hash: 3 functions    ││  Normal Mode:                                     │
│  │    Reset: 1min interval ││  • freq ≥ 3: Always admit                         │
│  └─────────────────────────┘│  • freq > victim: Always admit                    │
│  ┌─────────────────────────┐│  • freq = victim & freq > 0: Recency-based        │
│  │   Workload Detector     ││  • else: Adaptive probability 5-95%               │
│  │   Sequential patterns   ││                                                   │
│  │   Admission rate track  ││  Scan Mode (detected patterns):                   │
│  │   Miss streak tracking  ││  • Recency-based admission                        │
│  │   Adaptive probability  ││  • Lower admission threshold                      │
│  └─────────────────────────┘│  • Protect against cache pollution                │
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

3. **Workload Detector (Scan Resistance)**:
   - Tracks admission rate (admissions/second) with threshold of 100/sec
   - Detects sequential access patterns using circular buffer of recent keys
   - Monitors consecutive miss streaks with threshold of 50 misses
   - Adapts admission strategy during detected scanning workloads

4. **Adaptive Admission Algorithm**:
   ```
   Phase 1: Doorkeeper Check
   if in_doorkeeper(key):
       refresh_doorkeeper(key)
       return ADMIT

   Phase 2: Scan Detection
   if detect_scan(key):
       if victim_age > 100ms:
           return ADMIT    // Prefer recent items during scan
       else:
           return ADMIT if hash(key) % 100 < min_probability(5%)

   Phase 3: Update Frequency & Doorkeeper
   new_freq = frequency_filter.increment(key)
   doorkeeper.add(key)

   Phase 4: Adaptive Admission Decision
   if new_freq >= 3:                    // High frequency guarantee
       return ADMIT
   elif new_freq > victim_frequency:    // Better than victim
       return ADMIT
   elif new_freq == victim_frequency && new_freq > 0:  // Recency tie-breaking
       return ADMIT if victim_age > 1_second
   else:                                // Adaptive probabilistic admission
       probability = adaptive_probability(5-95%) - (victim_frequency * 10)
       return ADMIT if hash(key) % 100 < max(probability, 5%)

   Phase 5: Probability Adjustment
   adjust_probability_based_on_eviction_pressure()
   ```

5. **Adaptive Probability Control**:
   - Dynamic admission probability between 5% and 95%
   - Decreases probability (-5%) when eviction rate > 100/second
   - Increases probability (+5%) when eviction rate < 10/second
   - Eviction pressure monitored every second

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
