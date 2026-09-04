package kioshun

import "time"

const (
	// NoExpiration keeps an item until it is evicted, deleted or cleared.
	NoExpiration time.Duration = -1
	// DefaultExpiration uses Config.DefaultTTL for Set and related methods.
	DefaultExpiration time.Duration = 0
)

const (
	defaultMaxSize         = 10000
	defaultCleanupInterval = 5 * time.Minute
	defaultTTL             = 30 * time.Minute
	defaultWriteBufferSize = 256
	defaultWriteBatchSize  = 64

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

// CostAdmission controls how SieveTinyLFU compares weighted items.
type CostAdmission int

const (
	// CostAdmissionFrequency compares estimated access counts.
	CostAdmissionFrequency CostAdmission = iota
	// CostAdmissionBalanced favors frequent items without letting cost dominate.
	CostAdmissionBalanced
	// CostAdmissionDensity favors items with the most accesses per unit of cost.
	CostAdmissionDensity
)

// Config controls capacity, sharding, eviction, and queued writes. Start with
// DefaultConfig and change the fields your application needs.
type Config struct {
	MaxSize         int64          // max resident items; 0 => unlimited
	MaxCost         int64          // max total weighted cost; 0 => disabled
	ShardCount      int            // shard count; 0 => auto (scaled to CPUs, 2^n)
	CleanupInterval time.Duration  // expired-item sweep interval; 0 => no sweep
	DefaultTTL      time.Duration  // TTL for Set with DefaultExpiration; NoExpiration => none
	EvictionPolicy  EvictionPolicy // replacement policy; default SieveTinyLFU
	StatsEnabled    bool           // collect hit, miss, and eviction counts
	ProbationRatio  uint8          // SieveTinyLFU probation window, % of capacity; 0 => default
	GhostRatio      uint8          // SieveTinyLFU B1 ghost size, % of main; 0 => default
	CostAdmission   CostAdmission  // how weighted items compete at admission
	WriteBufferSize int            // per-shard async write queue capacity; 0 => default
	WriteBatchSize  int            // max writes applied per drain batch; 0 => default
}

// DefaultConfig returns the recommended SieveTinyLFU settings. It chooses the
// shard count from the number of CPUs and leaves statistics disabled.
func DefaultConfig() Config {
	return Config{
		MaxSize:         defaultMaxSize,
		MaxCost:         0,
		ShardCount:      0,
		CleanupInterval: defaultCleanupInterval,
		DefaultTTL:      defaultTTL,
		EvictionPolicy:  SieveTinyLFU,
		StatsEnabled:    false,
		ProbationRatio:  defaultProbationRatio,
		GhostRatio:      defaultGhostRatio,
		CostAdmission:   CostAdmissionFrequency,
		WriteBufferSize: defaultWriteBufferSize,
		WriteBatchSize:  defaultWriteBatchSize,
	}
}

// Validate reports invalid cache configuration values.
func (c Config) Validate() error {
	if c.MaxSize < 0 {
		return newConfigError("MaxSize", c.MaxSize, "must be >= 0")
	}
	if c.MaxCost < 0 {
		return newConfigError("MaxCost", c.MaxCost, "must be >= 0")
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
	policy := c.EvictionPolicy
	if policy == DefaultEvictionPolicy {
		policy = DefaultConfig().EvictionPolicy
	}
	// SieveTinyLFU sizes its internal structures from the item limit, so a
	// weighted cache must also set MaxSize.
	if policy == SieveTinyLFU && c.MaxSize <= 0 && c.MaxCost > 0 {
		return newConfigError("MaxSize", c.MaxSize, "must be > 0 for SieveTinyLFU when MaxCost is set")
	}
	if c.CostAdmission < CostAdmissionFrequency || c.CostAdmission > CostAdmissionDensity {
		return newConfigError("CostAdmission", c.CostAdmission, "must be a known cost admission mode")
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
