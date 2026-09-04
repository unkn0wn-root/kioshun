package kioshun

import (
	"errors"
	"io"
	"reflect"
	"slices"
	"sync"
)

var globalManager = NewManager()

type statser interface {
	Stats() Stats
}

type cacheRegistration struct {
	config    Config
	cacheType reflect.Type
	factory   func() (any, error)
}

// Manager owns named cache instances and their registered configurations.
type Manager struct {
	caches        sync.Map
	registrations map[string]cacheRegistration
	configMu      sync.RWMutex
}

// NewManager returns an empty Manager.
func NewManager() *Manager {
	return &Manager{
		registrations: make(map[string]cacheRegistration),
	}
}

// Register stores the configuration for a named cache. Register a name before
// its first GetCache call.
func (m *Manager) Register(name string, config Config) error {
	if err := config.Validate(); err != nil {
		return newCacheError("register", name, err)
	}

	return m.register(name, cacheRegistration{config: config})
}

// RegisterCache registers a named cache with typed options such as WithWeigher.
func RegisterCache[K comparable, V any](m *Manager, name string, config Config, opts ...Option[K, V]) error {
	if err := config.Validate(); err != nil {
		return newCacheError("register", name, err)
	}

	opts = slices.Clone(opts)
	reg := cacheRegistration{
		config:    config,
		cacheType: cacheTypeOf[K, V](),
		factory: func() (any, error) {
			return New(config, opts...)
		},
	}
	return m.register(name, reg)
}

func (m *Manager) register(name string, reg cacheRegistration) error {
	m.configMu.Lock()
	defer m.configMu.Unlock()

	if _, exists := m.registrations[name]; exists {
		return newCacheError("register", name, ErrCacheExists)
	}

	m.registrations[name] = reg
	return nil
}

// GetCache returns the named cache, creating it from its registered configuration
// on first use. It returns ErrCacheNotRegistered for an unknown name.
func GetCache[K comparable, V any](m *Manager, name string) (*Cache[K, V], error) {
	if cached, ok := m.caches.Load(name); ok {
		return assertCache[K, V](name, cached)
	}

	m.configMu.RLock()
	reg, exists := m.registrations[name]
	m.configMu.RUnlock()

	if !exists {
		return nil, newCacheError("get", name, ErrCacheNotRegistered)
	}
	if reg.cacheType != nil && reg.cacheType != cacheTypeOf[K, V]() {
		return nil, newCacheError("get", name, ErrTypeMismatch)
	}

	return createCache[K, V](m, name, reg)
}

// GetCacheWithConfig returns the named cache, creating it from config if needed.
// If it already exists, config and opts are ignored.
func GetCacheWithConfig[K comparable, V any](
	m *Manager,
	name string,
	config Config,
	opts ...Option[K, V],
) (*Cache[K, V], error) {
	if cached, ok := m.caches.Load(name); ok {
		return assertCache[K, V](name, cached)
	}
	return createCache(m, name, cacheRegistration{config: config}, opts...)
}

func createCache[K comparable, V any](
	m *Manager,
	name string,
	reg cacheRegistration,
	opts ...Option[K, V],
) (*Cache[K, V], error) {
	var created any
	var err error
	if reg.factory != nil {
		created, err = reg.factory()
	} else {
		created, err = New(reg.config, opts...)
	}
	if err != nil {
		return nil, newCacheError("get", name, err)
	}

	cache, ok := created.(*Cache[K, V])
	if !ok {
		if closer, ok := created.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, newCacheError("get", name, ErrTypeMismatch)
	}

	if actual, loaded := m.caches.LoadOrStore(name, cache); loaded {
		_ = cache.Close()
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

func cacheTypeOf[K comparable, V any]() reflect.Type {
	return reflect.TypeFor[*Cache[K, V]]()
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

// CloseAll closes and removes every cache instance. Registrations remain, so a
// later GetCache can recreate them.
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

// Remove closes the named cache and removes its registration.
func (m *Manager) Remove(name string) error {
	m.configMu.Lock()
	delete(m.registrations, name)
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

// RegisterGlobalTypedCache registers a typed cache factory in the global manager.
func RegisterGlobalTypedCache[K comparable, V any](name string, config Config, opts ...Option[K, V]) error {
	return RegisterCache(globalManager, name, config, opts...)
}

// GetGlobalCache returns a named cache from the global manager, creating it from
// its registration on first use.
func GetGlobalCache[K comparable, V any](name string) (*Cache[K, V], error) {
	return GetCache[K, V](globalManager, name)
}

// GetGlobalCacheWithConfig returns a named global cache, creating it from config
// and opts if needed.
func GetGlobalCacheWithConfig[K comparable, V any](
	name string,
	config Config,
	opts ...Option[K, V],
) (*Cache[K, V], error) {
	return GetCacheWithConfig(globalManager, name, config, opts...)
}

// GetGlobalCacheStats returns stats for all caches in the global manager.
func GetGlobalCacheStats() map[string]Stats {
	return globalManager.Stats()
}

// CloseAllGlobalCaches closes all caches in the global manager.
func CloseAllGlobalCaches() error {
	return globalManager.CloseAll()
}
