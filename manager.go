package cache

import (
	"fmt"
	"sync"
)

// GlobalManager holds global cache instances
var GlobalManager = NewManager()

// Manager manages multiple named cache instances with different configurations
type Manager struct {
	caches   sync.Map // map of cache instances by name
	configs  map[string]Config
	configMu sync.RWMutex
}

// NewManager creates a new cache manager instance
func NewManager() *Manager {
	return &Manager{
		configs: make(map[string]Config),
	}
}

// RegisterCache registers a configuration for a named cache.
// Returns an error if a configuration with the same name already exists.
func (m *Manager) RegisterCache(name string, config Config) error {
	m.configMu.Lock()
	defer m.configMu.Unlock()

	if _, exists := m.configs[name]; exists {
		return newCacheError("register", name, ErrCacheExists)
	}

	m.configs[name] = config
	return nil
}

// GetCache retrieves an existing cache or creates a new one with the registered
// configuration. If no configuration is registered, uses DefaultConfig().
func GetCache[K comparable, V any](m *Manager, name string) (*InMemoryCache[K, V], error) {
	// Fast path: return existing cache if found (most common case)
	if cached, ok := m.caches.Load(name); ok {
		if cache, ok := cached.(*InMemoryCache[K, V]); ok {
			return cache, nil
		}
		// Type mismatch: cache exists but with different generic parameters
		return nil, newCacheError("get", name, ErrTypeMismatch)
	}

	m.configMu.RLock()
	config, exists := m.configs[name]
	m.configMu.RUnlock()

	if !exists {
		config = DefaultConfig()
	}

	// Slow path: create new cache
	cache := New[K, V](config)
	// Atomic LoadOrStore handles race condition where multiple goroutines
	// attempt to create the same cache simultaneously
	if actual, loaded := m.caches.LoadOrStore(name, cache); loaded {
		// Another goroutine created the cache first
		// Clean up our unused cache to prevent resource leaks
		cache.Close()
		// Return the winner's cache if types match
		if existingCache, ok := actual.(*InMemoryCache[K, V]); ok {
			return existingCache, nil
		}
		return nil, newCacheError("get", name, ErrTypeMismatch)
	}

	return cache, nil
}

// GetCacheStats returns performance statistics for all managed caches.
func (m *Manager) GetCacheStats() map[string]Stats {
	stats := make(map[string]Stats)

	m.caches.Range(func(key, value any) bool {
		if name, ok := key.(string); ok {
			if cache, ok := value.(interface{ Stats() Stats }); ok {
				stats[name] = cache.Stats()
			}
		}
		return true
	})

	return stats
}

// CloseAll closes all managed cache instances and returns any errors encountered.
// Uses two-phase approach: first close all caches, then clear the registry.
func (m *Manager) CloseAll() error {
	var closeErrors []error

	// Phase 1: Close all cache instances
	m.caches.Range(func(key, value any) bool {
		if cache, ok := value.(interface{ Close() error }); ok {
			if err := cache.Close(); err != nil {
				if name, ok := key.(string); ok {
					closeErrors = append(closeErrors, newCacheError("close", name, err))
				} else {
					closeErrors = append(closeErrors, wrapError("close", err))
				}
			}
		}
		return true // Continue iteration
	})

	// Phase 2: Clear all registry entries
	m.caches.Range(func(key, value any) bool {
		m.caches.Delete(key)
		return true // Continue iteration
	})

	if len(closeErrors) > 0 {
		return fmt.Errorf("errors closing caches: %v", closeErrors)
	}

	return nil
}

// RemoveCache removes and closes the named cache instance.
// Removes from registry and cleans up both runtime and configuration state.
func (m *Manager) RemoveCache(name string) error {
	// Atomically remove from active cache registry
	if cached, ok := m.caches.LoadAndDelete(name); ok {
		if cache, ok := cached.(interface{ Close() error }); ok {
			return cache.Close()
		}
	}

	m.configMu.Lock()
	delete(m.configs, name)
	m.configMu.Unlock()

	return nil
}

// RegisterGlobalCache registers a configuration in the global manager.
func RegisterGlobalCache(name string, config Config) error {
	return GlobalManager.RegisterCache(name, config)
}

// GetGlobalCache retrieves or creates a cache from the global manager.
func GetGlobalCache[K comparable, V any](name string) (*InMemoryCache[K, V], error) {
	return GetCache[K, V](GlobalManager, name)
}

// GetGlobalCacheStats returns stats for all caches in the global manager.
func GetGlobalCacheStats() map[string]Stats {
	return GlobalManager.GetCacheStats()
}

// CloseAllGlobalCaches closes all caches in the global manager.
func CloseAllGlobalCaches() error {
	return GlobalManager.CloseAll()
}
