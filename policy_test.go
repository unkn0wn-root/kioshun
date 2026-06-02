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
	for r := 0; r < rounds; r++ {
		for k := 0; k < keys; k++ {
			tr = append(tr, k)
		}
	}
	return tr
}

func scanTrace(stable, warm, scan int) []int {
	tr := make([]int, 0, stable*warm+scan)
	for i := 0; i < warm; i++ {
		for k := 0; k < stable; k++ {
			tr = append(tr, k)
		}
	}
	for k := 0; k < scan; k++ {
		tr = append(tr, 1_000_000+k)
	}
	return tr
}

func shiftingTrace(seed int64, phases, phaseLen, hot, cold int) []int {
	r := rand.New(rand.NewSource(seed))
	tr := make([]int, 0, phases*phaseLen)
	for p := 0; p < phases; p++ {
		base := p * hot
		for i := 0; i < phaseLen; i++ {
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
	for b := 0; b < bursts; b++ {
		base := b * hot
		for rep := 0; rep < 6; rep++ {
			for k := 0; k < hot; k++ {
				tr = append(tr, base+k)
			}
		}
		for i := 0; i < cold; i++ {
			tr = append(tr, 2_000_000+r.Intn(100_000))
		}
	}
	return tr
}

func writeHeavyTrace(seed int64, n, hot, cold int) []int {
	r := rand.New(rand.NewSource(seed))
	tr := make([]int, 0, n)
	for i := 0; i < n; i++ {
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

	t.Logf("scan admission=%.2f%% lru=%.2f%% survivors admission=%d lru=%d",
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
