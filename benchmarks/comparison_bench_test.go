package main_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/coocood/freecache"
	"github.com/dgraph-io/ristretto"
	gocache "github.com/patrickmn/go-cache"
	kioshun "github.com/unkn0wn-root/kioshun"
)

const (
	benchTargetEntries = 100_000
	benchKeySpace      = 50_000
	benchOpCount       = 1 << 16
	benchTTL           = time.Hour
	benchCleanInterval = 5 * time.Minute
	benchMemoryBudget  = 256 * 1024 * 1024
)

var errWriteNotAccepted = errors.New("write not accepted")

type writeMode uint8

const (
	asyncWrites writeMode = iota
	strictWrites
)

type operation uint8

const (
	opGet operation = iota
	opSet
	opDelete
)

type cacheBench interface {
	Set(string, []byte) error
	SetStrict(string, []byte) error
	Get(string) ([]byte, bool)
	Delete(string) bool
	Flush()
	Close() error
}

type workloadOp struct {
	key   string
	value []byte
	op    operation
}

type cacheOptions struct {
	valueSize int
	ttl       bool
}

var cacheOrder = []string{"kioshun", "ristretto", "bigcache", "freecache", "go-cache"}

type KioshunWrapper struct {
	cache *kioshun.Cache[string, []byte]
	ttl   time.Duration
}

func (c *KioshunWrapper) Set(key string, value []byte) error {
	return c.cache.SetAsync(key, value, c.ttl)
}

func (c *KioshunWrapper) SetStrict(key string, value []byte) error {
	return c.cache.Set(key, value, c.ttl)
}

func (c *KioshunWrapper) Get(key string) ([]byte, bool) {
	return c.cache.Get(key)
}

func (c *KioshunWrapper) Delete(key string) bool {
	return c.cache.Delete(key)
}

func (c *KioshunWrapper) Flush() {
	_ = c.cache.Sync()
}

func (c *KioshunWrapper) Close() error {
	return c.cache.Close()
}

type BigCacheWrapper struct {
	cache *bigcache.BigCache
	once  sync.Once
}

func (c *BigCacheWrapper) Set(key string, value []byte) error {
	return c.cache.Set(key, value)
}

func (c *BigCacheWrapper) SetStrict(key string, value []byte) error {
	return c.Set(key, value)
}

func (c *BigCacheWrapper) Get(key string) ([]byte, bool) {
	value, err := c.cache.Get(key)
	return value, err == nil
}

func (c *BigCacheWrapper) Delete(key string) bool {
	return c.cache.Delete(key) == nil
}

func (c *BigCacheWrapper) Flush() {}

func (c *BigCacheWrapper) Close() error {
	var err error
	c.once.Do(func() {
		err = c.cache.Close()
	})
	return err
}

type FreeCacheWrapper struct {
	cache         *freecache.Cache
	expireSeconds int
}

func (c *FreeCacheWrapper) Set(key string, value []byte) error {
	return c.cache.Set([]byte(key), value, c.expireSeconds)
}

func (c *FreeCacheWrapper) SetStrict(key string, value []byte) error {
	return c.Set(key, value)
}

func (c *FreeCacheWrapper) Get(key string) ([]byte, bool) {
	value, err := c.cache.Get([]byte(key))
	return value, err == nil
}

func (c *FreeCacheWrapper) Delete(key string) bool {
	return c.cache.Del([]byte(key))
}

func (c *FreeCacheWrapper) Flush() {}

func (c *FreeCacheWrapper) Close() error {
	c.cache.Clear()
	return nil
}

type GoCacheWrapper struct {
	cache *gocache.Cache
}

func (c *GoCacheWrapper) Set(key string, value []byte) error {
	c.cache.Set(key, value, gocache.DefaultExpiration)
	return nil
}

func (c *GoCacheWrapper) SetStrict(key string, value []byte) error {
	return c.Set(key, value)
}

func (c *GoCacheWrapper) Get(key string) ([]byte, bool) {
	value, found := c.cache.Get(key)
	if !found {
		return nil, false
	}
	return value.([]byte), true
}

func (c *GoCacheWrapper) Delete(key string) bool {
	c.cache.Delete(key)
	return true
}

func (c *GoCacheWrapper) Flush() {}

func (c *GoCacheWrapper) Close() error {
	c.cache.Flush()
	return nil
}

type RistrettoWrapper struct {
	cache *ristretto.Cache
	ttl   time.Duration
}

func (c *RistrettoWrapper) Set(key string, value []byte) error {
	if !c.cache.SetWithTTL(key, value, int64(len(value)), c.ttl) {
		return errWriteNotAccepted
	}
	return nil
}

func (c *RistrettoWrapper) SetStrict(key string, value []byte) error {
	if !c.cache.SetWithTTL(key, value, int64(len(value)), c.ttl) {
		return errWriteNotAccepted
	}
	c.cache.Wait()
	return nil
}

func (c *RistrettoWrapper) Get(key string) ([]byte, bool) {
	value, found := c.cache.Get(key)
	if !found {
		return nil, false
	}
	return value.([]byte), true
}

func (c *RistrettoWrapper) Delete(key string) bool {
	c.cache.Del(key)
	return true
}

func (c *RistrettoWrapper) Flush() {
	c.cache.Wait()
}

func (c *RistrettoWrapper) Close() error {
	c.cache.Close()
	return nil
}

func newBenchCache(tb testing.TB, name string, opts cacheOptions) cacheBench {
	tb.Helper()

	ttl := benchTTL
	kioshunTTL := benchTTL
	goCacheTTL := benchTTL
	freeCacheTTL := int(benchTTL.Seconds())
	bigCacheTTL := benchTTL
	if !opts.ttl {
		ttl = 0
		kioshunTTL = kioshun.NoExpiration
		goCacheTTL = gocache.NoExpiration
		freeCacheTTL = 0
		bigCacheTTL = 24 * time.Hour
	}

	kioshunConfig := kioshun.DefaultConfig()
	kioshunConfig.MaxSize = benchTargetEntries
	kioshunConfig.DefaultTTL = benchTTL
	kioshunConfig.StatsEnabled = false

	switch name {
	case "kioshun":
		return &KioshunWrapper{
			cache: newKioshunCache[string, []byte](tb, kioshunConfig),
			ttl:   kioshunTTL,
		}
	case "ristretto":
		ristrettoCache, err := ristretto.NewCache(&ristretto.Config{
			NumCounters: int64(benchTargetEntries * 10),
			MaxCost:     int64(benchMemoryBudget),
			BufferItems: 64,
			Metrics:     false,
		})
		if err != nil {
			tb.Fatalf("create ristretto: %v", err)
		}
		return &RistrettoWrapper{cache: ristrettoCache, ttl: ttl}
	case "bigcache":
		bigCacheConfig := bigcache.DefaultConfig(bigCacheTTL)
		bigCacheConfig.CleanWindow = benchCleanInterval
		if !opts.ttl {
			bigCacheConfig.CleanWindow = 0
		}
		bigCacheConfig.MaxEntriesInWindow = benchTargetEntries
		bigCacheConfig.MaxEntrySize = opts.valueSize + 256
		bigCacheConfig.HardMaxCacheSize = benchMemoryBudget / (1024 * 1024)
		bigCacheConfig.StatsEnabled = false
		bigCacheConfig.Verbose = false
		bigCache, err := bigcache.New(context.Background(), bigCacheConfig)
		if err != nil {
			tb.Fatalf("create bigcache: %v", err)
		}
		return &BigCacheWrapper{cache: bigCache}
	case "freecache":
		return &FreeCacheWrapper{
			cache:         freecache.NewCache(benchMemoryBudget),
			expireSeconds: freeCacheTTL,
		}
	case "go-cache":
		return &GoCacheWrapper{
			cache: gocache.NewFrom(goCacheTTL, benchCleanInterval, make(map[string]gocache.Item, benchTargetEntries)),
		}
	default:
		tb.Fatalf("unknown benchmark cache %q", name)
		return nil
	}
}

func forEachCache(b *testing.B, opts cacheOptions, fn func(*testing.B, cacheBench)) {
	b.Helper()

	for _, name := range cacheOrder {
		b.Run(name, func(b *testing.B) {
			c := newBenchCache(b, name, opts)
			defer func() {
				b.StopTimer()
				_ = c.Close()
			}()
			fn(b, c)
		})
	}
}

func makeKeys(prefix string, n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s:%06d", prefix, i)
	}
	return keys
}

func makeValue(size int) []byte {
	value := make([]byte, size)
	for i := range value {
		value[i] = byte(i)
	}
	return value
}

func makeValues(sizes ...int) [][]byte {
	values := make([][]byte, len(sizes))
	for i, size := range sizes {
		values[i] = makeValue(size)
	}
	return values
}

func runParallelCycle(b *testing.B, n int, fn func(int)) {
	b.Helper()

	var workers atomic.Uint64
	b.RunParallel(func(pb *testing.PB) {
		start := int((workers.Add(1) * 7919) % uint64(n))
		i := start
		for pb.Next() {
			fn(i)
			i++
			if i == n {
				i = 0
			}
		}
	})
}

func prepopulate(tb testing.TB, c cacheBench, keys []string, value []byte) {
	tb.Helper()
	for _, key := range keys {
		if err := c.Set(key, value); err != nil {
			tb.Fatalf("prepopulate set %q: %v", key, err)
		}
	}
	c.Flush()
}

func makeWorkload(keys []string, values [][]byte, readRatio, writeRatio float64, deleteRatio float64, seed int64) []workloadOp {
	rng := rand.New(rand.NewSource(seed))
	ops := make([]workloadOp, benchOpCount)
	for i := range ops {
		p := rng.Float64()
		op := opDelete
		switch {
		case p < readRatio:
			op = opGet
		case p < readRatio+writeRatio:
			op = opSet
		case p < readRatio+writeRatio+deleteRatio:
			op = opDelete
		default:
			op = opGet
		}
		ops[i] = workloadOp{
			key:   keys[rng.Intn(len(keys))],
			value: values[rng.Intn(len(values))],
			op:    op,
		}
	}
	return ops
}

func makeHotWorkload(hotKeys, coldKeys []string, values [][]byte, seed int64) []workloadOp {
	rng := rand.New(rand.NewSource(seed))
	ops := make([]workloadOp, benchOpCount)
	for i := range ops {
		keys := coldKeys
		if rng.Float64() < 0.8 {
			keys = hotKeys
		}
		op := opSet
		if rng.Float64() < 0.6 {
			op = opGet
		}
		ops[i] = workloadOp{
			key:   keys[rng.Intn(len(keys))],
			value: values[rng.Intn(len(values))],
			op:    op,
		}
	}
	return ops
}

func applyOp(c cacheBench, op workloadOp, mode writeMode) error {
	switch op.op {
	case opGet:
		c.Get(op.key)
	case opSet:
		if mode == strictWrites {
			return c.SetStrict(op.key, op.value)
		}
		return c.Set(op.key, op.value)
	case opDelete:
		c.Delete(op.key)
	}
	return nil
}

func reportWriteFailures(b *testing.B, failures uint64) {
	b.Helper()
	if failures == 0 {
		return
	}
	b.ReportMetric(float64(failures)/float64(b.N), "write_fail/op")
}

func benchmarkSet(b *testing.B, mode writeMode) {
	keys := makeKeys("set", benchTargetEntries)
	value := makeValue(1024)

	forEachCache(b, cacheOptions{valueSize: len(value), ttl: true}, func(b *testing.B, c cacheBench) {
		var failures atomic.Uint64
		b.ResetTimer()
		runParallelCycle(b, len(keys), func(i int) {
			var err error
			if mode == strictWrites {
				err = c.SetStrict(keys[i], value)
			} else {
				err = c.Set(keys[i], value)
			}
			if err != nil {
				failures.Add(1)
			}
		})
		b.StopTimer()
		reportWriteFailures(b, failures.Load())
		c.Flush()
	})
}

func benchmarkGet(b *testing.B, ttl bool) {
	keys := makeKeys("get", benchKeySpace)
	value := makeValue(1024)

	forEachCache(b, cacheOptions{valueSize: len(value), ttl: ttl}, func(b *testing.B, c cacheBench) {
		prepopulate(b, c, keys, value)
		b.ResetTimer()
		runParallelCycle(b, len(keys), func(i int) {
			c.Get(keys[i])
		})
	})
}

func benchmarkWorkload(b *testing.B, mode writeMode, name string, ops []workloadOp, prefillKeys []string, prefillValue []byte) {
	b.Helper()

	b.Run(name, func(b *testing.B) {
		forEachCache(b, cacheOptions{valueSize: len(prefillValue), ttl: true}, func(b *testing.B, c cacheBench) {
			prepopulate(b, c, prefillKeys, prefillValue)
			var failures atomic.Uint64
			b.ResetTimer()
			runParallelCycle(b, len(ops), func(i int) {
				if err := applyOp(c, ops[i], mode); err != nil {
					failures.Add(1)
				}
			})
			b.StopTimer()
			reportWriteFailures(b, failures.Load())
			c.Flush()
		})
	})
}

func benchmarkLargeValues(b *testing.B, mode writeMode) {
	keys := makeKeys("large", benchKeySpace)
	for _, size := range []int{1024, 4096, 16 * 1024, 64 * 1024} {
		b.Run(fmt.Sprintf("%dKB", size/1024), func(b *testing.B) {
			values := makeValues(size)
			ops := makeWorkload(keys, values, 0.35, 0.65, 0, int64(size))
			forEachCache(b, cacheOptions{valueSize: size, ttl: true}, func(b *testing.B, c cacheBench) {
				prepopulate(b, c, keys[:1000], values[0])
				var failures atomic.Uint64
				b.ResetTimer()
				runParallelCycle(b, len(ops), func(i int) {
					if err := applyOp(c, ops[i], mode); err != nil {
						failures.Add(1)
					}
				})
				b.StopTimer()
				reportWriteFailures(b, failures.Load())
				c.Flush()
			})
		})
	}
}

func TestBenchmarkComparisonGetSetup(t *testing.T) {
	for _, ttl := range []bool{true, false} {
		t.Run(fmt.Sprintf("ttl_%t", ttl), func(t *testing.T) {
			keys := makeKeys("sanity-get", benchKeySpace)
			value := makeValue(1024)
			opts := cacheOptions{valueSize: len(value), ttl: ttl}

			for _, name := range cacheOrder {
				t.Run(name, func(t *testing.T) {
					c := newBenchCache(t, name, opts)
					defer c.Close()

					prepopulate(t, c, keys, value)
					for _, key := range keys {
						got, found := c.Get(key)
						if !found {
							t.Fatalf("expected prepopulated key %q to be present", key)
						}
						if len(got) != len(value) {
							t.Fatalf("value length for %q = %d, want %d", key, len(got), len(value))
						}
					}
				})
			}
		})
	}
}

func BenchmarkCacheComparison_Set_Async(b *testing.B) {
	benchmarkSet(b, asyncWrites)
}

func BenchmarkCacheComparison_Set_Strict(b *testing.B) {
	benchmarkSet(b, strictWrites)
}

func BenchmarkCacheComparison_Get_TTL(b *testing.B) {
	benchmarkGet(b, true)
}

func BenchmarkCacheComparison_Get_NoTTL(b *testing.B) {
	benchmarkGet(b, false)
}

func BenchmarkCacheComparison_Mixed_Async(b *testing.B) {
	keys := makeKeys("mixed", benchKeySpace)
	value := makeValue(1024)
	ops := makeWorkload(keys, [][]byte{value}, 0.7, 0.3, 0, 1)
	benchmarkWorkload(b, asyncWrites, "70read_30write", ops, keys, value)
}

func BenchmarkCacheComparison_Mixed_Strict(b *testing.B) {
	keys := makeKeys("mixed", benchKeySpace)
	value := makeValue(1024)
	ops := makeWorkload(keys, [][]byte{value}, 0.7, 0.3, 0, 1)
	benchmarkWorkload(b, strictWrites, "70read_30write", ops, keys, value)
}

func BenchmarkCacheComparison_ReadHeavy_Async(b *testing.B) {
	keys := makeKeys("read-heavy", benchKeySpace)
	value := makeValue(1024)
	ops := makeWorkload(keys, [][]byte{value}, 0.9, 0.1, 0, 2)
	benchmarkWorkload(b, asyncWrites, "90read_10write", ops, keys, value)
}

func BenchmarkCacheComparison_WriteHeavy_Async(b *testing.B) {
	keys := makeKeys("write-heavy", benchKeySpace)
	value := makeValue(1024)
	ops := makeWorkload(keys, [][]byte{value}, 0.1, 0.9, 0, 3)
	benchmarkWorkload(b, asyncWrites, "10read_90write", ops, keys, value)
}

func BenchmarkCacheComparison_HighContention_Async(b *testing.B) {
	hotKeys := makeKeys("hot", 100)
	coldKeys := makeKeys("cold", benchKeySpace)
	value := makeValue(1024)
	ops := makeHotWorkload(hotKeys, coldKeys, [][]byte{value}, 4)
	prefill := append(append([]string{}, hotKeys...), coldKeys...)
	benchmarkWorkload(b, asyncWrites, "80hot_60read", ops, prefill, value)
}

func BenchmarkCacheComparison_RealWorld_Async(b *testing.B) {
	keys := makeKeys("user:profile", benchKeySpace)
	values := makeValues(512, 1024, 1536, 2048, 2560)
	ops := makeWorkload(keys, values, 0.6, 0.35, 0.05, 5)
	benchmarkWorkload(b, asyncWrites, "60read_35write_5delete", ops, keys, values[1])
}

func BenchmarkCacheComparison_RealWorld_Strict(b *testing.B) {
	keys := makeKeys("user:profile", benchKeySpace)
	values := makeValues(512, 1024, 1536, 2048, 2560)
	ops := makeWorkload(keys, values, 0.6, 0.35, 0.05, 5)
	benchmarkWorkload(b, strictWrites, "60read_35write_5delete", ops, keys, values[1])
}

func BenchmarkCacheComparison_LargeValues_Async(b *testing.B) {
	benchmarkLargeValues(b, asyncWrites)
}

func BenchmarkCacheComparison_LargeValues_Strict(b *testing.B) {
	benchmarkLargeValues(b, strictWrites)
}
