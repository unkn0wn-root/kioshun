<div align="center">
  <img src="assets/logo.JPG" alt="Kioshun Logo" width="200"/>

  # Kioshun - In-Memory Cache for Go

  *"kee-oh-shoon" /kiːoʊʃuːn/*

  [![Go Version](https://img.shields.io/badge/Go-1.21+-blue.svg)](https://golang.org)
  [![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
  [![CI](https://github.com/unkn0wn-root/kioshun/actions/workflows/test.yml/badge.svg)](https://github.com/unkn0wn-root/kioshun/actions)


  *Thread-safe, sharded in-memory cache for Go - with an optional peer-to-peer cluster backend*
</div>

## Table of Contents

- [Cluster (Overview)](#cluster-overview)
- [Architecture](#architecture)
  - [Sharded Design](#sharded-design)
  - [Evictions Implementation](#eviction-policy-implementation)
  - [Eviction Policy Implementation](#eviction-policy-implementation)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [Configuration](#configuration)
- [API Reference](#api-reference)
- [HTTP Middleware](#http-middleware)
- [Cache Invalidation Setup](#cache-invalidation-setup)
- [Benchmark Results](_benchmarks/benchmarks.md)


## Cluster Overview

> <span style="color:#ff6600">**Experimental:**</span> the cluster implementation is under active development.
  Backward compatibility is not guaranteed across minor releases. Review release notes before upgrading.

Kioshun can run as a **peer-to-peer mesh**: each service instance embeds a cluster node that discovers peers (*Seeds*), forms a weighted rendezvous ring, and replicates writes with configurable RF/WC. Reads route to the primary owner; read‑through population uses single‑flight leases.

```
┌─────────────┐      Gossip + Weights      ┌─────────────┐
│  Service A  │◀──────────────────────────▶│  Service B  │
│  + Node     │◀───────────▶◀───────────▶  │  + Node     │
└──────┬──────┘                            └──────┬──────┘
       │   Owner‑routed Get/Set (RF)              │
       └──────────────▶◀──────────────────────────┘
                  Service C + Node
```

Small multinode example:

```
# on each server
CACHE_BIND=:4443
CACHE_PUBLIC=srv-a:4443   # srv-b / srv-c on others
CACHE_SEEDS=srv-a:4443,srv-b:4443,srv-c:4443
CACHE_AUTH=supersecret

// in code
local := cache.NewWithDefaults[string, []byte]()
cfg := cluster.Default()
cfg.BindAddr = os.Getenv("CACHE_BIND")
cfg.PublicURL = os.Getenv("CACHE_PUBLIC")
cfg.Seeds = strings.Split(os.Getenv("CACHE_SEEDS"), ",")
cfg.ReplicationFactor = 3; cfg.WriteConcern = 2
cfg.Sec.AuthToken = os.Getenv("CACHE_AUTH")
node := cluster.NewNode[string, []byte](cfg, cluster.StringKeyCodec[string]{}, local, cluster.BytesCodec{})
if err := node.Start(); err != nil { panic(err) }
dc := cluster.NewDistributedCache[string, []byte](node)
```

> See **CLUSTER.md** for more details.

## Architecture

### Sharded Design


```
┌──────────────────────────────────────────────────────────────────────────┐
│                            Kioshun Cache                                 │
├─────────────────┬─────────────────┬─────────────────┬─────────────────────┤
│     Shard 0     │     Shard 1     │     Shard 2     │  ...  │   Shard N   │
│   RWMutex       │   RWMutex       │   RWMutex       │       │   RWMutex   │
│                 │                 │                 │       │             │
│ ┌─────────────┐ │ ┌─────────────┐ │ ┌─────────────┐ │       │ ┌─────────┐ │
│ │ Hash Map    │ │ │ Hash Map    │ │ │ Hash Map    │ │       │ │Hash Map │ │
│ │ K -> Item   │ │ │ K -> Item   │ │ │ K -> Item   │ │       │ │K -> Item│ │
│ └─────────────┘ │ └─────────────┘ │ └─────────────┘ │       │ └─────────┘ │
│                 │                 │                 │       │             │
│ ┌─────────────┐ │ ┌─────────────┐ │ ┌─────────────┐ │       │ ┌─────────┐ │
│ │ LRU List    │ │ │ LRU List    │ │ │ LRU List    │ │       │ │LRU List │ │
│ │ head ←→ tail│ │ │ head ←→ tail│ │ │ head ←→ tail│ │       │ │head↔tail│ │
│ │ (sentinel)  │ │ │ (sentinel)  │ │ │ (sentinel)  │ │       │ │(sentinel│ │
│ └─────────────┘ │ └─────────────┘ │ └─────────────┘ │       │ └─────────┘ │
│                 │                 │                 │       │             │
│ ┌─────────────┐ │ ┌─────────────┐ │ ┌─────────────┐ │       │ ┌─────────┐ │
│ │LFU FreqList │ │ │LFU FreqList │ │ │LFU FreqList │ │       │ │LFU Freq │ │
│ │(if LFU mode)│ │ │(if LFU mode)│ │ │(if LFU mode)│ │       │ │(LFU only│ │
│ └─────────────┘ │ └─────────────┘ │ └─────────────┘ │       │ └─────────┘ │
│                 │                 │                 │       │             │
│ ┌─────────────┐ │ ┌─────────────┐ │ ┌─────────────┐ │       │ ┌─────────┐ │
│ │Admission    │ │ │Admission    │ │ │Admission    │ │       │ │Admission│ │
│ │Filter       │ │ │Filter       │ │ │Filter       │ │       │ │Filter   │ │
│ │(AdmLFU only)│ │ │(AdmLFU only)│ │ │(AdmLFU only)│ │       │ │(AdmLFU) │ │
│ └─────────────┘ │ └─────────────┘ │ └─────────────┘ │       │ └─────────┘ │
│                 │                 │                 │       │             │
│ Stats Counter   │ Stats Counter   │ Stats Counter   │       │ Stats       │
└─────────────────┴─────────────────┴─────────────────┴─────────────────────┘
```

- Keys are sharded by a 64-bit hasher: integers are avalanched, short strings (≤8B) use FNV-1a, and longer strings use xxHash64. Shard index = `hash & (shardCount-1)`.
- Each shard maintains an independent read-write mutex
- Shard count auto-detected based on CPU cores (default: 4× CPU cores, max 256)
- Object pooling reduces GC pressure

### Eviction Policy Implementation

#### LRU (Least Recently Used)
**Implementation**: Circular doubly-linked list with sentinel nodes
- **Access**: Move item to head position in O(1) - bypasses null checks using sentinels
- **Eviction**: Remove least recent item (tail.prev) in O(1)
- **Memory**: Minimal overhead per item (2 pointers: prev, next)
- head ←→ item1 ←→ item2 ←→ ... ←→ itemN ←→ tail (sentinels)

**LRU Operations:**
```go
// O(1) access update - no null checks needed due to sentinels
moveToLRUHead(item):
  if head.next == item: return  // already at head
  remove item from current position
  insert item after head sentinel

// O(1) eviction - always valid due to sentinel structure
evictLRU():
  victim = tail.prev           // LRU item
  removeFromLRU(victim)        // unlink from list
  delete from hashmap          // remove from shard.data
```

#### LFU (Least Frequently Used) - **Frequency-Node Architecture**
**Implementation**: Doubly-linked list of frequency buckets, each containing items with same access count
- **Access**: Increment frequency and move item to next bucket in O(1) amortized
- **Eviction**: Remove item from lowest frequency bucket in O(1)
- **Memory**: Additional frequency tracking per item + bucket management

**LFU Structure:**
```
Frequency Buckets (sorted by access count):
┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐
│ Freq: 1  │←→  │ Freq: 2  │←→  │ Freq: 5  │←→  │ Freq: 8  │
│ item1    │    │ item3    │    │ item2    │    │ item4    │
│ item5    │    │ item7    │    │          │    │          │
└──────────┘    └──────────┘    └──────────┘    └──────────┘
```

**LFU Operations:**
```go
// O(1) amortized frequency increment
increment(item):
  currentBucket = itemFreq[item]
  newFreq = currentBucket.freq + 1

  // Move to next bucket or create new one
  if nextBucket.freq == newFreq:
    target = nextBucket
  else:
    target = createBucket(newFreq) // splice after current

  move item from currentBucket to target
  if currentBucket.isEmpty(): removeBucket(currentBucket)

// O(1) eviction from lowest frequency bucket
evictLFU():
  lowestBucket = head.next     // first non-sentinel bucket
  victim = any item from lowestBucket.items
  remove victim from bucket and hashmap
```

#### FIFO (First In, First Out)
**Implementation**: Uses LRU list structure but treats it as insertion-order queue
- **Access**: No position updates on access (maintains insertion order)
- **Eviction**: Remove oldest inserted item (at tail.prev) in O(1)

#### AdmissionLFU (Adaptive Admission-controlled Least Frequently Used) - **Default**
**Implementation**: Approximate LFU with admission control and scan resistance
- **Access**: Update frequency counter in O(1) using Count-Min Sketch, no heap maintenance
- **Eviction**: Random sampling (default 5 items, max 20) with LFU selection in O(k) where k = sample size
- **Admission Control**: Multi-layered frequency-based admission with adaptive thresholds
- **Scan Resistance**: Detects and adapts to scanning workloads automatically
- Frequency bloom filter (10×shard size) + doorkeeper bloom filter (1/8 size) + workload detector per shard

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
   - Tracks consecutive admission rejections (fast signal of cold scans)
   - Optional admissions/sec signal (disabled by default; see note below)
   - Switches to a more recency-biased admission during detected scans

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
    LRU        EvictionPolicy = iota // Least Recently Used
    LFU                              // Least Frequently Used
    FIFO                             // First In, First Out
    AdmissionLFU                     // Sampled LFU with admission control (default)
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
    EvictionPolicy:  cache.AdmissionLFU, // Eviction algorithm (default)
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
    Evictions   int64   // Evictions (all policies)
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

// FIFO (First In, First Out)
config.EvictionPolicy = cache.FIFO

// LRU (Least Recently Used)
config.EvictionPolicy = cache.LRU

// LFU (Least Frequently Used)
config.EvictionPolicy = cache.LFU

// AdmissionLFU - Approximate LFU with Admission Control
config.EvictionPolicy = cache.AdmissionLFU
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
