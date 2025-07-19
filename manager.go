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

// RegisterCache registers a configuration for a named cache
//
// Provides configuration pre-registration for cache instances that will be created later:
//
// Registration process:
// 1. Acquires write lock
// 2. Checks if a configuration already exists for the given name
// 3. Rejects registration if duplicate name detected (prevents accidental overwrites)
// 4. Stores configuration in internal map for later use by GetCache()
//
// Configuration lifecycle:
// - Configurations are stored separately from actual cache instances
// - GetCache() uses registered config when creating new cache instances
// - If no config is registered, GetCache() falls back to DefaultConfig()
// - Configs remain available even after cache instances are removed
func (m *Manager) RegisterCache(name string, config Config) error {
	m.configMu.Lock()
	defer m.configMu.Unlock()

	if _, exists := m.configs[name]; exists {
		return newCacheError("register", name, ErrCacheExists)
	}

	m.configs[name] = config
	return nil
}

// GetCache retrieves or creates acache instance
// Fast path:
// 1. Attempts to load existing cache from sync.Map (lock-free read)
// 2. Performs type assertion to ensure cache matches requested types K, V
// 3. Returns immediately if found and types match (most common case)
//
// Slow path - cache creation:
// 1. Acquires read lock to retrieve configuration for the named cache
// 2. Falls back to DefaultConfig() if no specific configuration registered
// 3. Creates new cache instance with given configuration
//
// - Uses atomic LoadOrStore operation to handle concurrent cache creation
// - If another goroutine creates cache concurrently, discards local instance
// - Closes abandoned cache (no resource leaks)
// - Returns the winner's cache instance after type verification
func GetCache[K comparable, V any](m *Manager, name string) (*InMemoryCache[K, V], error) {
	// Try to get existing cache
	if cached, ok := m.caches.Load(name); ok {
		if cache, ok := cached.(*InMemoryCache[K, V]); ok {
			return cache, nil
		}
		return nil, newCacheError("get", name, ErrTypeMismatch)
	}

	m.configMu.RLock()
	config, exists := m.configs[name]
	m.configMu.RUnlock()

	if !exists {
		config = DefaultConfig()
	}

	cache := New[K, V](config)
	if actual, loaded := m.caches.LoadOrStore(name, cache); loaded {
		cache.Close()
		if existingCache, ok := actual.(*InMemoryCache[K, V]); ok {
			return existingCache, nil
		}
		return nil, newCacheError("get", name, ErrTypeMismatch)
	}

	return cache, nil
}

// GetCacheStats aggregates performance statistics from all managed cache
// Collection process:
// 1. Iterates through all cached instances using sync.Map.Range()
// 2. Performs type assertions to ensure proper string keys and Stats interface
// 3. Calls Stats() method on each cache to retrieve current performance metrics
//
// Statistics included (per cache):
// - Hit/miss ratios and counts
// - Eviction and expiration counts
// - Current size and capacity
// - Shard count and other configuration details
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

// CloseAll performs shutdown of all managed cache instances
func (m *Manager) CloseAll() error {
	var closeErrors []error

	// Close all caches and collect any errors
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
		return true
	})

	m.caches.Range(func(key, value any) bool {
		m.caches.Delete(key)
		return true
	})

	if len(closeErrors) > 0 {
		return fmt.Errorf("errors closing caches: %v", closeErrors)
	}

	return nil
}

// RemoveCache performs removal and cleanup of a specific named cache instance
//
// 1. Uses LoadAndDelete() for atomic "remove and return" operation
// 2. Eliminates race conditions where cache could be accessed during removal
// 3. Ensures cache is immediately unavailable to new requests
func (m *Manager) RemoveCache(name string) error {
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

// RegisterGlobalCache registers a configuration in the global manager
func RegisterGlobalCache(name string, config Config) error {
	return GlobalManager.RegisterCache(name, config)
}

// GetGlobalCache retrieves or creates a cache from the global manager
func GetGlobalCache[K comparable, V any](name string) (*InMemoryCache[K, V], error) {
	return GetCache[K, V](GlobalManager, name)
}

// GetGlobalCacheStats returns stats for all caches in the global manager
func GetGlobalCacheStats() map[string]Stats {
	return GlobalManager.GetCacheStats()
}

// CloseAllGlobalCaches closes all caches in the global manager
func CloseAllGlobalCaches() error {
	return GlobalManager.CloseAll()
}
