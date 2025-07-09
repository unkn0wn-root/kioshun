package main_test

import (
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/unkn0wn-root/kioshun"
)

// BenchmarkCacheHeavyLoad tests high load scenarios
func BenchmarkCacheHeavyLoad(b *testing.B) {
	scenarios := []struct {
		name       string
		cacheSize  int64
		numKeys    int
		numThreads int
		readRatio  float64
		valueSize  int
	}{
		{"Small_HighConcurrency", 1000, 500, 100, 0.8, 64},
		{"Medium_MixedLoad", 10000, 5000, 50, 0.7, 256},
		{"Large_ReadHeavy", 100000, 50000, 20, 0.9, 1024},
		{"XLarge_WriteHeavy", 500000, 250000, 10, 0.3, 2048},
		{"Extreme_Balanced", 1000000, 500000, 25, 0.5, 512},
	}

	for _, scenario := range scenarios {
		b.Run(scenario.name, func(b *testing.B) {
			config := cache.Config{
				MaxSize:         scenario.cacheSize,
				ShardCount:      runtime.NumCPU() * 4,
				CleanupInterval: 30 * time.Second,
				DefaultTTL:      10 * time.Minute,
				EvictionPolicy:  cache.LRU,
				StatsEnabled:    true,
			}
			c := cache.New[string, []byte](config)
			defer c.Close()

			// Pre-populate cache
			value := make([]byte, scenario.valueSize)
			for i := 0; i < len(value); i++ {
				value[i] = byte(i % 256)
			}

			for i := 0; i < scenario.numKeys/2; i++ {
				c.Set(fmt.Sprintf("key_%d", i), value, 10*time.Minute)
			}

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				localRand := rand.New(rand.NewSource(time.Now().UnixNano()))
				for pb.Next() {
					keyIndex := localRand.Intn(scenario.numKeys)
					key := fmt.Sprintf("key_%d", keyIndex)

					if localRand.Float64() < scenario.readRatio {
						c.Get(key)
					} else {
						c.Set(key, value, 10*time.Minute)
					}
				}
			})
		})
	}
}

// BenchmarkCacheContentionStress tests high contention scenarios
func BenchmarkCacheContentionStress(b *testing.B) {
	c := cache.NewWithDefaults[string, []byte]()
	defer c.Close()

	// Create large value to stress memory allocation
	value := make([]byte, 4096)
	for i := range value {
		value[i] = byte(i % 256)
	}

	// Pre-populate with hot keys
	hotKeys := 100
	for i := 0; i < hotKeys; i++ {
		c.Set(fmt.Sprintf("hot_key_%d", i), value, 1*time.Hour)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		localRand := rand.New(rand.NewSource(time.Now().UnixNano()))
		for pb.Next() {
			// 80% of operations hit the same hot keys (high contention)
			var key string
			if localRand.Float64() < 0.8 {
				key = fmt.Sprintf("hot_key_%d", localRand.Intn(hotKeys))
			} else {
				key = fmt.Sprintf("cold_key_%d", localRand.Intn(100000))
			}

			if localRand.Float64() < 0.6 {
				c.Get(key)
			} else {
				c.Set(key, value, 1*time.Hour)
			}
		}
	})
}

// BenchmarkCacheEvictionStress tests heavy eviction scenarios
func BenchmarkCacheEvictionStress(b *testing.B) {
	policies := []cache.EvictionPolicy{cache.LRU, cache.LFU, cache.FIFO, cache.Random}
	policyNames := []string{"LRU", "LFU", "FIFO", "Random"}

	for i, policy := range policies {
		b.Run(fmt.Sprintf("Eviction_%s", policyNames[i]), func(b *testing.B) {
			config := cache.Config{
				MaxSize:         1000, // Small cache to force evictions
				ShardCount:      16,
				CleanupInterval: 1 * time.Minute,
				DefaultTTL:      1 * time.Hour,
				EvictionPolicy:  policy,
				StatsEnabled:    true,
			}
			c := cache.New[string, []byte](config)
			defer c.Close()

			value := make([]byte, 1024)
			for i := range value {
				value[i] = byte(i % 256)
			}

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					// Generate many unique keys to force evictions
					key := fmt.Sprintf("evict_key_%d", i)
					c.Set(key, value, 1*time.Hour)
					i++
				}
			})
		})
	}
}

// BenchmarkCacheMemoryPressure tests behavior under memory pressure
func BenchmarkCacheMemoryPressure(b *testing.B) {
	valueSizes := []int{1024, 4096, 16384, 65536} // 1KB to 64KB

	for _, size := range valueSizes {
		b.Run(fmt.Sprintf("ValueSize_%dKB", size/1024), func(b *testing.B) {
			c := cache.NewWithDefaults[string, []byte]()
			defer c.Close()

			value := make([]byte, size)
			for i := range value {
				value[i] = byte(i % 256)
			}

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					key := fmt.Sprintf("mem_key_%d", i%10000)
					if i%3 == 0 {
						c.Get(key)
					} else {
						c.Set(key, value, 30*time.Minute)
					}
					i++
				}
			})
		})
	}
}

// BenchmarkCacheExpirationStress tests heavy expiration scenarios
func BenchmarkCacheExpirationStress(b *testing.B) {
	c := cache.NewWithDefaults[string, []byte]()
	defer c.Close()

	value := make([]byte, 512)
	for i := range value {
		value[i] = byte(i % 256)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		localRand := rand.New(rand.NewSource(time.Now().UnixNano()))
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("expire_key_%d", i)

			// Mix of short and long TTLs
			var ttl time.Duration
			if localRand.Float64() < 0.3 {
				ttl = time.Duration(localRand.Intn(100)) * time.Millisecond // Short TTL
			} else {
				ttl = time.Duration(localRand.Intn(3600)) * time.Second // Long TTL
			}

			if localRand.Float64() < 0.7 {
				c.Set(key, value, ttl)
			} else {
				c.Get(key)
			}
			i++
		}
	})
}

// BenchmarkCacheShardingEfficiency tests sharding performance
func BenchmarkCacheShardingEfficiency(b *testing.B) {
	shardCounts := []int{1, 2, 4, 8, 16, 32, 64, 128}

	for _, shardCount := range shardCounts {
		b.Run(fmt.Sprintf("Shards_%d", shardCount), func(b *testing.B) {
			config := cache.Config{
				MaxSize:         100000,
				ShardCount:      shardCount,
				CleanupInterval: 5 * time.Minute,
				DefaultTTL:      1 * time.Hour,
				EvictionPolicy:  cache.LRU,
				StatsEnabled:    true,
			}
			c := cache.New[string, []byte](config)
			defer c.Close()

			value := make([]byte, 256)
			for i := range value {
				value[i] = byte(i % 256)
			}

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				localRand := rand.New(rand.NewSource(time.Now().UnixNano()))
				i := 0
				for pb.Next() {
					key := fmt.Sprintf("shard_key_%d", i)
					if localRand.Float64() < 0.7 {
						c.Get(key)
					} else {
						c.Set(key, value, 1*time.Hour)
					}
					i++
				}
			})
		})
	}
}

// BenchmarkCacheHashingPerformance tests different key types and hashing
func BenchmarkCacheHashingPerformance(b *testing.B) {
	b.Run("StringKeys", func(b *testing.B) {
		c := cache.NewWithDefaults[string, int]()
		defer c.Close()

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				key := fmt.Sprintf("string_key_%d_%d", i, i*17)
				c.Set(key, i, 1*time.Hour)
				i++
			}
		})
	})

	b.Run("IntKeys", func(b *testing.B) {
		c := cache.NewWithDefaults[int, int]()
		defer c.Close()

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				c.Set(i, i*2, 1*time.Hour)
				i++
			}
		})
	})

	b.Run("Int64Keys", func(b *testing.B) {
		c := cache.NewWithDefaults[int64, int]()
		defer c.Close()

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := int64(0)
			for pb.Next() {
				c.Set(i, int(i*2), 1*time.Hour)
				i++
			}
		})
	})
}

// BenchmarkCacheStressTest combines multiple stress factors
func BenchmarkCacheStressTest(b *testing.B) {
	c := cache.NewWithDefaults[string, []byte]()
	defer c.Close()

	// Create different sized values
	values := make([][]byte, 5)
	for i := range values {
		size := 1024 * (i + 1) // 1KB to 5KB
		values[i] = make([]byte, size)
		for j := range values[i] {
			values[i][j] = byte(j % 256)
		}
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		localRand := rand.New(rand.NewSource(time.Now().UnixNano()))
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("stress_key_%d", i)
			value := values[localRand.Intn(len(values))]

			operation := localRand.Float64()
			switch {
			case operation < 0.5: // 50% reads
				c.Get(key)
			case operation < 0.8: // 30% writes
				ttl := time.Duration(localRand.Intn(3600)) * time.Second
				c.Set(key, value, ttl)
			case operation < 0.9: // 10% deletes
				c.Delete(key)
			case operation < 0.95: // 5% exists checks
				c.Exists(key)
			default: // 5% stats calls
				c.Stats()
			}
			i++
		}
	})
}

// BenchmarkCacheHighThroughput tests maximum throughput
func BenchmarkCacheHighThroughput(b *testing.B) {
	goroutineCounts := []int{1, 10, 50, 100, 500, 1000}

	for _, goroutineCount := range goroutineCounts {
		b.Run(fmt.Sprintf("Goroutines_%d", goroutineCount), func(b *testing.B) {
			c := cache.NewWithDefaults[string, []byte]()
			defer c.Close()

			value := make([]byte, 1024)
			for i := range value {
				value[i] = byte(i % 256)
			}

			b.ResetTimer()

			var wg sync.WaitGroup
			opsPerGoroutine := b.N / goroutineCount

			for g := 0; g < goroutineCount; g++ {
				wg.Add(1)
				go func(goroutineID int) {
					defer wg.Done()
					localRand := rand.New(rand.NewSource(time.Now().UnixNano() + int64(goroutineID)))

					for i := 0; i < opsPerGoroutine; i++ {
						key := fmt.Sprintf("throughput_key_%d_%d", goroutineID, i)

						if localRand.Float64() < 0.7 {
							c.Set(key, value, 1*time.Hour)
						} else {
							c.Get(key)
						}
					}
				}(g)
			}
			wg.Wait()
		})
	}
}

// BenchmarkCacheCleanupStress tests cleanup performance under stress
func BenchmarkCacheCleanupStress(b *testing.B) {
	config := cache.Config{
		MaxSize:         10000,
		ShardCount:      16,
		CleanupInterval: 100 * time.Millisecond, // Aggressive cleanup
		DefaultTTL:      500 * time.Millisecond, // Short default TTL
		EvictionPolicy:  cache.LRU,
		StatsEnabled:    true,
	}
	c := cache.New[string, []byte](config)
	defer c.Close()

	value := make([]byte, 512)
	for i := range value {
		value[i] = byte(i % 256)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		localRand := rand.New(rand.NewSource(time.Now().UnixNano()))
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("cleanup_key_%d", i)

			// Mix of operations that stress cleanup
			operation := localRand.Float64()
			switch {
			case operation < 0.4: // 40% short TTL sets
				c.Set(key, value, 50*time.Millisecond)
			case operation < 0.7: // 30% normal sets
				c.Set(key, value, 5*time.Second)
			case operation < 0.9: // 20% gets
				c.Get(key)
			default: // 10% manual cleanup triggers
				c.TriggerCleanup()
			}
			i++
		}
	})
}

// BenchmarkCacheAdvancedOperations tests advanced cache operations
func BenchmarkCacheAdvancedOperations(b *testing.B) {
	c := cache.NewWithDefaults[string, []byte]()
	defer c.Close()

	value := make([]byte, 1024)
	for i := range value {
		value[i] = byte(i % 256)
	}

	// Pre-populate
	for i := 0; i < 10000; i++ {
		c.Set(fmt.Sprintf("advanced_key_%d", i), value, 1*time.Hour)
	}

	b.ResetTimer()
	b.Run("GetWithTTL", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				c.GetWithTTL(fmt.Sprintf("advanced_key_%d", i%10000))
				i++
			}
		})
	})

	b.Run("Exists", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				c.Exists(fmt.Sprintf("advanced_key_%d", i%10000))
				i++
			}
		})
	})

	b.Run("Keys", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			c.Keys()
		}
	})

	b.Run("Size", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				c.Size()
			}
		})
	})

	b.Run("Stats", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				c.Stats()
			}
		})
	})
}
