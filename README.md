<div align="center">
  <img src="assets/logo.JPG" alt="Kioshun Logo" width="200"/>

  # Kioshun - In-Memory Cache for Go

  *"kee-oh-shoon" /kiːoʊʃuːn/*

  [![Go Version](https://img.shields.io/badge/Go-1.24+-blue.svg)](https://golang.org)
  [![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
  [![CI](https://github.com/unkn0wn-root/kioshun/actions/workflows/test.yml/badge.svg)](https://github.com/unkn0wn-root/kioshun/actions)


  *Thread-safe, sharded in-memory cache for Go*
</div>

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

Kioshun is a thread-safe (and fast!), sharded in-memory cache for Go.

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
    c.Set("user:123", "David Nice 1", kioshun.DefaultExpiration)

    // Set with no expiration
    c.Set("user:123", "David Nice 2", kioshun.NoExpiration)

    // Queue a write for high-throughput paths
    c.SetAsync("user:456", "David Nice 3", 5*time.Minute)
    c.Wait()

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
config := kioshun.Config{
    MaxSize:         100000,             // Maximum number of items
    ShardCount:      16,                 // Number of shards (0 = auto-detect)
    CleanupInterval: 5 * time.Minute,    // Cleanup frequency
    DefaultTTL:      30 * time.Minute,   // Default expiration time
    EvictionPolicy:  kioshun.SieveTinyLFU, // Eviction algorithm (default)
    StatsEnabled:    true,               // Enable statistics collection
    WriteBufferSize: 1024,               // Per-shard async write queue
    WriteBatchSize:  64,                 // Max commands applied per worker batch
}

c, err := kioshun.New[string, any](config)
if err != nil {
    // handle invalid configuration
}
```

## API

```go
c.Set(key, value, ttl time.Duration) error
c.SetAsync(key, value, ttl time.Duration) error
c.SetWithCallback(key, value, ttl, callback func(key, value)) error
c.Get(key) (value, found bool)
c.GetWithTTL(key) (value, ttl time.Duration, found bool)
c.Keys() []K
c.Clear()
c.Wait() error
c.Delete(key) bool
c.Exists(key) bool
c.Size() int64
c.Stats() Stats
c.PolicyStats() PolicyStats
c.TriggerCleanup()
c.Close() error
```

`Set` is synchronous and gives immediate read-after-write visibility for the key.
Use `SetAsync` for queued high-throughput writes, and call `Wait` when a caller
needs a global fence for queued writes across all shards.

Invalid configurations return `ConfigError` and match `ErrInvalidConfig` with
`errors.Is`.

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
    // handle invalid configuration
}
defer middleware.Close()

http.Handle("/api/users", middleware.Middleware(usersHandler))
```
> See **[MIDDLEWARE.md](MIDDLEWARE.md)** for complete documentation, examples, and advanced configuration.

## Benchmark Results

Latest benchmark run (Apple M4 Max, Go 1.24.7):
- `SET`: 100,000,000 ops/sec · 75.55 ns/op · 41 B/op · 3 allocs/op
- `GET`: 231,967,180 ops/sec · 25.87 ns/op · 31 B/op · 2 allocs/op
- `Real-World`: 52,742,550 ops/sec · 65.25 ns/op · 48 B/op · 3 allocs/op

Full suite: [benchmarks/README.md](benchmarks/README.md)
