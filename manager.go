package kioshun

import (
	"fmt"
	"sync"
)

// GlobalManager backs the package-level cache helpers.
var GlobalManager = NewManager()

// Manager owns named cache instances and their registered configurations.
type Manager struct {
	caches   sync.Map // map of cache instances by name
	configs  map[string]Config
	configMu sync.RWMutex
}

// NewManager creates an empty cache manager.
func NewManager() *Manager {
	return &Manager{
		configs: make(map[string]Config),
	}
}

// Register registers a configuration for a named cache.
// Returns an error if a configuration with the same name already exists.
func (m *Manager) Register(name string, config Config) error {
	if err := config.Validate(); err != nil {
		return newCacheError("register", name, err)
	}

	m.configMu.Lock()
	defer m.configMu.Unlock()

	if _, exists := m.configs[name]; exists {
		return newCacheError("register", name, ErrCacheExists)
	}

	m.configs[name] = config
	return nil
}

// GetCache returns the named typed cache, creating it from the registered
// configuration or DefaultConfig when none is registered.
func GetCache[K comparable, V any](m *Manager, name string) (*Cache[K, V], error) {
	if cached, ok := m.caches.Load(name); ok {
		if cache, ok := cached.(*Cache[K, V]); ok {
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

	cache, err := New[K, V](config)
	if err != nil {
		return nil, newCacheError("get", name, err)
	}
	// LoadOrStore closes the loser so concurrent creators do not leak workers.
	if actual, loaded := m.caches.LoadOrStore(name, cache); loaded {
		cache.Close()
		if existingCache, ok := actual.(*Cache[K, V]); ok {
			return existingCache, nil
		}
		return nil, newCacheError("get", name, ErrTypeMismatch)
	}

	return cache, nil
}

// Stats returns performance statistics for all managed caches.
func (m *Manager) Stats() map[string]Stats {
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
func (m *Manager) CloseAll() error {
	var closeErrors []error

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

	m.caches.Range(func(key, _ any) bool {
		m.caches.Delete(key)
		return true
	})

	if len(closeErrors) > 0 {
		return fmt.Errorf("errors closing caches: %v", closeErrors)
	}

	return nil
}

// Remove removes and closes the named cache instance.
func (m *Manager) Remove(name string) error {
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
	return GlobalManager.Register(name, config)
}

// GetGlobalCache retrieves or creates a cache from the global manager.
func GetGlobalCache[K comparable, V any](name string) (*Cache[K, V], error) {
	return GetCache[K, V](GlobalManager, name)
}

// GetGlobalCacheStats returns stats for all caches in the global manager.
func GetGlobalCacheStats() map[string]Stats {
	return GlobalManager.Stats()
}

// CloseAllGlobalCaches closes all caches in the global manager.
func CloseAllGlobalCaches() error {
	return GlobalManager.CloseAll()
}
