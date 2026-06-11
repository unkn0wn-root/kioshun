# Internals

This document explains how the current v1 cache core is put together with a focus on ownership, concurrency boundaries and policy behavior. The public API is covered in `README.md`.

## Package Layout

The root `kioshun` package contains the generic cache:

- `cache.go`: public API, construction, sharding, reads, stats, cleanup.
- `writes.go`: queued writes, synchronous mutation ordering, barriers, workers.
- `shard.go`: per-shard state, item removal, read-sample draining, TTL cleanup.
- `htable.go`: per-shard open-addressing table with lock-free lookups.
- `item.go`: cache entry metadata and immutable reader-visible fields.
- `evict.go`: removal reasons and removal/eviction listener options.
- `evictor.go`: LRU, LFU, and FIFO eviction adapters.
- `lfu.go`: exact LFU frequency bucket list.
- `sieve.go`, `sketch.go`, `ghost.go`, `read_buffer.go`: SieveTinyLFU policy.
- `queue.go`: bounded per-shard MPSC write queue.
- `pid.go`: best-effort P id lookup used for striped read buffers and stats.
- `internal/keyhash`: type-specialized hashing.
- `manager.go`: named cache registry and global manager helpers.

The `httpcache` package layers HTTP response caching on top:

- `httpcache/config.go`: middleware configuration and default resolution.
- `httpcache/middleware.go`: handler wrapping, response capture, policies, keys.
- `httpcache/pattern.go`: path-segment trie indexing cached keys for pattern
  invalidation, kept in sync by the cache's removal listener.

## Core Data Model

`Cache[K, V]` is split into shards. A shard is the unit of ownership for table state, policy state, queued writes, and capacity accounting. Each shard keeps:

- one `htable[K,V]` for key/value storage
- one `sync.RWMutex` for writers, scans, expiration removals, and non-Sieve policy reads
- a bounded MPSC write queue and one write-worker goroutine
- a `drainMu` single-consumer token shared by the worker, synchronous mutators, miss-path catch-up and opportunistic inline writes
- an LRU-style list for LRU, FIFO, and generic unlinking
- an exact LFU bucket list when `EvictionPolicy == LFU`
- SieveTinyLFU state and a read-sample buffer when the policy is bounded
- atomic resident size/cost counters
- optional removal listener staging buffers

Bounded SieveTinyLFU reads do not take the shard lock. Writers still serialize through the lock, but readers can probe the table while a writer is active.
That design drives the item lifetime rules:

- Reader-visible item fields (`key`, `hash`, `value`, `expireTime`) are written before publication and are never mutated after publication.
- SieveTinyLFU updates publish a fresh item instead of mutating the old one.
- An evicted item may still be held by a reader, so cache items are not returned to `sync.Pool`; the Go GC owns item reclamation.
- Only synchronous write waiters are pooled.

`cacheItem` carries the stored value plus the metadata needed by the active policy:

- `value`, `key`, `hash`, `expireTime`: immutable while visible to lock-free readers.
- `cost`: weigher-reported cost, updated only on the serialized write path.
- `prev`, `next`: intrusive list/queue links.
- `queue`, `queueOwner`: Sieve queue identity for probation/main ownership.
- `reuse`, `visited`: Sieve reuse state. Readers set `visited` atomically; the maintenance path consumes and clears it.
- `unpublished`: marks a Sieve insert candidate living in policy queues before it is published into the table.

## Configuration

`DefaultConfig()` uses:

- `MaxSize = 10000`
- `MaxCost = 0`
- `ShardCount = 0`, meaning auto-detect
- `CleanupInterval = 5 * time.Minute`
- `DefaultTTL = 30 * time.Minute`
- `EvictionPolicy = SieveTinyLFU`
- `StatsEnabled = false`
- `ProbationRatio = 1`
- `GhostRatio = 100`
- `CostAdmission = CostAdmissionFrequency`
- `WriteBufferSize = 256`
- `WriteBatchSize = 64`

`New` validates the config, resolves `DefaultEvictionPolicy` to `SieveTinyLFU` and fills in default write queue settings when they are left as zero.

A zero `ShardCount` lets the cache choose a count from the machine: it starts at `runtime.NumCPU() * 4` and caps that at 256. The final count is rounded to a
power of two so shard lookup is just `hash & shardMask`. For bounded caches, `New` also caps the count so tiny `MaxSize` or `MaxCost` values do not create
empty shards. `MaxSize` is split exactly across shards: every shard gets `MaxSize / shards`, and the first `MaxSize % shards` shards get one extra slot.

`MaxSize == 0` means entries are unlimited. In that mode SieveTinyLFU has no eviction or admission work to do, so the per-shard Sieve state is not allocated.
A SieveTinyLFU cache with `MaxCost > 0` must also have `MaxSize > 0`; the policy needs a bounded resident entry set to size its queues and sketch.

`MaxCost` adds a weighted resident budget. The budget is distributed per shard and `WithWeigher` supplies item cost. With weighted SieveTinyLFU:

- `CostAdmissionFrequency` compares TinyLFU frequency only.
- `CostAdmissionBalanced` compares frequency divided by `sqrt(cost)`.
- `CostAdmissionDensity` compares frequency divided by `cost`.

## Hashing

`internal/keyhash.New[K]` picks a hash function once for the key type:

- strings use the Go runtime `memhash` backend by default
- builds with `-tags kioshun_purego` use FNV-1a for strings of length `<= 8` and a local `xxHash64(seed=0)` implementation for longer strings
- integer-like keys are read directly and avalanched with the xxHash64 finalizer
- other comparable keys fall back to `fmt.Sprintf("%v", key)` and hash that string

The default string path uses `unsafe` plus `go:linkname` to call `runtime.memhash`. The call only reads string bytes and does not keep the
pointer, but it does depend on a runtime internal symbol. Build with `-tags kioshun_purego` when that tradeoff is not acceptable.

The hash selects a shard by `hash & shardMask`. The table still compares full keys on tag matches, so hash collisions affect distribution and probe length not key equality.

## Hash Table

Each shard has a single-writer, multi-reader open-addressing table. Readers call `lookup` without taking a lock:

1. Snapshot the current slot array with an atomic pointer load.
2. Linear probe a dense array of `{tag, item}` cells.
3. Stop on tag `0` (empty), continue past tag `1` (tombstone), and dereference
   the item pointer only on a matching normalized hash tag.

The table relies on a strict publication order:

- stores write the item pointer before publishing the tag
- removals write the tombstone tag before clearing the item pointer
- rehash and clear publish a fresh slot array atomically

Writers are serialized by the shard lock, so table metadata like `live` and `tombs` is writer-only. A reader may still be walking an old table snapshot, or
holding an old item, after a writer has removed it. That is safe because both the old table array and the old item remain reachable to the GC until the reader is done.

Sieve inserts use a two-step table path. `probe` either returns an existing key's item and slot so the writer can swap in a fresh immutable item, or returns a
cursor where a new candidate could be published. New candidates run admission before they enter the table. A rejected candidate was never visible to readers
and leaves no tombstone. An admitted candidate is published at the captured cursor.

Removals reclaim tombstones at the end of their probe cluster: a tombstone followed by an empty slot lies on no live item's probe path, so it (and any
tombstones immediately before it) converts back to empty. This is safe under concurrent lock-free lookups - a reader stops one slot earlier with the same
result - and keeps miss probes short on eviction-heavy shards. The slot a probe cursor is waiting to fill is pinned: reclamation neither clears it nor treats
it as the empty trigger, so a pending insert's probe path stays intact.

## TTL And Expiration

`Set` and `SetAsync` normalize TTLs before they reach the mutation path:

- `DefaultExpiration` (`0`) uses `Config.DefaultTTL`
- positive TTLs become an absolute `expireTime`
- `NoExpiration` (`-1`) results in `expireTime == 0`

Any other non-positive effective TTL is treated as no expiration.

Expired items leave the cache through three paths:

- `Get`, when it encounters an expired item
- `Exists`, which takes the write lock when removal is needed
- `Cleanup`, either called manually or by the cleanup worker

`Keys` returns a point-in-time snapshot of non-expired keys. It does not clean up expired entries while scanning. `SetWithCallback` schedules its timer after
the write commits, then re-checks the item when the timer wakes. The callback fires only if that same key still exists and is expired; cleanup still owns
actual removal.

## Removal Listener

`WithOnRemove` installs a listener for keys that leave the cache through capacity eviction, SieveTinyLFU admission rejection, TTL expiration, or
`Delete`. Each notification includes a `RemovalReason`. `WithOnEvict` is the narrower hook for capacity evictions only.

Listeners are not called for `Clear`, and they are not called when an existing key is replaced. Removal paths only stage `(key, value, reason)` in a per-shard
buffer while holding the shard lock. A dedicated worker drains those buffers and calls listeners without holding shard locks, so listeners may call back into the
cache. If no listener is configured, no buffer or worker is allocated. Delivery is asynchronous, so secondary indexes must handle a notification arriving after
the same key has already been inserted again.

## Write Path

Every shard has a bounded multi-producer, single-consumer ring queue. Producers claim slots with CAS on `head`; the shard worker owns `tail`. Sequence numbers
on each cell distinguish free, published, and stale slots across ring laps. The ring size is rounded to a power of two and is never smaller than 2.

`SetAsync` is the fast write API. It tries the cheapest path first: apply the write inline when the shard is completely idle.

1. `drainMu.TryLock()` succeeds.
2. The write queue is quiescent.
3. `s.mu.TryLock()` succeeds.

The inline path drains read samples before applying the write under the shard lock. That keeps SieveTinyLFU admission consistent with what the worker would
have done. If any `TryLock` or quiescence check fails, `SetAsync` falls back to the queue. A nil error means the write was accepted; it may already be visible,
or it may still be waiting for the worker. Call `Sync` when committed visibility matters.

`Set`, `SetWithCallback`, and `Delete` take the ordered synchronous path:

1. Check that the cache is open.
2. Take `drainMu`, the shard's single-consumer token.
3. Drain queued writes accepted before this mutation.
4. Take the shard write lock.
5. Apply the mutation directly.
6. Release locks and schedule callbacks outside the shard lock.

This preserves per-shard ordering and gives `Set` immediate read-after-write visibility.

Shard workers sleep on a coalesced `wake` channel. Each wake runs this sequence:

1. Drain buffered SieveTinyLFU read samples into the sketch.
2. Dequeue up to `WriteBatchSize` commands.
3. Apply the batch under one shard write lock.
4. Acknowledge commands that carry result channels.
5. Drain read samples again between batches.

`Sync`, `Clear`, and shutdown use barrier commands. A barrier sent to every shard is acknowledged only after earlier commands on that shard have been
applied. `Close` marks the cache closed, flushes accepted writes, closes `closeCh`, waits for workers, and clears all shards. Producers blocked on a full
queue wake through `closeCh` and return `ErrCacheClosed`.

## Read Path

After `Close`, reads return misses.

For LRU and LFU, `Get` takes the shard write lock because a read changes policy state. LRU moves the item to the list head. LFU increments the exact frequency bucket.

For FIFO, `Get` takes the read lock unless it has to remove an expired item. FIFO does not update list position on access.

For bounded SieveTinyLFU, `Get` probes the shard table without taking any shard lock. On a miss, it may opportunistically claim `drainMu`, drain pending writes,
and look again. This handles the common race where a `Get` follows a queued `SetAsync` for the same key, without forcing every async write to become synchronous.

On a Sieve hit after warmup:

- `visited` is set with a conditional atomic store
- the key hash is sampled into the per-shard read buffer
- stats are recorded only when `StatsEnabled` is true

The value and expiry are copied from immutable item fields. TTL is checked after the table lookup. If the item is expired, the read revalidates it under the
shard write lock, removes it, and returns a miss. Before warmup, Sieve skips read sampling and visited-bit writes because admission is unconditional and the
frequency sketch is not used yet.

The SieveTinyLFU warmup threshold is `size*2 < cap`. Before that threshold, new writes skip frequency/admission work and are allowed to fill probation.

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

LRU uses the shard's intrusive list with sentinel head and tail nodes. New and updated items go to the head. Reads move the item back to the head. Eviction
drops `tail.prev`.

Operations are O(1) under the shard write lock.

### FIFO

FIFO uses the same list, but reads leave nodes in place. New items are added at the head, so `tail.prev` remains the oldest inserted resident and is evicted.

### LFU

LFU uses an ascending circular list of frequency buckets. Each bucket contains the items at one exact frequency, and `itemFreq` maps each item back to its bucket.

New items start at frequency 1. Reads move the item to frequency `n+1`, creating or removing buckets as needed. Eviction picks one item from the lowest
frequency bucket. Current LFU eviction does not use recency to break ties. Updating an existing LFU key resets it to frequency 1.

### SieveTinyLFU

SieveTinyLFU is the default bounded policy. It combines a small recency window with frequency admission and SIEVE replacement:

In this document, "adaptive", "self-adapting", and "self-tuning" refer to
policy-level behavior inside SieveTinyLFU: admission, replacement, and the
probation/main split. They do not mean whole-system auto-tuning of shard count,
write allocation strategy, queue sizing, lock choice, GC behavior, or runtime
environment.

- a probation FIFO queue for new entries
- a main SIEVE queue for entries with reuse evidence
- a B1 ghost FIFO of recently evicted probation fingerprints
- a B2 ghost FIFO of recently evicted main fingerprints
- a doorkeeper plus count-min sketch frequency estimator
- a policy-level adaptive controller for the probation/main split
- a policy-level self-tuning admission controller for recency-biased vs frequency-biased tie-breaking

The policy state exists only on bounded Sieve shards.

#### Segment Sizing

`ProbationRatio` and `GhostRatio` are percentages. Zero means "use the
default", not "allocate zero".

For a shard capacity `c`:

- minimum probation cap is `max(1, c/100)`
- maximum probation cap is `max(min, c*60/100)`, capped below `c` when possible
- initial probation cap is clamped from `c * ProbationRatio / 100`
- main cap is `c - probationCap`
- B1 ghost cap is `mainCap * GhostRatio / 100`, with a minimum of 1 when main exists
- B2 ghost cap is `c`, about one shard-capacity of recent main-victim fingerprints
- adaptation step is `max(1, c/100)`
- sketch/doorkeeper counter count starts at `max(c*10, 1024)`

#### Frequency Estimator

The frequency estimator is a doorkeeper in front of a count-min sketch:

- the doorkeeper is a 2-hash Bloom-style filter
- the first observation sets doorkeeper bits only
- later observations add to the sketch
- `estimate` returns the sketch estimate plus one if the doorkeeper still contains the item
- the sketch has 4-bit saturating counters packed 16 per `uint64`, grouped into cache-line blocks of 8 words
- one xxHash64 avalanche feeds everything: its low bits select the block (and the doorkeeper indexes), and four disjoint higher bit ranges select the in-block counters, so an add or estimate costs one hash and touches one sketch cache line
- counters are aged by right-shifting after `resetAt` observations
- aging also clears the doorkeeper

Read hits do not update the sketch directly. They append key hashes to a striped, lossy, per-shard read buffer. The worker drains those samples into the
estimator before and between write batches. If a stripe fills faster than the worker can drain it, old samples are overwritten. That can reduce frequency
accuracy, but it cannot corrupt cache contents.

Read misses do no admission work at all: a miss that becomes a Set is counted at insert, and a miss that never becomes a Set is not an admission candidate.
Every insert advances the estimator timebase by exactly two observations (the unsampled miss plus the Set), so sketch aging and the adaptive-cycle cadence
run the same in every regime. How much the insert deposits is the adaptive part, deliberately decoupled from that timebase. During shifting working sets
both observations deposit into the doorkeeper/sketch, which inflates candidates against long-resident victims so a moving working set displaces stale
entries quickly. During stationary or loop regimes only the first deposits - plain per-request TinyLFU - so candidates must prove reuse before a stable
frequency core yields to them. `adaptSize` picks the weight from the same per-cycle signals that size the probation window, defaulting to the shifting
weight.

The read buffer has up to 16 stripes, rounded to a power of two from `GOMAXPROCS`. `procID()` uses `runtime.procPin` through `go:linkname` as a stripe hint. A
per-buffer dirty mask carries one bit per stripe: producers set it on a stripe's first sample, and the consumer clears it only when the stripe turns out to be
quiet. That makes the frequent "any samples pending?" check on the write path a single load, and a busy stripe costs neither side a read-modify-write.

#### Ghost Queues

B1 and B2 store full 64-bit key hashes, not full keys. Each ghost queue is a fixed-size FIFO ring plus an open-addressed index, so membership checks, adds,
and removes are O(1) while the shard is being maintained. A ghost false positive requires a full 64-bit hash collision.

B1 hits mean a probation victim returned quickly, so the item is readmitted directly into main. B2 hits mean a main victim returned. That is the
"resurrection" signal used by both adaptive controllers.

#### Admission And Replacement

SieveTinyLFU inserts work like this:

1. During warmup, insert into probation and skip frequency/admission work.
2. After warmup, record the key hash in the estimator at the controller-selected insert weight (see the frequency estimator above), and note a B2 resurrection if the key was a recent main-eviction victim.
3. If the hash is in B1, remove the ghost entry and insert the item directly into main with `visited=true`.
4. Otherwise insert into probation with `reuse=0` and `visited=false`.
5. Enforce resident capacity and segment pressure.
6. Publish the item into the table only if it still belongs to a Sieve queue.

When probation is over pressure, eviction starts at the probation tail:

- if the item has `reuse >= 1` or `visited=true`, it is promoted to main
- otherwise it is evicted and recorded in B1

Main eviction uses a SIEVE hand:

- start from `hand`, falling back to the main tail when needed
- scan up to `defaultMainVictimScan` items
- if an item is visited, clear `visited`, decrement `reuse` if positive, count a main survival, and move backward
- the first unvisited item is the main victim
- forced eviction can take a candidate after the bounded scan

When a probation promotion or B1 ghost-hit item competes with a main victim, `shouldAdmit` compares their TinyLFU estimates. The admission tuner decides how
ties are handled:

- recency mode favors candidates with proven short-term reuse, such as B1 ghost hits and probation promotions
- frequency mode is plain TinyLFU: the candidate is admitted only when its estimate strictly beats the victim's, so the incumbent wins ties

Capacity enforcement is bounded by `maxEvictionWork`. If that pass spends its
budget promoting probation entries and still does not restore capacity, a forced
tail drop brings the shard back under its limit.

#### Policy-Level Adaptation

Adaptation is internal to SieveTinyLFU's policy state; there is no public switch
for it. It does not tune write-path memory layout, queue sizes, shard count, or
other runtime/system parameters. `tick` runs once per maintenance window. The window is `capacity * 4`, with a minimum of 8192
observations for shards whose capacity is at least 1024.

Both controllers run while the shard is being maintained, not on the lock-free read path. Reads only set `visited` and sample hashes.
They do not increment adaptive counters directly.

Segment sizing watches B1 ghost hits, probation evictions, promotions, main evictions, B2 resurrections, and main survivals. `mainSurvivals` is the count
of main residents the SIEVE hand spared because reads set their visited bit. It acts as a low-contention proxy for "main is useful".

The hard case is telling a stationary skew from a shifting hot set. Both can produce probation churn and B1 hits. The B2 resurrection rate separates them: a
looping stationary workload keeps bringing main victims back, while a shifting hot set abandons them. Segment sizing grows probation aggressively when main
victims are abandoned but probation victims keep returning. It shrinks probation when probation churns more than it promotes and main is earning its capacity.

Admission tuning chooses recency or frequency tie-breaking per shard. It starts in recency mode. When the B2 resurrection rate shows a loop signature, the
shard runs one frequency-mode trial and keeps it only if churn cost (`evictions + rejects`) falls. A shard committed to frequency switches back to
recency when churn climbs again, with exponential backoff after failed trials.

## Stats

`Stats()` always reports `Size`, `Cost`, `Capacity`, `MaxCost`, and `Shards`.
When `StatsEnabled` is true, it also aggregates hits, misses, evictions, and expirations, then computes `HitRatio`.

Stats counters are striped by best-effort P id, capped at 16 stripes, so hot reads do not all hammer the same counter. `StatsEnabled` is false in the root
`DefaultConfig` because those increments still have runtime cost.

`PolicyStats()` aggregates SieveTinyLFU counters under each shard's read lock:

- `Admits`
- `Rejects`
- `GhostHits`
- `Promotions`
- `ProbationEvictions`
- `MainEvictions`

An admission reject means a newly inserted candidate lost admission and was removed during capacity enforcement.

## Manager

`Manager` keeps named configs in a locked map and live cache instances in a `sync.Map`. `GetCache` creates a cache lazily from the registered config, or
from `DefaultConfig` when nothing was registered. If concurrent creators race, `LoadOrStore` keeps one cache and closes the loser so worker goroutines do not leak.

Named caches are typed. Asking for the same name with a different `K, V` pair returns `ErrTypeMismatch`.

## HTTP Middleware

`httpcache.New` resolves middleware config, creates a backing
`kioshun.Cache[string, *Response]`, and wires:

- `DefaultKeyGenerator`
- `DefaultCachePolicy`
- ignored response headers
- cacheable method set
- response body size limit
- optional pattern index for invalidation

The HTTP cache default uses SieveTinyLFU, 16 shards, max 100000 responses, a 5 minute TTL, GET/HEAD, common cacheable status codes, ignored volatile headers,
and a 10 MB body capture limit. Unlike the root cache default, HTTP middleware enables cache stats unless `DisableStats` is set.

Pattern invalidation is opt-in. Supplying `Config.PathExtractor` enables the path index and attaches a removal listener to the backing cache. The index
stores keys by URL path and reconciles removals by response identity, so a delayed removal notification will not delete a newer response cached under the same key.

`Wrap` bypasses non-cacheable request methods. For a cacheable request it:

1. Generate the cache key.
2. Serve a cached response when present.
3. Otherwise wrap the `ResponseWriter`.
4. Forward the request to the next handler.
5. Capture final status, headers, and body.
6. Ask the policy whether to cache and for what TTL.
7. When pattern invalidation is enabled and the key maps to a path, add it to the pattern index before the store, so a removal during the store, including a SieveTinyLFU admission rejection, is reconciled by the removal listener.
8. Store a defensive response snapshot in the backing cache, rolling back the index entry if the store fails.

The response writer:

- treats 1xx responses as informational and does not commit final status
- marks 101 Switching Protocols, flushes, and hijacks as streamed and not cacheable
- preserves `Unwrap` for `http.ResponseController`
- sets the miss header before committing a miss response
- captures body bytes up to `MaxBodySize`

`DefaultCachePolicy` rejects non-cacheable methods, non-cacheable statuses, oversized bodies, and `Cache-Control: no-cache`, `no-store`, or `private`.
It uses a positive `max-age` first, then a positive `Expires`, then `DefaultTTL`.

Key generators include:

- `DefaultKeyGenerator`: MD5 of method, full URL, and common vary headers
- `KeyWithVaryHeaders`: MD5 of method, full URL, and caller-specified headers
- `KeyWithoutQuery`: plain `METHOD:/path`
- `KeyWithoutQueryHashed`: MD5 of method, path, and common vary headers
- `KeyWithUserID`: MD5 of method, full URL, and a user-id header
- `PathBasedKeyGenerator`: plain `METHOD:/path`

`Invalidate(pattern)` asks the path index for matching keys and deletes them from the cache. Exact patterns match keys stored at one path. Patterns ending in `*` match the base path and its descendants. Without a configured
`PathExtractor`, the index stays empty and `Invalidate` returns 0.

## Algorithm References

The implementation is not a direct copy of any one paper. The terminology and policy design are grounded in these references:

- [SIEVE: Cache eviction can be simple, effective, and scalable](https://www.usenix.org/system/files/nsdi24-zhang-yazhuo.pdf)
- [TinyLFU: A Highly Efficient Cache Admission Policy](https://arxiv.org/abs/1512.00727)
- [An improved data stream summary: the count-min sketch and its applications](https://www.cs.helsinki.fi/u/jilu/paper/countMin.pdf)
- [xxHash specification](https://github.com/Cyan4973/xxHash/blob/dev/doc/xxhash_spec.md)
- [Go `net/http.ResponseController`](https://go.dev/src/net/http/responsecontroller.go)
