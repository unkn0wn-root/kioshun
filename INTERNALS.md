# Internals

## Package Layout

The root `kioshun` package implements a generic in-memory cache:

- `cache.go`: public cache API, configuration, sharding, reads, stats, cleanup.
- `writes.go`: queued writes, synchronous mutation ordering, barriers, workers.
- `shard.go`: per-shard state, list maintenance, item removal, TTL cleanup.
- `eviction.go`: LRU, LFU, and FIFO eviction adapters.
- `lfu.go`: exact LFU frequency bucket list.
- `sieve.go`, `sketch.go`, `ghost.go`, `read_buffer.go`: SieveTinyLFU policy.
- `queue.go`: bounded per-shard MPSC write queue.
- `hash.go`, `fnv.go`: type-specialized hashing.
- `manager.go`: named cache registry and global manager helpers.

The `httpcache` package builds HTTP response caching middleware on top of the
root cache:

- `httpcache/config.go`: HTTP middleware configuration and default resolution.
- `httpcache/middleware.go`: handler wrapping, response capture, policies, keys.
- `httpcache/pattern.go`: path-segment trie indexing cached keys for pattern
  invalidation, kept in sync by the cache's eviction listener.

## Core Data Model

`Cache[K, V]` is a sharded cache. Each shard owns:

- `data map[K]*cacheItem[K,V]`
- one `sync.RWMutex`
- a bounded write queue and one worker goroutine
- an LRU-style intrusive list for LRU, FIFO, and general item unlinking
- an LFU bucket list when `EvictionPolicy == LFU`
- SieveTinyLFU policy state when `EvictionPolicy == SieveTinyLFU` and the shard is bounded
- per-shard stats counters

`cacheItem` stores the value and all policy metadata in one pooled object:

- `expireTime`: absolute Unix nanoseconds; `0` means no expiration.
- `lastAccess`: updated by LRU/LFU/Sieve reads and writes where relevant.
- `prev` and `next`: intrusive list pointers.
- `lfuFreq`: exact LFU counter for the LFU policy.
- `sieveQ`, `queue`, `reuse`, `visited`: SieveTinyLFU queue and SIEVE metadata.
- `hash` and `tag`: cached hash and compact non-zero tag for ghost entries.
- `key`: original map key so eviction can delete from `data`.

Items and synchronous write waiters are reused through `sync.Pool`. Releasing an
item resets the whole struct before it returns to the pool.

## Configuration

`DefaultConfig()` uses:

- `MaxSize = 10000`
- `ShardCount = 0`, meaning auto-detect
- `CleanupInterval = 5 * time.Minute`
- `DefaultTTL = 30 * time.Minute`
- `EvictionPolicy = SieveTinyLFU`
- `StatsEnabled = true`
- `ProbationRatio = 10`
- `GhostRatio = 100`
- `Adapt = true`
- `WriteBufferSize = 1024`
- `WriteBatchSize = 64`

`New` validates the config, resolves `DefaultEvictionPolicy` to
`SieveTinyLFU`, and fills zero write queue settings with their defaults.

Shard count is rounded to a power of two because shard lookup is
`hash & shardMask`. When `ShardCount <= 0`, the cache starts with
`runtime.NumCPU() * 4`, capped at 256. For bounded caches, the shard count is
also capped so small `MaxSize` values do not create zero-capacity shards. The
total capacity is distributed exactly: each shard gets `MaxSize / shards`, and
the first `MaxSize % shards` shards get one extra slot.

`MaxSize == 0` means unlimited capacity. In that mode SieveTinyLFU policy state
is not allocated, because no eviction or admission decision is needed.

## Hashing

`newHasher[K]` selects a hash function once per cache type:

- strings of length `<= 8` use FNV-1a
- longer strings use the local `xxHash64(seed=0)` implementation
- integer-like keys are read directly and avalanched with xxHash64's finalizer
- other comparable keys fall back to `fmt.Sprintf("%v", key)` and hash that string

The hash selects the shard by `hash & shardMask`. The map still stores full keys,
so hash collisions affect distribution only, not key equality.

SieveTinyLFU ghost entries use `(hash, tag)`, where `tag` is a non-zero `uint16`
derived from an avalanched salted hash. The tag reduces false ghost hits without
storing full keys in the ghost queue.

## TTL And Expiration

`Set` and `SetAsync` translate TTLs before enqueueing:

- `DefaultExpiration` (`0`) uses `Config.DefaultTTL`
- positive TTLs become an absolute `expireTime`
- `NoExpiration` (`-1`) results in `expireTime == 0`

Current code treats any other non-positive effective TTL as no expiration.

Expired items can be removed by:

- `Get`, when it encounters an expired item
- `Exists`, which uses a write lock and removes if expired
- `Cleanup`, either called manually or by the cleanup worker

`Keys` returns a point-in-time snapshot of non-expired keys, but it does not
remove expired entries. `SetWithCallback` schedules a timer after commit and
fires only if the item still exists and is expired when the timer wakes; cleanup
is still responsible for removing the item.

### Removal Listener

`WithOnEvict` registers a listener invoked once for every key that leaves the
cache through capacity eviction, TTL expiration or `Delete`. It is not called
for `Clear` or when an existing key's value is replaced. To keep the listener off
the hot path, the two removal (`dropItem` and `cleanup`) stage the
removed `(key, value)` pairs in a per-shard buffer under the shard lock, and a
dedicated worker drains the buffers and runs the listener holding no shard lock -
so the listener may call back into the cache. When no listener is configured no
buffer is allocated and the worker is never started. Delivery is asynchronous, so
consumers that maintain a secondary index (such as the httpcache pattern index)
must tolerate notifications that arrive after a key has been reinserted.

## Write Path

Every shard has a bounded multi-producer, single-consumer ring queue. Producers
claim slots with CAS on `head`; the shard worker owns `tail`. Each cell has a
sequence number so the queue can distinguish empty, published, and freed slots
across ring laps. The ring size is rounded to a power of two and is at least 2.

`SetAsync` only enqueues. A nil error means the command was accepted, not that it
has committed. Call `Sync` when committed visibility is required.

`Set`, `SetWithCallback`, and `Delete` use the synchronous mutation path:

1. Take the shard's `drainMu`, which is the single-consumer token for that shard.
2. Drain queued writes accepted before this mutation.
3. Take the shard write lock.
4. Apply the mutation directly.
5. Release locks and schedule callbacks outside the shard lock.

That preserves per-shard ordering while avoiding a worker round trip for strict
read-after-write operations.

Shard workers wait on a coalesced `wake` channel. On wake they:

1. Drain buffered SieveTinyLFU read samples into the sketch.
2. Dequeue up to `WriteBatchSize` commands.
3. Apply the batch under one shard write lock.
4. Acknowledge commands that carry result channels.
5. Drain read samples again between batches.

`Sync`, `Clear`, and shutdown use barrier commands. A barrier enqueued to every
shard is acknowledged only after all earlier commands for that shard have been
applied. `Close` marks the cache closed, flushes accepted writes, closes
`closeCh`, waits for workers, and clears all shards. Producers blocked on a full
queue wake through `closeCh` and return `ErrCacheClosed`.

## Read Path

All reads return misses after the cache is closed.

For LRU and LFU, `Get` takes the shard write lock because the read mutates policy
metadata. LRU moves the item to the list head. LFU increments the exact frequency
bucket and updates `lastAccess`.

For FIFO, `Get` takes the read lock unless it has to remove an expired item. FIFO
does not update list position on access.

For bounded SieveTinyLFU, `Get` first tries `TryRLock`. If the shard is
contended, it falls back to the write lock path. A hit records the key hash into
the read buffer. After the shard has warmed past the warmup threshold, a hit also
marks the item as visited. A miss records the hash only after warmup. Expired
items are removed under the write lock. On the fast path, SieveTinyLFU copies
the value and marks the item visited before resolving TTL outside the read lock;
expired entries are revalidated and removed under the write lock before the read
returns a miss.

The SieveTinyLFU warmup threshold is `size*2 < cap`. Before that threshold, new
writes skip admission/frequency work and segment pressure is not enforced; new
entries are allowed to fill probation.

## Eviction Policies

`EvictionPolicy` values are:

```go
const (
    DefaultEvictionPolicy EvictionPolicy = iota
    LRU
    LFU
    FIFO
    SieveTinyLFU
)
```

### LRU

LRU uses the shard's intrusive list with sentinel head and tail nodes. New and
updated items are inserted at the head. Reads move the item to the head. Eviction
drops `tail.prev`.

Operations are O(1) under the shard write lock.

### FIFO

FIFO uses the same intrusive list, but reads do not move nodes. New items are
added at the head, so `tail.prev` is the oldest inserted item and is evicted.

### LFU

LFU uses an ascending circular list of frequency buckets. Each bucket contains a
map of items at that exact frequency, and `itemFreq` maps each item back to its
bucket.

New items start at frequency 1. Reads move the item to frequency `n+1`,
creating or removing buckets as needed. Eviction picks one item from the lowest
frequency bucket. Current LFU eviction does not use recency to break ties.

Updating an existing LFU key resets it to frequency 1.

### SieveTinyLFU

SieveTinyLFU is the default bounded policy. It combines:

- a probation FIFO queue for new entries
- a main SIEVE queue for entries with reuse evidence
- a compact ghost FIFO of recently evicted probation fingerprints
- a TinyLFU-style frequency estimator
- a small adaptive controller for the probation/main split

It is allocated only for shards with `cap > 0`.

#### Segment Sizing

`ProbationRatio` and `GhostRatio` are percentages. A zero value means "use the
default", not "zero capacity".

For a shard capacity `c`:

- minimum probation cap is `max(1, c/100)`
- maximum probation cap is `max(min, c*60/100)`, capped below `c` when possible
- initial probation cap is clamped from `c * ProbationRatio / 100`
- main cap is `c - probationCap`
- ghost cap is `mainCap * GhostRatio / 100`, with a minimum of 1 when main exists
- adaptation step is `max(1, c/100)`
- sketch/doorkeeper sample size is `max(c*10, 1024)`

#### Frequency Estimator

The frequency path uses a doorkeeper plus a count-min sketch:

- the doorkeeper is a 2-hash Bloom-style filter
- the first observation sets doorkeeper bits only
- later observations add to the sketch
- `estimate` returns the sketch estimate plus one if the doorkeeper still contains the item
- the sketch has 4-bit saturating counters packed 16 per `uint64`
- four indexes are derived by rotating the key hash and applying xxHash64 avalanche
- all counters are aged by right-shifting after `sampleSize * 10` observations
- aging also clears the doorkeeper

Reads do not update the sketch directly. They write hashes into a striped,
lossy, per-shard read buffer. The worker drains those samples into the estimator
before and between write batches.

The read buffer has up to 16 stripes, rounded to a power of two from
`GOMAXPROCS`. `procID()` uses `runtime.procPin` through `go:linkname` only as a
best-effort stripe hint. A full stripe overwrites old samples; that can reduce
frequency accuracy but cannot corrupt cache contents.

#### Admission And Replacement

SieveTinyLFU inserts follow this path:

1. During warmup, insert into probation and skip frequency/admission work.
2. After warmup, record the key hash in the estimator.
3. If `(hash, tag)` is in the ghost queue, remove the ghost entry and insert the
   item directly into main with `visited=true`.
4. Otherwise insert into probation with `reuse=0` and `visited=false`.
5. Enforce resident capacity and segment pressure.

Probation eviction examines the probation tail:

- if the item has `reuse >= 1` or `visited=true`, it is promoted to main
- otherwise it is evicted and recorded in the ghost queue

Main eviction uses a SIEVE hand:

- start from `hand`, falling back to the main tail when needed
- scan up to `defaultMainVictimScan` items
- if an item is visited, clear `visited`, decrement `reuse` if positive, and move backward
- the first unvisited item is the main victim
- forced eviction can take a candidate after the bounded scan

When a probation promotion or ghost-hit item competes with a main victim,
`shouldAdmit` compares TinyLFU estimates. Ties favor proven short-term reuse:
ghost hits, probation promotions, unvisited victims, and victims with zero reuse.

Capacity enforcement is bounded by `maxEvictionWork`. If the bounded pass cannot
restore capacity because it spent work promoting probation entries, a forced tail
drop restores the shard to capacity.

#### Adaptation

The controller tracks ghost hits, probation evictions, promotions, main
survivals, and observations. Every one of these is maintained on the
single-consumer maintenance path (the write worker, or a caller holding the
shard write lock), so the read path never writes shared policy counters. Main
usefulness is measured by `mainSurvivals`: the count of visited main residents
the SIEVE hand spares during eviction sweeps. It is a maintenance-path proxy for
"main read hits" that avoids a contended atomic increment on every read hit, and
it tracks how many distinct main entries reads keep alive rather than being
skewed by a single hammered key.

Two mechanisms can change segment sizing:

- an immediate ghost hit can grow probation by one adaptation step
- periodic `tick` checks every `capacity * 10` observations and can also grow
  probation when ghost hits dominate probation evictions
- periodic `tick` can shrink probation when probation is evicting much more than
  it promotes and main is earning its keep (more survivals than promotions)

The shrink path moves one adaptation step of capacity from probation to main.

## Stats

`Stats()` always reports `Size`, `Capacity`, and `Shards`. When
`StatsEnabled == true`, it also aggregates per-shard hits, misses, evictions,
expirations, and computes `HitRatio`.

`PolicyStats()` aggregates SieveTinyLFU counters:

- `Admits`
- `Rejects`
- `GhostHits`
- `Promotions`
- `ProbationEvictions`
- `MainEvictions`

An admission reject means the newly inserted item lost admission and was removed
during capacity enforcement.

## Manager

`Manager` keeps named configs in a locked map and cache instances in a
`sync.Map`. `GetCache` creates a cache lazily using the registered config or
`DefaultConfig`. If concurrent creators race, `LoadOrStore` keeps one cache and
closes the loser so worker goroutines do not leak.

Named caches are typed. Asking for the same name with a different `K,V` pair
returns `ErrTypeMismatch`.

## HTTP Middleware

`httpcache.New` resolves middleware config, constructs a backing
`kioshun.Cache[string, *Response]`, and installs:

- `DefaultKeyGenerator`
- `DefaultCachePolicy`
- ignored response headers
- cacheable method set
- response body size limit
- pattern index for invalidation, enabled when `Config.PathExtractor` is set

`DefaultConfig` uses SieveTinyLFU, 16 shards, max 100000 responses, 5 minute TTL,
GET/HEAD, common cacheable status codes, ignored volatile headers, and a 10 MB
body capture limit.

`Wrap` bypasses non-cacheable request methods. For cacheable methods:

1. Generate the cache key.
2. Serve a cached response when present.
3. Otherwise wrap the `ResponseWriter`.
4. Forward the request to the next handler.
5. Capture final status, headers, and body.
6. Ask the policy whether to cache and for what TTL.
7. When pattern invalidation is enabled and the key maps to a path, add it to the
   pattern index before the store, so an eviction during the store (including a
   SIEVE admission rejection) is reconciled by the eviction listener.
8. Store a defensive response snapshot in the backing cache, rolling back the
   index entry if the store fails.

The response writer:

- treats 1xx responses as informational and does not commit final status
- marks 101 Switching Protocols, flushes, and hijacks as streamed and not cacheable
- preserves `Unwrap` for `http.ResponseController`
- sets the miss header before committing a miss response
- captures body bytes up to `MaxBodySize`

`DefaultCachePolicy` rejects non-cacheable methods, non-cacheable statuses,
oversized bodies, and `Cache-Control: no-cache`, `no-store`, or `private`. It
uses positive `max-age` first, then positive `Expires`, then `DefaultTTL`.

Key generators include:

- `DefaultKeyGenerator`: MD5 of method, full URL, and common vary headers
- `KeyWithVaryHeaders`: MD5 of method, full URL, and caller-specified headers
- `KeyWithoutQuery`: plain `METHOD:/path`
- `KeyWithoutQueryHashed`: MD5 of method, path, and common vary headers
- `KeyWithUserID`: MD5 of method, full URL, and a user-id header
- `PathBasedKeyGenerator`: plain `METHOD:/path`

`Invalidate(pattern)` queries the path index for matching keys and deletes them
from the cache; the eviction listener then reconciles the index. Exact patterns
match keys stored at one path, and patterns ending in `*` match the base path and
its descendants. Pattern invalidation is enabled by setting `Config.PathExtractor`
(which also wires the eviction listener) together with a compatible key generator,
such as `KeyWithoutQuery` with `PathExtractorFromKey`; without it the index stays
empty and `Invalidate` returns 0.

## Algorithm References

The implementation is not a direct copy of any one paper, but the terminology
and policy design are grounded in these primary references:

- [SIEVE: Cache eviction can be simple, effective, and scalable](https://www.usenix.org/system/files/nsdi24-zhang-yazhuo.pdf)
- [TinyLFU: A Highly Efficient Cache Admission Policy](https://arxiv.org/abs/1512.00727)
- [An improved data stream summary: the count-min sketch and its applications](https://www.cs.helsinki.fi/u/jilu/paper/countMin.pdf)
- [xxHash specification](https://github.com/Cyan4973/xxHash/blob/dev/doc/xxhash_spec.md)
- [Go `net/http.ResponseController`](https://go.dev/src/net/http/responsecontroller.go)
