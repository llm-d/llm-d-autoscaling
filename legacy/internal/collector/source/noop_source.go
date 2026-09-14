package source

import (
	"context"
)

// NoOpSource is a minimal MetricsSource implementation with no queries.
// Useful for testing or as a placeholder when no metrics collection is needed.
type NoOpSource struct {
	queryList *QueryList
}

// NewNoOpSource creates a new no-op metrics source with an empty query list.
func NewNoOpSource() *NoOpSource {
	return &NoOpSource{
		queryList: NewQueryList(),
	}
}

// QueryList returns an empty, immutable query list.
func (n *NoOpSource) QueryList() *QueryList {
	return n.queryList
}

// Refresh always returns an empty result map.
func (n *NoOpSource) Refresh(ctx context.Context, spec RefreshSpec) (map[string]*MetricResult, error) {
	return make(map[string]*MetricResult), nil
}

// Get always returns nil since no values are cached.
func (n *NoOpSource) Get(queryName string, params map[string]string) *CachedValue {
	return nil
}
