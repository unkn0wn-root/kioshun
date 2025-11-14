<div align="center">
  <img src="assets/logo.JPG" alt="Kioshun Logo" width="200"/>

  # Kioshun - In-Memory Cache for Go

  *"kee-oh-shoon" /kiːoʊʃuːn/*

  [![Go Version](https://img.shields.io/badge/Go-1.24+-blue.svg)](https://golang.org)
  [![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
  [![CI](https://github.com/unkn0wn-root/kioshun/actions/workflows/test.yml/badge.svg)](https://github.com/unkn0wn-root/kioshun/actions)


  *Thread-safe, sharded in-memory cache for Go - with an optional peer-to-peer cluster backend*
</div>

## Index

- [What is Kioshun?](#what-is-kioshun)
- [Cluster (Overview)](#cluster-overview)
- [Internals](INTERNALS.md)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [Configuration](#configuration)
- [API](#api)
- [HTTP Middleware](MIDDLEWARE.md)
- [Benchmark Results](#benchmark-results)

## What is Kioshun?

Kioshun is a thread-safe (and fast!), in-memory cache for Go. You can **run it as a local cache** just like any other in-memory caches, or turn on the **peer-to-peer cluster** when you want replicas across hosts.

If you want to know more about Kioshun internals and how it works under the hood - see [Kioshun Internals](INTERNALS.md)

## Cluster Overview

> [!NOTE]
> Clustering is fully **optional**. If you don’t enable the cluster, Kioshun runs as a standalone, in‑memory cache.

Kioshun’s cluster turns every service instance into a **small, self-managing peer-to-peer cache**. You just point each one at a few reachable *Seeds* and it discovers the rest, builds a weighted rendezvous and replicates writes with configurable RF/WC so hot data stays local. Gossip keeps the peer list fresh, hinted handoff plus backfill repair all gaps, and reads go straight to the primary owner while read-through uses single-flight leases.

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
if err := node.Start(); err != nil {
    panic(err)
}

dc := cluster.NewDistributedCache[string, []byte](node)
```

Only a subset of nodes need to appear in `CACHE_SEEDS`. The list is purely for bootstrap - include a few stable peers so new processes can reach at least one live seed, then gossip distributes the rest of the membership automatically, whether you run 3 caches or 20.

> See **CLUSTER.md** for more details.

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
    c.Set("user:123", "David Nice 1", cache.DefaultExpiration)

    // Set with no expiration
    c.Set("user:123", "David Nice 2", cache.NoExpiration)

    // Set value with custom TTL
    c.Set("user:123", "David Nice 3", 5*time.Minute)

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
    MaxSize:         100000,             // Maximum number of items
    ShardCount:      16,                 // Number of shards (0 = auto-detect)
    CleanupInterval: 5 * time.Minute,    // Cleanup frequency
    DefaultTTL:      30 * time.Minute,   // Default expiration time
    EvictionPolicy:  cache.AdmissionLFU, // Eviction algorithm (default)
    StatsEnabled:    true,               // Enable statistics collection
}

cache := cache.New[string, any](config)
```

## API

```go
cache.Set(key, value, ttl time.Duration) error
cache.SetWithCallback(key, value, ttl, callback func(key, value)) error
cache.Get(key) (value, found bool)
cache.GetWithTTL(key) (value, ttl time.Duration, found bool)
cache.Keys() []K
cache.Clear()
cache.Delete(key) bool
cache.Exists(key) bool
cache.Size() int64
cache.Stats() Stats
cache.TriggerCleanup()
cache.Close() error
```

### Statistics

```go
type Stats struct {
    Hits        int64
    Misses      int64
    Evictions   int64
    Expirations int64
    Size        int64
    Capacity    int64
    HitRatio    float64
    Shards      int
}
```

## HTTP Middleware

Kioshun provides HTTP middleware out-of-the-box.

```go
config := cache.DefaultMiddlewareConfig()
config.DefaultTTL = 5 * time.Minute
config.MaxSize = 100000

middleware := cache.NewHTTPCacheMiddleware(config)
defer middleware.Close()

http.Handle("/api/users", middleware.Middleware(usersHandler))
```
> See **[MIDDLEWARE.md](MIDDLEWARE.md)** for complete documentation, examples, and advanced configuration.

## Benchmark Results

Latest benchmark run (Apple M4 Max, Go 1.24.7):
- `SET`: 100,000,000 ops/sec · 75.55 ns/op · 41 B/op · 3 allocs/op
- `GET`: 231,967,180 ops/sec · 25.87 ns/op · 31 B/op · 2 allocs/op
- `Real-World`: 52,742,550 ops/sec · 65.25 ns/op · 48 B/op · 3 allocs/op

Full suite: [_benchmarks/README.md](_benchmarks/README.md)
