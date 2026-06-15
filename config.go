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
	defaultWriteBufferSize = 256
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

// CostAdmission controls how SieveTinyLFU compares an incoming weighted item
// against a resident victim when MaxCost/WithWeigher are used.
type CostAdmission int

const (
	// CostAdmissionFrequency compares estimate(candidate) > estimate(victim).
	CostAdmissionFrequency CostAdmission = iota
	// CostAdmissionBalanced compares frequency / sqrt(cost): a middle ground
	// between request-hit and byte-hit objectives.
	CostAdmissionBalanced
	// CostAdmissionDensity compares frequency / cost, favoring dense hot entries
	// when request hit ratio matters more than byte hit ratio.
	CostAdmissionDensity
)

// Config controls cache capacity, sharding, eviction, and the async write
// pipeline. Use DefaultConfig for recommended settings.
type Config struct {
	MaxSize         int64          // max resident items; 0 => unlimited
	MaxCost         int64          // max total weighted cost; 0 => disabled
	ShardCount      int            // shard count; 0 => auto (scaled to CPUs, 2^n)
	CleanupInterval time.Duration  // expired-item sweep interval; 0 => no sweep
	DefaultTTL      time.Duration  // TTL for Set with DefaultExpiration; NoExpiration => none
	EvictionPolicy  EvictionPolicy // replacement policy; default SieveTinyLFU
	StatsEnabled    bool           // collect hit/miss/eviction telemetry (off by default)
	ProbationRatio  uint8          // SieveTinyLFU probation window, % of capacity; 0 => default
	GhostRatio      uint8          // SieveTinyLFU B1 ghost size, % of main; 0 => default
	CostAdmission   CostAdmission  // how weighted items compete at admission
	WriteBufferSize int            // per-shard async write queue capacity; 0 => default
	WriteBatchSize  int            // max writes applied per drain batch; 0 => default
}

// DefaultConfig returns self-tuning SieveTinyLFU with stats disabled and shard
// count scaled to the number of CPUs.
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
	// SieveTinyLFU sizes its sketch/ghost/probation split from an item count, so a
	// cost-only budget would mis-size them: a weighted Sieve cache must bound items too.
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
