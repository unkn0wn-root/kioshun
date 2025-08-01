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

**Test Environment:**
- **CPU:** Apple M4 Max (14 cores)
- **OS:** macOS (Darwin ARM64)
- **Kioshun Shards:** 56 (14 × 4)
- **Go Version:** 1.21+

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
| **Kioshun** | 100,000,000 | **61.19** | 42 | 3 |
| **Ristretto** | 82,496,528 | 81.48 | 151 | 5 |
| **BigCache** | 37,729,303 | 153.9 | 40 | 2 |
| **go-cache** | 19,802,497 | 352.3 | 57 | 3 |
| **freecache** | 12,485,425 | 540.2 | 920 | 2 |

### GET Operations
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Ristretto** | 310,158,865 | **19.35** | 31 | 2 |
| **Kioshun** | 258,173,943 | **23.15** | 31 | 2 |
| **BigCache** | 92,523,937 | 82.81 | 1047 | 3 |
| **freecache** | 81,388,165 | 76.40 | 1039 | 2 |
| **go-cache** | 45,881,512 | 132.9 | 15 | 1 |

## Workload-Specific

### Mixed Operations (70% reads, 30% writes)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 100,000,000 | **49.68** | 31 | 2 |
| **Ristretto** | 85,088,882 | 60.14 | 69 | 3 |
| **BigCache** | 40,550,335 | 149.4 | 742 | 3 |
| **go-cache** | 30,110,391 | 200.5 | 22 | 2 |
| **freecache** | 22,155,008 | 266.9 | 1039 | 2 |

### High Contention Scenarios
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 83,795,762 | **73.72** | 35 | 2 |
| **BigCache** | 35,587,495 | 155.3 | 570 | 2 |
| **go-cache** | 29,478,771 | 233.1 | 33 | 1 |
| **Ristretto** | 27,075,663 | 228.9 | 83 | 3 |
| **freecache** | 19,292,937 | 306.2 | 918 | 2 |

### Read-Heavy Workloads (90% reads, 10% writes)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Ristretto** | 109,256,152 | **32.95** | 45 | 3 |
| **Kioshun** | 87,788,641 | **36.73** | 31 | 2 |
| **BigCache** | 26,907,688 | 130.0 | 946 | 3 |
| **freecache** | 22,986,230 | 156.4 | 1039 | 2 |
| **go-cache** | 19,272,679 | 185.3 | 18 | 2 |

### Write-Heavy Workloads (90% writes, 10% reads)
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 91,341,868 | **38.24** | 31 | 2 |
| **Ristretto** | 25,319,823 | 167.6 | 132 | 5 |
| **BigCache** | 20,838,819 | 169.9 | 133 | 3 |
| **go-cache** | 12,607,954 | 275.5 | 37 | 2 |
| **freecache** | 8,449,123 | 376.2 | 1039 | 2 |

## Simulate 'Real-World' Workflow

### Real-World Workload Simulation
| Cache Library | Ops/sec | ns/op | B/op | allocs/op |
|---------------|---------|-------|------|-----------|
| **Kioshun** | 56,552,866 | **64.99** | 48 | 3 |
| **Ristretto** | 27,220,159 | 121.7 | 97 | 3 |
| **BigCache** | 20,555,629 | 180.3 | 812 | 3 |
| **go-cache** | 16,119,222 | 229.1 | 40 | 2 |
| **freecache** | 8,877,139 | 375.4 | 1163 | 2 |

### Memory Efficiency
| Cache Library | Ops/sec | ns/op | bytes/op |
|---------------|---------|-------|----------|
| **Kioshun** | 89,788,641 | **36.73** | **31.0** |
| **Ristretto** | 109,256,152 | 32.95 | 45.0 |
| **BigCache** | 26,907,688 | 130.0 | 946.0 |
| **freecache** | 22,986,230 | 156.4 | 1039.0 |
| **go-cache** | 19,272,679 | 185.3 | 18.0 |

## Performance Characteristics

- **~19-87ns per operation** for most cache operations using AdmissionLFU
- **56+ million operations/sec** throughput in real-world scenarios
- **Peak throughput**: 310M+ operations/sec for GET operations

## Stress Test Results

### High Load Scenarios
| Load Profile | Ops/sec | ns/op | B/op | allocs/op | Description |
|-------------|---------|-------|------|-----------|-------------|
| **Small + High Concurrency** | 56,066,578 | **59.72** | 27 | 2 | Many goroutines, small cache |
| **Medium + Mixed Load** | 55,117,999 | **62.48** | 31 | 2 | Balanced read/write operations |
| **Large + Read Heavy** | 63,296,053 | **51.89** | 38 | 2 | Large cache, mostly reads |
| **XLarge + Write Heavy** | 43,971,300 | **67.03** | 40 | 3 | Very large cache, mostly writes |
| **Extreme + Balanced** | 43,159,774 | **81.41** | 41 | 3 | Maximum scale, balanced ops |

### Advanced Stress Test Results

#### Contention Stress Test
| Test | Ops/sec | ns/op | B/op | allocs/op |
|------|---------|-------|------|-----------|
| **High Contention** | 47,936,589 | **73.58** | 34 | 2 |

#### Eviction Policy Performance
| Eviction Policy | Ops/sec | ns/op | B/op | allocs/op |
|-----------------|---------|-------|------|-----------|
| **AdmissionLFU** | 35,685,565 | **124.7** | 53 | 3 |
| **FIFO** | 42,240,423 | 146.4 | 56 | 3 |
| **LRU** | 36,104,913 | 165.0 | 59 | 3 |
| **LFU** | 24,205,524 | 195.0 | 58 | 3 |

#### Memory Pressure Tests
| Value Size | Ops/sec | ns/op | B/op | allocs/op |
|------------|---------|-------|------|-----------|
| **1KB** | 53,634,813 | **71.20** | 40 | 3 |
| **4KB** | 52,830,013 | **71.18** | 40 | 3 |
| **16KB** | 53,889,962 | **70.94** | 40 | 3 |
| **64KB** | 54,438,056 | **71.44** | 40 | 3 |

#### Sharding Efficiency Analysis
| Shards | Ops/sec | ns/op | B/op | allocs/op |
|--------|---------|-------|------|-----------|
| **1** | 15,326,596 | 352.5 | 49 | 3 |
| **2** | 15,610,903 | 310.2 | 48 | 3 |
| **4** | 19,235,764 | 212.7 | 46 | 3 |
| **8** | 25,790,181 | 157.0 | 47 | 3 |
| **16** | 33,085,953 | 119.4 | 47 | 3 |
| **32** | 41,616,372 | 95.76 | 48 | 3 |
| **64** | 50,870,149 | 78.81 | 48 | 3 |
| **128** | 63,269,726 | **62.85** | 49 | 3 |