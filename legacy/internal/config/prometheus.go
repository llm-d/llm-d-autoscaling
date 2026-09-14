package config

import (
	"time"
)

// prometheusConfig holds all Prometheus-related configuration
// (both connection settings and cache config)
type prometheusConfig struct {
	// Immutable (set at startup)
	baseURL            string
	bearerToken        string
	tokenPath          string
	insecureSkipVerify bool
	allowHTTP          bool
	caCertPath         string
	clientCertPath     string
	clientKeyPath      string
	serverName         string

	// Mutable (can change at runtime)
	cache *CacheConfig
}

// CacheConfig holds configuration for the metrics cache.
// This is the shared configuration type used by all collector plugins (Prometheus, EPP, etc.).
type CacheConfig struct {
	TTL             time.Duration
	CleanupInterval time.Duration
	// FetchInterval is how often to fetch metrics in background (0 = disable background fetching)
	FetchInterval time.Duration
	// FreshnessThresholds define when metrics are considered fresh/stale/unavailable
	FreshnessThresholds FreshnessThresholds
}

// FreshnessThresholds defines when metrics are considered fresh, stale, or unavailable.
// This is the shared type used by all collector plugins.
type FreshnessThresholds struct {
	FreshThreshold       time.Duration // Metrics are fresh if age < this (default: 1 minute)
	StaleThreshold       time.Duration // Metrics are stale if age >= this but < unavailable (default: 2 minutes)
	UnavailableThreshold time.Duration // Metrics are unavailable if age >= this (default: 5 minutes)
}

// DetermineStatus determines the freshness status based on age.
// Returns "fresh", "stale", or "unavailable" based on the configured thresholds.
func (ft FreshnessThresholds) DetermineStatus(age time.Duration) string {
	if age < ft.FreshThreshold {
		return "fresh"
	} else if age < ft.UnavailableThreshold {
		return "stale"
	}
	return "unavailable"
}

// DefaultFreshnessThresholds returns default freshness thresholds
func DefaultFreshnessThresholds() FreshnessThresholds {
	return FreshnessThresholds{
		FreshThreshold:       1 * time.Minute,
		StaleThreshold:       2 * time.Minute,
		UnavailableThreshold: 5 * time.Minute,
	}
}

// ============================================================================
// Prometheus Getters (thread-safe)
// ============================================================================

// PrometheusBaseURL returns the Prometheus base URL.
// Thread-safe.
func (c *Config) PrometheusBaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.prometheus.baseURL
}

// PrometheusBearerToken returns the Prometheus bearer token.
// Thread-safe.
func (c *Config) PrometheusBearerToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.prometheus.bearerToken
}

// PrometheusTokenPath returns the Prometheus token path.
// Thread-safe.
func (c *Config) PrometheusTokenPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.prometheus.tokenPath
}

// PrometheusInsecureSkipVerify returns whether to skip TLS verification.
// Thread-safe.
func (c *Config) PrometheusInsecureSkipVerify() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.prometheus.insecureSkipVerify
}

// PrometheusAllowHTTP returns whether plain HTTP connections to Prometheus are allowed.
// Thread-safe.
func (c *Config) PrometheusAllowHTTP() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.prometheus.allowHTTP
}

// PrometheusCACertPath returns the Prometheus CA certificate path.
// Thread-safe.
func (c *Config) PrometheusCACertPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.prometheus.caCertPath
}

// PrometheusClientCertPath returns the Prometheus client certificate path.
// Thread-safe.
func (c *Config) PrometheusClientCertPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.prometheus.clientCertPath
}

// PrometheusClientKeyPath returns the Prometheus client key path.
// Thread-safe.
func (c *Config) PrometheusClientKeyPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.prometheus.clientKeyPath
}

// PrometheusServerName returns the Prometheus server name.
// Thread-safe.
func (c *Config) PrometheusServerName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.prometheus.serverName
}

// PrometheusCacheConfig returns the current Prometheus cache configuration.
// Thread-safe. Returns a copy to prevent external modifications.
func (c *Config) PrometheusCacheConfig() *CacheConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.prometheus.cache == nil {
		return nil
	}
	// Return a copy to prevent external modifications
	cp := *c.prometheus.cache
	return &cp
}
