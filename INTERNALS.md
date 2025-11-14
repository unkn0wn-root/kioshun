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

### AdmissionLFU (Adaptive Admission-controlled Least Frequently Used) - **Default**
**Implementation**: Approximate LFU with admission control and scan resistance
- **Access**: Update frequency counter in O(1) using Count-Min Sketch, no heap maintenance
- **Eviction**: Random sampling (default 5 items, max 20) with LFU selection in O(k) where k = sample size
- **Admission Control**: Multi-layered frequency-based admission with adaptive thresholds
- **Scan Resistance**: Detects and adapts to scanning workloads automatically
- Frequency bloom filter (10×shard size) + doorkeeper bloom filter (1/8 size) + workload detector per shard

**AdmissionLFU Architecture:**
```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                          Adaptive AdmissionLFU Shard                            │
├─────────────────────────────┬───────────────────────────────────────────────────┤
│ Adaptive Admission Filter   │                Cache Items                        │
│  ┌─────────────────────────┐│  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐                  │
│  │  Frequency Bloom Filter ││  │Item │ │Item │ │Item │ │Item │ ...              │
│  │  Count-Min Sketch       ││  │Freq:│ │Freq:│ │Freq:│ │Freq:│                  │
│  │  4-bit counters x10*cap ││  │  5  │ │  12 │ │  3  │ │  8  │                  │
│  │  Hash: 4 functions      ││  └─────┘ └─────┘ └─────┘ └─────┘                  │
│  │  Auto-aging threshold   ││                                                   │
│  └─────────────────────────┘│  Random Sample 5 → Compare Frequencies            │
│  ┌─────────────────────────┐│  Victim Selection: Min(freq, lastAccess)          │
│  │    Doorkeeper Filter    ││  ↓                                                │
│  │    Bloom Filter         ││  Adaptive Admission Decision                      │
│  │    Size: cap/8          ││                                                   │
│  │    Hash: 3 functions    ││  Normal Mode:                                     │
│  │    Reset: 1min interval ││  • freq ≥ 3: Always admit                         │
│  └─────────────────────────┘│  • freq > victim: Always admit                    │
│  ┌─────────────────────────┐│  • freq = victim & freq > 0: Recency-based        │
│  │   Workload Detector     ││  • else: Adaptive probability 5-95%               │
│  │   Sequential patterns   ││                                                   │
│  │   Admission rate track  ││  Scan Mode (detected patterns):                   │
│  │   Miss streak tracking  ││  • Recency-based admission                        │
│  │   Adaptive probability  ││  • Lower admission threshold                      │
│  └─────────────────────────┘│  • Protect against cache pollution                │
└─────────────────────────────┴───────────────────────────────────────────────────┘
```

**Admission Control Components:**

1. **Frequency Estimation (Count-Min Sketch)**:
   - 4-bit counters packed in uint64 arrays (16 counters per word)
   - 4 hash functions with xxHash64-based avalanche mixing
   - Automatic aging: counters halved when total increments ≥ size × 10
   - Size: 10× shard capacity counters (min 1024 per shard)

2. **Doorkeeper Bloom Filter**:
   - 3 hash functions for recent access tracking
   - Size: 1/8 of frequency filter size for memory efficiency
   - Periodic reset every 1 minute (configurable via AdmissionResetInterval)
   - Immediate admission for items in doorkeeper (bypass frequency check)

3. **Workload Detector (Scan Resistance)**:
   - Tracks consecutive admission rejections (fast signal of cold scans)
   - Uses an admissions/sec signal to flag sudden bursts (defaults: 100 req/s, 50 misses)
   - Switches to a more recency-biased admission during detected scans (recency window defaults: 100 ms / 1 s)

   Override these thresholds via `Config.AdmissionScanRateThreshold`, `Config.AdmissionScanMissThreshold`, `Config.AdmissionScanRecencyThreshold`, and `Config.AdmissionRecencyTieBreak` while keeping the same defaults when unset.

4. **Adaptive Admission Algorithm**:
   ```
   Phase 1: Doorkeeper Check
   if in_doorkeeper(key):
       refresh_doorkeeper(key)
       return ADMIT

   Phase 2: Scan Detection
   if detect_scan(key):
       if victim_age > 100ms:
           return ADMIT    // Prefer recent items during scan
       else:
           return ADMIT if hash(key) % 100 < min_probability(5%)

   Phase 3: Update Frequency & Doorkeeper
   new_freq = frequency_filter.increment(key)
   doorkeeper.add(key)

   Phase 4: Adaptive Admission Decision
   if new_freq >= 3:                    // High frequency guarantee
       return ADMIT
   elif new_freq > victim_frequency:    // Better than victim
       return ADMIT
   elif new_freq == victim_frequency && new_freq > 0:  // Recency tie-breaking
       return ADMIT if victim_age > 1_second
   else:                                // Adaptive probabilistic admission
       probability = adaptive_probability(5-95%) - (victim_frequency * 10)
       return ADMIT if hash(key) % 100 < max(probability, 5%)

   Phase 5: Probability Adjustment
   adjust_probability_based_on_eviction_pressure()
   ```

5. **Adaptive Probability Control**:
   - Dynamic admission probability between 5% and 95%
   - Decreases probability (-5%) when eviction rate > 100/second
   - Increases probability (+5%) when eviction rate < 10/second
   - Eviction pressure monitored every second

## Eviction Policies

```go
const (
    LRU        EvictionPolicy = iota // Least Recently Used
    LFU                              // Least Frequently Used
    FIFO                             // First In, First Out
    AdmissionLFU                     // Sampled LFU with admission control (default)
)
```
