<div align="center">
  <img src="assets/logo.JPG" alt="Kioshun Logo" width="200"/>

  # Kioshun - In-Memory Cache for Go

  *"kee-oh-shoon" /kiːoʊʃuːn/*

  [![Go Version](https://img.shields.io/badge/Go-1.24+-blue.svg)](https://golang.org)
  [![License](https://img.shields.io/badge/license-Apache%20License%202.0-green)](LICENSE)
  [![CI](https://github.com/unkn0wn-root/kioshun/actions/workflows/test.yml/badge.svg)](https://github.com/unkn0wn-root/kioshun/actions)


  *Fast, sharded in-memory cache for Go*
</div>

## Index

- [Installation](#installation)
- [Quick Start](#quick-start)
- [Configuration](#configuration) ([full reference](CONFIGURATION.md))
- [API](#api)
- [HTTP Middleware](MIDDLEWARE.md)
- [Internals](INTERNALS.md)
- [Benchmarks](#benchmarks)

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

    // Set commits the write before returning so the key is immediately readable.
    c.Set("user:456", "John", kioshun.NoExpiration)

    // SetAsync returns early. It may commit inline when the shard is idle
    // otherwise it queues the write for that shard's worker.
    c.SetAsync("user:789", "Paul", 5*time.Minute)

    // Optional: call Sync() when committed visibility is required.
    c.Sync()

    // Get value
    if value, found := c.Get("user:123"); found {
        fmt.Printf("User: %s\n", value)
    }

}
```

## Configuration

For most caches, start with the built-in defaults:

```go
c := kioshun.NewDefault[string, string]()
defer c.Close()
```

`NewDefault` uses `DefaultConfig()`: `MaxSize=10000`, `DefaultTTL=30m`,
automatic shard count, `CleanupInterval=5m`, SieveTinyLFU eviction, stats
disabled and the default async write buffer/batch sizes.

When you only need a few changes, start from `DefaultConfig()` and override the
specific fields:

```go
config := kioshun.DefaultConfig()
config.MaxSize = 100000
config.DefaultTTL = time.Hour

c, err := kioshun.New[string, string](config)
if err != nil {
    // handle error
}
```

> Every `Config` field, weighted capacity (`MaxCost` + `WithWeigher`) and the
> named-cache `Manager` are covered in **[CONFIGURATION.md](CONFIGURATION.md)**.

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

> `Set` is synchronous and gives immediate 'read-after-write' visibility for the key.
> `SetAsync` is optional - it may have committed inline already or it may still be queued.
> Use `Sync` when committed visibility is required.

Full API reference: [pkg.go.dev/github.com/unkn0wn-root/kioshun](https://pkg.go.dev/github.com/unkn0wn-root/kioshun)

## HTTP Middleware

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

### Hit ratio and throughput

Kioshun is compared against [Ristretto](https://github.com/dgraph-io/ristretto),
[Otter](https://github.com/maypok86/otter) and [Theine](https://github.com/Yiling-J/theine-go)
by replaying ARC and LIRS request traces (P3, P8, S3, DS1, OLTP, LOOP)
through every cache in the **same harness** at a 100,000 entry cap. Hit ratio is
plotted as a percentage of the Belady optimum.

![Hit ratio vs the Belady optimum](benchmarks/chart/hitratio_opt.svg)

![Hit ratio vs cache size](benchmarks/chart/hitratio_vs_size.svg)

![Throughput](benchmarks/chart/throughput.svg)

![Hit ratio vs throughput](benchmarks/chart/spot.svg)

The 100k entry cap.

| trace | OPT | kioshun | theine | otter | ristretto |
|---|---|---|---|---|---|
| p3 | 62.00% | **44.94%** | 38.14% | 36.41% | 21.22% |
| p8 | 77.12% | **59.85%** | 54.10% | 56.56% | 55.83% |
| s3 | 25.42% | **13.46%** | 11.90% | 9.06% | 9.80% |
| ds1 | 5.16% | **1.97%** | 1.51% | 1.57% | 1.36% |
| oltp | 79.56% | **79.29%** | 76.67% | 76.41% | 28.30% |
| loop | 99.80% | 99.26% | **99.79%** | 99.60% | 90.39% |

Hit ratio as a percent of that ceiling:

| trace | kioshun | theine | otter | ristretto |
|---|---|---|---|---|
| p3 | **71.3** | 61.5 | 58.7 | 34.2 |
| p8 | **77.6** | 70.2 | 73.3 | 72.4 |
| s3 | **49.3** | 46.8 | 35.6 | 38.6 |
| ds1 | **36.8** | 29.3 | 30.4 | 26.4 |
| oltp | **98.8** | 96.4 | 96.0 | 35.6 |
| loop | 99.4 | **100.0** | 99.8 | 90.6 |
| geomean | **67.9** | 62.1 | 59.5 | 44.8 |

### Microbenchmarks

A separate suite compares Kioshun with Ristretto, BigCache, FreeCache and go-cache using
pre-generated workloads with async and strict write modes reported separately:

```bash
cd benchmarks
go test -run=TestBenchmarkComparisonGetSetup -count=1 .
go test -bench='BenchmarkCacheComparison' -benchmem -run=^$ -benchtime=1s .
go test -bench=. -benchmem -run=^$ -benchtime=1s -timeout=30m .
```
