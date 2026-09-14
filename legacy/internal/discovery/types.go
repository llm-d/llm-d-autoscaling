package discovery

// AcceleratorModelInfo contains details about a discovered accelerator model on a node.
type AcceleratorModelInfo struct {
	Count  int
	Memory string
}

// NodeInfo is the canonical per-node view returned by NodeDiscovery.
// It includes the node's labels and all discovered accelerator models,
// enabling label-aware features (e.g., namespace-scoped inventory, node affinity)
// without re-listing nodes.
type NodeInfo struct {
	// Name is the Kubernetes node name.
	Name string
	// Labels is a copy of the node's labels (safe to mutate).
	Labels map[string]string
	// Accelerators is a map of accelerator model name (e.g., "NVIDIA-A100-PCIE-80GB")
	// to AcceleratorModelInfo (count, memory).
	Accelerators map[string]AcceleratorModelInfo
}
