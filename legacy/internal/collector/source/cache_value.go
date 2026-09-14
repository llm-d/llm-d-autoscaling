package source

import (
	"sort"
	"strings"
	"time"
)

// CacheKey uniquely identifies a cached query result.
// It combines the query name with the parameter values used.
type CacheKey string

// CachedValue wraps a MetricResult with cache metadata.
type CachedValue struct {
	Result   MetricResult
	CachedAt time.Time
	TTL      time.Duration
	// Params stores the parameters used to generate this cached value.
	Params map[string]string
}

// IsExpired returns true if the cached value has exceeded its TTL.
func (c *CachedValue) IsExpired() bool {
	if c == nil {
		return true
	}
	return time.Since(c.CachedAt) > c.TTL
}

// Age returns how long ago the value was cached.
func (c *CachedValue) Age() time.Duration {
	if c == nil {
		return 0
	}
	return time.Since(c.CachedAt)
}

// IsFresh returns true if the cached value is within the freshness threshold.
func (c *CachedValue) IsFresh(threshold time.Duration) bool {
	if c == nil {
		return false
	}
	return time.Since(c.CachedAt) <= threshold
}

// BuildCacheKey constructs a cache key from query name and parameters.
// The key format is: "queryName:key1=value1,key2=value2" with sorted keys.
func BuildCacheKey(queryName string, params map[string]string) CacheKey {
	if len(params) == 0 {
		return CacheKey(queryName)
	}

	// Sort keys for deterministic key generation
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Build the cache key using strings.Builder to minimize allocations.
	var b strings.Builder

	// Estimate capacity: query name + ":" + each "key=value" pair and commas.
	estimated := len(queryName) + 1 // for the colon
	for i, k := range keys {
		estimated += len(k) + 1 + len(params[k]) // "key" + "=" + "value"
		if i > 0 {
			estimated++ // comma between pairs
		}
	}
	b.Grow(estimated)

	b.WriteString(queryName)
	b.WriteByte(':')

	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}

	return CacheKey(b.String())
}
