package cache

import (
	"fmt"
	"sync"
)

type Manager struct {
	caches   sync.Map
	configs  map[string]Config
	configMu sync.RWMutex
}

func NewManager() *Manager {
	return &Manager{
		configs: make(map[string]Config),
	}
}

var GlobalManager = NewManager()

func (m *Manager) RegisterCache(name string, config Config) error {
	m.configMu.Lock()
	defer m.configMu.Unlock()

	if _, exists := m.configs[name]; exists {
		return fmt.Errorf("cache '%s' already exists", name)
	}

	m.configs[name] = config
	return nil
}

func GetCache[K comparable, V any](m *Manager, name string) (*InMemoryCache[K, V], error) {
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
	if actual, loaded := m.caches.LoadOrStore(name, cache); loaded {
		// another goroutine created it first, close our instance and return the existing one
		cache.Close()
		if existingCache, ok := actual.(*InMemoryCache[K, V]); ok {
			return existingCache, nil
		}
		return nil, fmt.Errorf("cache '%s' exists but with different types", name)
	}

	return cache, nil
}

func (m *Manager) GetCacheStats() map[string]Stats {
	stats := make(map[string]Stats)

	m.caches.Range(func(key, value interface{}) bool {
		if name, ok := key.(string); ok {
			if cache, ok := value.(interface{ Stats() Stats }); ok {
				stats[name] = cache.Stats()
			}
		}
		return true
	})

	return stats
}

func (m *Manager) CloseAll() error {
	var errors []error

	m.caches.Range(func(key, value interface{}) bool {
		if cache, ok := value.(interface{ Close() error }); ok {
			if err := cache.Close(); err != nil {
				errors = append(errors, err)
			}
		}
		return true
	})

	m.caches.Range(func(key, value interface{}) bool {
		m.caches.Delete(key)
		return true
	})

	if len(errors) > 0 {
		return fmt.Errorf("errors closing caches: %v", errors)
	}

	return nil
}

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

func RegisterGlobalCache(name string, config Config) error {
	return GlobalManager.RegisterCache(name, config)
}

func GetGlobalCache[K comparable, V any](name string) (*InMemoryCache[K, V], error) {
	return GetCache[K, V](GlobalManager, name)
}

func GetGlobalCacheStats() map[string]Stats {
	return GlobalManager.GetCacheStats()
}

func CloseAllGlobalCaches() error {
	return GlobalManager.CloseAll()
}
