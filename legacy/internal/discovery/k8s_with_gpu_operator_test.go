package discovery

import (
	"context"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func init() {
	// Initialize metrics for all discovery tests
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		panic("failed to initialize metrics: " + err.Error())
	}
}

func TestDiscover_NvidiaOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-nvidia-1",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-A100-PCIE-80GB",
					"nvidia.com/gpu.memory":  "81920",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("4"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-nvidia-2",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-SXM5-80GB",
					"nvidia.com/gpu.memory":  "81920",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("8"),
				},
			},
		},
		// CPU-only node (should be excluded)
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-cpu-only",
				Labels: map[string]string{},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	// Should find 2 NVIDIA nodes
	assert.Len(t, result, 2)
	assert.Contains(t, result, "node-nvidia-1")
	assert.Contains(t, result, "node-nvidia-2")

	// Verify node-nvidia-1
	assert.Equal(t, 4, result["node-nvidia-1"]["NVIDIA-A100-PCIE-80GB"].Count)
	assert.Equal(t, "81920", result["node-nvidia-1"]["NVIDIA-A100-PCIE-80GB"].Memory)

	// Verify node-nvidia-2
	assert.Equal(t, 8, result["node-nvidia-2"]["NVIDIA-H100-SXM5-80GB"].Count)
}

func TestDiscover_AMDOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-amd-1",
				Labels: map[string]string{
					"amd.com/gpu.product-name": "AMD-MI300X-192G",
					"amd.com/gpu.memory":       "196608",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"amd.com/gpu": resource.MustParse("8"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-amd-2",
				Labels: map[string]string{
					"amd.com/gpu.product-name": "AMD-MI250-128G",
					"amd.com/gpu.memory":       "131072",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"amd.com/gpu": resource.MustParse("4"),
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	// Should find 2 AMD nodes
	assert.Len(t, result, 2)
	assert.Contains(t, result, "node-amd-1")
	assert.Contains(t, result, "node-amd-2")

	// Verify AMD node details
	assert.Equal(t, 8, result["node-amd-1"]["AMD-MI300X-192G"].Count)
	assert.Equal(t, "196608", result["node-amd-1"]["AMD-MI300X-192G"].Memory)
	assert.Equal(t, 4, result["node-amd-2"]["AMD-MI250-128G"].Count)
}

func TestDiscover_MixedVendors(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-nvidia",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-SXM5-80GB",
					"nvidia.com/gpu.memory":  "81920",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("4"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-amd",
				Labels: map[string]string{
					"amd.com/gpu.product-name": "AMD-MI300X-192G",
					"amd.com/gpu.memory":       "196608",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"amd.com/gpu": resource.MustParse("8"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-intel-gaudi",
				Labels: map[string]string{
					"habana.ai/product.name":  "Intel-Gaudi-2-96GB",
					"habana.ai/device.memory": "98304",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"habana.ai/gaudi": resource.MustParse("8"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-intel-i915",
				Labels: map[string]string{
					"gpu.intel.com/product": "Max_1100",
					"gpu.intel.com/memory":  "49152",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"gpu.intel.com/i915": resource.MustParse("4"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-intel-xe",
				Labels: map[string]string{
					"gpu.intel.com/product": "Pro-B60-Graphics",
					"gpu.intel.com/memory":  "24576",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"gpu.intel.com/xe": resource.MustParse("2"),
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	// Should find all 5 nodes from different vendors
	assert.Len(t, result, 5)
	assert.Contains(t, result, "node-nvidia")
	assert.Contains(t, result, "node-amd")
	assert.Contains(t, result, "node-intel-gaudi")
	assert.Contains(t, result, "node-intel-i915")
	assert.Contains(t, result, "node-intel-xe")

	// Verify each vendor's GPU details
	assert.Equal(t, 4, result["node-nvidia"]["NVIDIA-H100-SXM5-80GB"].Count)
	assert.Equal(t, 8, result["node-amd"]["AMD-MI300X-192G"].Count)
	assert.Equal(t, 8, result["node-intel-gaudi"]["Intel-Gaudi-2-96GB"].Count)
	assert.Equal(t, 4, result["node-intel-i915"]["Max_1100"].Count)
	assert.Equal(t, 2, result["node-intel-xe"]["Pro-B60-Graphics"].Count)
}

func TestDiscover_WithNodeSelector(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-shard-a",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-A100-PCIE-80GB",
					"wva.llmd.ai/shard":      "a",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("4"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-shard-b",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-A100-PCIE-80GB",
					"wva.llmd.ai/shard":      "b",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("4"),
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	// Set node selector to only match shard-a
	t.Setenv("WVA_NODE_SELECTOR", "wva.llmd.ai/shard=a")

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	// Should only find shard-a node
	assert.Len(t, result, 1)
	assert.Contains(t, result, "node-shard-a")
}

func TestDiscoverUsage_MixedVendors(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-nvidia",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-SXM5-80GB",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("4"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-amd",
				Labels: map[string]string{
					"amd.com/gpu.product-name": "AMD-MI300X-192G",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"amd.com/gpu": resource.MustParse("8"),
				},
			},
		},
	}

	pods := []runtime.Object{
		// Pod on NVIDIA node using 2 GPUs
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-nvidia-1",
				Namespace: "default",
			},
			Spec: corev1.PodSpec{
				NodeName: "node-nvidia",
				Containers: []corev1.Container{
					{
						Name: "gpu-container",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								"nvidia.com/gpu": resource.MustParse("2"),
							},
						},
					},
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
		// Pod on AMD node using 4 GPUs
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-amd-1",
				Namespace: "default",
			},
			Spec: corev1.PodSpec{
				NodeName: "node-amd",
				Containers: []corev1.Container{
					{
						Name: "gpu-container",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								"amd.com/gpu": resource.MustParse("4"),
							},
						},
					},
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
	}

	allObjects := make([]runtime.Object, 0, len(nodes)+len(pods))
	allObjects = append(allObjects, nodes...)
	allObjects = append(allObjects, pods...)
	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(allObjects...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.DiscoverUsage(context.Background())
	require.NoError(t, err)

	// Should track usage per GPU type
	assert.Equal(t, 2, result["NVIDIA-H100-SXM5-80GB"])
	assert.Equal(t, 4, result["AMD-MI300X-192G"])
}

func TestDiscoverNodeGPUTypes_MixedVendors(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-nvidia",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-SXM5-80GB",
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-amd",
				Labels: map[string]string{
					"amd.com/gpu.product-name": "AMD-MI300X-192G",
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-intel-gaudi",
				Labels: map[string]string{
					"habana.ai/product.name": "Intel-Gaudi-2-96GB",
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-intel-i915",
				Labels: map[string]string{
					"gpu.intel.com/product": "Max_1100",
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-intel-xe",
				Labels: map[string]string{
					"gpu.intel.com/product": "Pro-B60-Graphics",
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.discoverNodeGPUTypes(context.Background())
	require.NoError(t, err)

	assert.Len(t, result, 5)
	assert.Equal(t, "NVIDIA-H100-SXM5-80GB", result["node-nvidia"])
	assert.Equal(t, "AMD-MI300X-192G", result["node-amd"])
	assert.Equal(t, "Intel-Gaudi-2-96GB", result["node-intel-gaudi"])
	assert.Equal(t, "Max_1100", result["node-intel-i915"])
	assert.Equal(t, "Pro-B60-Graphics", result["node-intel-xe"])
}

func TestGetPodGPURequests_MixedVendors(t *testing.T) {
	// Pod with both NVIDIA and AMD GPU requests (unusual but should be handled)
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "nvidia-container",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("2"),
						},
					},
				},
				{
					Name: "amd-container",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							"amd.com/gpu": resource.MustParse("4"),
						},
					},
				},
			},
		},
	}

	result := getPodGPURequests(pod)
	// Should sum all GPU requests across vendors
	assert.Equal(t, 6, result)
}

func TestDiscover_EmptyCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestDiscover_NoGPUNodes(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-cpu-1",
				Labels: map[string]string{},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-cpu-2",
				Labels: map[string]string{},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestDiscoverNodeGPUTypes_MultiVendorNode_LastWins(t *testing.T) {
	// Behavior preservation test: for a node labeled by multiple vendors,
	// discoverNodeGPUTypes returns the LAST vendor in the iteration order
	// (nvidia, amd, intel → intel wins if present). This matches the
	// pre-refactor semantics where `nodeGPUType[name] = model` overwrote
	// on each vendor iteration.
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-nvidia-amd",
				Labels: map[string]string{
					"nvidia.com/gpu.product":   "NVIDIA-A100-PCIE-80GB",
					"amd.com/gpu.product-name": "AMD-MI300X-192G",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("2"),
					"amd.com/gpu":    resource.MustParse("4"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-nvidia-intel",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-SXM5-80GB",
					"habana.ai/product.name": "Intel-Gaudi-2-96GB",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu":  resource.MustParse("2"),
					"habana.ai/gaudi": resource.MustParse("8"),
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.discoverNodeGPUTypes(context.Background())
	require.NoError(t, err)

	// nvidia+amd → amd wins (later in vendor order)
	assert.Equal(t, "AMD-MI300X-192G", result["node-nvidia-amd"])
	// nvidia+intel → intel wins (latest in vendor order)
	assert.Equal(t, "Intel-Gaudi-2-96GB", result["node-nvidia-intel"])
}

func TestDiscoverNodes_SingleVendor(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-nvidia",
				Labels: map[string]string{
					"nvidia.com/gpu.product":      "NVIDIA-A100-PCIE-80GB",
					"nvidia.com/gpu.memory":       "81920",
					"topology.kubernetes.io/zone": "us-east-1a",
					"team":                        "prod",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("4"),
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.DiscoverNodes(context.Background())
	require.NoError(t, err)
	require.Len(t, result, 1)

	ni, ok := result["node-nvidia"]
	require.True(t, ok)
	assert.Equal(t, "node-nvidia", ni.Name)

	// Accelerators captured with correct count and memory
	require.Contains(t, ni.Accelerators, "NVIDIA-A100-PCIE-80GB")
	assert.Equal(t, 4, ni.Accelerators["NVIDIA-A100-PCIE-80GB"].Count)
	assert.Equal(t, "81920", ni.Accelerators["NVIDIA-A100-PCIE-80GB"].Memory)

	// All node labels copied (both GPU-related and non-GPU)
	assert.Equal(t, "NVIDIA-A100-PCIE-80GB", ni.Labels["nvidia.com/gpu.product"])
	assert.Equal(t, "us-east-1a", ni.Labels["topology.kubernetes.io/zone"])
	assert.Equal(t, "prod", ni.Labels["team"])
}

func TestDiscoverNodes_MultiVendorNode(t *testing.T) {
	// A single node labeled for two vendors (unusual but supported by the vendor-loop).
	// The node should appear once with both accelerators merged.
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-multi",
				Labels: map[string]string{
					"nvidia.com/gpu.product":   "NVIDIA-A100-PCIE-80GB",
					"nvidia.com/gpu.memory":    "81920",
					"amd.com/gpu.product-name": "AMD-MI300X-192G",
					"amd.com/gpu.memory":       "196608",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("2"),
					"amd.com/gpu":    resource.MustParse("4"),
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.DiscoverNodes(context.Background())
	require.NoError(t, err)

	// Node appears once, not twice
	require.Len(t, result, 1)
	ni, ok := result["node-multi"]
	require.True(t, ok)

	// Both accelerators present
	require.Len(t, ni.Accelerators, 2)
	assert.Equal(t, 2, ni.Accelerators["NVIDIA-A100-PCIE-80GB"].Count)
	assert.Equal(t, "81920", ni.Accelerators["NVIDIA-A100-PCIE-80GB"].Memory)
	assert.Equal(t, 4, ni.Accelerators["AMD-MI300X-192G"].Count)
	assert.Equal(t, "196608", ni.Accelerators["AMD-MI300X-192G"].Memory)
}

func TestDiscoverNodes_RespectsWVANodeSelector(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-shard-a",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-SXM5-80GB",
					"wva.llmd.ai/shard":      "a",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("8"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-shard-b",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-SXM5-80GB",
					"wva.llmd.ai/shard":      "b",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("8"),
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	t.Setenv("WVA_NODE_SELECTOR", "wva.llmd.ai/shard=a")

	result, err := discoverer.DiscoverNodes(context.Background())
	require.NoError(t, err)

	// Only shard-a should be returned
	require.Len(t, result, 1)
	_, ok := result["node-shard-a"]
	assert.True(t, ok)
	_, ok = result["node-shard-b"]
	assert.False(t, ok)
}

func TestDiscoverNodes_NodeWithGPULabelButNoAllocatable(t *testing.T) {
	// A node labeled as GPU-bearing but with no Allocatable resource.
	// Existing Discover() behavior: Count = 0, still included in result.
	// DiscoverNodes should preserve that behavior.
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-labeled-no-alloc",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-A100-PCIE-80GB",
				},
			},
			// No Allocatable
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.DiscoverNodes(context.Background())
	require.NoError(t, err)
	require.Len(t, result, 1)

	ni := result["node-labeled-no-alloc"]
	require.Contains(t, ni.Accelerators, "NVIDIA-A100-PCIE-80GB")
	assert.Equal(t, 0, ni.Accelerators["NVIDIA-A100-PCIE-80GB"].Count)
}

func TestDiscoverNodes_EmptyCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.DiscoverNodes(context.Background())
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestDiscoverNodes_ExcludesCPUOnlyNodes(t *testing.T) {
	// Nodes without any vendor/gpu.product label must not appear in DiscoverNodes output.
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-gpu",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-SXM5-80GB",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("8"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-cpu-only",
				Labels: map[string]string{"team": "prod"},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.DiscoverNodes(context.Background())
	require.NoError(t, err)
	require.Len(t, result, 1)
	_, ok := result["node-gpu"]
	assert.True(t, ok)
	_, ok = result["node-cpu-only"]
	assert.False(t, ok)
}

func TestDiscoverNodes_InvalidWVANodeSelectorReturnsError(t *testing.T) {
	// An invalid WVA_NODE_SELECTOR should cause the discovery to return an error
	// rather than silently ignoring the selector.
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	discoverer := NewK8sWithGpuOperator(client)

	// A malformed selector string (empty operator).
	t.Setenv("WVA_NODE_SELECTOR", "key!")

	_, err := discoverer.DiscoverNodes(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WVA_NODE_SELECTOR")

	// Same error path propagates through the Discover() projection.
	_, err = discoverer.Discover(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WVA_NODE_SELECTOR")
}

func TestDiscoverNodes_LabelsAreIndependentCopy(t *testing.T) {
	// Mutating NodeInfo.Labels must not affect the underlying corev1.Node labels.
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-nvidia",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-A100-PCIE-80GB",
					"team":                   "prod",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("4"),
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.DiscoverNodes(context.Background())
	require.NoError(t, err)
	require.Len(t, result, 1)

	ni := result["node-nvidia"]
	// Mutate the returned Labels map — should have no effect on subsequent calls.
	ni.Labels["team"] = "dev"
	delete(ni.Labels, "nvidia.com/gpu.product")

	// Re-fetch and verify the underlying node labels are untouched.
	result2, err := discoverer.DiscoverNodes(context.Background())
	require.NoError(t, err)
	ni2 := result2["node-nvidia"]
	assert.Equal(t, "prod", ni2.Labels["team"])
	assert.Equal(t, "NVIDIA-A100-PCIE-80GB", ni2.Labels["nvidia.com/gpu.product"])
}

// Compile-time assertion that K8sWithGpuOperator satisfies FullDiscovery
// (which now requires NodeDiscovery too).
var _ FullDiscovery = (*K8sWithGpuOperator)(nil)
