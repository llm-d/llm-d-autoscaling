package e2e

import (
	"fmt"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// Coordinator / gpu-rebalance coverage.
//
// The Coordinator reads EXPERIMENTAL_COORDINATOR_ENABLED once at manager startup, so
// it cannot be toggled per-spec. The deploy sets it (deploy/lib/infra_wva.sh, gated on
// ENABLE_COORDINATOR) before the manager becomes Ready, and this suite carries its own
// "coordinator" label so it runs in a dedicated job — the saturation-path specs are
// never exercised against a cluster with the Coordinator ticking.
//
// Acceptance case: two managed ScaledObjects sharing one namespace GPU ResourceQuota.
// gpu-rebalance reserves each scaler's effective minimum, then splits what is left by
// EPP queue-depth weight, giving any rounding remainder to the highest-weight pool.
// With both queues at zero the weights are equal, so the ceilings must come out even.
const (
	coordNamespaceQuotaName = "coordinator-e2e-gpu-quota"

	// coordGPUQuota is chosen so the expected split is exact rather than
	// rounding-dependent: two scalers with effectiveMin=1 reserve 2, leaving 6 to
	// divide by weight 0.5 each, so each ceiling is 1 + 3 = 4 with no remainder to
	// hand out. A quota that did not divide evenly would make the assertion depend
	// on which pool won the remainder, which is not what this case is pinning.
	coordGPUQuota = int64(8)

	coordScalerAName = "coordinator-e2e-a"
	coordScalerBName = "coordinator-e2e-b"
	coordPoolAName   = "coordinator-e2e-pool-a"
	coordPoolBName   = "coordinator-e2e-pool-b"

	coordScalerMinReplicas = int32(1)
	// Seeded away from the expected post-rebalance value so a passing assertion
	// proves the Coordinator wrote the ceiling, rather than it having been correct
	// from creation.
	coordScalerInitialMax = int32(1)

	coordExpectedCeiling = int32(4)
)

var _ = Describe("Coordinator - gpu-rebalance", Label("coordinator"), Ordered, func() {
	var (
		quotaTimeout  time.Duration
		pollInterval  time.Duration
		createdScaler []string
	)

	BeforeAll(func() {
		if !cfg.CoordinatorEnabled {
			Skip("This suite requires the Coordinator to be enabled at deploy time: " +
				"run `make test-e2e-coordinator-with-setup`, or deploy with COORDINATOR_ENABLED=true " +
				"so EXPERIMENTAL_COORDINATOR_ENABLED is set before the manager starts.")
		}

		quotaTimeout = time.Duration(cfg.EventuallyStandardSec) * time.Second
		pollInterval = time.Duration(cfg.PollIntervalSec) * time.Second

		By(fmt.Sprintf("creating a %d-GPU ResourceQuota in %s", coordGPUQuota, cfg.LLMDNamespace))
		Expect(fixtures.EnsureGPUResourceQuota(
			ctx, crClient, cfg.LLMDNamespace, coordNamespaceQuotaName, coordGPUQuota,
		)).To(Succeed())

		promURL := fixtures.PrometheusURLFor(cfg.MonitoringNS)

		for _, s := range []struct{ name, pool string }{
			{coordScalerAName, coordPoolAName},
			{coordScalerBName, coordPoolBName},
		} {
			By("creating scale target and managed ScaledObject for " + s.name)
			Expect(createCoordinatorScaleTarget(s.name)).To(Succeed())

			Expect(fixtures.EnsureScaledObject(
				ctx, crClient,
				cfg.LLMDNamespace, s.name, s.name, s.name,
				coordScalerMinReplicas, coordScalerInitialMax,
				cfg.MonitoringNS,
				fixtures.WithScaledObjectCoordinatorManaged(s.pool, promURL),
			)).To(Succeed())

			createdScaler = append(createdScaler, s.name)
		}
	})

	AfterAll(func() {
		if !cfg.CoordinatorEnabled {
			return
		}
		for _, name := range createdScaler {
			Expect(fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, name)).To(Succeed())
			Expect(deleteCoordinatorScaleTarget(name)).To(Succeed())
		}
		Expect(fixtures.DeleteGPUResourceQuota(
			ctx, crClient, cfg.LLMDNamespace, coordNamespaceQuotaName,
		)).To(Succeed())
	})

	It("selects both managed ScaledObjects", func() {
		// Guards the selection contract the rest of the suite rests on: a ScaledObject
		// is only rebalanced when it is annotated managed and does not carry a
		// wva_desired_replicas trigger. If the fixture regressed to the default
		// WVA trigger, the Coordinator would silently skip both objects and the split
		// assertion below would fail for a reason that has nothing to do with the split.
		for _, name := range []string{coordScalerAName, coordScalerBName} {
			so := &kedav1alpha1.ScaledObject{}
			Expect(crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace,
				Name:      name + "-so",
			}, so)).To(Succeed())

			Expect(so.Annotations).To(HaveKeyWithValue("llm-d.ai/managed", "true"))
			Expect(so.Annotations).To(HaveKey("llm-d.ai/epp-inference-pool"))

			for _, t := range so.Spec.Triggers {
				Expect(t.Metadata["query"]).ToNot(ContainSubstring("wva_desired_replicas"),
					"a wva_desired_replicas trigger excludes the ScaledObject from Coordinator control")
			}
		}
	})

	It("splits the GPU quota evenly when every pool queue is zero", func() {
		// No load is driven against either pool, so both EPP queue depths are zero and
		// the plugin falls back to equal weights. Asserting on both objects together
		// (rather than one Eventually each) keeps the check honest: the invariant is
		// that the ceilings are equal *and* sum to the quota at the same instant, not
		// that each one passed through the right value at some point.
		Eventually(func(g Gomega) {
			maxA := currentMaxReplicas(g, coordScalerAName)
			maxB := currentMaxReplicas(g, coordScalerBName)

			g.Expect(maxA).To(Equal(coordExpectedCeiling),
				"pool A ceiling should be quota/2 once reserved minimums are returned")
			g.Expect(maxB).To(Equal(coordExpectedCeiling),
				"pool B ceiling should be quota/2 once reserved minimums are returned")
			g.Expect(int64(maxA+maxB)).To(Equal(coordGPUQuota),
				"the Coordinator must never hand out more ceiling than the namespace quota")
		}, quotaTimeout, pollInterval).Should(Succeed())
	})

	It("keeps the combined ceiling within the quota after it shrinks", func() {
		// Halving the quota exercises the reserve-then-distribute path in the shrink
		// direction: minimums (1 each) are still reserved, leaving 2 to split evenly,
		// so both ceilings land on 2. This is the case that catches a plugin that only
		// ever ratchets ceilings upward.
		//
		// Unlike the even-split case above, this one lands a *downgrade*, so it does
		// not appear within a single Coordinator tick once the damping in #1427 is in
		// place: a lower ceiling must persist for several consecutive ticks before it
		// is written. At the deploy-time COORDINATOR_INTERVAL of 5s that is well
		// inside EventuallyStandardSec, so the assertion holds either way — but do not
		// tighten this timeout to a single interval on the assumption that a ceiling
		// change is always immediate. Increases are; decreases deliberately are not.
		const shrunkQuota = int64(4)
		const shrunkCeiling = int32(2)

		By("shrinking the ResourceQuota to 4 GPUs")
		Expect(fixtures.EnsureGPUResourceQuota(
			ctx, crClient, cfg.LLMDNamespace, coordNamespaceQuotaName, shrunkQuota,
		)).To(Succeed())

		Eventually(func(g Gomega) {
			maxA := currentMaxReplicas(g, coordScalerAName)
			maxB := currentMaxReplicas(g, coordScalerBName)

			g.Expect(maxA).To(Equal(shrunkCeiling))
			g.Expect(maxB).To(Equal(shrunkCeiling))
			g.Expect(int64(maxA + maxB)).To(Equal(shrunkQuota))
		}, quotaTimeout, pollInterval).Should(Succeed())
	})

	// Asymmetric ceilings under uneven EPP queue depth are the other half of #1458's
	// acceptance criteria. Driving that needs real EPP flow-control queue depth, which
	// means binding these pool annotations to live inference pools and pushing burst
	// load through one of them rather than the standalone scale targets used above.
	// Left pending deliberately rather than asserted against a fabricated metric, so
	// the shape is visible and reviewable without claiming coverage it does not have.
	PIt("gives the busier pool a higher ceiling under uneven EPP queue depth", func() {})
})

// currentMaxReplicas reads the ScaledObject's configured ceiling. It reads spec rather
// than the KEDA-generated HPA because gpu-rebalance patches maxReplicaCount on the
// ScaledObject; the HPA is downstream and would add KEDA's reconcile latency to every
// assertion.
func currentMaxReplicas(g Gomega, name string) int32 {
	so := &kedav1alpha1.ScaledObject{}
	g.Expect(crClient.Get(ctx, client.ObjectKey{
		Namespace: cfg.LLMDNamespace,
		Name:      name + "-so",
	}, so)).To(Succeed())
	g.Expect(so.Spec.MaxReplicaCount).ToNot(BeNil(), "ScaledObject %s has no maxReplicaCount", name)
	return *so.Spec.MaxReplicaCount
}

// createCoordinatorScaleTarget stands up a minimal Deployment for a ScaledObject to
// point at. gpu-rebalance only reads and patches the ScaledObject's replica bounds, so
// the target never has to serve traffic — but KEDA will not admit a ScaledObject whose
// scaleTargetRef does not resolve, and an unadmitted object is a confusing thing to
// assert against. The pod requests no GPU, so it is unaffected by the GPU ResourceQuota.
func createCoordinatorScaleTarget(name string) error {
	replicas := coordScalerMinReplicas
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.LLMDNamespace,
			Labels:    map[string]string{"test-resource": "true", "app": name},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(int64(0)),
					Containers: []corev1.Container{{
						Name:            "pause",
						Image:           "registry.k8s.io/pause:3.10",
						ImagePullPolicy: corev1.PullIfNotPresent,
					}},
				},
			},
		},
	}
	return client.IgnoreAlreadyExists(crClient.Create(ctx, deploy))
}

func deleteCoordinatorScaleTarget(name string) error {
	return client.IgnoreNotFound(crClient.Delete(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cfg.LLMDNamespace},
	}))
}
