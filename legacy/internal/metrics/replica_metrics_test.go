package metrics

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	llmdOptv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newReplicaTestVA(name, namespace string) *llmdOptv1alpha1.VariantAutoscaling {
	return &llmdOptv1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

var _ = Describe("EmitReplicaMetrics", func() {
	var (
		registry *prometheus.Registry
		emitter  *MetricsEmitter
		ctx      context.Context
	)

	BeforeEach(func() {
		resetMetrics()
		registry = prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		emitter = NewMetricsEmitter()
		ctx = context.Background()
	})

	// series returns the series of the named metric family from the registry.
	series := func(name string) []*dto.Metric {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		for _, mf := range mfs {
			if mf.GetName() == name {
				return mf.GetMetric()
			}
		}
		return nil
	}

	// The scaling signal must be emitted even when the accelerator is unresolved,
	// labelled with the bounded "unresolved" value (never the "unknown" sentinel
	// and never empty).
	DescribeTable("labels an unresolved accelerator as \"unresolved\"",
		func(in string) {
			Expect(emitter.EmitReplicaMetrics(ctx, newReplicaTestVA("v1", "ns1"), 2, 3, in)).To(Succeed())

			s := series(constants.WVADesiredReplicas)
			Expect(s).To(HaveLen(1))
			Expect(getLabelValue(s[0], constants.LabelAcceleratorType)).To(Equal(constants.UnresolvedAcceleratorType))
			Expect(s[0].GetGauge().GetValue()).To(Equal(3.0))
		},
		Entry("empty", ""),
		Entry("unknown sentinel", constants.DefaultAcceleratorName),
		Entry("\"unresolved\" fed back in (idempotent)", constants.UnresolvedAcceleratorType),
	)

	It("labels a resolved accelerator verbatim", func() {
		Expect(emitter.EmitReplicaMetrics(ctx, newReplicaTestVA("v1", "ns1"), 1, 2, "A100")).To(Succeed())

		s := series(constants.WVADesiredReplicas)
		Expect(s).To(HaveLen(1))
		Expect(getLabelValue(s[0], constants.LabelAcceleratorType)).To(Equal("A100"))
	})

	It("falls back to the desired count for the ratio when current==0", func() {
		Expect(emitter.EmitReplicaMetrics(ctx, newReplicaTestVA("v1", "ns1"), 0, 5, "A100")).To(Succeed())

		s := series(constants.WVADesiredRatio)
		Expect(s).To(HaveLen(1))
		Expect(s[0].GetGauge().GetValue()).To(Equal(5.0), "ratio falls back to desired count when current==0")
	})

	// The HPA/KEDA scaler matches a VA on variant_name+namespace, NOT
	// accelerator_type, so an accelerator transition must leave exactly one series.
	It("keeps a single series when the accelerator resolves (unresolved -> real)", func() {
		va := newReplicaTestVA("v1", "ns1")
		Expect(emitter.EmitReplicaMetrics(ctx, va, 2, 3, "")).To(Succeed())
		Expect(emitter.EmitReplicaMetrics(ctx, va, 2, 4, "A100")).To(Succeed())

		s := series(constants.WVADesiredReplicas)
		Expect(s).To(HaveLen(1), "stale 'unresolved' series must be evicted")
		Expect(getLabelValue(s[0], constants.LabelAcceleratorType)).To(Equal("A100"))
		Expect(s[0].GetGauge().GetValue()).To(Equal(4.0))
		// Eviction must cover all three replica gauges, not just desired_replicas.
		Expect(series(constants.WVACurrentReplicas)).To(HaveLen(1))
		Expect(series(constants.WVADesiredRatio)).To(HaveLen(1))
	})

	It("keeps a single series when the accelerator regresses (real -> unresolved)", func() {
		va := newReplicaTestVA("v1", "ns1")
		Expect(emitter.EmitReplicaMetrics(ctx, va, 2, 4, "A100")).To(Succeed())
		Expect(emitter.EmitReplicaMetrics(ctx, va, 2, 3, "")).To(Succeed())

		s := series(constants.WVADesiredReplicas)
		Expect(s).To(HaveLen(1), "stale 'A100' series must be evicted")
		Expect(getLabelValue(s[0], constants.LabelAcceleratorType)).To(Equal(constants.UnresolvedAcceleratorType))
		Expect(s[0].GetGauge().GetValue()).To(Equal(3.0))
		Expect(series(constants.WVACurrentReplicas)).To(HaveLen(1))
		Expect(series(constants.WVADesiredRatio)).To(HaveLen(1))
	})

	It("does not churn the series when the accelerator is unchanged across cycles", func() {
		va := newReplicaTestVA("v1", "ns1")
		Expect(emitter.EmitReplicaMetrics(ctx, va, 1, 2, "A100")).To(Succeed())
		Expect(emitter.EmitReplicaMetrics(ctx, va, 1, 3, "A100")).To(Succeed())

		s := series(constants.WVADesiredReplicas)
		Expect(s).To(HaveLen(1), "same accelerator => single stable series, no eviction")
		Expect(getLabelValue(s[0], constants.LabelAcceleratorType)).To(Equal("A100"))
		Expect(s[0].GetGauge().GetValue()).To(Equal(3.0), "value updates in place")
	})

	It("does not evict another VA's series in the same namespace", func() {
		Expect(emitter.EmitReplicaMetrics(ctx, newReplicaTestVA("va-a", "ns"), 1, 2, "A100")).To(Succeed())
		Expect(emitter.EmitReplicaMetrics(ctx, newReplicaTestVA("va-b", "ns"), 1, 3, "")).To(Succeed())

		Expect(series(constants.WVADesiredReplicas)).To(HaveLen(2), "one series per VA")
	})

	It("sets the controller_instance label when configured", func() {
		// controller_instance is read from the env at InitMetrics, so set it and
		// re-init on a fresh registry for this spec only.
		GinkgoT().Setenv(ControllerInstanceEnvVar, "ctrl-a")
		resetMetrics()
		registry = prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		DeferCleanup(resetMetrics)

		Expect(emitter.EmitReplicaMetrics(ctx, newReplicaTestVA("v1", "ns1"), 1, 2, "A100")).To(Succeed())

		s := series(constants.WVADesiredReplicas)
		Expect(s).To(HaveLen(1))
		Expect(getLabelValue(s[0], constants.LabelControllerInstance)).To(Equal("ctrl-a"))
	})
})
