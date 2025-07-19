package cache

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

func wrapError(op string, err error) *CacheError {
	return &CacheError{
		Op:    op,
		Cause: err,
	}
}
