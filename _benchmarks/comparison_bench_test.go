package main_test

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"testing"
	"time"

	cache "github.com/unkn0wn-root/kioshun"

	"github.com/allegro/bigcache/v3"
	"github.com/coocood/freecache"
	"github.com/dgraph-io/ristretto"
	gocache "github.com/patrickmn/go-cache"
)

// Test data structures
type CacheInterface interface {
	Set(key string, value []byte) error
	Get(key string) ([]byte, bool)
	Delete(key string) bool
	Clear()
	Size() int64
	Close() error
}

// Wrapper for kioshun cache
type KioshunWrapper struct {
	cache *cache.InMemoryCache[string, []byte]
}

func (c *KioshunWrapper) Set(key string, value []byte) error {
	return c.cache.Set(key, value, 1*time.Hour)
}

func (c *KioshunWrapper) Get(key string) ([]byte, bool) {
	return c.cache.Get(key)
}

func (c *KioshunWrapper) Delete(key string) bool {
	return c.cache.Delete(key)
}

func (c *KioshunWrapper) Clear() {
	c.cache.Clear()
}

func (c *KioshunWrapper) Size() int64 {
	return c.cache.Size()
}

func (c *KioshunWrapper) Close() error {
	return c.cache.Close()
}

// Wrapper for BigCache
type BigCacheWrapper struct {
	cache *bigcache.BigCache
}

func (b *BigCacheWrapper) Set(key string, value []byte) error {
	return b.cache.Set(key, value)
}

func (b *BigCacheWrapper) Get(key string) ([]byte, bool) {
	val, err := b.cache.Get(key)
	if err != nil {
		return nil, false
	}
	return val, true
}

func (b *BigCacheWrapper) Delete(key string) bool {
	err := b.cache.Delete(key)
	return err == nil
}

func (b *BigCacheWrapper) Clear() {
	b.cache.Reset()
}

func (b *BigCacheWrapper) Size() int64 {
	return int64(b.cache.Len())
}

func (b *BigCacheWrapper) Close() error {
	return b.cache.Close()
}

// Wrapper for FreeCache
type FreeCacheWrapper struct {
	cache *freecache.Cache
	mu    sync.RWMutex
	count int64
}

func (f *FreeCacheWrapper) Set(key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	keyBytes := []byte(key)
	_, err := f.cache.Get(keyBytes)
	if err != nil {
		f.count++
	}
	return f.cache.Set(keyBytes, value, 3600)
}

func (f *FreeCacheWrapper) Get(key string) ([]byte, bool) {
	val, err := f.cache.Get([]byte(key))
	if err != nil {
		return nil, false
	}
	return val, true
}

func (f *FreeCacheWrapper) Delete(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	keyBytes := []byte(key)
	_, err := f.cache.Get(keyBytes)
	if err == nil {
		f.count--
		return f.cache.Del(keyBytes)
	}
	return false
}

func (f *FreeCacheWrapper) Clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cache.Clear()
	f.count = 0
}

func (f *FreeCacheWrapper) Size() int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.count
}

func (f *FreeCacheWrapper) Close() error {
	f.cache.Clear()
	return nil
}

// Wrapper for go-cache
type GoCacheWrapper struct {
	cache *gocache.Cache
}

func (g *GoCacheWrapper) Set(key string, value []byte) error {
	g.cache.Set(key, value, gocache.DefaultExpiration)
	return nil
}

func (g *GoCacheWrapper) Get(key string) ([]byte, bool) {
	val, found := g.cache.Get(key)
	if !found {
		return nil, false
	}
	return val.([]byte), true
}

func (g *GoCacheWrapper) Delete(key string) bool {
	g.cache.Delete(key)
	return true
}

func (g *GoCacheWrapper) Clear() {
	g.cache.Flush()
}

func (g *GoCacheWrapper) Size() int64 {
	return int64(g.cache.ItemCount())
}

func (g *GoCacheWrapper) Close() error {
	g.cache.Flush()
	return nil
}

// Wrapper for Ristretto
type RistrettoWrapper struct {
	cache *ristretto.Cache
}

func (r *RistrettoWrapper) Set(key string, value []byte) error {
	r.cache.Set(key, value, 1)
	return nil
}

func (r *RistrettoWrapper) Get(key string) ([]byte, bool) {
	val, found := r.cache.Get(key)
	if !found {
		return nil, false
	}
	return val.([]byte), true
}

func (r *RistrettoWrapper) Delete(key string) bool {
	r.cache.Del(key)
	return true
}

func (r *RistrettoWrapper) Clear() {
	r.cache.Clear()
}

func (r *RistrettoWrapper) Size() int64 {
	return int64(r.cache.Metrics.KeysAdded() - r.cache.Metrics.KeysEvicted())
}

func (r *RistrettoWrapper) Close() error {
	r.cache.Close()
	return nil
}

// Cache factory functions
func createCaches() map[string]CacheInterface {
	caches := make(map[string]CacheInterface)

	// Kioshun cache
	kioshuConfig := cache.Config{
		MaxSize:         100000,
		ShardCount:      runtime.NumCPU() * 4,
		CleanupInterval: 5 * time.Minute,
		DefaultTTL:      1 * time.Hour,
		EvictionPolicy:  cache.AdmissionLFU,
		StatsEnabled:    false,
	}
	caches["kioshun"] = &KioshunWrapper{cache: cache.New[string, []byte](kioshuConfig)}

	// BigCache
	bigCacheConfig := bigcache.DefaultConfig(1 * time.Hour)
	// Find the next power of two that's >= runtime.NumCPU()
	shards := 1
	for shards < runtime.NumCPU() {
		shards *= 2
	}
	bigCacheConfig.Shards = shards
	bigCacheConfig.MaxEntriesInWindow = 100000
	bigCacheConfig.MaxEntrySize = 64 * 1024
	bigCacheConfig.Verbose = false
	bigCacheConfig.HardMaxCacheSize = 256 // MB
	bigCache, err := bigcache.New(context.Background(), bigCacheConfig)
	if err == nil {
		caches["bigcache"] = &BigCacheWrapper{cache: bigCache}
	}

	// FreeCache (128MB)
	freeCache := freecache.NewCache(128 * 1024 * 1024)
	caches["freecache"] = &FreeCacheWrapper{cache: freeCache}

	// Go-cache
	goCache := gocache.New(1*time.Hour, 5*time.Minute)
	caches["go-cache"] = &GoCacheWrapper{cache: goCache}

	// Ristretto
	ristrettoConfig := &ristretto.Config{
		NumCounters: 1000000,
		MaxCost:     100000,
		BufferItems: 64,
	}
	ristrettoCache, err := ristretto.NewCache(ristrettoConfig)
	if err == nil {
		caches["ristretto"] = &RistrettoWrapper{cache: ristrettoCache}
	}

	return caches
}

// Benchmark comparison tests
func BenchmarkCacheComparison_Set(b *testing.B) {
	caches := createCaches()
	defer func() {
		for _, cache := range caches {
			cache.Close()
		}
	}()

	value := make([]byte, 1024)
	for i := range value {
		value[i] = byte(i % 256)
	}

	for name, cache := range caches {
		b.Run(name, func(b *testing.B) {
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					cache.Set(fmt.Sprintf("key_%d", i), value)
					i++
				}
			})
		})
	}
}

func BenchmarkCacheComparison_Get(b *testing.B) {
	caches := createCaches()
	defer func() {
		for _, cache := range caches {
			cache.Close()
		}
	}()

	value := make([]byte, 1024)
	for i := range value {
		value[i] = byte(i % 256)
	}

	// Pre-populate all caches
	for _, cache := range caches {
		for i := 0; i < 10000; i++ {
			cache.Set(fmt.Sprintf("key_%d", i), value)
		}
	}

	for name, cache := range caches {
		b.Run(name, func(b *testing.B) {
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					cache.Get(fmt.Sprintf("key_%d", i%10000))
					i++
				}
			})
		})
	}
}

func BenchmarkCacheComparison_Mixed(b *testing.B) {
	caches := createCaches()
	defer func() {
		for _, cache := range caches {
			cache.Close()
		}
	}()

	value := make([]byte, 1024)
	for i := range value {
		value[i] = byte(i % 256)
	}

	for name, cache := range caches {
		b.Run(name, func(b *testing.B) {
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				localRand := rand.New(rand.NewSource(time.Now().UnixNano()))
				i := 0
				for pb.Next() {
					key := fmt.Sprintf("key_%d", i%10000)

					if localRand.Float64() < 0.7 {
						cache.Get(key)
					} else {
						cache.Set(key, value)
					}
					i++
				}
			})
		})
	}
}

func BenchmarkCacheComparison_HighContention(b *testing.B) {
	caches := createCaches()
	defer func() {
		for _, cache := range caches {
			cache.Close()
		}
	}()

	value := make([]byte, 1024)
	for i := range value {
		value[i] = byte(i % 256)
	}

	// Pre-populate with hot keys
	hotKeys := 100
	for _, cache := range caches {
		for i := 0; i < hotKeys; i++ {
			cache.Set(fmt.Sprintf("hot_key_%d", i), value)
		}
	}

	for name, cache := range caches {
		b.Run(name, func(b *testing.B) {
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				localRand := rand.New(rand.NewSource(time.Now().UnixNano()))
				i := 0
				for pb.Next() {
					// 80% of operations hit the same hot keys
					var key string
					if localRand.Float64() < 0.8 {
						key = fmt.Sprintf("hot_key_%d", localRand.Intn(hotKeys))
					} else {
						key = fmt.Sprintf("cold_key_%d", i)
					}

					if localRand.Float64() < 0.6 {
						cache.Get(key)
					} else {
						cache.Set(key, value)
					}
					i++
				}
			})
		})
	}
}

func BenchmarkCacheComparison_LargeValues(b *testing.B) {
	caches := createCaches()
	defer func() {
		for _, cache := range caches {
			cache.Close()
		}
	}()

	valueSizes := []int{1024, 4096, 16384, 65536} // 1KB to 64KB

	for _, size := range valueSizes {
		value := make([]byte, size)
		for i := range value {
			value[i] = byte(i % 256)
		}

		for name, cache := range caches {
			b.Run(fmt.Sprintf("%s_size_%dKB", name, size/1024), func(b *testing.B) {
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					i := 0
					for pb.Next() {
						key := fmt.Sprintf("key_%d", i%1000)
						if i%3 == 0 {
							cache.Get(key)
						} else {
							cache.Set(key, value)
						}
						i++
					}
				})
			})
		}
	}
}

func BenchmarkCacheComparison_Scalability(b *testing.B) {
	goroutineCounts := []int{1, 10, 50, 100, 500}

	for _, goroutineCount := range goroutineCounts {
		b.Run(fmt.Sprintf("goroutines_%d", goroutineCount), func(b *testing.B) {
			caches := createCaches()
			defer func() {
				for _, cache := range caches {
					cache.Close()
				}
			}()

			value := make([]byte, 1024)
			for i := range value {
				value[i] = byte(i % 256)
			}

			for name, cache := range caches {
				b.Run(name, func(b *testing.B) {
					b.ResetTimer()

					var wg sync.WaitGroup
					opsPerGoroutine := b.N / goroutineCount

					for g := 0; g < goroutineCount; g++ {
						wg.Add(1)
						go func(goroutineID int) {
							defer wg.Done()
							localRand := rand.New(rand.NewSource(time.Now().UnixNano() + int64(goroutineID)))

							for i := 0; i < opsPerGoroutine; i++ {
								key := fmt.Sprintf("key_%d_%d", goroutineID, i)

								if localRand.Float64() < 0.7 {
									cache.Set(key, value)
								} else {
									cache.Get(key)
								}
							}
						}(g)
					}
					wg.Wait()
				})
			}
		})
	}
}

func BenchmarkCacheComparison_MemoryEfficiency(b *testing.B) {
	caches := createCaches()
	defer func() {
		for _, cache := range caches {
			cache.Close()
		}
	}()

	value := make([]byte, 1024)
	for i := range value {
		value[i] = byte(i % 256)
	}

	for name, cache := range caches {
		b.Run(name, func(b *testing.B) {
			var m1, m2 runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&m1)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cache.Set(fmt.Sprintf("key_%d", i), value)
			}
			b.StopTimer()

			runtime.GC()
			runtime.ReadMemStats(&m2)

			b.ReportMetric(float64(m2.Alloc-m1.Alloc)/float64(b.N), "bytes/op")
		})
	}
}

func BenchmarkCacheComparison_ReadHeavy(b *testing.B) {
	caches := createCaches()
	defer func() {
		for _, cache := range caches {
			cache.Close()
		}
	}()

	value := make([]byte, 1024)
	for i := range value {
		value[i] = byte(i % 256)
	}

	// Pre-populate all caches
	for _, cache := range caches {
		for i := 0; i < 10000; i++ {
			cache.Set(fmt.Sprintf("key_%d", i), value)
		}
	}

	for name, cache := range caches {
		b.Run(name, func(b *testing.B) {
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				localRand := rand.New(rand.NewSource(time.Now().UnixNano()))
				i := 0
				for pb.Next() {
					key := fmt.Sprintf("key_%d", i%10000)

					// 90% reads, 10% writes
					if localRand.Float64() < 0.9 {
						cache.Get(key)
					} else {
						cache.Set(key, value)
					}
					i++
				}
			})
		})
	}
}

func BenchmarkCacheComparison_WriteHeavy(b *testing.B) {
	caches := createCaches()
	defer func() {
		for _, cache := range caches {
			cache.Close()
		}
	}()

	value := make([]byte, 1024)
	for i := range value {
		value[i] = byte(i % 256)
	}

	for name, cache := range caches {
		b.Run(name, func(b *testing.B) {
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				localRand := rand.New(rand.NewSource(time.Now().UnixNano()))
				i := 0
				for pb.Next() {
					key := fmt.Sprintf("key_%d", i%10000)

					// 90% writes, 10% reads
					if localRand.Float64() < 0.9 {
						cache.Set(key, value)
					} else {
						cache.Get(key)
					}
					i++
				}
			})
		})
	}
}

func BenchmarkCacheComparison_RealWorldWorkload(b *testing.B) {
	caches := createCaches()
	defer func() {
		for _, cache := range caches {
			cache.Close()
		}
	}()

	// Different sized values to simulate real-world data
	values := make([][]byte, 5)
	for i := range values {
		size := 512 * (i + 1) // 512B to 2.5KB
		values[i] = make([]byte, size)
		for j := range values[i] {
			values[i][j] = byte(j % 256)
		}
	}

	for name, cache := range caches {
		b.Run(name, func(b *testing.B) {
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				localRand := rand.New(rand.NewSource(time.Now().UnixNano()))
				i := 0
				for pb.Next() {
					key := fmt.Sprintf("user:profile:%d", i%50000)
					value := values[localRand.Intn(len(values))]

					// Realistic access pattern: 60% reads, 35% writes, 5% deletes
					operation := localRand.Float64()
					switch {
					case operation < 0.6:
						cache.Get(key)
					case operation < 0.95:
						cache.Set(key, value)
					default:
						cache.Delete(key)
					}
					i++
				}
			})
		})
	}
}
