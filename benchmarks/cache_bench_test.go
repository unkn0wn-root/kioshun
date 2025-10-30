package main_test

import (
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"testing"
	"time"

	cache "github.com/unkn0wn-root/kioshun"
)

func BenchmarkCacheSet(b *testing.B) {
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 1*time.Hour)
			i++
		}
	})
}

func BenchmarkCacheGet(b *testing.B) {
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	// Pre-populate cache
	for i := 0; i < 10000; i++ {
		cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 1*time.Hour)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.Get(fmt.Sprintf("key%d", i%10000))
			i++
		}
	})
}

func BenchmarkCacheGetMiss(b *testing.B) {
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.Get(fmt.Sprintf("nonexistent%d", i))
			i++
		}
	})
}

func BenchmarkCacheSetGet(b *testing.B) {
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key%d", i%1000)
			if i%2 == 0 {
				cache.Set(key, fmt.Sprintf("value%d", i), 1*time.Hour)
			} else {
				cache.Get(key)
			}
			i++
		}
	})
}

func BenchmarkCacheHeavyRead(b *testing.B) {
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	// Pre-populate cache
	for i := 0; i < 1000; i++ {
		cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 1*time.Hour)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			// 90% reads, 10% writes
			if i%10 == 0 {
				cache.Set(fmt.Sprintf("key%d", i%1000), fmt.Sprintf("value%d", i), 1*time.Hour)
			} else {
				cache.Get(fmt.Sprintf("key%d", i%1000))
			}
			i++
		}
	})
}

func BenchmarkCacheHeavyWrite(b *testing.B) {
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			// 90% writes, 10% reads
			if i%10 == 0 {
				cache.Get(fmt.Sprintf("key%d", i%1000))
			} else {
				cache.Set(fmt.Sprintf("key%d", i%1000), fmt.Sprintf("value%d", i), 1*time.Hour)
			}
			i++
		}
	})
}

func BenchmarkCacheExpiration(b *testing.B) {
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 1*time.Millisecond)
			i++
		}
	})
}

func BenchmarkCacheSize(b *testing.B) {
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	// Pre-populate cache
	for i := 0; i < 1000; i++ {
		cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 1*time.Hour)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.Size()
	}
}

func BenchmarkCacheStats(b *testing.B) {
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	// Pre-populate cache and generate some stats
	for i := 0; i < 1000; i++ {
		cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 1*time.Hour)
	}
	for i := 0; i < 500; i++ {
		cache.Get(fmt.Sprintf("key%d", i))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.Stats()
	}
}

func BenchmarkCacheDelete(b *testing.B) {
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	// Pre-populate cache
	for i := 0; i < b.N; i++ {
		cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 1*time.Hour)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.Delete(fmt.Sprintf("key%d", i))
	}
}

func BenchmarkCacheEviction(b *testing.B) {
	config := cache.Config{
		MaxSize:         1000,
		ShardCount:      16,
		CleanupInterval: 1 * time.Minute,
		DefaultTTL:      1 * time.Hour,
		EvictionPolicy:  cache.LRU,
		StatsEnabled:    true,
	}
	cache := cache.New[string, string](config)
	defer cache.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 1*time.Hour)
	}
}

func BenchmarkCacheWithTTL(b *testing.B) {
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	// Pre-populate cache
	for i := 0; i < 1000; i++ {
		cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 1*time.Hour)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.GetWithTTL(fmt.Sprintf("key%d", i%1000))
			i++
		}
	})
}

func BenchmarkCacheExists(b *testing.B) {
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	// Pre-populate cache
	for i := 0; i < 1000; i++ {
		cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 1*time.Hour)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.Exists(fmt.Sprintf("key%d", i%1000))
			i++
		}
	})
}

func BenchmarkCacheKeys(b *testing.B) {
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	// Pre-populate cache
	for i := 0; i < 1000; i++ {
		cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 1*time.Hour)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.Keys()
	}
}

func BenchmarkCacheScalability(b *testing.B) {
	sizes := []int{1000, 10000, 100000}
	goroutines := []int{1, 10, 100, 1000}

	for _, size := range sizes {
		for _, numGoroutines := range goroutines {
			b.Run(fmt.Sprintf("size-%d-goroutines-%d", size, numGoroutines), func(b *testing.B) {
				cache := cache.NewWithDefaults[string, string]()
				defer cache.Close()

				// Pre-populate cache
				for i := 0; i < size; i++ {
					cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 1*time.Hour)
				}

				b.ResetTimer()

				var wg sync.WaitGroup
				opsPerGoroutine := b.N / numGoroutines

				for g := 0; g < numGoroutines; g++ {
					wg.Add(1)
					go func(goroutineID int) {
						defer wg.Done()
						for i := 0; i < opsPerGoroutine; i++ {
							key := fmt.Sprintf("key%d", rand.Intn(size))
							if i%2 == 0 {
								cache.Get(key)
							} else {
								cache.Set(key, fmt.Sprintf("value%d", i), 1*time.Hour)
							}
						}
					}(g)
				}
				wg.Wait()
			})
		}
	}
}

func BenchmarkCacheMemoryUsage(b *testing.B) {
	cache := cache.NewWithDefaults[string, string]()
	defer cache.Close()

	var m1, m2 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m1)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 1*time.Hour)
	}
	b.StopTimer()

	runtime.GC()
	runtime.ReadMemStats(&m2)

	b.ReportMetric(float64(m2.Alloc-m1.Alloc)/float64(b.N), "bytes/op")
}

func BenchmarkCacheShardComparison(b *testing.B) {
	shardCounts := []int{1, 4, 16, 64}

	for _, shardCount := range shardCounts {
		b.Run(fmt.Sprintf("shards-%d", shardCount), func(b *testing.B) {
			config := cache.Config{
				MaxSize:         10000,
				ShardCount:      shardCount,
				CleanupInterval: 1 * time.Minute,
				DefaultTTL:      1 * time.Hour,
				EvictionPolicy:  cache.LRU,
				StatsEnabled:    true,
			}
			cache := cache.New[string, string](config)
			defer cache.Close()

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					key := fmt.Sprintf("key%d", i%1000)
					if i%2 == 0 {
						cache.Set(key, fmt.Sprintf("value%d", i), 1*time.Hour)
					} else {
						cache.Get(key)
					}
					i++
				}
			})
		})
	}
}

func BenchmarkCacheEvictionPolicyComparison(b *testing.B) {
	policies := []cache.EvictionPolicy{cache.LRU, cache.LFU, cache.FIFO, cache.AdmissionLFU}
	policyNames := []string{"LRU", "LFU", "FIFO", "AdmissionLFU"}

	for i, policy := range policies {
		b.Run(policyNames[i], func(b *testing.B) {
			config := cache.Config{
				MaxSize:         1000,
				ShardCount:      16,
				CleanupInterval: 1 * time.Minute,
				DefaultTTL:      1 * time.Hour,
				EvictionPolicy:  policy,
				StatsEnabled:    true,
			}
			cache := cache.New[string, string](config)
			defer cache.Close()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 1*time.Hour)
			}
		})
	}
}

func BenchmarkCacheStatsEnabled(b *testing.B) {
	b.Run("stats-enabled", func(b *testing.B) {
		config := cache.Config{
			MaxSize:         10000,
			ShardCount:      16,
			CleanupInterval: 1 * time.Minute,
			DefaultTTL:      1 * time.Hour,
			EvictionPolicy:  cache.LRU,
			StatsEnabled:    true,
		}
		cache := cache.New[string, string](config)
		defer cache.Close()

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				cache.Get(fmt.Sprintf("key%d", i%1000))
				i++
			}
		})
	})

	b.Run("stats-disabled", func(b *testing.B) {
		config := cache.Config{
			MaxSize:         10000,
			ShardCount:      16,
			CleanupInterval: 1 * time.Minute,
			DefaultTTL:      1 * time.Hour,
			EvictionPolicy:  cache.LRU,
			StatsEnabled:    false,
		}
		cache := cache.New[string, string](config)
		defer cache.Close()

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				cache.Get(fmt.Sprintf("key%d", i%1000))
				i++
			}
		})
	})
}

func BenchmarkCacheRealisticWorkload(b *testing.B) {
	cache := cache.NewWithDefaults[string, []byte]()
	defer cache.Close()

	data := make([]byte, 1024) // 1KB values
	for i := range data {
		data[i] = byte(i % 256)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("user:profile:%d", i%10000)

			// simulate realistic access patterns
			switch i % 10 {
			case 0, 1, 2, 3, 4, 5, 6: // 70% reads
				cache.Get(key)
			case 7, 8: // 20% writes
				cache.Set(key, data, 1*time.Hour)
			case 9: // 10% deletes
				cache.Delete(key)
			}
			i++
		}
	})
}
