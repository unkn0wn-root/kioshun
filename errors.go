package kioshun

import (
	"errors"
	"fmt"
)

var (
	// ErrCacheExists is returned when registering a name that already has a configuration.
	ErrCacheExists = errors.New("cache already exists")
	// ErrCacheNotRegistered is returned by GetCache for a name with no live instance
	// and no registered config; use GetCacheWithConfig to get-or-create.
	ErrCacheNotRegistered = errors.New("cache not registered")
	// ErrTypeMismatch is returned when a cached instance's type parameters differ from those requested.
	ErrTypeMismatch = errors.New("cache type mismatch")
	// ErrInvalidConfig is the sentinel that ConfigError unwraps to.
	ErrInvalidConfig = errors.New("invalid cache configuration")
	// ErrCacheClosed is returned by operations on a closed cache.
	ErrCacheClosed = errors.New("cache is closed")
	// ErrInvalidCost is returned when a configured weigher reports a negative cost.
	ErrInvalidCost = errors.New("cache item cost is invalid")
	// ErrItemTooLarge is returned when a weighted item cannot fit in its shard cost budget.
	ErrItemTooLarge = errors.New("cache item cost exceeds shard budget")
)

// CacheError describes a failure from a named cache operation (register, get, close).
type CacheError struct {
	Op    string
	Name  string
	Cause error
}

func (e *CacheError) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("cache %s %s: %v", e.Op, e.Name, e.Cause)
	}
	return fmt.Sprintf("cache %s: %v", e.Op, e.Cause)
}

func (e *CacheError) Unwrap() error {
	return e.Cause
}

func newCacheError(op, name string, cause error) *CacheError {
	return &CacheError{
		Op:    op,
		Name:  name,
		Cause: cause,
	}
}

// ConfigError describes an invalid configuration field; it unwraps to ErrInvalidConfig.
type ConfigError struct {
	Field  string
	Value  any
	Reason string
}

func (e *ConfigError) Error() string {
	if e == nil {
		return ErrInvalidConfig.Error()
	}
	if e.Reason == "" {
		return fmt.Sprintf("%v: %s has invalid value %v", ErrInvalidConfig, e.Field, e.Value)
	}
	return fmt.Sprintf("%v: %s %s (got %v)", ErrInvalidConfig, e.Field, e.Reason, e.Value)
}

func (e *ConfigError) Unwrap() error {
	return ErrInvalidConfig
}

func newConfigError(field string, value any, reason string) *ConfigError {
	return &ConfigError{
		Field:  field,
		Value:  value,
		Reason: reason,
	}
}
