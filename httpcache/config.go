package httpcache

import (
	"errors"
	"slices"
	"time"

	"github.com/unkn0wn-root/kioshun"
)

// Config controls HTTP response caching behavior.
// Zero-valued fields are filled from DefaultConfig. Use the Disable* fields to
// turn off features that are enabled by default.
type Config struct {
	MaxSize               int64
	ShardCount            int
	CleanupInterval       time.Duration
	DefaultTTL            time.Duration
	EvictionPolicy        kioshun.EvictionPolicy
	ProbationRatio        uint8
	GhostRatio            uint8
	DisableAdapt          bool // SieveTinyLFU adaptive sizing.
	DisableStats          bool // cache statistics collection.
	DisableCleanup        bool // periodic cleanup.
	DisableCacheSizeLimit bool // makes MaxSize unlimited in the backing cache.
	DisableBodySizeLimit  bool // allows caching responses of any body size.

	CacheableMethods []string
	CacheableStatus  []int
	IgnoreHeaders    []string
	MaxBodySize      int64

	HitHeader  string // Default: "X-Cache"
	MissHeader string // Default: "X-Cache"

	// PathExtractor enables URL-pattern invalidation via Invalidate by recovering
	// a request path from a cache key. It must be paired with a path preserving
	// key generator (for example KeyWithoutQuery together with PathExtractorFromKey).
	// When nil, the pattern index is disabled and Invalidate is a no-op.
	PathExtractor PathExtractor
}

// DefaultConfig returns the default HTTP cache middleware configuration.
func DefaultConfig() Config {
	return Config{
		MaxSize:          100000,
		ShardCount:       16,
		CleanupInterval:  5 * time.Minute,
		DefaultTTL:       5 * time.Minute,
		EvictionPolicy:   kioshun.SieveTinyLFU,
		CacheableMethods: []string{"GET", "HEAD"},
		CacheableStatus:  []int{200, 201, 300, 301, 302, 304, 404, 410},
		IgnoreHeaders:    []string{"Date", "Server", "X-Request-Id", "X-Trace-Id"},
		MaxBodySize:      10 * 1024 * 1024, // 10MB
		HitHeader:        "X-Cache",
		MissHeader:       "X-Cache",
	}
}

// Validate reports invalid HTTP cache configuration values.
func (c Config) Validate() error {
	if c.MaxSize < 0 {
		return errors.New("httpcache: MaxSize must be >= 0")
	}
	if c.ShardCount < 0 {
		return errors.New("httpcache: ShardCount must be >= 0")
	}
	if c.CleanupInterval < 0 {
		return errors.New("httpcache: CleanupInterval must be >= 0")
	}
	if c.DefaultTTL < 0 && c.DefaultTTL != kioshun.NoExpiration {
		return errors.New("httpcache: DefaultTTL must be >= 0 or NoExpiration")
	}

	switch c.EvictionPolicy {
	case kioshun.DefaultEvictionPolicy, kioshun.LRU, kioshun.LFU, kioshun.FIFO, kioshun.SieveTinyLFU:
	default:
		return errors.New("httpcache: EvictionPolicy must be a known eviction policy")
	}

	if c.ProbationRatio > 100 {
		return errors.New("httpcache: ProbationRatio must be <= 100")
	}
	if c.GhostRatio > 100 {
		return errors.New("httpcache: GhostRatio must be <= 100")
	}
	if c.MaxBodySize < 0 {
		return errors.New("httpcache: MaxBodySize must be >= 0")
	}
	return nil
}

func resolveConfig(config Config) (Config, error) {
	if err := config.Validate(); err != nil {
		return Config{}, err
	}

	config = configWithDefaults(config)
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func configWithDefaults(config Config) Config {
	defaults := DefaultConfig()
	cfg := defaults

	if config.MaxSize != 0 {
		cfg.MaxSize = config.MaxSize
	}
	if config.ShardCount != 0 {
		cfg.ShardCount = config.ShardCount
	}
	if config.CleanupInterval != 0 {
		cfg.CleanupInterval = config.CleanupInterval
	}
	if config.DefaultTTL != 0 {
		cfg.DefaultTTL = config.DefaultTTL
	}
	if config.EvictionPolicy != kioshun.DefaultEvictionPolicy {
		cfg.EvictionPolicy = config.EvictionPolicy
	}
	if config.ProbationRatio != 0 {
		cfg.ProbationRatio = config.ProbationRatio
	}
	if config.GhostRatio != 0 {
		cfg.GhostRatio = config.GhostRatio
	}
	if config.CacheableMethods != nil {
		cfg.CacheableMethods = slices.Clone(config.CacheableMethods)
	}
	if config.CacheableStatus != nil {
		cfg.CacheableStatus = slices.Clone(config.CacheableStatus)
	}
	if config.IgnoreHeaders != nil {
		cfg.IgnoreHeaders = slices.Clone(config.IgnoreHeaders)
	}
	if config.MaxBodySize != 0 {
		cfg.MaxBodySize = config.MaxBodySize
	}
	if config.HitHeader != "" {
		cfg.HitHeader = config.HitHeader
	}
	if config.MissHeader != "" {
		cfg.MissHeader = config.MissHeader
	}

	cfg.DisableAdapt = config.DisableAdapt
	cfg.DisableStats = config.DisableStats
	cfg.DisableCleanup = config.DisableCleanup
	cfg.DisableCacheSizeLimit = config.DisableCacheSizeLimit
	cfg.DisableBodySizeLimit = config.DisableBodySizeLimit
	cfg.PathExtractor = config.PathExtractor
	if cfg.DisableCleanup {
		cfg.CleanupInterval = 0
	}
	if cfg.DisableCacheSizeLimit {
		cfg.MaxSize = 0
	}
	if cfg.DisableBodySizeLimit {
		cfg.MaxBodySize = 0
	}
	return cfg
}
