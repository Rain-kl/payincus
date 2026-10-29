// Port of server/src/lib/config-cache.ts.
//
// Process-local cache for system-config values (PERF001). The architecture
// invariant is a single private backend process (see AGENTS.md), so an in-memory
// cache is authoritative: a second process would keep its own copy and break
// update invalidation, and would have to be moved to PostgreSQL or another shared
// store first.
//
// Every entry carries an absolute expiry; entries are dropped lazily on read, on
// write when the store is full, and eagerly on invalidation. All access is guarded
// by a mutex because routes and workers touch the cache concurrently.
package lib

import (
	"container/list"
	"sync"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// Cache configuration, mirroring CACHE_CONFIG.
const (
	// configCacheTTL is the entry lifetime (5 minutes; config changes are rare).
	configCacheTTL = 5 * time.Minute
	// configCacheMaxSize bounds the number of cached entries.
	configCacheMaxSize = 100
	// configCacheEvictBatch is how many oldest entries are dropped when the
	// cache is still full after expired entries were purged.
	configCacheEvictBatch = 10
)

// configCacheNow is swappable in tests so TTL expiry can be exercised without
// sleeping. Production code must leave it as time.Now.
var configCacheNow = time.Now

type configCacheEntry struct {
	key       string
	value     *string
	expiresAt time.Time
}

// configCacheStore keeps insertion order (like a JS Map) alongside O(1) lookups.
// The order matters: the Node implementation evicts the ten oldest keys, and
// re-setting an existing key must not move it to the back.
type configCacheStore struct {
	mu    sync.Mutex
	items map[string]*list.Element
	order *list.List
}

func newConfigCacheStore() *configCacheStore {
	return &configCacheStore{
		items: make(map[string]*list.Element),
		order: list.New(),
	}
}

var configCache = newConfigCacheStore()

func (s *configCacheStore) get(key string) (*string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	element, ok := s.items[key]
	if !ok {
		return nil, false
	}
	entry := element.Value.(*configCacheEntry)
	if configCacheNow().After(entry.expiresAt) {
		s.deleteLocked(key)
		return nil, false
	}
	return entry.value, true
}

func (s *configCacheStore) set(key string, value *string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.evictIfFullLocked()

	if element, ok := s.items[key]; ok {
		// Map.set on an existing key keeps its original insertion position.
		entry := element.Value.(*configCacheEntry)
		entry.value = value
		entry.expiresAt = configCacheNow().Add(configCacheTTL)
		return
	}

	entry := &configCacheEntry{key: key, value: value, expiresAt: configCacheNow().Add(configCacheTTL)}
	s.items[key] = s.order.PushBack(entry)
}

// evictIfFullLocked drops expired entries first, then the oldest keys, mirroring
// setCachedConfig's MAX_SIZE handling. Callers must hold s.mu.
func (s *configCacheStore) evictIfFullLocked() {
	if s.order.Len() < configCacheMaxSize {
		return
	}

	now := configCacheNow()
	for element := s.order.Front(); element != nil; {
		next := element.Next()
		if now.After(element.Value.(*configCacheEntry).expiresAt) {
			s.order.Remove(element)
			delete(s.items, element.Value.(*configCacheEntry).key)
		}
		element = next
	}

	if s.order.Len() >= configCacheMaxSize {
		for removed := 0; removed < configCacheEvictBatch && s.order.Len() > 0; removed++ {
			oldest := s.order.Front()
			s.order.Remove(oldest)
			delete(s.items, oldest.Value.(*configCacheEntry).key)
		}
	}
}

func (s *configCacheStore) deleteLocked(key string) {
	if element, ok := s.items[key]; ok {
		s.order.Remove(element)
		delete(s.items, key)
	}
}

func (s *configCacheStore) invalidate(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteLocked(key)
}

func (s *configCacheStore) invalidateMany(keys []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range keys {
		s.deleteLocked(key)
	}
}

func (s *configCacheStore) invalidateAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = make(map[string]*list.Element)
	s.order.Init()
}

func (s *configCacheStore) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len()
}

// GetCachedConfig mirrors getCachedConfig. It returns the cached value and
// whether the key is present; a present key may legitimately hold a nil value
// (a cached "null" for a missing config row).
func GetCachedConfig(key string) (*string, bool) {
	return configCache.get(key)
}

// SetCachedConfig mirrors setCachedConfig.
func SetCachedConfig(key string, value *string) {
	configCache.set(key, value)
}

// InvalidateCachedConfig mirrors invalidateCachedConfig.
func InvalidateCachedConfig(key string) {
	configCache.invalidate(key)
}

// InvalidateAllConfigCache mirrors invalidateAllConfigCache.
func InvalidateAllConfigCache() {
	configCache.invalidateAll()
}

// InvalidateCachedConfigs mirrors invalidateCachedConfigs.
func InvalidateCachedConfigs(keys []string) {
	configCache.invalidateMany(keys)
}

// ConfigCacheStats mirrors the getCacheStats() return shape; TTL is in
// milliseconds to match the Node implementation.
type ConfigCacheStats struct {
	Size    int   `json:"size"`
	MaxSize int   `json:"maxSize"`
	TTL     int64 `json:"ttl"`
}

// GetCacheStats mirrors getCacheStats.
func GetCacheStats() ConfigCacheStats {
	return ConfigCacheStats{
		Size:    configCache.size(),
		MaxSize: configCacheMaxSize,
		TTL:     int64(configCacheTTL / time.Millisecond),
	}
}

// systemConfigCacheBackend adapts this cache to the interface db/system-config.ts
// consumes.
type systemConfigCacheBackend struct{}

func (systemConfigCacheBackend) Get(key string) (*string, bool) { return GetCachedConfig(key) }
func (systemConfigCacheBackend) Set(key string, value *string)  { SetCachedConfig(key, value) }
func (systemConfigCacheBackend) Invalidate(key string)          { InvalidateCachedConfig(key) }
func (systemConfigCacheBackend) InvalidateAll()                 { InvalidateAllConfigCache() }
func (systemConfigCacheBackend) InvalidateMany(keys []string)   { InvalidateCachedConfigs(keys) }

// The config cache lives in package lib, but db/system-config.go needs it and
// package lib already imports package db (lib/operation_verification.go), so db
// cannot import lib back. The dependency is inverted: register the cache into db
// at package-init time. db is always initialized before lib because lib imports it.
func init() {
	db.RegisterSystemConfigCache(systemConfigCacheBackend{})
}
