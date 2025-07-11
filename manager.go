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
func (m *Manager) RegisterCache(name string, config Config) error {
	m.configMu.Lock()
	defer m.configMu.Unlock()

	if _, exists := m.configs[name]; exists {
		return fmt.Errorf("cache '%s' already exists", name)
	}

	m.configs[name] = config
	return nil
}

// GetCache retrieves or creates a typed cache instance by name
func GetCache[K comparable, V any](m *Manager, name string) (*InMemoryCache[K, V], error) {
	// Try to get existing cache
	if cached, ok := m.caches.Load(name); ok {
		if cache, ok := cached.(*InMemoryCache[K, V]); ok {
			return cache, nil
		}
		return nil, fmt.Errorf("cache '%s' exists but with different types", name)
	}

	m.configMu.RLock()
	config, exists := m.configs[name]
	m.configMu.RUnlock()

	if !exists {
		config = DefaultConfig()
	}

	cache := New[K, V](config)
	// Atomic store - if another goroutine created it first, use theirs
	if actual, loaded := m.caches.LoadOrStore(name, cache); loaded {
		// Close our instance since another goroutine won the race
		cache.Close()
		if existingCache, ok := actual.(*InMemoryCache[K, V]); ok {
			return existingCache, nil
		}
		return nil, fmt.Errorf("cache '%s' exists but with different types", name)
	}

	return cache, nil
}

// GetCacheStats returns statistics for all managed caches
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

// CloseAll closes all managed cache instances
func (m *Manager) CloseAll() error {
	var errors []error

	// Close all caches and collect any errors
	m.caches.Range(func(key, value any) bool {
		if cache, ok := value.(interface{ Close() error }); ok {
			if err := cache.Close(); err != nil {
				errors = append(errors, err)
			}
		}
		return true
	})

	m.caches.Range(func(key, value any) bool {
		m.caches.Delete(key)
		return true
	})

	if len(errors) > 0 {
		return fmt.Errorf("errors closing caches: %v", errors)
	}

	return nil
}

// RemoveCache removes and closes a specific cache by name
func (m *Manager) RemoveCache(name string) error {
	// Atomically remove and get the cache
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
