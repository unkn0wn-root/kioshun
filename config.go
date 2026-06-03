package kioshun

import "time"

// Sentinel TTLs for Set and related methods: NoExpiration keeps an item until it
// is evicted or deleted; DefaultExpiration falls back to Config.DefaultTTL.
const (
	NoExpiration      time.Duration = -1
	DefaultExpiration time.Duration = 0
)

const (
	defaultMaxSize         = 10000
	defaultCleanupInterval = 5 * time.Minute
	defaultTTL             = 30 * time.Minute
	defaultWriteBufferSize = 1024
	defaultWriteBatchSize  = 64

	// scale by CPUs and round to 2^n
	maxShardCount   = 256
	shardMultiplier = 4
)

type EvictionPolicy int

const (
	DefaultEvictionPolicy EvictionPolicy = iota
	LRU
	LFU
	FIFO
	SieveTinyLFU
)

// Config controls cache capacity, sharding, eviction, and the async write
// pipeline. Use DefaultConfig for recommended settings.
type Config struct {
	MaxSize         int64
	ShardCount      int
	CleanupInterval time.Duration
	DefaultTTL      time.Duration
	EvictionPolicy  EvictionPolicy
	// StatsEnabled records cache activity metrics such as hits, misses and
	// evictions. Tracking those counters adds runtime cost so enable it for
	// tests, diagnostics or deployments where throughput perf.is less critical.
	StatsEnabled    bool
	ProbationRatio  uint8
	GhostRatio      uint8
	Adapt           bool
	WriteBufferSize int // bounded per-shard queue depth for async writes.
	WriteBatchSize  int // caps how many queued writes a shard worker applies under one lock.
}

// DefaultConfig returns adaptive SieveTinyLFU with stats disabled and shard
// count scaled to the number of CPUs.
func DefaultConfig() Config {
	return Config{
		MaxSize:         defaultMaxSize,
		ShardCount:      0,
		CleanupInterval: defaultCleanupInterval,
		DefaultTTL:      defaultTTL,
		EvictionPolicy:  SieveTinyLFU,
		StatsEnabled:    false,
		ProbationRatio:  defaultProbationRatio,
		GhostRatio:      defaultGhostRatio,
		Adapt:           true,
		WriteBufferSize: defaultWriteBufferSize,
		WriteBatchSize:  defaultWriteBatchSize,
	}
}

// Validate reports invalid cache configuration values.
func (c Config) Validate() error {
	if c.MaxSize < 0 {
		return newConfigError("MaxSize", c.MaxSize, "must be >= 0")
	}
	if c.ShardCount < 0 {
		return newConfigError("ShardCount", c.ShardCount, "must be >= 0")
	}
	if c.CleanupInterval < 0 {
		return newConfigError("CleanupInterval", c.CleanupInterval, "must be >= 0")
	}
	if c.DefaultTTL < 0 && c.DefaultTTL != NoExpiration {
		return newConfigError("DefaultTTL", c.DefaultTTL, "must be >= 0 or NoExpiration")
	}
	if c.EvictionPolicy < DefaultEvictionPolicy || c.EvictionPolicy > SieveTinyLFU {
		return newConfigError("EvictionPolicy", c.EvictionPolicy, "must be a known eviction policy")
	}
	if c.ProbationRatio > 100 {
		return newConfigError("ProbationRatio", c.ProbationRatio, "must be <= 100")
	}
	if c.GhostRatio > 100 {
		return newConfigError("GhostRatio", c.GhostRatio, "must be <= 100")
	}
	if c.WriteBufferSize < 0 {
		return newConfigError("WriteBufferSize", c.WriteBufferSize, "must be >= 0")
	}
	if c.WriteBatchSize < 0 {
		return newConfigError("WriteBatchSize", c.WriteBatchSize, "must be >= 0")
	}
	return nil
}
