<div align="center">
  <img src="assets/logo.JPG" alt="Kioshun Logo" width="200"/>

  # Kioshun - In-Memory Cache for Go

  *"kee-oh-shoon" /kiːoʊʃuːn/*

  [![Go Version](https://img.shields.io/badge/Go-1.24+-blue.svg)](https://golang.org)
  [![License](https://img.shields.io/badge/license-Apache%20License%202.0-green)](LICENSE)
  [![CI](https://github.com/unkn0wn-root/kioshun/actions/workflows/test.yml/badge.svg)](https://github.com/unkn0wn-root/kioshun/actions)


  *Thread-safe, sharded in-memory cache for Go*
</div>

> [!WARNING]
> <b>v1</b> is a complete redesign, not a drop-in upgrade from earlier releases!
>
> The biggest change is that clustering has been removed - the old peer-to-peer features are no longer part of the project. The focus is now on continuously improving cache performance and correctness.
> The cache core was also rebuilt - `AdmissionLFU` default has been replaced by `SieveTinyLFU` with probation/main queues, ghost entries, TinyLFU sketching, adaptive segment sizing and queued/batched writes.
>
> Public APIs changed as part of the redesign, including `New` and `NewDefault` replacing `NewWithDefaults`, cache policy/config names changing and HTTP middleware moving to the `httpcache` package.

## Index

- [What is Kioshun?](#what-is-kioshun)
- [Internals](INTERNALS.md)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [Configuration](#configuration)
- [API](#api)
- [HTTP Middleware](MIDDLEWARE.md)
- [Benchmark Results](#benchmark-results)

## What is Kioshun?

Kioshun is a fast, sharded in-memory cache for Go.

If you want to know more about Kioshun internals and how it works under the hood - see [Kioshun Internals](INTERNALS.md)

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
    c := kioshun.NewDefault[string, string]()
    defer c.Close()

    // Set with default TTL (30 min)
    c.Set("user:123", "David", kioshun.DefaultExpiration)

    // `Set` commits the write before returning so the key is immediately readable.
    c.Set("user:456", "John", kioshun.NoExpiration)

    // `SetAsync` queues the write and returns early.
    c.SetAsync("user:789", "Paul", 5*time.Minute)

    // Optional: call `Sync()` if you want to wait for value back.
    c.Sync()

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

### Basic

```go
config := kioshun.Config{
    MaxSize:         100000,               // Maximum number of items
    ShardCount:      16,                   // Number of shards (0 = auto-detect)
    CleanupInterval: 5 * time.Minute,      // Cleanup frequency
    DefaultTTL:      30 * time.Minute,     // Default expiration time
    EvictionPolicy:  kioshun.SieveTinyLFU, // Eviction algorithm (default)
    StatsEnabled:    true,                 // Enable statistics collection
    WriteBufferSize: 1024,                 // Per-shard async write queue
    WriteBatchSize:  64,                   // Max commands applied per worker batch
}

c, err := kioshun.New[string, any](config)
if err != nil {
    // handle error
}
```

> Each cache runs one write-worker goroutine per shard (plus a cleanup goroutine when `CleanupInterval > 0`),
> so `ShardCount` sets the number of background goroutines - default `min(NumCPU*4, 256)`.
> If you create many caches (e.g. via the cache `Manager`), set `ShardCount` explicitly to bound the total.

## API

```go
c.Set(key, value, ttl time.Duration) error
c.SetAsync(key, value, ttl time.Duration) error
c.SetWithCallback(key, value, ttl, callback func(key, value)) error
c.Get(key) (value, found bool)
c.GetWithTTL(key) (value, ttl time.Duration, found bool)
c.Keys() []K
c.Clear()
c.Sync() error
c.Delete(key) bool
c.Exists(key) bool
c.Size() int64
c.Stats() Stats
c.PolicyStats() PolicyStats
c.Cleanup()
c.Close() error
```

> `Set` is synchronous and gives immediate read-after-write visibility for the key.
> `SetAsync` is optional and only accepts the write into the shard queue meaning that the value
> becomes visible after a background worker commits it.

> Create with `kioshun.New(config, kioshun.WithOnRemove(func(key K, value V, reason kioshun.RemovalReason) { ... }))`
> to receive a callback for every key removed by capacity eviction,
> SieveTinyLFU admission rejection, TTL expiration or `Delete`. Use
> `WithOnEvict(func(key K, value V) { ... })` for capacity evictions only.

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

type PolicyStats struct {
    Admits              int64
    Rejects             int64
    GhostHits           int64
    Promotions          int64
    ProbationEvictions  int64
    MainEvictions       int64
}
```

## HTTP Middleware

Kioshun provides HTTP middleware out-of-the-box.

```go
import "github.com/unkn0wn-root/kioshun/httpcache"

config := httpcache.DefaultConfig()
config.DefaultTTL = 5 * time.Minute
config.MaxSize = 100000

middleware, err := httpcache.New(config)
if err != nil {
    // handle error
}
defer middleware.Close()

http.Handle("/api/users", middleware.Wrap(usersHandler))
```
> See **[MIDDLEWARE.md](MIDDLEWARE.md)** for complete documentation.

## Benchmarks

You can find comparison tests in [benchmarks](benchmarks/).
Those compares Kioshun with Ristretto, BigCache, FreeCache, and go-cache using
pre-generated workloads, with async and strict write modes reported separately.

You can rerun the suite on your machine:

```bash
cd benchmarks
GOCACHE=/tmp/kioshun-go-build go test -run=TestBenchmarkComparisonGetSetup -count=1 .
GOCACHE=/tmp/kioshun-go-build go test -bench='BenchmarkCacheComparison' -benchmem -run=^$ -benchtime=1s .
GOCACHE=/tmp/kioshun-go-build go test -bench=. -benchmem -run=^$ -benchtime=1s -timeout=30m .
```

Latest checked-in Kioshun snapshot from the comparison suite
(`2026-06-02`, Apple M4 Max, Go `go1.26.0 darwin/arm64`):

`Measured ops` is the Go benchmark iteration count for that timed workload, not
the number of unique cache entries.

| Workload | Measured ops | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Set async | 39,253,849 | 40.44 | 0 | 0 |
| Set strict | 18,755,151 | 67.31 | 0 | 0 |
| Get TTL | 52,658,330 | 23.53 | 0 | 0 |
| Get no TTL | 83,651,143 | 15.26 | 0 | 0 |
| 70% read / 30% write async | 27,072,325 | 44.70 | 0 | 0 |
| Real-world strict | 22,050,884 | 56.33 | 0 | 0 |
