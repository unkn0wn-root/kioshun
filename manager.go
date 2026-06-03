package kioshun

import (
	"errors"
	"io"
	"sync"
)

// globalManager backs the package-level cache helpers (RegisterGlobalCache,
// GetGlobalCache and friends). Caches created through it live for the lifetime
// of the process unless released with CloseAllGlobalCaches.
var globalManager = NewManager()

type statser interface {
	Stats() Stats
}

// Manager owns named cache instances and their registered configurations.
type Manager struct {
	caches   sync.Map // name -> *Cache[K, V]; each name is written once, read many.
	configs  map[string]Config
	configMu sync.RWMutex
}

// NewManager returns an empty Manager with no registered configurations or caches.
func NewManager() *Manager {
	return &Manager{
		configs: make(map[string]Config),
	}
}

// Register registers a configuration for a named cache.
// A name is bound to its configuration by the first GetCache call, so
// registering a name that already has a live cache does not change that
// instance; register before first use.
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
// configuration (or DefaultConfig when none is registered) on first use.
func GetCache[K comparable, V any](m *Manager, name string) (*Cache[K, V], error) {
	if cached, ok := m.caches.Load(name); ok {
		return assertCache[K, V](name, cached)
	}

	m.configMu.RLock()
	config, exists := m.configs[name]
	m.configMu.RUnlock()

	if !exists {
		config = DefaultConfig()
	}

	return createCache[K, V](m, name, config)
}

// GetCacheWithConfig returns the named typed cache, creating it from config when
// it does not yet exist, and needs no prior Register call. If the cache already
// exists the existing instance is returned and config is ignored (get-or-create).
func GetCacheWithConfig[K comparable, V any](m *Manager, name string, config Config) (*Cache[K, V], error) {
	if cached, ok := m.caches.Load(name); ok {
		return assertCache[K, V](name, cached)
	}
	return createCache[K, V](m, name, config)
}

// createCache builds a cache from config and races to publish it under name.
// LoadOrStore closes the loser of a concurrent create so its workers do not leak.
func createCache[K comparable, V any](m *Manager, name string, config Config) (*Cache[K, V], error) {
	cache, err := New[K, V](config)
	if err != nil {
		return nil, newCacheError("get", name, err)
	}
	if actual, loaded := m.caches.LoadOrStore(name, cache); loaded {
		cache.Close()
		return assertCache[K, V](name, actual)
	}
	return cache, nil
}

func assertCache[K comparable, V any](name string, cached any) (*Cache[K, V], error) {
	if cache, ok := cached.(*Cache[K, V]); ok {
		return cache, nil
	}
	return nil, newCacheError("get", name, ErrTypeMismatch)
}

// Stats returns performance statistics for all managed caches.
func (m *Manager) Stats() map[string]Stats {
	stats := make(map[string]Stats)

	m.caches.Range(func(key, value any) bool {
		if cache, ok := value.(statser); ok {
			stats[key.(string)] = cache.Stats()
		}
		return true
	})

	return stats
}

// CloseAll closes and removes every managed cache instance.
// Registered configurations are left intact, so the same names
// can be recreated from this manager afterwards.
func (m *Manager) CloseAll() error {
	var errs []error

	m.caches.Range(func(key, value any) bool {
		if cache, ok := value.(io.Closer); ok {
			if err := cache.Close(); err != nil {
				errs = append(errs, newCacheError("close", key.(string), err))
			}
		}
		m.caches.Delete(key)
		return true
	})

	return errors.Join(errs...)
}

// Remove closes and removes the named cache instance and drops its registered
// configuration.
func (m *Manager) Remove(name string) error {
	m.configMu.Lock()
	delete(m.configs, name)
	m.configMu.Unlock()

	if cached, ok := m.caches.LoadAndDelete(name); ok {
		if cache, ok := cached.(io.Closer); ok {
			return cache.Close()
		}
	}

	return nil
}

// RegisterGlobalCache registers a configuration in the global manager.
func RegisterGlobalCache(name string, config Config) error {
	return globalManager.Register(name, config)
}

// GetGlobalCache retrieves or creates a cache from the global manager. Caches
// created this way live for the lifetime of the process unless released with
// CloseAllGlobalCaches.
func GetGlobalCache[K comparable, V any](name string) (*Cache[K, V], error) {
	return GetCache[K, V](globalManager, name)
}

// GetGlobalCacheStats returns stats for all caches in the global manager.
func GetGlobalCacheStats() map[string]Stats {
	return globalManager.Stats()
}

// CloseAllGlobalCaches closes all caches in the global manager.
func CloseAllGlobalCaches() error {
	return globalManager.CloseAll()
}
