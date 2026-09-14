package discovery

import "context"

// CapacityDiscovery defines the interface for discovering accelerator capacity in the cluster.
type CapacityDiscovery interface {
	// Discover returns a map of node names to their accelerator inventory.
	// The inner map is keyed by accelerator model name (e.g. "NVIDIA-A100").
	// Returns detailed per-node info needed for capacity planning and node selection.
	Discover(ctx context.Context) (map[string]map[string]AcceleratorModelInfo, error)
}

// UsageDiscovery defines the interface for discovering current GPU usage in the cluster.
type UsageDiscovery interface {
	// DiscoverUsage returns a map of accelerator type to used GPU count.
	// This is calculated by summing GPU requests from all running pods.
	// Returns aggregated counts (not per-node) since limiter only needs cluster-wide totals.
	// Note: The return type differs from Discover() intentionally - usage tracking needs
	// simple aggregated counts, while capacity discovery needs detailed per-node info.
	DiscoverUsage(ctx context.Context) (map[string]int, error)
}

// NodeDiscovery defines the interface for discovering per-node information including
// labels and accelerator capacity. Enables label-aware features that need to match
// node selectors against cluster nodes (e.g., namespace-scoped inventory).
type NodeDiscovery interface {
	// DiscoverNodes returns per-node info keyed by node name. Only nodes with at
	// least one discovered accelerator are included. The returned map and all
	// nested maps are freshly allocated and safe to mutate.
	DiscoverNodes(ctx context.Context) (map[string]NodeInfo, error)
}

// FullDiscovery combines capacity, usage, and node discovery for complete inventory tracking.
type FullDiscovery interface {
	CapacityDiscovery
	UsageDiscovery
	NodeDiscovery
}
