package metrics

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

var _ = Describe("IncPodMappingMiss", func() {
	// series returns the gathered series for wva_pod_mapping_miss_total.
	series := func(registry *prometheus.Registry) []*dto.Metric {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		for _, mf := range mfs {
			if mf.GetName() == constants.WVAPodMappingMissTotal {
				return mf.GetMetric()
			}
		}
		return nil
	}

	It("counts misses per namespace, keyed by namespace and reason", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		// Two misses in ns-a, one in ns-b — the counter aggregates per namespace.
		IncPodMappingMiss("ns-a", constants.PodMappingMissUnresolved)
		IncPodMappingMiss("ns-a", constants.PodMappingMissUnresolved)
		IncPodMappingMiss("ns-b", constants.PodMappingMissUnresolved)

		got := series(registry)
		Expect(got).To(HaveLen(2), "one series per namespace")

		counts := map[string]float64{}
		for _, m := range got {
			Expect(m.GetCounter()).NotTo(BeNil(), "expected a counter metric")
			Expect(getLabelValue(m, constants.LabelReason)).To(Equal(constants.PodMappingMissUnresolved))
			counts[getLabelValue(m, constants.LabelNamespace)] = m.GetCounter().GetValue()
		}
		Expect(counts).To(HaveKeyWithValue("ns-a", float64(2)))
		Expect(counts).To(HaveKeyWithValue("ns-b", float64(1)))
	})

	It("includes the controller_instance label when configured", func() {
		// InitMetrics reads CONTROLLER_INSTANCE and rebuilds the metric vectors,
		// so save/restore the globals it mutates. GinkgoT().Setenv restores the
		// env var after the spec.
		savedInstance := controllerInstance
		savedMetric := podMappingMissTotal
		DeferCleanup(func() {
			controllerInstance = savedInstance
			podMappingMissTotal = savedMetric
		})
		GinkgoT().Setenv(ControllerInstanceEnvVar, testControllerInstance)

		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		IncPodMappingMiss("ns-a", constants.PodMappingMissUnresolved)

		got := series(registry)
		Expect(got).To(HaveLen(1))
		Expect(getLabelValue(got[0], constants.LabelControllerInstance)).To(Equal(testControllerInstance))
		Expect(getLabelValue(got[0], constants.LabelNamespace)).To(Equal("ns-a"))
	})
})
