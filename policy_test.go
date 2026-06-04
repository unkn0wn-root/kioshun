package kioshun

import (
	"math/rand"
	"testing"
	"time"
)

type polRun struct {
	name string
	pol  EvictionPolicy
	hit  int
	miss int
	size int64
}

func newPolCache(pol EvictionPolicy, cap int64) *Cache[int, int] {
	c, err := New[int, int](Config{
		MaxSize:         cap,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  pol,
		StatsEnabled:    true,
	})
	if err != nil {
		panic(err)
	}
	return c
}

func runTrace(pol EvictionPolicy, cap int64, tr []int) polRun {
	c := newPolCache(pol, cap)
	defer c.Close()

	r := polRun{name: polName(pol), pol: pol}
	r.hit, r.miss = runTraceInto(c, tr)
	r.size = c.Size()
	return r
}

func runTraceInto(c *Cache[int, int], tr []int) (hit, miss int) {
	for _, k := range tr {
		if _, ok := c.Get(k); ok {
			hit++
			continue
		}
		miss++
		if err := c.Set(k, k, time.Hour); err != nil {
			panic(err)
		}
		if err := c.Sync(); err != nil {
			panic(err)
		}
	}
	return hit, miss
}

func countFound(c *Cache[int, int], from, n int) int {
	var hit int
	for k := from; k < from+n; k++ {
		if _, ok := c.Get(k); ok {
			hit++
		}
	}
	return hit
}

func polName(pol EvictionPolicy) string {
	switch pol {
	case LRU:
		return "LRU"
	case LFU:
		return "LFU"
	case FIFO:
		return "FIFO"
	case SieveTinyLFU:
		return "SieveTinyLFU"
	default:
		return "unknown"
	}
}

func hitPct(r polRun) float64 {
	n := r.hit + r.miss
	if n == 0 {
		return 0
	}
	return float64(r.hit) / float64(n)
}

func assertCap(t *testing.T, r polRun, cap int64) {
	t.Helper()
	if r.size > cap {
		t.Fatalf("%s size=%d exceeds cap=%d", r.name, r.size, cap)
	}
}

func zipfTrace(seed int64, n, keys int, s float64) []int {
	r := rand.New(rand.NewSource(seed))
	z := rand.NewZipf(r, s, 1, uint64(keys-1))
	tr := make([]int, n)
	for i := range tr {
		tr[i] = int(z.Uint64())
	}
	return tr
}

func loopTrace(rounds, keys int) []int {
	tr := make([]int, 0, rounds*keys)
	for range rounds {
		for k := range keys {
			tr = append(tr, k)
		}
	}
	return tr
}

func scanTrace(stable, warm, scan int) []int {
	tr := make([]int, 0, stable*warm+scan)
	for range warm {
		for k := range stable {
			tr = append(tr, k)
		}
	}
	for k := range scan {
		tr = append(tr, 1_000_000+k)
	}
	return tr
}

func shiftingTrace(seed int64, phases, phaseLen, hot, cold int) []int {
	r := rand.New(rand.NewSource(seed))
	tr := make([]int, 0, phases*phaseLen)
	for p := range phases {
		base := p * hot
		for range phaseLen {
			if r.Intn(100) < 85 {
				tr = append(tr, base+r.Intn(hot))
			} else {
				tr = append(tr, 1_000_000+r.Intn(cold))
			}
		}
	}
	return tr
}

func burstTrace(seed int64, bursts, hot, cold int) []int {
	r := rand.New(rand.NewSource(seed))
	tr := make([]int, 0, bursts*(hot*6+cold))
	for b := range bursts {
		base := b * hot
		for range 6 {
			for k := range hot {
				tr = append(tr, base+k)
			}
		}
		for range cold {
			tr = append(tr, 2_000_000+r.Intn(100_000))
		}
	}
	return tr
}

func writeHeavyTrace(seed int64, n, hot, cold int) []int {
	r := rand.New(rand.NewSource(seed))
	tr := make([]int, 0, n)
	for range n {
		if r.Intn(100) < 35 {
			tr = append(tr, r.Intn(hot))
		} else {
			tr = append(tr, 3_000_000+r.Intn(cold))
		}
	}
	return tr
}

func TestPolicyZipfHitRatio(t *testing.T) {
	cap := int64(128)
	tr := zipfTrace(11, 30_000, 2_000, 1.12)

	ad := runTrace(SieveTinyLFU, cap, tr)
	fi := runTrace(FIFO, cap, tr)

	t.Logf("zipf admission=%.2f%% fifo=%.2f%%", hitPct(ad)*100, hitPct(fi)*100)
	assertCap(t, ad, cap)
	assertCap(t, fi, cap)
	if ad.hit < fi.hit {
		t.Fatalf("SieveTinyLFU hit count regressed on zipf trace: admission=%d fifo=%d", ad.hit, fi.hit)
	}
}

func TestPolicyScanResistance(t *testing.T) {
	cap := int64(64)
	tr := scanTrace(int(cap), 12, 2_000)

	ad := newPolCache(SieveTinyLFU, cap)
	defer ad.Close()
	lru := newPolCache(LRU, cap)
	defer lru.Close()

	ah, am := runTraceInto(ad, tr)
	lh, lm := runTraceInto(lru, tr)
	as := countFound(ad, 0, int(cap))
	ls := countFound(lru, 0, int(cap))

	t.Logf(
		"scan admission=%.2f%% lru=%.2f%% survivors admission=%d lru=%d",
		float64(ah)/float64(ah+am)*100,
		float64(lh)/float64(lh+lm)*100,
		as,
		ls,
	)
	if ad.Size() > cap {
		t.Fatalf("SieveTinyLFU size=%d exceeds cap=%d", ad.Size(), cap)
	}
	if lru.Size() > cap {
		t.Fatalf("LRU size=%d exceeds cap=%d", lru.Size(), cap)
	}
	if as <= ls {
		t.Fatalf("SieveTinyLFU should retain more warmed keys after scan: admission=%d lru=%d", as, ls)
	}
}

func TestPolicyLoopWorkload(t *testing.T) {
	cap := int64(64)
	tr := loopTrace(80, 80)

	ad := runTrace(SieveTinyLFU, cap, tr)
	lru := runTrace(LRU, cap, tr)

	t.Logf("loop admission=%.2f%% lru=%.2f%%", hitPct(ad)*100, hitPct(lru)*100)
	assertCap(t, ad, cap)
	assertCap(t, lru, cap)
	if ad.hit < lru.hit {
		t.Fatalf("SieveTinyLFU regressed below LRU on loop trace: admission=%d lru=%d", ad.hit, lru.hit)
	}
}

func TestPolicyShiftingHotset(t *testing.T) {
	cap := int64(128)
	tr := shiftingTrace(23, 8, 4_000, 96, 8_000)

	ad := runTrace(SieveTinyLFU, cap, tr)
	lru := runTrace(LRU, cap, tr)

	t.Logf("shifting admission=%.2f%% lru=%.2f%%", hitPct(ad)*100, hitPct(lru)*100)
	assertCap(t, ad, cap)
	assertCap(t, lru, cap)
	if hitPct(ad) < 0.45 {
		t.Fatalf("SieveTinyLFU collapsed on shifting hotset: admission=%.2f%%", hitPct(ad)*100)
	}
}

func TestPolicyBurstRecency(t *testing.T) {
	cap := int64(96)
	tr := burstTrace(37, 40, 48, 120)

	ad := runTrace(SieveTinyLFU, cap, tr)
	fi := runTrace(FIFO, cap, tr)

	t.Logf("burst admission=%.2f%% fifo=%.2f%%", hitPct(ad)*100, hitPct(fi)*100)
	assertCap(t, ad, cap)
	assertCap(t, fi, cap)
	if hitPct(ad) < 0.35 {
		t.Fatalf("SieveTinyLFU collapsed on burst-recency trace: admission=%.2f%%", hitPct(ad)*100)
	}
}

func TestPolicyWriteHeavy(t *testing.T) {
	cap := int64(256)
	tr := writeHeavyTrace(41, 25_000, 128, 25_000)

	ad := runTrace(SieveTinyLFU, cap, tr)
	lru := runTrace(LRU, cap, tr)

	t.Logf("write-heavy admission=%.2f%% lru=%.2f%%", hitPct(ad)*100, hitPct(lru)*100)
	assertCap(t, ad, cap)
	assertCap(t, lru, cap)
	if ad.hit*10 < lru.hit*7 {
		t.Fatalf("SieveTinyLFU regressed too far on write-heavy trace: admission=%d lru=%d", ad.hit, lru.hit)
	}
}

func runTraceSieve(cap int64, tr []int) (r polRun, pcStart, pcEnd int64) {
	c, err := New[int, int](Config{
		MaxSize:         cap,
		ShardCount:      1,
		CleanupInterval: 0,
		DefaultTTL:      time.Hour,
		EvictionPolicy:  SieveTinyLFU,
		StatsEnabled:    true,
	})
	if err != nil {
		panic(err)
	}
	defer c.Close()

	pcStart = c.shards[0].sieve.probationCap
	r = polRun{name: "SieveTinyLFU", pol: SieveTinyLFU}
	r.hit, r.miss = runTraceInto(c, tr)
	r.size = c.Size()
	pcEnd = c.shards[0].sieve.probationCap
	return r, pcStart, pcEnd
}

func TestPolicySieveTinyLFUHitRatio(t *testing.T) {
	cap := int64(128)

	zipf := zipfTrace(11, 30_000, 2_000, 1.12)
	ad, pc0, pc1 := runTraceSieve(cap, zipf)
	fi := runTrace(FIFO, cap, zipf)
	t.Logf("zipf sieve=%.2f%% fifo=%.2f%% probationCap %d->%d",
		hitPct(ad)*100, hitPct(fi)*100, pc0, pc1)
	assertCap(t, ad, cap)
	if ad.hit < fi.hit {
		t.Fatalf("SieveTinyLFU below FIFO on zipf: sieve=%d fifo=%d", ad.hit, fi.hit)
	}

	shift := shiftingTrace(23, 8, 4_000, 96, 8_000)
	sad, spc0, spc1 := runTraceSieve(cap, shift)
	t.Logf("shifting sieve=%.2f%% probationCap %d->%d", hitPct(sad)*100, spc0, spc1)
	assertCap(t, sad, cap)
	if hitPct(sad) < 0.45 {
		t.Fatalf("SieveTinyLFU collapsed on shifting hotset: %.2f%%", hitPct(sad)*100)
	}
}

func TestPolicySieveGrowsProbationOnRecency(t *testing.T) {
	cap := int64(256)
	tr := burstTrace(37, 50, 64, 256)

	ad, pc0, pc1 := runTraceSieve(cap, tr)
	lru := runTrace(LRU, cap, tr)
	t.Logf("burst sieve=%.2f%% lru=%.2f%% probationCap %d->%d",
		hitPct(ad)*100, hitPct(lru)*100, pc0, pc1)
	assertCap(t, ad, cap)

	if pc1 <= pc0 {
		t.Fatalf("probation should grow on a recency-heavy workload: probationCap %d->%d", pc0, pc1)
	}
	if ad.hit*100 < lru.hit*85 {
		t.Fatalf("SieveTinyLFU trails LRU too far on burst: sieve=%d lru=%d (%.1f%% of LRU)",
			ad.hit, lru.hit, float64(ad.hit)/float64(lru.hit)*100)
	}
}
