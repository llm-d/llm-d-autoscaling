package source

import (
	"fmt"
	"sync"
)

// SourceRegistry manages multiple metrics sources.
// Use DefaultSourceRegistry() to access the singleton instance,
// or NewSourceRegistry() to create isolated instances for testing.
type SourceRegistry struct {
	mu      sync.RWMutex
	sources map[string]MetricsSource
}

// NewSourceRegistry creates a new source registry.
// For production use, prefer DefaultSourceRegistry() to access the singleton.
// This constructor is useful for testing with isolated registries.
func NewSourceRegistry() *SourceRegistry {
	return &SourceRegistry{
		sources: make(map[string]MetricsSource),
	}
}

// Register adds a metrics source to the registry.
// The source is identified by a unique name (e.g., "prometheus").
func (r *SourceRegistry) Register(name string, source MetricsSource) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.sources[name]; exists {
		return fmt.Errorf("source %q already registered", name)
	}

	r.sources[name] = source
	return nil
}

// MustRegister is like Register but panics on error.
func (r *SourceRegistry) MustRegister(name string, source MetricsSource) {
	if err := r.Register(name, source); err != nil {
		panic(fmt.Sprintf("failed to register source: %v", err))
	}
}

// Get retrieves a registered source by name.
func (r *SourceRegistry) Get(name string) MetricsSource {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.sources[name]
}

// List returns all registered source names.
func (r *SourceRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.sources))
	for name := range r.sources {
		names = append(names, name)
	}
	return names
}

// Unregister removes a source from the registry.
// This operation is idempotent - it succeeds even if the source is not registered.
// This makes deletion paths clean and idempotent, especially useful for reconcile
// retries and race conditions where PoolDelete may be called multiple times.
func (r *SourceRegistry) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Idempotent: no-op if source doesn't exist
	delete(r.sources, name)
	return nil
}

// Update replaces an existing source or registers a new one.
// Unlike Register, this method does not return an error if the source already exists.
func (r *SourceRegistry) Update(name string, source MetricsSource) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.sources[name] = source
	return nil
}
