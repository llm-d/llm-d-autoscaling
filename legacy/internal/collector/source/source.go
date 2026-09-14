// Package source provides metrics collection functionality.
//
// This file defines the MetricsSource interface and related types for
// collecting and caching metrics from various backends.
package source

import (
	"context"
	"time"
)

// MetricsSource defines the interface for a metrics collection source.
// Implementations collect metrics from a specific backend and cache results.
type MetricsSource interface {
	// QueryList returns the query registry for this source.
	// Use this to register queries specific to this source.
	QueryList() *QueryList

	// Refresh executes queries and updates the cache.
	// If spec.Queries is empty, refreshes all registered queries.
	// Returns a map of query name to result.
	Refresh(ctx context.Context, spec RefreshSpec) (map[string]*MetricResult, error)

	// Get retrieves a cached value for a query with the given parameters.
	// The cache key is constructed from both queryName and params.
	// Returns nil if not cached or expired
	// The returned CachedValue must not be modified by the caller.
	Get(queryName string, params map[string]string) *CachedValue
}

// MetricValue represents a single metric value with its metadata.
type MetricValue struct {
	// Value is the metric value (scalar).
	Value float64
	// Timestamp is when the metric was sampled by the backend.
	// For Prometheus, this is the sample timestamp from the query result.
	Timestamp time.Time
	// Labels contains any labels associated with the metric (e.g., pod name).
	Labels map[string]string
}

// IsStale returns true if the metric sample timestamp is older than the given threshold.
func (v MetricValue) IsStale(threshold time.Duration) bool {
	if v.Timestamp.IsZero() {
		return true
	}
	return time.Since(v.Timestamp) > threshold
}

// Age returns how old the metric sample is based on its timestamp.
func (v MetricValue) Age() time.Duration {
	if v.Timestamp.IsZero() {
		return 0
	}
	return time.Since(v.Timestamp)
}

// MetricResult represents the result of a single query.
type MetricResult struct {
	// QueryName is the registered query name (e.g., "kv_cache_usage").
	QueryName string
	// Values contains all metric values returned by the query.
	// For scalar queries, this will have one element.
	// For vector queries, this will have one element per time series.
	Values []MetricValue
	// CollectedAt is when this result was fetched from the backend.
	CollectedAt time.Time
	// Error is set if the query failed.
	Error error
}

// IsStale returns true if any metric value's timestamp is older than the given threshold.
// Returns true if there are no values or if the oldest sample exceeds the threshold.
func (r *MetricResult) IsStale(threshold time.Duration) bool {
	if r == nil || len(r.Values) == 0 {
		return true
	}
	// Check if any value is stale (use oldest timestamp)
	for _, v := range r.Values {
		if v.IsStale(threshold) {
			return true
		}
	}
	return false
}

// OldestTimestamp returns the oldest metric sample timestamp from all values.
func (r *MetricResult) OldestTimestamp() time.Time {
	if r == nil || len(r.Values) == 0 {
		return time.Time{}
	}
	oldest := r.Values[0].Timestamp
	for _, v := range r.Values[1:] {
		if v.Timestamp.Before(oldest) {
			oldest = v.Timestamp
		}
	}
	return oldest
}

// Age returns the age of the oldest metric sample based on its timestamp.
func (r *MetricResult) Age() time.Duration {
	oldest := r.OldestTimestamp()
	if oldest.IsZero() {
		return 0
	}
	return time.Since(oldest)
}

// HasError returns true if the query resulted in an error.
func (r *MetricResult) HasError() bool {
	return r != nil && r.Error != nil
}

// FirstValue returns the first metric value, or zero value if none exist.
func (r *MetricResult) FirstValue() MetricValue {
	if r == nil || len(r.Values) == 0 {
		return MetricValue{}
	}
	return r.Values[0]
}

// RefreshSpec specifies which queries to refresh and with what parameters.
type RefreshSpec struct {
	// Queries is the list of query names to refresh.
	// If empty, all registered queries for the backend will be refreshed.
	Queries []string
	// Params are the parameters to use for query building.
	Params map[string]string
}
