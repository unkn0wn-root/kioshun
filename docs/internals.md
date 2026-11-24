# Internals

This document explains how the current v1 cache core is put together with a focus on ownership, concurrency boundaries and policy behavior. The public API is covered in `README.md`.

## Package Layout

The root `kioshun` package contains the generic cache:

- `cache.go`: public API, construction, sharding, reads, stats, cleanup.
- `config.go`: `Config`, defaults, and validation.
- `errors.go`: sentinel errors, `CacheError`, and `ConfigError`.
- `writes.go`: queued writes, synchronous mutation ordering, barriers, workers.
- `callback.go`: per-shard timer and deadline heap for `SetWithCallback`.
- `shard.go`: cache entry metadata, per-shard state, item removal, read-sample draining, TTL cleanup.
- `htable.go`: per-shard open-addressing table with lock-free lookups.
- `eviction.go`: removal reasons, removal/eviction listener options, and the LRU, LFU, and FIFO eviction adapters.
- `lfu.go`: exact LFU frequency bucket list.
- `sieve.go`, `sketch.go`, `ghost.go`, `readbuffer.go`: SieveTinyLFU policy.
- `mpsc.go`: bounded per-shard MPSC write queue.
- `stripeid.go`: stripe id used by striped read buffers and stats.
- `stats.go`: striped hit, miss, eviction, and expiration counters.
- `cacheline.go`: cache-line padding size.
- `internal/keyhash`: type-specialized hashing.
- `internal/mathx`: power-of-two rounding.
- `manager.go`: named cache registry and global manager helpers.

The `httpcache` package layers HTTP response caching on top:

- `httpcache/config.go`: middleware configuration and default resolution.
- `httpcache/middleware.go`: handler wrapping, response capture, policies, keys,
  the epoch table for variant invalidation.
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
- `GhostRatio = 75`
- `CostAdmission = CostAdmissionFrequency`
- `WriteBufferSize = 256`
- `WriteBatchSize = 64`

`New` validates the config, resolves `DefaultEvictionPolicy` to `SieveTinyLFU` and fills in default write queue settings when they are left as zero.

A zero `ShardCount` lets the cache choose a count from the machine: it starts at `runtime.NumCPU() * 4` and caps that at 256. When `MaxSize` is set, the
count is also capped at `MaxSize / 64` rounded down to a power of two (at least 1), so each shard holds at least `minShardEntries` (64) entries unless
there is only one. Each shard enforces its own limit and keys spread unevenly, so with `c` slots per shard a cache filled with `MaxSize` distinct keys
keeps about `1 - 1/sqrt(2πc)` of them, about 95% at 64 slots. At one slot per shard the expected share under uniform hashing is `1 - (1 - 1/n)^n` for `n` shards, about 63%.
An explicit `ShardCount` skips this rule.

The final count is rounded up to a power of two so shard lookup is just `hash & shardMask`. Automatic or explicit, when `MaxSize` or `MaxCost` is set,
the count is capped at the largest power of two that is at most 256 and at most that limit, so every shard gets at least one entry of `MaxSize` and one unit of `MaxCost`. A shard whose share rounded
down to zero would have no limit at all. `MaxSize` is split exactly across shards: every shard gets `MaxSize / shards`, and the first `MaxSize % shards`
shards get one extra slot.

`MaxSize == 0` means entries are unlimited. In that mode SieveTinyLFU has no eviction or admission work to do, so the per-shard Sieve state is not allocated.
A SieveTinyLFU cache with `MaxCost > 0` must also have `MaxSize > 0`; the policy needs a bounded resident entry set to size its queues and sketch.

`MaxCost` adds a weighted resident budget and `WithWeigher` supplies item cost. The budget is split across shards the same way as `MaxSize`, and each
shard enforces only its own share, so one item can cost at most `MaxCost / shards`, give or take one. A costlier item is rejected before it reaches the
write path, with an error that wraps `ErrItemTooLarge` and names the cost, the shard budget, `MaxCost`, and the shard count. With weighted SieveTinyLFU:

- `CostAdmissionFrequency` compares TinyLFU frequency only.
- `CostAdmissionBalanced` compares frequency divided by `sqrt(cost)`.
- `CostAdmissionDensity` compares frequency divided by `cost`.

## Hashing

`internal/keyhash.New[K]` picks a hash function once for the key type:

- strings use `hash/maphash.String`
- integer-like keys are read directly and avalanched with the xxHash64 finalizer
- other comparable keys use `hash/maphash.Comparable`, which hashes the value's memory representation the way the built-in map would

`hash/maphash` is the public API over the runtime's own seeded hash, so the cache does not depend on runtime internals or `go:linkname`.
Each cache gets its own seed, the way the runtime gives every map its own: hashes stay stable for the cache's lifetime (they are never
persisted), and a key set that hashes badly into one cache does not hash badly into the rest. Integer keys skip the seed and only get the
avalanche mix.

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

`Set`, `SetAsync`, and `SetWithCallback` normalize TTLs before they reach the mutation path:

- `DefaultExpiration` (`0`) uses `Config.DefaultTTL`, and a resolved TTL of 0 results in `expireTime == 0`
- positive TTLs become an absolute `expireTime`, capped at `math.MaxInt64` so a huge TTL cannot wrap
- `NoExpiration` (`-1`) results in `expireTime == 0`
- any other negative TTL results in `expireTime == -1`, meaning already expired

An `expireTime` of -1 goes through the normal write path, inline or queued, so it stays ordered behind earlier writes to the same shard. When it is
applied, the key is deleted the same way `Delete` deletes it, so listeners see `RemovedDeleted`. Nothing is stored, so the item's cost is not checked, and
no callback is scheduled.

Expired items leave the cache through three paths:

- `Get`, when it encounters an expired item
- `Exists`, which takes the write lock only when it finds an expired item
- `Cleanup`, either called manually or by the cleanup worker

`Keys` returns a point-in-time snapshot of non-expired keys. It does not clean up expired entries while scanning. `SetWithCallback` puts the
callback in its shard's deadline heap after the write commits. Each shard has one runtime timer, set for the earliest deadline, so a pending callback
costs a heap entry and a copy of its key and value, and no timer or goroutine. A deadline a millisecond or more away wakes the timer a sixteenth of the
remaining time early, and the timer is set again from there. A due callback runs only if the cache is still open and the key still exists and is
expired, and a callback that blocks does not hold back the ones due after it. Cleanup still owns actual removal.

## Removal Listener

`WithOnRemove` installs a listener for keys that leave the cache through capacity eviction, SieveTinyLFU admission rejection, TTL expiration, or
`Delete`. Each notification includes a `RemovalReason`. `WithOnEvict` is the narrower hook for capacity evictions only.

Listeners are not called for `Clear`, and they are not called when an existing key is replaced. Removal paths only stage `(key, value, reason)` in a per-shard
buffer while holding the shard lock. A dedicated worker drains those buffers and calls listeners without holding shard locks, so listeners may call back into the
cache. If no listener is configured, no buffer or worker is allocated. Delivery is asynchronous, so secondary indexes must handle a notification arriving after
the same key has already been inserted again.

`Close` delivers every removal that happened before it cleared the cache, and it does not report the entries it clears. It may make the last listener calls
itself, on the goroutine that called it, without holding a shard lock. Listener calls never overlap, and none happens after `Close` returns. Because `Close`
returns only after those calls return, a listener must not call `Close` or wait for a lock held by the goroutine that calls `Close`.

Staging buffers are reused: when the worker takes a shard's staged removals, it leaves an empty buffer that it has already delivered, so removals do not
allocate new buffers while the worker keeps up. A buffer whose capacity a burst grew past 256 entries is dropped after delivery instead of kept.

## Write Path

Every shard has a bounded multi-producer, single-consumer ring queue. Producers claim slots with CAS on `head`; the shard worker owns `tail`. Sequence numbers
on each cell distinguish free, published, and stale slots across ring laps. The ring size is rounded to a power of two and is never smaller than 2. A
producer that finds the ring full waits for a free slot, and a drain that frees several slots lets that many waiting producers through.
Producers on one shard share one ring and one worker and compete for the same `head`, so many goroutines calling `SetAsync` on one shard get
less total throughput than the same calls spread over several shards. A small `ShardCount`, or keys that hash to the same shard, put more
producers on one shard.

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

Every drain of the write queue, whichever call runs it, applies at most the writes that were queued when it started. A steady stream of
`SetAsync` calls therefore cannot keep `Set`, `SetWithCallback`, or `Delete` waiting for `drainMu` indefinitely.

Shard workers sleep on a coalesced `wake` channel. Each wake runs this sequence:

1. Drain buffered SieveTinyLFU read samples into the sketch.
2. Dequeue up to `WriteBatchSize` commands.
3. Apply the batch under one shard write lock.
4. Acknowledge commands that carry result channels.
5. Drain read samples again between batches.

`Sync`, `Clear`, and shutdown use barrier commands. A barrier sent to every shard is acknowledged only after earlier commands on that shard have
been applied. When a shard's queue is full, `Sync`, `Clear`, and the shutdown flush do not wait for room. They apply that shard's queued writes
themselves, and `Clear` then empties the shard. `Close` marks the cache closed, stops each shard's callback timer and drops its heap, flushes
accepted writes, closes `closeCh`, waits for workers, clears all shards and their queues, and then delivers the removals still staged for the removal
listeners. Once the cache is closed, a `Sync`, `Clear`, or read that overlaps `Close` no longer applies queued writes itself. If a shard worker
applies its barrier first, such a `Sync` or `Clear` still takes effect, though `Sync` may return `ErrCacheClosed` even then. Otherwise it does
nothing, and `Sync` returns `ErrCacheClosed`. `Close` applies every write accepted before `Close` was called, then clears the shards, and no entries
remain after it returns. When `Close` clears a queue, it drops the writes still in it. The queue refuses writes from then on, and a `SetAsync` that
reaches it returns `ErrCacheClosed`. A `SetAsync` that overlaps `Close` may still return nil for a write that `Close` drops. No write queue or
removal staging buffer holds a key or value after `Close` returns. Pending callbacks are reachable only through the shard heaps, so nothing pending
stays reachable after `Close`. A callback already due may still run.

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

`Exists` checks membership without touching policy state. It does not set `visited`, does not sample the read, and does not count a hit or miss. On a
bounded SieveTinyLFU shard it probes the table without a lock and, on a miss, drains pending writes the same way `Get` does. LRU, LFU, FIFO, and unbounded
SieveTinyLFU update items in place, so there `Exists` takes the shard read lock, and concurrent `Exists` calls do not wait for each other. If the item has
expired, `Exists` takes the write lock, checks the item again, and removes it.

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

The read buffer has up to 16 stripes, rounded to a power of two from `GOMAXPROCS`. Producers pick a stripe with `stripeID()`. The id lives in a
small token kept in a `sync.Pool`, which in steady state holds one token per P, and ids come from a lowest-free-id allocator, so concurrently
running producers end up on distinct stripes - the same spread P ids used to give us, without touching runtime internals. A
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

`Manager` keeps named configs in a locked map and live cache instances in a `sync.Map`. `GetCache` creates a cache lazily from the registered config and
returns `ErrCacheNotRegistered` for unknown names, so a misspelled name fails instead of silently minting a default cache; `GetCacheWithConfig` is the
explicit get-or-create path. If concurrent creators race, `LoadOrStore` keeps one cache and closes the loser so worker goroutines do not leak.

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

The HTTP cache default uses SieveTinyLFU, 16 shards, max 100000 responses, GET/HEAD, the statuses 200, 201, 300, 301, 302, 404 and 410,
ignored `Server`, `X-Request-Id` and `X-Trace-Id` headers, and a 10 MB body capture limit. `DefaultTTL` (5 minutes) applies only to responses
marked `public` that carry no other freshness information. Unlike the root cache default, HTTP middleware enables cache stats unless `DisableStats` is set.

Pattern invalidation is opt-in. Supplying `Config.PathExtractor` enables the path index and attaches a removal listener to the backing cache. The index
stores keys by URL path and reconciles removals by response identity, so a delayed removal notification will not delete a newer response cached under the same key.
With a `PathExtractor`, a store holds one of 64 mutexes, picked by a hash of the key, across the index update and the cache write, and `Clear` holds all 64
while it empties the cache and the index. Concurrent stores of one key therefore leave the index holding the response the cache holds, and no store lands
between the two halves of a `Clear`. Without a `PathExtractor`, stores and `Clear` take none of these locks.

The first call to `SetKeyGenerator` allocates an epoch table, and the table being present is the switch for what follows. `DefaultKeyGenerator`, the
only generator `New` installs, reads only the method and the target URI, so without the table every path runs as before apart from one check on a hit
and one nil check each on the miss and unsafe paths. Every stored response is 16 bytes larger, since its two tag fields move `Response` into the next
allocation size class.
A custom generator can give the variants of one URL different keys, for example one per user. With the table, `Wrap` also computes resource keys.
A resource key is the key the generator returns for a bare copy of the request: no header, body or form values, a copy of the URL taken before the
handler runs, and the same context, remote address, host and TLS state. A copy that kept the headers would find only the requester's own variant.

The epoch table is a fixed array of atomic 64-bit counters. Its length is `MaxSize` rounded up to a power of two, at least 1024, or 65536 without a
size limit, and a later `SetKeyGenerator` keeps it. A resource key maps to a slot by `maphash` with a seed made in `New`, the same seed that picks the
pattern-index locks. A stored response whose key differs from its resource key records the slot, plus 1 so that 0 means untagged, and the slot's counter at
the time of the store. A response whose key equals its resource key, such as one stored without the header the generator reads,
is untagged.

To invalidate a resource key, `Wrap` deletes the entry stored under it and adds 1 to its slot's counter. A hit on a tagged response whose recorded epoch
no longer matches its slot is treated like a `Vary` mismatch. The request goes to the handler, and its response, if stored, replaces the old one under
the same key with the new epoch.

Two resource keys can share a slot. A write to one then also makes the tagged entries of the other miss once, and they are refilled on the next request.
Stale data is never served. A write moves two slots, one for the GET and one for the HEAD resource key. With the table at least as large as `MaxSize`
and the cache full of tagged entries, a write therefore costs about two extra misses on other resources on average. The counters are 64 bits wide,
so they do not wrap in practice.

The generator runs on the bare copy under `recover`. If it panics, as one that needs a header may, a store leaves the response untagged. An unsafe
request computes the GET and the HEAD resource key separately, so a panic for one method leaves the other method's resource key in use. For a method
that panicked, only the requester's own entry is removed.

An entry that is no longer served stays in memory until it is replaced, evicted or expires. Until then it counts toward `Stats`, the core counts a hit for
the lookup that finds it, and `InvalidateByFunc` sees its key. The table costs 8 bytes per slot and exists only after `SetKeyGenerator`. A GET that
started before a successful unsafe request and finishes after it can store the old response again.

`Wrap` first looks at the request method:

- A method in `CacheableMethods` goes on to the caching steps below.
- GET, HEAD, OPTIONS and TRACE outside `CacheableMethods` pass through untouched.
- Any other method is unsafe. Before the handler runs, since the handler may change the request, `Wrap` computes the GET and HEAD keys.
  After `SetKeyGenerator` it computes the resource key of each method from one bare copy of the request, each under its own `recover`.
  Without the table, or when the generator panics on the bare copy for either method, it also computes the generator's keys for a copy
  that keeps the request's headers. The handler runs behind a writer that records the status and buffers no body. On a 2xx or 3xx status,
  `Wrap` deletes those own keys if it computed them and invalidates the resource key of each method that did not panic (RFC 9111
  section 4.4).

For a cacheable method, a request with any field line of `Range`, `If-Range`, `If-Match`, `If-None-Match`, `If-Modified-Since` or
`If-Unmodified-Since`, even an empty one, passes through with no lookup and no store. Otherwise `Wrap`:

1. Generate the cache key.
2. Serve the stored response when present, when it is untagged or its recorded epoch still matches its slot, and when every header it recorded
   from `Vary` has the same values in this request. Anything else is a miss.
3. Otherwise clone the request headers, so the rules below see the request as received, and wrap the `ResponseWriter`. When the epoch table
   exists, also take a copy of the request and its URL on the stack, in a frame of its own, made only then.
4. Forward the request to the next handler.
5. Capture final status, headers, and body. The writer rebuilds the header map with canonical keys and merges the values of keys that
   differ only in case, in sorted key order as net/http writes them, so fields set under non-canonical keys are seen by every rule.
6. Apply the shared-cache storage restrictions, whatever the policy: no `206`, no `no-store`, `no-cache` or `private` in any
   `Cache-Control` line, no `Authorization` request unless the response has `public`, `s-maxage` or `must-revalidate`, no `Set-Cookie`,
   and no request `Cache-Control: no-store`. A field with an empty value counts as present.
7. Ask the policy whether to cache and for what TTL.
8. Read `Vary` over all field lines with canonicalized names. `Vary: *` stops here. Record the cloned request values for each name.
9. Build the snapshot: the stored headers minus ignored ones and hop-by-hop fields (`Connection`, the fields it names, `Keep-Alive`
   and `Proxy-Connection`), a `Date` added when absent, the `Vary` values, and the corrected initial age (the larger of now minus `Date`
   and the first member of `Age`, at most 2^31 seconds).
10. When the epoch table exists, build a bare request from the copy and compute the resource key from it. If it differs from the key, tag
    the snapshot with the resource key's slot and the slot's current epoch.
11. When pattern invalidation is enabled and the key maps to a path, take the key's lock and add the key to the pattern index before the
    store, so a removal during the store, including a SieveTinyLFU admission rejection, is reconciled by the removal listener.
12. Store the snapshot in the backing cache, rolling back the index entry if the store fails.

A hit writes the stored headers, the hit header and `Age`, computed as the initial age plus the time since the response was stored.

The response writer:

- treats 1xx responses as informational and does not commit final status
- marks 101 Switching Protocols, flushes, and hijacks as streamed and not cacheable
- preserves `Unwrap` for `http.ResponseController`
- sets the miss header before committing a miss response
- captures body bytes up to `MaxBodySize`

`DefaultCachePolicy` rejects non-cacheable methods, non-cacheable statuses and oversized bodies. It joins all `Cache-Control` lines and takes
the freshness lifetime from the first of `s-maxage`, `max-age`, `Expires` minus `Date` (or minus now without `Date`), and `DefaultTTL` when
the response is `public`. Without any of these the response is not stored. An unparsable `Expires` means already expired. Delta-seconds may be
quoted, an invalid value reads as 0, and values are clamped to 2^31 seconds. `max-age`, `s-maxage` and `Age` share one delta-seconds parser. The TTL is the lifetime minus the corrected initial age, and a
TTL of zero or less is not stored. `DefaultTTL` set to `kioshun.NoExpiration` stores `public` responses with no expiry.

Key generators include:

- `DefaultKeyGenerator`: MD5 of `METHOD scheme://host/path?query`, with the host from `r.Host` and the escaped path
- `KeyWithVaryHeaders`: MD5 of the same, then `"\nName: value"` for each named header
- `KeyWithoutQuery`: plain `METHOD:host/path`, escaped path, no query
- `KeyWithoutQueryHashed`: MD5 of `METHOD host/path`, then `"\nName: value"` for Accept, Accept-Encoding and Authorization
- `KeyWithUserID`: MD5 of `METHOD scheme://host/path?query`, then `"\nName: value"` for the user-id header
- `PathBasedKeyGenerator`: plain `METHOD:host/path`, the same as `KeyWithoutQuery`

Header values are joined over all field lines with ",". A newline cannot appear in a parsed header value or an escaped URI, so the
separators keep distinct requests from sharing a key. `PathExtractorFromKey` returns the part of a `METHOD:host/path` key from the first
"/" after the method, and still reads older `METHOD:/path` keys.

`Invalidate(pattern)` asks the path index for matching keys and deletes them from the cache. Exact patterns match keys stored at one path. Patterns ending in `*` match the base path and its descendants. Without a configured
`PathExtractor`, the index stays empty and `Invalidate` returns 0.

## Algorithm References

The implementation is not a direct copy of any one paper. The terminology and policy design are grounded in these references:

- [SIEVE: Cache eviction can be simple, effective, and scalable](https://www.usenix.org/system/files/nsdi24-zhang-yazhuo.pdf)
- [TinyLFU: A Highly Efficient Cache Admission Policy](https://arxiv.org/abs/1512.00727)
- [An improved data stream summary: the count-min sketch and its applications](https://www.cs.helsinki.fi/u/jilu/paper/countMin.pdf)
- [xxHash specification](https://github.com/Cyan4973/xxHash/blob/dev/doc/xxhash_spec.md)
- [Go `net/http.ResponseController`](https://go.dev/src/net/http/responsecontroller.go)
