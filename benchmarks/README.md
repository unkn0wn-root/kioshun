# Kioshun vs. Others

These benchmarks compare Kioshun against Ristretto, BigCache, FreeCache, and
go-cache using pre-generated workloads. Lower `ns/op` is better
for a given workload, but these results should be treated as environment specific
measurements.

## Methodology

Async and strict write semantics are reported separately:

- **Async**: Kioshun uses `SetAsync`; Ristretto uses `SetWithTTL`; async caches are
  flushed outside the timed section.
- **Strict**: Kioshun uses `Set`; Ristretto calls `Wait` after each write;
  synchronous caches use their normal `Set`.

Warmup and prepopulation use strict writes outside the timed section so setup is
not affected by async write buffers.

The harness reports a custom `write_fail/op` metric if a cache rejects writes
during the timed workload. Non-zero write failures mean `ns/op` is not accepted
write throughput by itself. The example tables include this metric where it was
non-zero.

### Cache Configuration

| Cache | Configuration |
| --- | --- |
| Kioshun | `DefaultConfig()` base, `MaxSize=100000`, `DefaultTTL=1h`, `StatsEnabled=false`, default SieveTinyLFU |
| Ristretto | `NumCounters=1000000`, `MaxCost=256MiB`, `BufferItems=64`, `Metrics=false`, value byte length as cost |
| BigCache | `DefaultConfig(1h)`, default 1024 shards, `MaxEntriesInWindow=100000`, `HardMaxCacheSize=256MB`, `StatsEnabled=false` |
| FreeCache | `NewCache(256MiB)` |
| go-cache | `NewFrom(1h, 5m, make(map[string]gocache.Item, 100000))` |

## Running

```bash
# From the repository root
make bench-compare
make bench
make stress-test
make bench-runner

# Or run the benchmark module directly
cd benchmarks
go test -run=TestBenchmarkComparisonGetSetup -count=1 .
go test -bench='BenchmarkCacheComparison' -benchmem -run=^$ -benchtime=1s .
go test -bench=. -benchmem -run=^$ -benchtime=1s -timeout=30m .
```

## Example Full Suite Run

Environment:

- Date: `2026-06-10`
- OS/arch: `darwin/arm64`
- OS version: `macOS 26.5.1 (Darwin 25.5.0)`
- CPU: `Apple M4 Max`
- Logical CPUs: `14`
- Memory: `36 GiB`
- Go: `go1.26.0 darwin/arm64`
- Setup sanity check: `go test -run=TestBenchmarkComparisonGetSetup -count=1 .`
- Full suite command: `go test -bench=. -benchmem -run=^$ -benchtime=1s -timeout=30m .`
- Full suite duration: `300.087s`

### Core Operations

#### Set Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 42.91 | - | 96 | 1 |
| FreeCache | 58.50 | - | 0 | 0 |
| BigCache | 79.38 | - | 44 | 0 |
| go-cache | 301.3 | - | 24 | 1 |
| Ristretto | 541.9 | 0.1501 | 100 | 1 |

#### Set Strict

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| FreeCache | 64.05 | - | 0 | 0 |
| BigCache | 69.62 | - | 47 | 0 |
| Kioshun | 71.35 | - | 96 | 1 |
| go-cache | 297.3 | - | 24 | 1 |
| Ristretto | 1280 | - | 338 | 3 |

#### Get TTL

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 6.714 | - | 0 | 0 |
| Ristretto | 23.04 | - | 0 | 0 |
| FreeCache | 87.64 | - | 1024 | 1 |
| BigCache | 99.44 | - | 1040 | 2 |
| go-cache | 120.9 | - | 0 | 0 |

#### Get No TTL

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 6.077 | - | 0 | 0 |
| Ristretto | 13.21 | - | 0 | 0 |
| FreeCache | 78.13 | - | 1024 | 1 |
| go-cache | 100.9 | - | 0 | 0 |
| BigCache | 108.1 | - | 1040 | 2 |

### Mixed Workloads

#### 70% Read / 30% Write Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 22.31 | - | 28 | 0 |
| BigCache | 45.39 | - | 271 | 0 |
| FreeCache | 83.23 | - | 718 | 0 |
| go-cache | 128.8 | - | 7 | 0 |
| Ristretto | 200.6 | - | 34 | 0 |

#### 70% Read / 30% Write Strict

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 41.10 | - | 28 | 0 |
| BigCache | 49.77 | - | 271 | 0 |
| FreeCache | 74.44 | - | 718 | 0 |
| go-cache | 139.7 | - | 7 | 0 |
| Ristretto | 414.0 | - | 96 | 0 |

#### 90% Read / 10% Write Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 11.97 | - | 9 | 0 |
| BigCache | 32.01 | - | 191 | 0 |
| Ristretto | 77.29 | - | 15 | 0 |
| FreeCache | 85.53 | - | 919 | 0 |
| go-cache | 157.3 | - | 2 | 0 |

#### 10% Read / 90% Write Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 39.25 | - | 86 | 0 |
| FreeCache | 65.26 | - | 100 | 0 |
| BigCache | 79.33 | - | 118 | 1 |
| go-cache | 326.0 | - | 21 | 0 |
| Ristretto | 527.2 | - | 87 | 0 |

### Contention And Real-World Workloads

#### 80% Hot Keys / 60% Read Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 25.19 | - | 38 | 0 |
| FreeCache | 78.87 | - | 617 | 0 |
| BigCache | 83.14 | - | 584 | 1 |
| go-cache | 128.5 | - | 9 | 0 |
| Ristretto | 246.5 | - | 42 | 0 |

#### 60% Read / 35% Write / 5% Delete Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 31.97 | - | 33 | 0 |
| BigCache | 56.17 | - | 377 | 0 |
| FreeCache | 76.21 | - | 691 | 0 |
| go-cache | 139.0 | - | 8 | 0 |
| Ristretto | 213.3 | 0.01084 | 43 | 0 |

#### 60% Read / 35% Write / 5% Delete Strict

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 32.81 | - | 33 | 0 |
| BigCache | 51.60 | - | 376 | 0 |
| FreeCache | 87.63 | - | 689 | 0 |
| go-cache | 133.7 | - | 8 | 0 |
| Ristretto | 519.1 | - | 117 | 1 |

### Large Values

#### Async ns/op

| Size | Kioshun | Ristretto | BigCache | FreeCache | go-cache |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1KB | 39.60 | 356.7 | 74.74 | 60.71 | 180.7 |
| 4KB | 40.40 | 349.9 | 121.3 | 98.04 | 189.0 |
| 16KB | 38.71 | 226.5 | 184.1 | 205.3 | 165.8 |
| 64KB | 41.88 | 103.0 | 463.6 | 236.7 | 230.6 |

#### Strict ns/op

| Size | Kioshun | Ristretto | BigCache | FreeCache | go-cache |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1KB | 53.13 | 964.9 | 65.88 | 64.67 | 243.1 |
| 4KB | 56.40 | 1051 | 108.7 | 103.7 | 234.3 |
| 16KB | 58.35 | 1184 | 189.4 | 210.9 | 238.9 |
| 64KB | 57.01 | 1026 | 490.8 | 248.8 | 234.8 |

Ristretto async large-value rows also reported write failures in this run:

| Size | Ristretto write_fail/op |
| ---: | ---: |
| 1KB | 0.04090 |
| 4KB | 0.04002 |
| 16KB | 0.2526 |
| 64KB | 0.5068 |
