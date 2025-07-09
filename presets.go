package cache

import "time"

func TemporaryCacheConfig() Config {
	return Config{
		MaxSize:         1000,
		ShardCount:      0,
		CleanupInterval: 1 * time.Minute,
		DefaultTTL:      5 * time.Minute,
		EvictionPolicy:  LRU,
		StatsEnabled:    true,
	}
}

func PersistentCacheConfig() Config {
	return Config{
		MaxSize:         100000,
		ShardCount:      0,
		CleanupInterval: 1 * time.Hour,
		DefaultTTL:      24 * time.Hour,
		EvictionPolicy:  LRU,
		StatsEnabled:    true,
	}
}

func UserCacheConfig() Config {
	return Config{
		MaxSize:         50000,
		ShardCount:      16,
		CleanupInterval: 30 * time.Minute,
		DefaultTTL:      2 * time.Hour,
		EvictionPolicy:  LRU,
		StatsEnabled:    true,
	}
}

func SessionCacheConfig() Config {
	return Config{
		MaxSize:         25000,
		ShardCount:      8,
		CleanupInterval: 15 * time.Minute,
		DefaultTTL:      30 * time.Minute,
		EvictionPolicy:  LRU,
		StatsEnabled:    true,
	}
}

func APICacheConfig() Config {
	return Config{
		MaxSize:         10000,
		ShardCount:      4,
		CleanupInterval: 5 * time.Minute,
		DefaultTTL:      15 * time.Minute,
		EvictionPolicy:  LRU,
		StatsEnabled:    true,
	}
}

func HPCacheConfig() Config {
	return Config{
		MaxSize:         1000000,
		ShardCount:      64,
		CleanupInterval: 1 * time.Hour,
		DefaultTTL:      6 * time.Hour,
		EvictionPolicy:  LRU,
		StatsEnabled:    true,
	}
}

func LowMemoryCacheConfig() Config {
	return Config{
		MaxSize:         500,
		ShardCount:      0,
		CleanupInterval: 30 * time.Second,
		DefaultTTL:      1 * time.Minute,
		EvictionPolicy:  LRU,
		StatsEnabled:    false,
	}
}
