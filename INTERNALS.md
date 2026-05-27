# Internals

```

┌──────────────────────────────────────────────────────────────────────────┐
│                            Kioshun Cache                                 │
├─────────────────┬─────────────────┬─────────────────┬────────────────────┤
│     Shard 0     │     Shard 1     │     Shard 2     │  ...  │   Shard N  │
│   RWMutex       │   RWMutex       │   RWMutex       │       │   RWMutex  │
│                 │                 │                 │       │            │
│ ┌─────────────┐ │ ┌─────────────┐ │ ┌─────────────┐ │       │ ┌─────────┐│
│ │ Hash Map    │ │ │ Hash Map    │ │ │ Hash Map    │ │       │ │Hash Map ││
│ │ K -> Item   │ │ │ K -> Item   │ │ │ K -> Item   │ │       │ │K -> Item││
│ └─────────────┘ │ └─────────────┘ │ └─────────────┘ │       │ └─────────┘│
│                 │                 │                 │       │            │
│ ┌─────────────┐ │ ┌─────────────┐ │ ┌─────────────┐ │       │ ┌─────────┐│
│ │ LRU List    │ │ │ LRU List    │ │ │ LRU List    │ │       │ │LRU List ││
│ │ head ←→ tail│ │ │ head ←→ tail│ │ │ head ←→ tail│ │       │ │head↔tail││
│ │ (sentinel)  │ │ │ (sentinel)  │ │ │ (sentinel)  │ │       │ │(sentinel││
│ └─────────────┘ │ └─────────────┘ │ └─────────────┘ │       │ └─────────┘│
│                 │                 │                 │       │            │
│ ┌─────────────┐ │ ┌─────────────┐ │ ┌─────────────┐ │       │ ┌─────────┐│
│ │LFU FreqList │ │ │LFU FreqList │ │ │LFU FreqList │ │       │ │LFU Freq ││
│ │(if LFU mode)│ │ │(if LFU mode)│ │ │(if LFU mode)│ │       │ │(LFU only││
│ └─────────────┘ │ └─────────────┘ │ └─────────────┘ │       │ └─────────┘│
│                 │                 │                 │       │            │
│ ┌─────────────┐ │ ┌─────────────┐ │ ┌─────────────┐ │       │ ┌─────────┐│
│ │Admission    │ │ │Admission    │ │ │Admission    │ │       │ │Admission││
│ │Filter       │ │ │Filter       │ │ │Filter       │ │       │ │Filter   ││
│ │(AdmLFU only)│ │ │(AdmLFU only)│ │ │(AdmLFU only)│ │       │ │(AdmLFU) ││
│ └─────────────┘ │ └─────────────┘ │ └─────────────┘ │       │ └─────────┘│
│                 │                 │                 │       │            │
│ Stats Counter   │ Stats Counter   │ Stats Counter   │       │ Stats      │
└─────────────────┴─────────────────┴─────────────────┴────────────────────┘

```

- Keys are sharded by a 64-bit hasher: integers are avalanched, short strings (≤8B) use FNV-1a, and longer strings use xxHash64. Shard index = `hash & (shardCount-1)`.
- Each shard maintains an independent read-write mutex
- Shard count auto-detected based on CPU cores (default: 4× CPU cores, max 256)
- Object pooling reduces GC pressure

## Eviction Policy

### LRU (Least Recently Used)
**Implementation**: Circular doubly-linked list with sentinel nodes
- **Access**: Move item to head position in O(1) - bypasses null checks using sentinels
- **Eviction**: Remove least recent item (tail.prev) in O(1)
- **Memory**: Minimal overhead per item (2 pointers: prev, next)
- head ←→ item1 ←→ item2 ←→ ... ←→ itemN ←→ tail (sentinels)

**LRU Operations:**
```go
// O(1) access update - no null checks needed due to sentinels
moveToLRUHead(item):
  if head.next == item: return  // already at head
  remove item from current position
  insert item after head sentinel

// O(1) eviction - always valid due to sentinel structure
evictLRU():
  victim = tail.prev           // LRU item
  removeFromLRU(victim)        // unlink from list
  delete from hashmap          // remove from shard.data
```

### LFU (Least Frequently Used) - **Frequency-Node Architecture**
**Implementation**: Doubly-linked list of frequency buckets, each containing items with same access count
- **Access**: Increment frequency and move item to next bucket in O(1) amortized
- **Eviction**: Remove item from lowest frequency bucket in O(1)
- **Memory**: Additional frequency tracking per item + bucket management

**LFU Structure:**
```
Frequency Buckets (sorted by access count):
┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐
│ Freq: 1  │←→  │ Freq: 2  │←→  │ Freq: 5  │←→  │ Freq: 8  │
│ item1    │    │ item3    │    │ item2    │    │ item4    │
│ item5    │    │ item7    │    │          │    │          │
└──────────┘    └──────────┘    └──────────┘    └──────────┘
```

**LFU Operations:**
```go
// O(1) amortized frequency increment
increment(item):
  currentBucket = itemFreq[item]
  newFreq = currentBucket.freq + 1

  // Move to next bucket or create new one
  if nextBucket.freq == newFreq:
    target = nextBucket
  else:
    target = createBucket(newFreq) // splice after current

  move item from currentBucket to target
  if currentBucket.isEmpty(): removeBucket(currentBucket)

// O(1) eviction from lowest frequency bucket
evictLFU():
  lowestBucket = head.next     // first non-sentinel bucket
  victim = any item from lowestBucket.items
  remove victim from bucket and hashmap
```

### FIFO (First In, First Out)
**Implementation**: Uses LRU list structure but treats it as insertion-order queue
- **Access**: No position updates on access (maintains insertion order)
- **Eviction**: Remove oldest inserted item (at tail.prev) in O(1)

### SieveTinyLFU (Probation-SIEVE TinyLFU) - **Default**
**Implementation**: segmented admission policy with a TinyLFU sketch, probation FIFO, main SIEVE queue, and compact ghost queue.
- **Access**: update the TinyLFU sketch on hits and misses. Main-cache hits set a visited bit instead of moving list nodes.
- **Insertion**: new items enter probation. Items seen in the ghost queue enter main immediately.
- **Promotion**: probation items with repeated hits move to main with `visited=true`.
- **Eviction**: probation drops one-hit items quickly; main uses a SIEVE hand that clears visited bits before evicting.
- **Adaptation**: shard-local controller shifts capacity between probation and main using ghost hits, probation evictions, promotions, and main hits.

**SieveTinyLFU Architecture:**
```
Shard
├── TinyLFU sketch: 4-bit Count-Min Sketch, aged by halving
├── Probation FIFO (P): new and unproven items
├── Main SIEVE queue (M): reused items, lazy visited-bit retention
└── Ghost queue (G): hash plus compact tag record of cold probation evictions
```

**Admission Control Components:**

1. **TinyLFU sketch (`countMinSketch`)**
   - 4-bit counters packed in `uint64` words.
   - Four rotated hash indexes using the cache hash avalanche.
   - Records successful `Get`, missed `Get`, new `Set`, and existing-key `Set`.
   - Ages by halving all counters after a fixed increment window.

2. **Probation queue (`P`)**
   - FIFO segment for new items.
   - One-hit items are evicted quickly and recorded in the ghost queue.
   - Reused items are promoted to main.

3. **Main queue (`M`)**
   - SIEVE-style list with one moving hand.
   - Hits set `visited=true`.
   - Eviction clears visited bits first; an unvisited item becomes the victim.

4. **Ghost queue (`G`)**
   - Fixed-size ring of evicted probation hashes.
   - A ghost hit means the key has recent reuse pressure, so the new item enters main.

5. **Adaptive split**
   - `Config.ProbationRatio` controls initial probation capacity.
   - `Config.GhostRatio` controls ghost capacity relative to main.
   - `Config.Adapt` enables the controller that adjusts probation/main split.

**Core Admission Flow:**
```go
on get(key):
    sketch.increment(hash(key))
    if hit in main: item.visited = true
    if hit in probation: item.reuse++
    if probation reuse is high: promote to main

on set(new key):
    sketch.increment(hash(key))
    if ghost.has(hash): insert into main
    else: insert into probation
    while shard is over capacity:
        evict from probation or main
```

## Async Write Event Loop

`Set` is enqueue-only on the caller path. It computes the shard hash and absolute
expiration, then pushes a command into the owning shard's bounded write queue.

Each shard has one worker goroutine that drains up to `Config.WriteBatchSize`
commands, takes the shard write lock once, and applies map updates, policy
metadata, SieveTinyLFU admission, evictions, and queue maintenance in FIFO order.
Synchronous callers can also help drain their own shard behind the same drain
mutex, preserving single-consumer queue semantics while avoiding a worker wakeup
on the strict mutation path.

`SetSync`, `Delete`, and `Import` wait for the affected shard commands to be
processed. `Wait`, `Clear`, and `Close` use global shard barriers when they need
committed state across the whole cache. `Wait` is the global fence: all writes
accepted before the barriers are visible after it returns.

## Eviction Policies

```go
const (
    LRU        EvictionPolicy = iota // Least Recently Used
    LFU                              // Least Frequently Used
    FIFO                             // First In, First Out
    SieveTinyLFU                     // Probation-SIEVE TinyLFU admission (default)
)
```
