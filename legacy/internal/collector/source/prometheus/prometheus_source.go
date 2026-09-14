// Package prometheus provides the Prometheus metrics source implementation.
//
// This package implements the Prometheus metrics source that executes
// registered queries and caches results.
package prometheus

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	prometheusutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/prometheus"
)

// PrometheusSourceConfig contains configuration for the Prometheus source.
type PrometheusSourceConfig struct {
	// DefaultTTL is the default cache TTL for query results.
	DefaultTTL time.Duration
	// QueryTimeout is the timeout for individual Prometheus queries.
	QueryTimeout time.Duration
}

// DefaultPrometheusSourceConfig returns sensible defaults.
func DefaultPrometheusSourceConfig() PrometheusSourceConfig {
	return PrometheusSourceConfig{
		DefaultTTL:   30 * time.Second,
		QueryTimeout: 10 * time.Second,
	}
}

// PrometheusSource implements MetricsSource for Prometheus backend.
type PrometheusSource struct {
	api      promv1.API
	registry *source.QueryList // registry stores query templates for this source
	config   PrometheusSourceConfig

	mu    sync.RWMutex // protects the cache and refresh operations
	cache *source.Cache
}

// NewPrometheusSource creates a new Prometheus metrics source with a default query registry.
func NewPrometheusSource(ctx context.Context, api promv1.API, config PrometheusSourceConfig) *PrometheusSource {
	return &PrometheusSource{
		api:      api,
		registry: source.NewQueryList(),
		config:   config,
		cache:    source.NewCache(ctx, config.DefaultTTL, 1*time.Second),
	}
}

// QueryList returns the query registry for this source.
// Use this to register queries specific to this source.
func (p *PrometheusSource) QueryList() *source.QueryList {
	return p.registry
}

// Refresh executes queries and updates the cache.
// If spec.Queries is empty, refreshes all registered queries for this source.
func (p *PrometheusSource) Refresh(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	logger := ctrl.LoggerFrom(ctx)

	// Determine which queries to execute
	queryNames := spec.Queries
	if len(queryNames) == 0 {
		// Get all registered query names for this source
		queryNames = p.registry.List()
	}

	if len(queryNames) == 0 {
		logger.V(logging.DEBUG).Info("No queries registered for this Prometheus source")
		return map[string]*source.MetricResult{}, nil
	}

	results := make(map[string]*source.MetricResult)
	var resultsMu sync.Mutex

	// Execute queries concurrently
	var wg sync.WaitGroup
	for _, name := range queryNames {
		wg.Add(1)
		go func(queryName string) {
			defer wg.Done()

			result := p.executeQuery(ctx, queryName, spec.Params)

			resultsMu.Lock()
			results[queryName] = result
			resultsMu.Unlock()

			// Update cache with key that includes params
			cacheKey := source.BuildCacheKey(queryName, spec.Params)
			p.cache.Set(cacheKey, *result, p.config.DefaultTTL)
		}(name)
	}

	wg.Wait()

	logger.V(logging.DEBUG).Info("Refreshed Prometheus metrics",
		"queriesExecuted", len(queryNames),
		"queriesSucceeded", countSuccessful(results))

	return results, nil
}

// executeQuery builds and executes a single query.
func (p *PrometheusSource) executeQuery(ctx context.Context, queryName string, params map[string]string) *source.MetricResult {
	logger := ctrl.LoggerFrom(ctx)

	// Escape parameter values to prevent PromQL injection
	escapedParams := make(map[string]string, len(params))
	for k, v := range params {
		escapedParams[k] = source.EscapePromQLValue(v)
	}

	// Build the query string
	queryStr, err := p.registry.Build(queryName, escapedParams)
	if err != nil {
		return &source.MetricResult{
			QueryName:   queryName,
			CollectedAt: time.Now(),
			Error:       fmt.Errorf("failed to build query: %w", err),
		}
	}

	// Apply query timeout
	queryCtx := ctx
	if p.config.QueryTimeout > 0 {
		var cancel context.CancelFunc
		queryCtx, cancel = context.WithTimeout(ctx, p.config.QueryTimeout)
		defer cancel()
	}

	// Execute query with backoff
	val, warnings, err := prometheusutil.QueryPrometheusWithBackoff(queryCtx, p.api, queryStr)
	if err != nil {
		return &source.MetricResult{
			QueryName:   queryName,
			CollectedAt: time.Now(),
			Error:       fmt.Errorf("query execution failed: %w", err),
		}
	}

	if len(warnings) > 0 {
		logger.V(logging.DEBUG).Info("Prometheus query warnings",
			"query", queryName,
			"warnings", warnings)
	}

	// Parse the result
	values := p.parseResult(val)

	return &source.MetricResult{
		QueryName:   queryName,
		Values:      values,
		CollectedAt: time.Now(),
	}
}

// parseResult converts Prometheus query result to source.MetricValues.
func (p *PrometheusSource) parseResult(val model.Value) []source.MetricValue {
	if val == nil {
		return nil
	}

	switch v := val.(type) {
	case model.Vector:
		return p.parseVector(v)
	case *model.Scalar:
		return p.parseScalar(v)
	case model.Matrix:
		return p.parseMatrix(v)
	default:
		return nil
	}
}

// parseVector parses a Prometheus vector result.
func (p *PrometheusSource) parseVector(vec model.Vector) []source.MetricValue {
	values := make([]source.MetricValue, 0, len(vec))
	for _, sample := range vec {
		value := float64(sample.Value)
		fixNaN(&value)

		labels := make(map[string]string)
		for k, v := range sample.Metric {
			labels[string(k)] = string(v)
		}

		values = append(values, source.MetricValue{
			Value:     value,
			Timestamp: sample.Timestamp.Time(),
			Labels:    labels,
		})
	}
	return values
}

// parseScalar parses a Prometheus scalar result.
func (p *PrometheusSource) parseScalar(scalar *model.Scalar) []source.MetricValue {
	if scalar == nil {
		return nil
	}

	value := float64(scalar.Value)
	fixNaN(&value)

	return []source.MetricValue{{
		Value:     value,
		Timestamp: scalar.Timestamp.Time(),
	}}
}

// parseMatrix parses a Prometheus matrix result (range query).
// Returns the latest value from each time series.
func (p *PrometheusSource) parseMatrix(matrix model.Matrix) []source.MetricValue {
	values := make([]source.MetricValue, 0, len(matrix))
	for _, stream := range matrix {
		if len(stream.Values) == 0 {
			continue
		}

		// Get the latest sample
		latest := stream.Values[len(stream.Values)-1]
		value := float64(latest.Value)
		fixNaN(&value)

		labels := make(map[string]string)
		for k, v := range stream.Metric {
			labels[string(k)] = string(v)
		}

		values = append(values, source.MetricValue{
			Value:     value,
			Timestamp: latest.Timestamp.Time(),
			Labels:    labels,
		})
	}
	return values
}

// Get retrieves a cached value for a query with given parameters.
// The cache key is constructed from both queryName and params.
// Returns nil if not cached or expired.
func (p *PrometheusSource) Get(queryName string, params map[string]string) *source.CachedValue {
	p.mu.RLock()
	defer p.mu.RUnlock()

	cacheKey := source.BuildCacheKey(queryName, params)
	cached, ok := p.cache.Get(cacheKey)
	if !ok {
		return nil
	}

	if cached.IsExpired() {
		return nil
	}

	return cached
}

// MustGet retrieves a cached result or refreshes if expired.
// This is a convenience method for cases where you always want a result.
func (p *PrometheusSource) MustGet(ctx context.Context, queryName string, params map[string]string) *source.MetricResult {
	cached := p.Get(queryName, params)
	if cached != nil {
		return &cached.Result
	}

	// Not cached or expired, refresh this specific query
	results, err := p.Refresh(ctx, source.RefreshSpec{
		Queries: []string{queryName},
		Params:  params,
	})
	if err != nil {
		return &source.MetricResult{
			QueryName:   queryName,
			CollectedAt: time.Now(),
			Error:       err,
		}
	}

	if result, ok := results[queryName]; ok {
		return result
	}

	return &source.MetricResult{
		QueryName:   queryName,
		CollectedAt: time.Now(),
		Error:       fmt.Errorf("query %q not found in results", queryName),
	}
}

// --- Helpers ---

// fixNaN replaces NaN and Inf values with 0.
func fixNaN(v *float64) {
	if math.IsNaN(*v) || math.IsInf(*v, 0) {
		*v = 0
	}
}

// countSuccessful counts results without errors.
func countSuccessful(results map[string]*source.MetricResult) int {
	count := 0
	for _, r := range results {
		if r != nil && r.Error == nil {
			count++
		}
	}
	return count
}
