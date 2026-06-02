# Benchmark Methodology - Kioshun vs. Popular Go Caches

These benchmarks compare Kioshun against Ristretto, BigCache, FreeCache, and
go-cache using deterministic, pre-generated workloads. Lower `ns/op` is better
for a given workload, but these results should be treated as environment-specific
measurements, not universal cache rankings.

## Methodology

The comparison suite avoids timing key formatting and random generation:
keys, values, and operation streams are generated before each benchmark timer is
started. Cache candidates are not benchmarked concurrently with each other. Each
sub-benchmark owns one cache instance; only the operations inside that candidate
use `b.RunParallel` to measure concurrent cache access.

Async and strict write semantics are reported separately:

- **Async**: Kioshun uses `SetAsync`; Ristretto uses `SetWithTTL`; async caches are
  flushed outside the timed section.
- **Strict**: Kioshun uses `Set`; Ristretto calls `Wait` after each write;
  synchronous caches use their normal `Set`.

The harness reports a custom `write_fail/op` metric if a cache rejects writes
during the timed workload. Non-zero write failures mean `ns/op` is not accepted
write throughput by itself. The example tables include this metric where it was
non-zero.

The benchmark module also includes a setup sanity test for the GET comparison:
it prepopulates the documented keyspace and verifies every key is readable before
the GET benchmark can be trusted.

### Cache Configuration

| Cache | Configuration |
| --- | --- |
| Kioshun | `DefaultConfig()` base, `MaxSize=100000`, `DefaultTTL=1h`, `StatsEnabled=false`, default SieveTinyLFU |
| Ristretto | `NumCounters=1000000`, `MaxCost=256MiB`, `BufferItems=64`, `Metrics=false`, value byte length as cost |
| BigCache | `DefaultConfig(1h)`, default 1024 shards, `MaxEntriesInWindow=100000`, `HardMaxCacheSize=256MB`, `StatsEnabled=false` |
| FreeCache | `NewCache(256MiB)` |
| go-cache | `NewFrom(1h, 5m, make(map[string]gocache.Item, 100000))` |

For no-TTL lookups, caches use no expiration where supported. BigCache uses a
long life window because it does not expose per-entry no-expiration writes.
Large-value benchmarks reuse prebuilt `[]byte` values; caches that copy payloads
on API boundaries will show that cost, while pointer-storing caches mostly show
metadata and policy cost. Do not cite the large-value rows as an equal
"copies N KB per operation" comparison.

Kioshun's `MaxSize` is an entry count. Ristretto, BigCache, and FreeCache use
byte-oriented memory budgets in this suite. That is a deliberate practical
configuration, but memory-bounded large-value workloads can have different hit
rates and eviction pressure across candidates.

## Running

```bash
# From the repository root
make bench-compare
make bench
make stress-test
make bench-runner

# Or run the benchmark module directly
cd benchmarks
GOCACHE=/tmp/kioshun-go-build go test -run=TestBenchmarkComparisonGetSetup -count=1 .
GOCACHE=/tmp/kioshun-go-build go test -bench='BenchmarkCacheComparison' -benchmem -run=^$ -benchtime=1s .
GOCACHE=/tmp/kioshun-go-build go test -bench=. -benchmem -run=^$ -benchtime=1s -timeout=30m .
```

## Example Full Suite Run

Environment:

- Date: `2026-06-02`
- OS/arch: `darwin/arm64`
- OS version: `macOS 26.5 (Darwin 25.5.0)`
- CPU: `Apple M4 Max`
- Logical CPUs: `14`
- Memory: `36 GiB`
- Go: `go1.26.0 darwin/arm64`
- Setup sanity check: `GOCACHE=/tmp/kioshun-go-build go test -run=TestBenchmarkComparisonGetSetup -count=1 .`
- Full suite command: `GOCACHE=/tmp/kioshun-go-build go test -bench=. -benchmem -run=^$ -benchtime=1s -timeout=30m .`
- Full suite duration: `278.633s`

### Core Operations

#### Set Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 40.44 | - | 0 | 0 |
| FreeCache | 70.51 | - | 0 | 0 |
| BigCache | 73.29 | - | 50 | 0 |
| go-cache | 318.9 | - | 24 | 1 |
| Ristretto | 473.6 | 0.2005 | 125 | 3 |

#### Set Strict

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 67.31 | - | 0 | 0 |
| FreeCache | 68.43 | - | 0 | 0 |
| BigCache | 77.32 | - | 50 | 0 |
| go-cache | 308.4 | - | 24 | 1 |
| Ristretto | 1290 | - | 253 | 5 |

#### Get TTL

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 23.53 | - | 0 | 0 |
| Ristretto | 27.45 | - | 16 | 1 |
| FreeCache | 90.20 | - | 1024 | 1 |
| BigCache | 108.8 | - | 1040 | 2 |
| go-cache | 111.8 | - | 0 | 0 |

#### Get No TTL

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 15.26 | - | 0 | 0 |
| Ristretto | 25.69 | - | 16 | 1 |
| FreeCache | 84.31 | - | 1024 | 1 |
| go-cache | 96.29 | - | 0 | 0 |
| BigCache | 113.9 | - | 1040 | 2 |

### Mixed Workloads

#### 70% Read / 30% Write Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 44.70 | - | 0 | 0 |
| BigCache | 53.15 | - | 275 | 0 |
| FreeCache | 80.68 | - | 718 | 0 |
| go-cache | 149.9 | - | 7 | 0 |
| Ristretto | 239.1 | - | 52 | 1 |

#### 70% Read / 30% Write Strict

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| BigCache | 46.18 | - | 279 | 0 |
| Kioshun | 56.16 | - | 0 | 0 |
| FreeCache | 84.71 | - | 718 | 0 |
| go-cache | 133.0 | - | 7 | 0 |
| Ristretto | 481.6 | - | 81 | 2 |

#### 90% Read / 10% Write Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 31.74 | - | 0 | 0 |
| BigCache | 34.47 | - | 197 | 0 |
| FreeCache | 87.63 | - | 919 | 0 |
| Ristretto | 95.90 | - | 32 | 1 |
| go-cache | 154.0 | - | 2 | 0 |

#### 10% Read / 90% Write Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 36.86 | - | 0 | 0 |
| FreeCache | 69.08 | - | 100 | 0 |
| BigCache | 72.51 | - | 119 | 1 |
| go-cache | 339.3 | - | 21 | 0 |
| Ristretto | 547.8 | - | 110 | 2 |

### Contention And Real-World Workloads

#### 80% Hot Keys / 60% Read Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Kioshun | 44.44 | - | 0 | 0 |
| FreeCache | 85.22 | - | 617 | 0 |
| BigCache | 97.05 | - | 591 | 1 |
| go-cache | 123.0 | - | 9 | 0 |
| Ristretto | 268.3 | - | 62 | 1 |

#### 60% Read / 35% Write / 5% Delete Async

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| BigCache | 63.24 | - | 379 | 0 |
| Kioshun | 80.47 | - | 0 | 0 |
| FreeCache | 82.02 | - | 677 | 0 |
| go-cache | 153.0 | - | 8 | 0 |
| Ristretto | 250.8 | 0.01092 | 61 | 1 |

#### 60% Read / 35% Write / 5% Delete Strict

| Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| BigCache | 56.28 | - | 379 | 0 |
| Kioshun | 56.33 | - | 0 | 0 |
| FreeCache | 81.81 | - | 686 | 0 |
| go-cache | 147.0 | - | 8 | 0 |
| Ristretto | 593.6 | - | 95 | 2 |

### Large Values

#### Async ns/op

| Size | Kioshun | Ristretto | BigCache | FreeCache | go-cache |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1KB | 40.50 | 362.9 | 63.69 | 65.10 | 180.0 |
| 4KB | 40.75 | 380.6 | 122.9 | 104.2 | 185.5 |
| 16KB | 42.17 | 246.6 | 186.3 | 215.8 | 198.8 |
| 64KB | 37.48 | 117.3 | 459.9 | 230.8 | 252.8 |

#### Strict ns/op

| Size | Kioshun | Ristretto | BigCache | FreeCache | go-cache |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1KB | 54.86 | 1088 | 64.48 | 61.77 | 244.2 |
| 4KB | 50.42 | 1095 | 114.5 | 95.80 | 247.5 |
| 16KB | 51.42 | 1166 | 208.3 | 205.0 | 241.1 |
| 64KB | 53.12 | 1156 | 496.3 | 230.1 | 216.8 |

Ristretto async large-value rows also reported write failures in this run:

| Size | Ristretto write_fail/op |
| ---: | ---: |
| 1KB | 0.05954 |
| 4KB | 0.04296 |
| 16KB | 0.2543 |
| 64KB | 0.5042 |
