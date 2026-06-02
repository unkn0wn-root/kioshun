package kioshun

import (
	"errors"
	"fmt"
)

var (
	ErrCacheExists   = errors.New("cache already exists")
	ErrCacheNotFound = errors.New("cache not found")
	ErrTypeMismatch  = errors.New("cache type mismatch")
	ErrInvalidConfig = errors.New("invalid cache configuration")
	ErrCacheClosed   = errors.New("cache is closed")
)

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

// ConfigError describes an invalid cache configuration field.
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

// Unwrap returns ErrInvalidConfig so errors.Is can match configuration errors.
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
