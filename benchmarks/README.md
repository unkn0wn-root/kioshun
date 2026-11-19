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

## Example Run

Environment:

- Date: `2026-11-19`
- OS/arch: `darwin/arm64`
- OS version: `macOS 26.6.2 (Darwin 25.6.0)`
- CPU: `Apple M4 Max`
- Logical CPUs: `14`
- Memory: `36 GiB`
- Go: `go1.27.3 darwin/arm64`
- Library versions: Ristretto `v2.4.2`, BigCache `v3.2.0`, FreeCache `v1.2.7`, go-cache `v2.1.0+incompatible`
- Setup sanity check: `go test -run=TestBenchmarkComparisonGetSetup -count=1 .`
- Command: `go test -bench='BenchmarkCacheComparison' -benchmem -run=^$ -benchtime=1s -count=5 -timeout=60m .`
- Duration: `745.940s`

Each value is the median of 5 runs.

### Core Operations

#### Set Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 45.19 | - | 96 | 1 |
| FreeCache | 53.73 | - | 0 | 0 |
| BigCache | 61.27 | - | 21 | 0 |
| go-cache | 326.5 | - | 24 | 1 |
| Ristretto | 497.8 | 0.1391 | 100 | 1 |

#### Set Strict

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| BigCache | 52.34 | - | 23 | 0 |
| FreeCache | 53.09 | - | 0 | 0 |
| Kioshun | 60.64 | - | 96 | 1 |
| go-cache | 321.9 | - | 24 | 1 |
| Ristretto | 1153 | - | 333 | 3 |

#### Get TTL

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 7.538 | - | 0 | 0 |
| Ristretto | 20.67 | - | 0 | 0 |
| FreeCache | 73.31 | - | 1024 | 1 |
| BigCache | 88.14 | - | 1040 | 2 |
| go-cache | 131.3 | - | 0 | 0 |

#### Get No TTL

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 6.657 | - | 0 | 0 |
| Ristretto | 15.34 | - | 0 | 0 |
| FreeCache | 71.11 | - | 1024 | 1 |
| BigCache | 85.57 | - | 1040 | 2 |
| go-cache | 124.6 | - | 0 | 0 |

### Mixed Workloads

#### 70% Read / 30% Write Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 18.99 | - | 28 | 0 |
| BigCache | 34.16 | - | 262 | 0 |
| FreeCache | 66.35 | - | 718 | 0 |
| go-cache | 117.2 | - | 7 | 0 |
| Ristretto | 195.3 | - | 34 | 0 |

#### 70% Read / 30% Write Strict

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| BigCache | 34.45 | - | 262 | 0 |
| Kioshun | 37.78 | - | 28 | 0 |
| FreeCache | 66.76 | - | 718 | 0 |
| go-cache | 119.1 | - | 7 | 0 |
| Ristretto | 401.8 | - | 96 | 0 |

#### 90% Read / 10% Write Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 13.05 | - | 9 | 0 |
| BigCache | 24.85 | - | 181 | 0 |
| FreeCache | 76.73 | - | 919 | 0 |
| Ristretto | 78.58 | - | 15 | 0 |
| go-cache | 132.7 | - | 2 | 0 |

#### 10% Read / 90% Write Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 35.56 | - | 86 | 0 |
| BigCache | 56.90 | - | 94 | 0 |
| FreeCache | 61.90 | - | 100 | 0 |
| go-cache | 315.6 | - | 21 | 0 |
| Ristretto | 523.5 | - | 87 | 0 |

### Contention And Real-World Workloads

#### 80% Hot Keys / 60% Read Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 24.98 | - | 38 | 0 |
| FreeCache | 71.03 | - | 617 | 0 |
| BigCache | 76.80 | - | 573 | 1 |
| go-cache | 104.7 | - | 9 | 0 |
| Ristretto | 244.1 | - | 42 | 0 |

#### 60% Read / 35% Write / 5% Delete Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 27.83 | - | 33 | 0 |
| BigCache | 43.33 | - | 365 | 0 |
| FreeCache | 68.59 | - | 676 | 0 |
| go-cache | 110.4 | - | 8 | 0 |
| Ristretto | 209.4 | 0.01234 | 43 | 0 |

#### 60% Read / 35% Write / 5% Delete Strict

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 28.88 | - | 33 | 0 |
| BigCache | 51.18 | - | 365 | 0 |
| FreeCache | 70.18 | - | 692 | 0 |
| go-cache | 119.7 | - | 8 | 0 |
| Ristretto | 463.4 | - | 116 | 1 |

### Large Values

#### Async ns/op

| Size | Kioshun | Ristretto | BigCache | FreeCache | go-cache |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1KB | 32.03 | 345.1 | 47.95 | 52.50 | 130.9 |
| 4KB | 35.96 | 348.5 | 83.84 | 84.20 | 149.2 |
| 16KB | 31.78 | 217.5 | 157.3 | 182.3 | 128.7 |
| 64KB | 31.86 | 96.46 | 430.9 | 200.8 | 286.4 |

#### Strict ns/op

| Size | Kioshun | Ristretto | BigCache | FreeCache | go-cache |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1KB | 41.52 | 1060 | 55.16 | 51.80 | 276.4 |
| 4KB | 38.25 | 1033 | 84.14 | 81.03 | 274.7 |
| 16KB | 38.70 | 1036 | 172.4 | 162.5 | 282.8 |
| 64KB | 38.76 | 1021 | 432.7 | 207.8 | 288.7 |

Ristretto async large-value rows also reported write failures in this run:

| Size | Ristretto write_fail/op |
| ---: | ---: |
| 1KB | 0.04195 |
| 4KB | 0.04436 |
| 16KB | 0.2533 |
| 64KB | 0.5074 |
