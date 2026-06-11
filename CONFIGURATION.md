# Configuration

`Config` controls capacity, sharding, eviction, TTLs and the async write
pipeline. `New` validates the config and fills unset write pipeline fields
with defaults; `DefaultConfig()` is the recommended starting point (see the
README for the quick-start forms).

## All fields

```go
config := kioshun.Config{
    MaxSize:         100000,               // Maximum number of items
    MaxCost:         0,                    // Optional weighted capacity budget
    ShardCount:      16,                   // Number of shards (0 = auto-detect)
    CleanupInterval: 5 * time.Minute,      // Cleanup frequency
    DefaultTTL:      30 * time.Minute,     // Default expiration time
    EvictionPolicy:  kioshun.SieveTinyLFU, // Eviction algorithm (default)
    // StatsEnabled records cache activity metrics such as hits, misses and
    // evictions. Tracking those counters adds runtime cost so enable it only
    // if you can accept runtime performance tradeoff.
    StatsEnabled:    true,
    WriteBufferSize: 1024,                 // Per-shard async write queue
    WriteBatchSize:  64,                   // Max commands applied per worker batch
}

c, err := kioshun.New[string, any](config)
if err != nil {
    // handle error
}
```

> Each cache runs one write-worker goroutine per shard (plus a cleanup goroutine when `CleanupInterval > 0`),
> so `ShardCount` sets the number of background goroutines - default `min(NumCPU*4, 256)`.
> If you create many caches (e.g. via the cache `Manager`), set `ShardCount` explicitly to bound the total.

`ProbationRatio` and `GhostRatio` set the initial SieveTinyLFU probation
window and ghost size as a percent of capacity. The policy resizes both at
runtime, so leave them at zero unless you are experimenting.

## Weighted Capacity

`MaxSize` limits resident entry count. `MaxCost` optionally adds a weighted
resident budget. Without a custom weigher every entry costs `1`; with
`WithWeigher`, the cache can enforce byte-like or domain-specific weights.

```go
config := kioshun.DefaultConfig()
config.MaxSize = 100000
config.MaxCost = 256 << 20
config.CostAdmission = kioshun.CostAdmissionBalanced

c, err := kioshun.New[string, []byte](config, kioshun.WithWeigher(
    func(_ string, value []byte) int64 {
        return int64(len(value))
    },
))
```

`CostAdmission` selects how SieveTinyLFU compares a weighted candidate against
the eviction victim:

- `CostAdmissionFrequency` (default): compares frequency only.
- `CostAdmissionBalanced`: compares frequency divided by `sqrt(cost)`, a middle
  ground between request-hit and byte-hit objectives.
- `CostAdmissionDensity`: compares frequency divided by cost, favoring dense
  hot entries.

## Named caches

`Manager` owns named cache instances, so packages can share a cache by name
without passing it around:

```go
manager := kioshun.NewManager()

if err := manager.Register("users", config); err != nil {
    // handle error
}

c, err := kioshun.GetCache[string, User](manager, "users")
```

- `Register` binds a name to a configuration. Registering a name twice returns
  `ErrCacheExists`.
- `RegisterCache` registers a typed factory instead, for caches that need
  typed options such as `WithWeigher`, `WithOnRemove` or `WithOnEvict`.
- `GetCache` creates the cache from its registered configuration on first use.
  An unregistered name returns `ErrCacheNotRegistered`, so a misspelled name
  fails instead of silently creating a default cache.
- `GetCacheWithConfig` is the explicit get-or-create form: it creates the
  cache from the supplied config when the name does not exist yet, and returns
  the existing instance (ignoring the config) when it does.
- Names are typed: asking for an existing name with different `K, V` returns
  `ErrTypeMismatch`.
- `Remove` closes the named cache and drops its registration. `CloseAll`
  closes every instance but keeps registrations, so the same names can be
  recreated.

The package-level helpers (`RegisterGlobalCache`, `RegisterGlobalTypedCache`,
`GetGlobalCache`, `GetGlobalCacheWithConfig`, `GetGlobalCacheStats`,
`CloseAllGlobalCaches`) are the same operations on one process-wide manager.
Caches created through it live for the lifetime of the process unless released
with `CloseAllGlobalCaches`.
