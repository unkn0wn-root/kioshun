# Benchmark Results - Kioshun vs. Popular Go Caches

These benchmarks compare Kioshun against Ristretto, BigCache, FreeCache, and
go-cache using deterministic, pre-generated workloads. Lower `ns/op` is better.

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
metadata and policy cost.

## Running

```bash
# Cross-cache comparison
make bench-compare

# Direct command used for the latest tables below
cd benchmarks
GOCACHE=/tmp/kioshun-go-build go test -bench='BenchmarkCacheComparison' -benchmem -run=^$ -benchtime=1s .

# Kioshun-only stress and microbenchmarks
make bench
make stress-test
make bench-runner
```

## Latest Comparison Run

Environment:

- OS/arch: `linux/amd64`
- CPU: `Intel(R) Core(TM) i9-9980HK CPU @ 2.40GHz`
- Go: `go1.26.3-X:nodwarf5 linux/amd64`
- Command: `GOCACHE=/tmp/kioshun-go-build go test -bench='BenchmarkCacheComparison' -benchmem -run=^$ -benchtime=1s .`

### Core Operations

| Benchmark | Cache | ns/op | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: |
| Set async | Kioshun | 65.49 | 0 | 0 |
| Set async | FreeCache | 117.9 | 1 | 0 |
| Set async | BigCache | 181.5 | 105 | 0 |
| Set async | go-cache | 518.8 | 24 | 1 |
| Set async | Ristretto | 835.2 | 125 | 3 |
| Set strict | FreeCache | 118.8 | 1 | 0 |
| Set strict | Kioshun | 174.8 | 2 | 0 |
| Set strict | BigCache | 187.0 | 111 | 0 |
| Set strict | go-cache | 533.8 | 24 | 1 |
| Set strict | Ristretto | 2296 | 273 | 5 |
| Get TTL | Kioshun | 36.16 | 0 | 0 |
| Get TTL | Ristretto | 44.87 | 17 | 1 |
| Get TTL | go-cache | 81.00 | 0 | 0 |
| Get TTL | FreeCache | 194.0 | 1024 | 1 |
| Get TTL | BigCache | 235.0 | 1040 | 2 |
| Get no TTL | Kioshun | 34.05 | 0 | 0 |
| Get no TTL | Ristretto | 37.30 | 17 | 1 |
| Get no TTL | go-cache | 76.94 | 0 | 0 |
| Get no TTL | FreeCache | 209.6 | 1024 | 1 |
| Get no TTL | BigCache | 245.3 | 1040 | 2 |

### Mixed Workloads

| Benchmark | Cache | ns/op | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: |
| 70% read / 30% write async | Kioshun | 124.1 | 0 | 0 |
| 70% read / 30% write async | BigCache | 130.9 | 353 | 0 |
| 70% read / 30% write async | FreeCache | 190.0 | 718 | 0 |
| 70% read / 30% write async | go-cache | 404.3 | 7 | 0 |
| 70% read / 30% write async | Ristretto | 417.8 | 52 | 1 |
| 70% read / 30% write strict | Kioshun | 133.0 | 0 | 0 |
| 70% read / 30% write strict | BigCache | 134.2 | 347 | 0 |
| 70% read / 30% write strict | FreeCache | 181.8 | 718 | 0 |
| 70% read / 30% write strict | go-cache | 421.7 | 7 | 0 |
| 70% read / 30% write strict | Ristretto | 938.6 | 81 | 2 |
| 90% read / 10% write async | Kioshun | 83.26 | 0 | 0 |
| 90% read / 10% write async | BigCache | 115.8 | 430 | 0 |
| 90% read / 10% write async | Ristretto | 159.6 | 31 | 1 |
| 90% read / 10% write async | FreeCache | 216.8 | 919 | 0 |
| 90% read / 10% write async | go-cache | 420.0 | 2 | 0 |
| 10% read / 90% write async | Kioshun | 82.22 | 0 | 0 |
| 10% read / 90% write async | FreeCache | 122.2 | 100 | 0 |
| 10% read / 90% write async | BigCache | 209.9 | 188 | 1 |
| 10% read / 90% write async | go-cache | 479.3 | 21 | 0 |
| 10% read / 90% write async | Ristretto | 1027 | 110 | 2 |

### Contention And Real-World Workloads

| Benchmark | Cache | ns/op | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: |
| 80% hot keys / 60% read async | Kioshun | 94.89 | 0 | 0 |
| 80% hot keys / 60% read async | FreeCache | 122.9 | 617 | 0 |
| 80% hot keys / 60% read async | BigCache | 199.9 | 671 | 1 |
| 80% hot keys / 60% read async | go-cache | 272.2 | 9 | 0 |
| 80% hot keys / 60% read async | Ristretto | 448.2 | 62 | 1 |
| 60% read / 35% write / 5% delete async | Kioshun | 166.5 | 0 | 0 |
| 60% read / 35% write / 5% delete async | BigCache | 207.3 | 481 | 0 |
| 60% read / 35% write / 5% delete async | FreeCache | 218.4 | 693 | 0 |
| 60% read / 35% write / 5% delete async | go-cache | 405.5 | 8 | 0 |
| 60% read / 35% write / 5% delete async | Ristretto | 470.8 | 61 | 1 |
| 60% read / 35% write / 5% delete strict | Kioshun | 139.3 | 0 | 0 |
| 60% read / 35% write / 5% delete strict | BigCache | 200.0 | 462 | 0 |
| 60% read / 35% write / 5% delete strict | FreeCache | 221.7 | 694 | 0 |
| 60% read / 35% write / 5% delete strict | go-cache | 389.0 | 8 | 0 |
| 60% read / 35% write / 5% delete strict | Ristretto | 1085 | 95 | 2 |

### Large Values

| Benchmark | Size | Kioshun | Ristretto | BigCache | FreeCache | go-cache |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Async ns/op | 1KB | 95.04 | 688.3 | 178.6 | 111.6 | 422.3 |
| Async ns/op | 4KB | 97.77 | 668.3 | 570.4 | 407.9 | 387.6 |
| Async ns/op | 16KB | 99.13 | 439.1 | 2628 | 1398 | 402.7 |
| Async ns/op | 64KB | 95.43 | 153.0 | 8852 | 4452 | 412.3 |
| Strict ns/op | 1KB | 139.3 | 1619 | 181.3 | 108.9 | 395.0 |
| Strict ns/op | 4KB | 138.9 | 1580 | 563.1 | 428.7 | 401.0 |
| Strict ns/op | 16KB | 137.2 | 2327 | 2422 | 1598 | 413.0 |
| Strict ns/op | 64KB | 136.7 | 2245 | 8892 | 4475 | 398.6 |
