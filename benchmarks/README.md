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

- Date: `2026-06-11`
- OS/arch: `darwin/arm64`
- OS version: `macOS 26.5.1 (Darwin 25.5.0)`
- CPU: `Apple M4 Max`
- Logical CPUs: `14`
- Memory: `36 GiB`
- Go: `go1.26.0 darwin/arm64`
- Setup sanity check: `go test -run=TestBenchmarkComparisonGetSetup -count=1 .`
- Full suite command: `go test -bench=. -benchmem -run=^$ -benchtime=1s -timeout=30m .`
- Full suite duration: `265.001s`

### Core Operations

#### Set Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 45.37 | - | 96 | 1 |
| FreeCache | 55.43 | - | 0 | 0 |
| BigCache | 59.89 | - | 41 | 0 |
| go-cache | 277.1 | - | 24 | 1 |
| Ristretto | 542.0 | 0.1363 | 100 | 1 |

#### Set Strict

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| FreeCache | 56.41 | - | 0 | 0 |
| Kioshun | 57.99 | - | 96 | 1 |
| BigCache | 61.98 | - | 43 | 0 |
| go-cache | 320.5 | - | 24 | 1 |
| Ristretto | 1276 | - | 333 | 3 |

#### Get TTL

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 6.194 | - | 0 | 0 |
| Ristretto | 21.49 | - | 0 | 0 |
| FreeCache | 75.59 | - | 1024 | 1 |
| BigCache | 91.09 | - | 1040 | 2 |
| go-cache | 135.8 | - | 0 | 0 |

#### Get No TTL

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 5.285 | - | 0 | 0 |
| Ristretto | 16.51 | - | 0 | 0 |
| FreeCache | 72.97 | - | 1024 | 1 |
| BigCache | 110.8 | - | 1040 | 2 |
| go-cache | 130.5 | - | 0 | 0 |

### Mixed Workloads

#### 70% Read / 30% Write Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 18.14 | - | 28 | 0 |
| BigCache | 40.54 | - | 271 | 0 |
| FreeCache | 71.40 | - | 718 | 0 |
| go-cache | 149.3 | - | 7 | 0 |
| Ristretto | 208.0 | - | 34 | 0 |

#### 70% Read / 30% Write Strict

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 35.75 | - | 28 | 0 |
| BigCache | 39.38 | - | 272 | 0 |
| FreeCache | 69.70 | - | 718 | 0 |
| go-cache | 116.9 | - | 7 | 0 |
| Ristretto | 422.2 | - | 96 | 0 |

#### 90% Read / 10% Write Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 11.99 | - | 9 | 0 |
| BigCache | 26.51 | - | 194 | 0 |
| Ristretto | 77.52 | - | 15 | 0 |
| FreeCache | 77.72 | - | 919 | 0 |
| go-cache | 130.2 | - | 2 | 0 |

#### 10% Read / 90% Write Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 34.84 | - | 86 | 0 |
| FreeCache | 58.02 | - | 100 | 0 |
| BigCache | 77.48 | - | 117 | 1 |
| go-cache | 334.8 | - | 21 | 0 |
| Ristretto | 533.1 | - | 88 | 0 |

### Contention And Real-World Workloads

#### 80% Hot Keys / 60% Read Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 20.57 | - | 38 | 0 |
| BigCache | 73.18 | - | 583 | 1 |
| FreeCache | 73.20 | - | 617 | 0 |
| go-cache | 124.0 | - | 9 | 0 |
| Ristretto | 248.3 | - | 42 | 0 |

#### 60% Read / 35% Write / 5% Delete Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 27.64 | - | 33 | 0 |
| BigCache | 46.66 | - | 371 | 0 |
| FreeCache | 66.80 | - | 655 | 0 |
| go-cache | 118.8 | - | 8 | 0 |
| Ristretto | 217.4 | 0.01253 | 43 | 0 |

#### 60% Read / 35% Write / 5% Delete Strict

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 31.03 | - | 33 | 0 |
| BigCache | 46.92 | - | 373 | 0 |
| FreeCache | 68.54 | - | 652 | 0 |
| go-cache | 118.7 | - | 8 | 0 |
| Ristretto | 521.3 | - | 116 | 1 |

### Large Values

#### Async ns/op

| Size | Kioshun | Ristretto | BigCache | FreeCache | go-cache |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1KB | 31.40 | 365.4 | 71.66 | 55.04 | 160.2 |
| 4KB | 33.10 | 353.8 | 94.21 | 89.82 | 170.7 |
| 16KB | 32.12 | 222.2 | 172.7 | 179.6 | 161.8 |
| 64KB | 32.64 | 103.4 | 452.3 | 216.9 | 303.5 |

#### Strict ns/op

| Size | Kioshun | Ristretto | BigCache | FreeCache | go-cache |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1KB | 45.56 | 1096 | 59.33 | 54.69 | 301.9 |
| 4KB | 45.27 | 1108 | 94.38 | 93.36 | 276.0 |
| 16KB | 45.39 | 1264 | 161.1 | 177.3 | 300.3 |
| 64KB | 44.41 | 1168 | 430.9 | 208.6 | 301.3 |

Ristretto async large-value rows also reported write failures in this run:

| Size | Ristretto write_fail/op |
| ---: | ---: |
| 1KB | 0.04517 |
| 4KB | 0.04072 |
| 16KB | 0.2503 |
| 64KB | 0.5061 |
