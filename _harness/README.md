# Cache Eviction Policy Test API

Test API for validating different cache eviction policies (LRU, LFU, FIFO, Random, SampledLFU) under various traffic conditions.

## Quick Start

```bash
# From the _harness directory
go run main.go -eviction=lru

# Or test different policies
go run main.go -eviction=lfu
go run main.go -eviction=fifo
go run main.go -eviction=random
go run main.go -eviction=sampledlfu

# API will start on http://localhost:8080
```

## Supported Eviction Policies

- **LRU** (Least Recently Used) - Default policy
- **LFU** (Least Frequently Used)
- **FIFO** (First In, First Out)
- **Random**
- **SampledLFU**

## API Endpoints

### Data Endpoints (Cached)
- `GET /api/data/{id}` - Basic cached data requests
- `GET /api/heavy/{id}` - Heavy computation requests (50ms + 10KB payload)
- `GET /api/popular/` - Popular content with Zipfian
- `GET /api/random/` - Random keys for cache pollution testing

### Monitoring Endpoints
- `GET /stats` - Performance statistics and response times
- `GET /cache-stats` - Detailed cache internals and memory usage
- `GET /health` - Health check endpoint

### Testing Endpoints
- `GET /load-test?duration=60s&concurrency=50` - Built-in load testing
- `POST /clear-cache` - Clear all cache entries

## Load Testing Scenarios

The API includes 5 test scenarios, each optimized for different eviction policies:

### 1. Hot Spot Access (80/20 Rule)
- **Purpose**: Tests cache's ability to keep frequently accessed hot data
- **Pattern**: 80% of requests target 20% of data
- **Best for**: LRU, LFU, SampledLFU
- **Validates**: Temporal and frequency-based locality

### 2. Cache Pollution Resistance
- **Purpose**: Tests cache's resistance to random access pattern pollution
- **Pattern**: 20% random keys, 80% legitimate recurring keys
- **Best for**: SampledLFU, LFU
- **Validates**: Admission control and pollution resistance

### 3. Heavy Computation Caching
- **Purpose**: Tests caching effectiveness for expensive operations with Zipfian
- **Pattern**: Zipfian with 50ms computation delay
- **Best for**: LRU, LFU, SampledLFU
- **Validates**: Cost-based caching benefits

### 4. Mixed Workload Stress
- **Purpose**: Mixed workload to test overall performance and stability
- **Pattern**: 100 concurrent clients with mixed endpoints
- **Best for**: All policies (general stress test)
- **Validates**: Overall system performance under stress

### 5. Burst Traffic Simulation
- **Purpose**: Tests cache behavior under bursty traffic patterns and trending content
- **Pattern**: Alternating burst and normal phases
- **Best for**: LRU, SampledLFU
- **Validates**: Temporal locality and burst handling

## Performance Monitoring

### Real-time Statistics
```bash
curl http://localhost:8080/stats
```

Returns:
- Request count and rates
- Cache hit/miss ratios
- Average response times
- Uptime and system status

### Cache Internals
```bash
curl http://localhost:8080/cache-stats
```

Returns:
- Policy-specific metrics
- Memory usage and goroutine count
- Eviction statistics
- Shard-level performance data

## Policy-Specific Configuration

```go
config := cache.Config{
    MaxSize:         50000,             // 50k items max capacity
    ShardCount:      32,                // Sharding
    CleanupInterval: 30 * time.Second,  // Regular cleanup
    DefaultTTL:      5 * time.Minute,   // 5min default TTL
    EvictionPolicy:  evictionPolicy,    // Specified via -eviction flag
    StatsEnabled:    true,              // Monitoring
}

// SampledLFU gets additional configuration
if evictionPolicy == cache.SampledLFU {
    config.AdmissionResetInterval = 30 * time.Second
}
```

## Usage Examples

### Compare All Policies
```bash
# Test each policy for 2 minutes and compare results
for policy in lru lfu fifo random sampledlfu; do
    echo "Testing $policy policy..."
    go run main.go -eviction=$policy &
    PID=$!
    sleep 5  # Let server start

    curl "http://localhost:8080/load-test?duration=120s&concurrency=50"
    curl http://localhost:8080/cache-stats > "${policy}_results.json"

    kill $PID
    sleep 2
done
```

### Policy-Specific Testing
```bash
# Start with LRU policy
go run main.go -eviction=lru

# Run policy-optimized scenarios
curl "http://localhost:8080/load-test?duration=60s&concurrency=30"

# Check results
curl http://localhost:8080/cache-stats
```

### Heavy Traffic Simulation
```bash
# Multiple concurrent clients hitting different patterns
for i in {1..10}; do
    curl "http://localhost:8080/api/data/item_$i" &
    curl "http://localhost:8080/api/heavy/compute_$((i%3))" &
    curl "http://localhost:8080/api/popular/" &
done
wait
```

### Cache Pollution Test (Best for SampledLFU/LFU)
```bash
# Generate random keys to test admission filter
for i in {1..100}; do
    curl "http://localhost:8080/api/random/" &
done
wait

# Check if legitimate keys are still cached
curl http://localhost:8080/cache-stats
```

## Configuration Tuning

You can configure these parameters based on your testing needs:

```go
// Cache size - balance between memory usage and hit ratio
MaxSize: 50000,

// Concurrency - match your CPU cores and expected load
ShardCount: 32,

// Admission control (SampledLFU only) - tune based on workload characteristics
AdmissionResetInterval: 30 * time.Second,
```

## Benchmarking

For detailed benchmarking, see the `_benchmarks/` directory in the main project, which includes:
