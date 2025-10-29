# Benchmark Results - Kioshun vs. Ristretto, go-cache and freecache

## Benchmark Configuration

The benchmarks compare **Kioshun** with **AdmissionLFU** eviction policy against other popular Go cache libraries:

### Cache Configurations Used

| Cache Library | Configuration | Notes |
|---------------|---------------|-------|
| **Kioshun** | MaxSize: 100,000<br>ShardCount: CPU cores × 4<br>EvictionPolicy: **AdmissionLFU**<br>DefaultTTL: 1 hour<br>CleanupInterval: 5 min | AdmissionLFU eviction policy with admission control |
| **Ristretto** | NumCounters: 1,000,000<br>MaxCost: 100,000<br>BufferItems: 64 | TinyLFU-based admission policy |
| **BigCache** | MaxEntriesInWindow: 100,000<br>Shards: CPU cores (power of 2)<br>MaxEntrySize: 64KB<br>HardMaxCacheSize: 256MB | No eviction policy, size-based |
| **FreeCache** | Size: 128MB | Segmented LRU |
| **go-cache** | DefaultExpiration: 1 hour<br>CleanupInterval: 5 min | Simple map-based with cleanup |

**Test Environment (latest run):**
- **CPU:** Apple M4 Max (arm64)
- **OS:** macOS (Darwin arm64)
- **Go Version:** 1.24.7
- **Benchmark knobs:** `go test -bench … -benchmem`, 16-way parallelism, `-benchtime` 5s (core workloads) / 3s (stress suites)
- **Kioshun config:** AdmissionLFU, `ShardCount = runtime.NumCPU() * 4` (64 shards), `MaxSize = 100 000`

## Running Benchmarks

```bash
# Run comparison benchmarks
make bench-compare

# Run stress tests
make stress-test

# Run all benchmarks with the benchmark runner
make bench-runner

# Run all benchmark tests
make bench
```

## Core Operations

### SET Operations
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 100,000,000 | 75.55 | 41 | 3 |
| **FreeCache** | 81,768,051 | 74.19 | 24 | 1 |
| **Ristretto** | 58,714,996 | 90.86 | 154 | 5 |
| **BigCache** | 37,852,590 | 151.5 | 40 | 2 |
| **go-cache** | 19,841,619 | 341.0 | 57 | 3 |

### GET Operations
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Ristretto** | 244,472,186 | 23.09 | 31 | 2 |
| **Kioshun** | 239,967,180 | 25.87 | 31 | 2 |
| **FreeCache** | 77,851,767 | 79.62 | 1,039 | 2 |
| **BigCache** | 76,458,728 | 76.81 | 1,047 | 3 |
| **go-cache** | 44,541,900 | 136.6 | 15 | 1 |

## Workload-Specific

### Mixed Operations (70% reads, 30% writes)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 114,716,242 | 51.47 | 31 | 2 |
| **Ristretto** | 96,006,397 | 62.33 | 69 | 3 |
| **FreeCache** | 80,013,957 | 73.54 | 732 | 2 |
| **BigCache** | 38,290,142 | 150.0 | 742 | 3 |
| **go-cache** | 30,545,562 | 200.3 | 22 | 2 |

### High Contention Scenarios
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 85,443,963 | 77.03 | 34 | 2 |
| **FreeCache** | 68,861,860 | 87.68 | 554 | 1 |
| **BigCache** | 36,476,380 | 154.0 | 568 | 2 |
| **go-cache** | 29,068,076 | 228.2 | 33 | 1 |
| **Ristretto** | 27,175,748 | 223.5 | 83 | 3 |

### Read-Heavy Workloads (90% reads, 10% writes)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Ristretto** | 101,089,580 | 33.34 | 45 | 3 |
| **Kioshun** | 97,650,378 | 39.92 | 31 | 2 |
| **FreeCache** | 46,611,218 | 76.60 | 937 | 2 |
| **BigCache** | 26,093,739 | 132.6 | 946 | 3 |
| **go-cache** | 19,943,032 | 180.8 | 18 | 2 |

### Write-Heavy Workloads (90% writes, 10% reads)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 96,439,025 | 36.25 | 31 | 2 |
| **FreeCache** | 52,917,732 | 66.29 | 118 | 2 |
| **Ristretto** | 22,717,962 | 147.7 | 133 | 5 |
| **BigCache** | 21,079,129 | 167.2 | 133 | 3 |
| **go-cache** | 14,755,354 | 231.2 | 37 | 2 |

## Simulate 'Real-World' Workflow

### Real-World Workload Simulation
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 53,742,550 | 65.25 | 48 | 3 |
| **FreeCache** | 44,717,696 | 85.09 | 738 | 2 |
| **Ristretto** | 29,713,388 | 112.0 | 96 | 3 |
| **BigCache** | 21,115,576 | 185.9 | 818 | 3 |
| **go-cache** | 16,147,178 | 230.7 | 40 | 2 |

### Memory Efficiency
| Cache Library | Ops/sec | bytes/op |
|---------------|---------|----------|
| **Kioshun** | 45,916,828 | **40.0** |
| Value size sweep (1–64 KB) held steady at ~67–70 ns/op with 40 B/op and 2 allocs/op.

## Performance Characteristics (Kioshun AdmissionLFU)

- ~36–77 ns/op on write-heavy or high-contention microbenchmarks, ~26 ns/op on pure GETs
- ~53 M ops/sec in the mixed “real-world” pattern (65 ns/op average)
- Peak GET throughput observed: ~232 M ops/sec (26 ns/op)

## Stress Test Results

### High Load Scenarios
| Load Profile | Ops/sec | ns/op | B/op | allocs/op | Description |
|-------------|---------|-------|------|-----------|-------------|
| **Small + High Concurrency** | 55,777,849 | 61.52 | 27 | 2 | Many goroutines, small cache |
| **Medium + Mixed Load** | 53,624,493 | 66.63 | 31 | 2 | Balanced read/write operations |
| **Large + Read Heavy** | 64,021,102 | 55.19 | 38 | 2 | Large cache, mostly reads |
| **XLarge + Write Heavy** | 40,838,030 | 80.50 | 40 | 3 | Very large cache, mostly writes |
| **Extreme + Balanced** | 42,276,840 | 85.33 | 40 | 3 | Maximum scale, balanced ops |

### Advanced Stress Test Results

#### Contention Stress Test
| Test | Ops/sec | ns/op | B/op | allocs/op |
|------|---------|-------|------|-----------|
| **High Contention** | 40,442,905 | 83.94 | 34 | 2 |

#### Eviction Policy Performance
| Eviction Policy | Ops/sec | ns/op | B/op | allocs/op |
|-----------------|---------|-------|------|-----------|
| **FIFO** | **42,899,701** | 82.10 | 46 | 3 |
| **AdmissionLFU** | 41,337,319 | 177.0 | 59 | 3 |
| **LRU** | 31,638,396 | 153.1 | 57 | 3 |
| **LFU** | 24,112,208 | 194.8 | 57 | 3 |

#### Memory Pressure Tests
| Value Size | Ops/sec | ns/op | B/op | allocs/op |
|------------|---------|-------|------|-----------|
| **1KB** | 45,916,828 | 69.71 | 40 | 2 |
| **4KB** | 58,272,031 | 68.42 | 40 | 2 |
| **16KB** | 55,135,164 | 68.89 | 40 | 2 |
| **64KB** | 57,774,400 | 67.40 | 40 | 2 |

#### Sharding Efficiency Analysis
| Shards | Ops/sec | ns/op | B/op | allocs/op |
|--------|---------|-------|------|-----------|
| **1** | 15,451,604 | 341.2 | 45 | 3 |
| **2** | 15,700,284 | 299.6 | 44 | 3 |
| **4** | 20,301,433 | 205.3 | 45 | 3 |
| **8** | 27,256,491 | 145.4 | 45 | 3 |
| **16** | 35,702,301 | 115.4 | 46 | 3 |
| **32** | 41,248,432 | 91.13 | 46 | 3 |
| **64** | 53,728,068 | 76.04 | 47 | 3 |
| **128** | 66,081,164 | **62.31** | 47 | 3 |
