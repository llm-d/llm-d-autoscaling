package discovery

import (
	"context"
	"fmt"
	"os"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// K8sWithGpuOperator implements CapacityDiscovery for Kubernetes clusters with GPU Operator
type K8sWithGpuOperator struct {
	Client         client.Client
	metricsEmitter *metrics.MetricsEmitter
}

// NewK8sWithGpuOperator creates a new K8sWithGpuOperator instance.
func NewK8sWithGpuOperator(client client.Client) *K8sWithGpuOperator {
	return &K8sWithGpuOperator{
		Client:         client,
		metricsEmitter: metrics.NewMetricsEmitter(),
	}
}

// listGPUNodes queries GPU-bearing nodes across all supported vendors
// (NVIDIA, AMD, Intel) and returns a canonical per-node view keyed by node name.
// It queries per vendor because Kubernetes LabelSelectors don't support OR logic
// across different label keys. Multi-vendor nodes (nodes with labels from more
// than one vendor) are merged into a single NodeInfo entry.
//
// This is the single internal node-listing primitive; public methods Discover,
// discoverNodeGPUTypes, and DiscoverNodes project from its result.
func (d *K8sWithGpuOperator) listGPUNodes(ctx context.Context) (map[string]NodeInfo, error) {
	var err error
	defer func() {
		if err != nil {
			metrics.SetGpuDiscoveryUp(0)
		} else {
			metrics.SetGpuDiscoveryUp(1)
		}
	}()

	nodes := make(map[string]NodeInfo)

	// Parse WVA_NODE_SELECTOR once for reuse across vendor queries
	var userRequirements []labels.Requirement
	if selectorStr := os.Getenv("WVA_NODE_SELECTOR"); selectorStr != "" {
		userSelector, parseErr := labels.Parse(selectorStr)
		if parseErr != nil {
			err = fmt.Errorf("invalid WVA_NODE_SELECTOR: %w", parseErr)
			return nil, err
		}
		userRequirements, _ = userSelector.Requirements()
	}

	// Query nodes for each GPU vendor separately.
	// K8s LabelSelectors don't support OR logic across different keys (e.g. nvidia OR amd).
	for _, res := range constants.VendorResources {
		vendor := res.Vendor
		memKey := res.MemoryLabel
		prodKey := res.ProductLabel
		resName := corev1.ResourceName(res.ResourceName)

		req, reqErr := labels.NewRequirement(prodKey, selection.Exists, nil)
		if reqErr != nil {
			err = fmt.Errorf("failed to create label requirement for %s: %w", vendor, reqErr)
			return nil, err
		}
		selector := labels.NewSelector().Add(*req)

		// Add user requirements for sharding
		for _, userReq := range userRequirements {
			selector = selector.Add(userReq)
		}

		var nodeList corev1.NodeList
		if listErr := d.Client.List(ctx, &nodeList, &client.ListOptions{LabelSelector: selector}); listErr != nil {
			err = fmt.Errorf("failed to list nodes for vendor %s: %w", vendor, listErr)
			return nil, err
		}

		// Process nodes for this vendor
		accelerators := make(map[string]int)
		for _, node := range nodeList.Items {
			model, ok := node.Labels[prodKey]
			if !ok {
				continue
			}

			count := 0
			if cap, ok := node.Status.Allocatable[resName]; ok {
				count = int(cap.Value())
			}

			ni, exists := nodes[node.Name]
			if !exists {
				ni = NodeInfo{
					Name:         node.Name,
					Labels:       copyStringMap(node.Labels),
					Accelerators: make(map[string]AcceleratorModelInfo),
				}
			}
			// i915 and xe resources use the same gpu.intel.com/product label, so
			// a node can be selected once for the matching resource and once for the
			// sibling resource that has no allocatable entry. Preserve labeled
			// nodes with no allocatable resource, but skip the duplicate zero-count
			// pass when this node/model was already recorded.
			if _, seen := ni.Accelerators[model]; seen && count == 0 {
				nodes[node.Name] = ni
				continue
			}
			ni.Accelerators[model] = AcceleratorModelInfo{
				Count:  count,
				Memory: node.Labels[memKey],
			}
			nodes[node.Name] = ni
			accelerators[model] += count
		}

		// record metric as soon as accelerators are discovered. For this vendor, record number of GPUs per accelerator type.
		for model, count := range accelerators {
			d.metricsEmitter.RecordAvailableGPUsMetric(vendor, model, accelerator.NormalizeAcceleratorName(model), int32(count))
		}
	}

	return nodes, nil
}

// copyStringMap returns a shallow copy of m, or an empty map if m is nil.
// Used to ensure the labels map returned in NodeInfo is independent of the
// underlying corev1.Node object.
func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Discover discovers GPU capacity by iterating over nodes and checking GFD labels.
// It queries nodes for each GPU vendor (NVIDIA, AMD, Intel) separately since
// Kubernetes LabelSelectors don't support OR logic across different label keys.
//
// This is a projection of listGPUNodes into the CapacityDiscovery shape
// (per-node accelerator map without labels).
func (d *K8sWithGpuOperator) Discover(ctx context.Context) (map[string]map[string]AcceleratorModelInfo, error) {
	nodes, err := d.listGPUNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]AcceleratorModelInfo, len(nodes))
	for name, n := range nodes {
		out[name] = n.Accelerators
	}
	return out, nil
}

// DiscoverNodes returns per-node info (labels + accelerators) for all GPU-bearing
// nodes. Used by label-aware features such as the namespace-scoped limiter.
func (d *K8sWithGpuOperator) DiscoverNodes(ctx context.Context) (map[string]NodeInfo, error) {
	return d.listGPUNodes(ctx)
}

// DiscoverUsage calculates current GPU usage by summing GPU requests from running pods.
// Returns a map of accelerator type to used GPU count.
func (d *K8sWithGpuOperator) DiscoverUsage(ctx context.Context) (map[string]int, error) {
	// First, build a map of node name -> GPU type
	nodeGPUType, err := d.discoverNodeGPUTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to discover node GPU types: %w", err)
	}

	// List all pods (running or pending on a node)
	var podList corev1.PodList
	if err := d.Client.List(ctx, &podList); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Aggregate GPU requests by accelerator type
	usageByType := make(map[string]int)

	for _, pod := range podList.Items {
		// Skip pods that aren't scheduled or are completed/failed
		if pod.Spec.NodeName == "" {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		// Get the GPU type for this node
		gpuType, ok := nodeGPUType[pod.Spec.NodeName]
		if !ok {
			// Node doesn't have GPUs, skip
			continue
		}

		// Sum GPU requests from all containers
		gpuCount := getPodGPURequests(&pod)
		if gpuCount > 0 {
			usageByType[gpuType] += gpuCount
		}
	}

	return usageByType, nil
}

// discoverNodeGPUTypes returns a map of node name to GPU type (model name).
// For multi-vendor nodes (nodes labeled for more than one GPU vendor), the model
// from the LAST resource with matching productLabel wins.
//
// This preserves the pre-refactor behavior: the original implementation iterated
// VendorResources in order and assigned `nodeGPUType[node] = model` on each match,
// causing later assignments to overwrite earlier ones. Changing this would
// silently affect usage attribution for multi-vendor nodes, so the refactor
// keeps the exact same tie-break semantics.
//
// This is a projection of listGPUNodes into a single-model-per-node shape.
func (d *K8sWithGpuOperator) discoverNodeGPUTypes(ctx context.Context) (map[string]string, error) {
	nodes, err := d.listGPUNodes(ctx)
	if err != nil {
		return nil, err
	}

	out := make(map[string]string, len(nodes))
	for name, n := range nodes {
		// Iterate vendor resources in REVERSE order and break on first match so
		// the last item in `VendorResources` wins (Intel > AMD > NVIDIA).
		// Relies on the listGPUNodes invariant that n.Accelerators[model]
		// exists whenever n.Labels[productLabel] == model.
		for i := len(constants.VendorResources) - 1; i >= 0; i-- {
			prodKey := constants.VendorResources[i].ProductLabel
			if model, ok := n.Labels[prodKey]; ok {
				out[name] = model
				break
			}
		}
	}
	return out, nil
}

// getPodGPURequests returns the total GPU requests for a pod across all containers.
// For regular containers, GPUs are summed (they run concurrently).
// For init containers, we take the max (they run sequentially).
// The final result is max(initContainerMax, regularContainerSum) since init containers
// complete before regular containers start.
func getPodGPURequests(pod *corev1.Pod) int {
	// Sum GPU requests from regular containers (run concurrently)
	regularTotal := 0
	for _, container := range pod.Spec.Containers {
		for _, res := range constants.VendorResources {
			resName := corev1.ResourceName(res.ResourceName)
			if qty, ok := container.Resources.Requests[resName]; ok {
				regularTotal += int(qty.Value())
			}
		}
	}

	// Find max GPU request from init containers (run sequentially)
	initMax := 0
	for _, container := range pod.Spec.InitContainers {
		containerGPUs := 0
		for _, res := range constants.VendorResources {
			resName := corev1.ResourceName(res.ResourceName)
			if qty, ok := container.Resources.Requests[resName]; ok {
				containerGPUs += int(qty.Value())
			}
		}
		if containerGPUs > initMax {
			initMax = containerGPUs
		}
	}

	// Return max of init containers and regular containers
	// (init containers finish before regular containers start)
	if initMax > regularTotal {
		return initMax
	}
	return regularTotal
}

// Ensure K8sWithGpuOperator implements FullDiscovery
var _ FullDiscovery = (*K8sWithGpuOperator)(nil)
