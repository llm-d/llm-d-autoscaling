package source

import (
	"context"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Cache is an in-memory thread-safe metrics cache implementation
type Cache struct {
	// cache stores the cached metrics
	cache map[CacheKey]*CachedValue
	mu    sync.RWMutex // protects the cache map

	// configuredTTL is the time-to-live for cache entries configured at creation time.
	// This value is used when Set() is called with ttl=0 (meaning "use configured TTL").
	// It comes from CacheConfig.TTL but is stored here to avoid keeping a reference to the config.
	configuredTTL time.Duration

	// cleanupInterval is how often to run cleanup of expired entries
	cleanupInterval time.Duration
}

// NewCache creates a new in-memory cache
func NewCache(ctx context.Context, configuredTTL time.Duration, cleanupInterval time.Duration) *Cache {
	c := &Cache{
		configuredTTL:   configuredTTL,
		cleanupInterval: cleanupInterval,
		cache:           make(map[CacheKey]*CachedValue),
	}

	// Start background cleanup if interval is set
	if cleanupInterval > 0 {
		go c.startCleanup(ctx)
	}

	return c
}

// get retrieves cached metrics by key
// Returns the cached metrics and true if found and not expired, false otherwise
func (c *Cache) Get(key CacheKey) (*CachedValue, bool) {
	c.mu.RLock()
	value, ok := c.cache[key]
	c.mu.RUnlock()

	if !ok || value == nil || value.IsExpired() {
		return nil, false
	}

	return value, true
}

// Set stores metrics in the cache
func (c *Cache) Set(key CacheKey, data MetricResult, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Use configured TTL if not specified (ttl=0 means "use configured value")
	if ttl == 0 {
		ttl = c.configuredTTL
	}

	cached := &CachedValue{
		Result:   data,
		CachedAt: time.Now(),
		TTL:      ttl,
	}

	c.cache[key] = cached
}

// startCleanup runs a background goroutine to periodically clean up expired entries
func (c *Cache) startCleanup(ctx context.Context) {
	ticker := time.NewTicker(c.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.cleanupExpired()
		}
	}
}

// cleanupExpired removes all expired entries from the cache
func (c *Cache) cleanupExpired() {
	expiredCount := 0
	c.mu.Lock()
	for key, value := range c.cache {
		if value.IsExpired() {
			delete(c.cache, key)
			expiredCount++
		}
	}
	c.mu.Unlock()

	if expiredCount > 0 {
		ctrl.Log.V(logging.DEBUG).Info("Cache cleanup: removed expired entries", "count", expiredCount)
	}
}
