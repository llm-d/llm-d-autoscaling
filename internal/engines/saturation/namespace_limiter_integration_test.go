package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

// These tests exercise the namespace-scoped GPU limiter end to end against a
// real API server (envtest): GPU nodes are discovered through
// discovery.K8sWithGpuOperator, the limiter is built from a parsed limiter
// ConfigMap, and multiple namespaces contend for GPUs in a single Limit call.
// The unit tests in internal/engines/pipeline cover the allocation logic with a
// fake discovery; this verifies the config -> discovery -> limiter wiring the
// engine actually uses.
var _ = Describe("Namespace inventory limiter (integration)", func() {
	// createGPUNode registers a GPU node carrying the GFD product label, a pool
	// label, and allocatable GPUs, then schedules its deletion.
	createGPUNode := func(name, pool, model string, gpus int64) {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: map[string]string{"nvidia.com/gpu.product": model, "team": pool},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
		})
		qty := *resource.NewQuantity(gpus, resource.DecimalSI)
		node.Status.Capacity = corev1.ResourceList{"nvidia.com/gpu": qty}
		node.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": qty}
		Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
	}

	// scaleUp builds a decision that requests far more GPUs than any test pool
	// holds, so the limiter's cap is the only thing bounding the result.
	scaleUp := func(ns, accel string) *domain.VariantDecision {
		return &domain.VariantDecision{
			VariantName: ns + "-variant", Namespace: ns, AcceleratorName: accel,
			CurrentReplicas: 0, TargetReplicas: 1000, GPUsPerReplica: 1,
		}
	}

	// buildLimiter goes through the production path: the inline limiters: entry
	// on the global saturation config selects the mode, and the limiter factory
	// constructs it.
	buildLimiter := func(selectors map[string]config.NodeSelector) pipeline.Limiter {
		cfg := config.NewTestConfig()
		cfg.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
			"default": {Limiters: []config.QuotaLimiterConfig{{
				Type:      string(config.LimiterTypeNamespaceInventory),
				Name:      "namespace-inventory",
				Selectors: selectors,
			}}},
		})
		Expect(cfg.EffectiveLimiterMode()).To(Equal(config.LimiterTypeNamespaceInventory))

		lim, err := pipeline.NewLimiterFromConfig(cfg, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(lim).NotTo(BeNil())
		return lim
	}

	team := func(name string) config.NodeSelector {
		return config.NodeSelector{MatchLabels: map[string]string{"team": name}}
	}

	It("isolates per-namespace pools so one tenant cannot consume another's GPUs", func() {
		createGPUNode("itg-prod", "prod", "NVIDIA-H100-SXM5-80GB", 8)
		createGPUNode("itg-dev", "dev", "NVIDIA-H100-SXM5-80GB", 4)

		lim := buildLimiter(map[string]config.NodeSelector{
			"ns-prod": team("prod"),
			"ns-dev":  team("dev"),
		})
		// Both namespaces demand far more than their pool holds, in one batch.
		prod := scaleUp("ns-prod", "H100")
		dev := scaleUp("ns-dev", "H100")
		Expect(lim.Limit(ctx, []*domain.VariantDecision{prod, dev})).To(Succeed())

		// Each is bounded by its own pool; neither consumes the other's GPUs.
		Expect(prod.TargetReplicas).To(Equal(8))
		Expect(dev.TargetReplicas).To(Equal(4))
		Expect(prod.WasLimited).To(BeTrue())
		Expect(dev.WasLimited).To(BeTrue())
	})

	It("splits a shared default pool under contention without exceeding capacity", func() {
		createGPUNode("itg-shared", "shared", "NVIDIA-A100-PCIE-80GB", 6)

		lim := buildLimiter(map[string]config.NodeSelector{
			pipeline.DefaultSelectorKey: team("shared"),
		})
		a := scaleUp("ns-a", "A100")
		b := scaleUp("ns-b", "A100")
		Expect(lim.Limit(ctx, []*domain.VariantDecision{a, b})).To(Succeed())

		// Both fall to the shared default pool; combined new replicas exhaust but
		// never exceed its 6 GPUs.
		Expect(a.TargetReplicas + b.TargetReplicas).To(Equal(6))
	})
})
