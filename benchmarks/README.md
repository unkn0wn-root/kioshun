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

# Or run the comparison module directly
cd benchmarks
GOCACHE=/tmp/kioshun-go-build go test -run=TestBenchmarkComparisonGetSetup -count=1 .
GOCACHE=/tmp/kioshun-go-build go test -bench='BenchmarkCacheComparison' -benchmem -run=^$ -benchtime=1s .
```

## Example Comparison Run

Environment:

- Date: `2026-06-02`
- OS/arch: `linux/amd64`
- CPU: `Intel(R) Core(TM) i9-9980HK CPU @ 2.40GHz`
- Go: `go1.26.3-X:nodwarf5 linux/amd64`
- Command: `GOCACHE=/tmp/kioshun-go-build go test -bench='BenchmarkCacheComparison' -benchmem -run=^$ -benchtime=1s .`

### Core Operations

| Benchmark | Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: | ---: |
| Set async | Kioshun | 57.75 | - | 0 | 0 |
| Set async | FreeCache | 95.26 | - | 1 | 0 |
| Set async | BigCache | 189.4 | - | 110 | 0 |
| Set async | go-cache | 510.7 | - | 24 | 1 |
| Set async | Ristretto | 702.7 | 0.2864 | 126 | 3 |
| Set strict | FreeCache | 111.8 | - | 1 | 0 |
| Set strict | Kioshun | 176.1 | - | 1 | 0 |
| Set strict | BigCache | 186.5 | - | 102 | 0 |
| Set strict | go-cache | 529.7 | - | 24 | 1 |
| Set strict | Ristretto | 2226 | - | 269 | 5 |
| Get TTL | Kioshun | 35.50 | - | 0 | 0 |
| Get TTL | Ristretto | 42.07 | - | 17 | 1 |
| Get TTL | go-cache | 77.51 | - | 0 | 0 |
| Get TTL | FreeCache | 193.2 | - | 1024 | 1 |
| Get TTL | BigCache | 212.9 | - | 1040 | 2 |
| Get no TTL | Kioshun | 31.18 | - | 0 | 0 |
| Get no TTL | Ristretto | 35.39 | - | 17 | 1 |
| Get no TTL | go-cache | 75.81 | - | 0 | 0 |
| Get no TTL | FreeCache | 179.6 | - | 1024 | 1 |
| Get no TTL | BigCache | 221.7 | - | 1040 | 2 |

### Mixed Workloads

| Benchmark | Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: | ---: |
| 70% read / 30% write async | Kioshun | 110.2 | - | 0 | 0 |
| 70% read / 30% write async | BigCache | 135.1 | - | 357 | 0 |
| 70% read / 30% write async | FreeCache | 183.4 | - | 718 | 0 |
| 70% read / 30% write async | Ristretto | 395.6 | - | 52 | 1 |
| 70% read / 30% write async | go-cache | 402.8 | - | 7 | 0 |
| 70% read / 30% write strict | Kioshun | 125.9 | - | 0 | 0 |
| 70% read / 30% write strict | BigCache | 140.7 | - | 355 | 0 |
| 70% read / 30% write strict | FreeCache | 180.2 | - | 717 | 0 |
| 70% read / 30% write strict | go-cache | 386.7 | - | 7 | 0 |
| 70% read / 30% write strict | Ristretto | 847.3 | - | 81 | 2 |
| 90% read / 10% write async | Kioshun | 84.28 | - | 0 | 0 |
| 90% read / 10% write async | BigCache | 102.7 | - | 389 | 0 |
| 90% read / 10% write async | Ristretto | 170.3 | - | 31 | 1 |
| 90% read / 10% write async | FreeCache | 206.3 | - | 919 | 0 |
| 90% read / 10% write async | go-cache | 395.3 | - | 2 | 0 |
| 10% read / 90% write async | Kioshun | 70.75 | - | 0 | 0 |
| 10% read / 90% write async | FreeCache | 99.93 | - | 100 | 0 |
| 10% read / 90% write async | BigCache | 187.4 | - | 178 | 1 |
| 10% read / 90% write async | go-cache | 466.9 | - | 21 | 0 |
| 10% read / 90% write async | Ristretto | 921.0 | - | 110 | 2 |

### Contention And Real-World Workloads

| Benchmark | Cache | ns/op | write_fail/op | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: | ---: |
| 80% hot keys / 60% read async | Kioshun | 93.49 | - | 0 | 0 |
| 80% hot keys / 60% read async | FreeCache | 116.9 | - | 617 | 0 |
| 80% hot keys / 60% read async | BigCache | 184.1 | - | 657 | 1 |
| 80% hot keys / 60% read async | go-cache | 268.3 | - | 9 | 0 |
| 80% hot keys / 60% read async | Ristretto | 432.0 | - | 62 | 1 |
| 60% read / 35% write / 5% delete async | Kioshun | 157.9 | - | 0 | 0 |
| 60% read / 35% write / 5% delete async | FreeCache | 186.3 | - | 694 | 0 |
| 60% read / 35% write / 5% delete async | BigCache | 201.7 | - | 484 | 0 |
| 60% read / 35% write / 5% delete async | go-cache | 400.1 | - | 8 | 0 |
| 60% read / 35% write / 5% delete async | Ristretto | 447.5 | 0.007828 | 61 | 1 |
| 60% read / 35% write / 5% delete strict | Kioshun | 131.6 | - | 0 | 0 |
| 60% read / 35% write / 5% delete strict | FreeCache | 192.7 | - | 694 | 0 |
| 60% read / 35% write / 5% delete strict | BigCache | 196.4 | - | 479 | 0 |
| 60% read / 35% write / 5% delete strict | go-cache | 409.5 | - | 8 | 0 |
| 60% read / 35% write / 5% delete strict | Ristretto | 1014 | - | 95 | 2 |

### Large Values

| Benchmark | Size | Kioshun | Ristretto | BigCache | FreeCache | go-cache |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Async ns/op | 1KB | 89.43 | 617.1 | 190.9 | 102.8 | 378.0 |
| Async ns/op | 4KB | 97.77 | 649.9 | 572.3 | 413.8 | 401.0 |
| Async ns/op | 16KB | 90.22 | 428.0 | 2485 | 1405 | 374.8 |
| Async ns/op | 64KB | 92.13 | 152.6 | 8506 | 4226 | 382.4 |
| Strict ns/op | 1KB | 135.4 | 1595 | 194.5 | 108.2 | 377.6 |
| Strict ns/op | 4KB | 132.7 | 1650 | 525.2 | 389.1 | 378.3 |
| Strict ns/op | 16KB | 131.2 | 2219 | 2644 | 1440 | 394.4 |
| Strict ns/op | 64KB | 140.3 | 2126 | 8080 | 4409 | 398.8 |

Ristretto async large-value rows also reported write failures in this run:

| Size | Ristretto write_fail/op |
| ---: | ---: |
| 1KB | 0.1028 |
| 4KB | 0.07453 |
| 16KB | 0.2719 |
| 64KB | 0.5295 |
