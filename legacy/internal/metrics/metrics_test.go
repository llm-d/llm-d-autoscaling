package metrics

import (
	"context"
	"os"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	llmdOptv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordError(t *testing.T) {
	tests := []struct {
		name          string
		component     string
		errorType     string
		callCount     int
		expectedValue float64
	}{
		{
			name:          "single error increment",
			component:     constants.ComponentController,
			errorType:     "TestError",
			callCount:     1,
			expectedValue: 1.0,
		},
		{
			name:          "multiple error increments",
			component:     constants.ComponentController,
			errorType:     "AnotherError",
			callCount:     3,
			expectedValue: 3.0,
		},
		{
			name:          "analyzer error",
			component:     constants.ComponentAnalyzer,
			errorType:     "AnalyzerError",
			callCount:     2,
			expectedValue: 2.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a new registry for each test to ensure isolation
			registry := prometheus.NewRegistry()

			// Initialize metrics with the test registry
			err := InitMetrics(registry)
			require.NoError(t, err)

			// Call RecordError the specified number of times
			for i := 0; i < tt.callCount; i++ {
				RecordError(tt.component, tt.errorType)
			}

			// Method 1: Using testutil.ToFloat64 - simplest for single metric
			metricName := constants.WVAErrorsTotal
			labels := prometheus.Labels{
				constants.LabelComponent: tt.component,
				constants.LabelErrorType: tt.errorType,
			}
			actualValue := testutil.ToFloat64(errorsTotal.With(labels))
			assert.Equal(t, tt.expectedValue, actualValue,
				"Counter should be incremented to %v", tt.expectedValue)

			// Method 2: Using Gather and manual inspection
			metricFamilies, err := registry.Gather()
			require.NoError(t, err)

			found := false
			for _, mf := range metricFamilies {
				if mf.GetName() == metricName {
					for _, metric := range mf.GetMetric() {
						// Check if labels match
						labelMatch := true
						for _, label := range metric.GetLabel() {
							expectedVal, exists := labels[label.GetName()]
							if exists && label.GetValue() != expectedVal {
								labelMatch = false
								break
							}
						}

						if labelMatch {
							found = true
							assert.Equal(t, tt.expectedValue, metric.GetCounter().GetValue(),
								"Counter value from Gather should match expected")
						}
					}
				}
			}
			assert.True(t, found, "Metric with matching labels should be found")
		})
	}
}

// Note: Testing with controller_instance label requires a separate test run
// because Prometheus metrics cannot change their label cardinality after creation.
// To test controller_instance behavior, set CONTROLLER_INSTANCE env var before
// running the test suite, or test it in integration/e2e tests.

func TestRecordErrorNotInitialized(t *testing.T) {
	// Save original errorsTotal
	originalErrorsTotal := errorsTotal
	defer func() {
		errorsTotal = originalErrorsTotal
	}()

	// Set errorsTotal to nil to simulate uninitialized state
	errorsTotal = nil

	// RecordError should not panic when errorsTotal is nil - it returns early gracefully
	// to handle cases where metrics may not be initialized
	require.NotPanics(t, func() {
		RecordError(constants.ComponentController, "TestError")
	}, "RecordError should not panic when errorsTotal is nil (metrics not initialized)")
}

func TestRecordErrorMetricFormat(t *testing.T) {
	// Test that the metric can be scraped in Prometheus exposition format
	registry := prometheus.NewRegistry()
	err := InitMetrics(registry)
	require.NoError(t, err)

	RecordError(constants.ComponentController, "ConfigMapError")

	// Use testutil.CollectAndCompare to verify metric format
	expected := `
		# HELP wva_errors_total Total number of errors by component
		# TYPE wva_errors_total counter
		wva_errors_total{component="controller",error_type="ConfigMapError"} 1
	`

	err = testutil.CollectAndCompare(errorsTotal, strings.NewReader(expected))
	assert.NoError(t, err, "Metric should match expected Prometheus format")
}

// resetMetrics clears package-level metric vars so each test starts fresh.
// Callers MUST invoke InitMetrics before recording any metric afterwards —
// the Record* methods deliberately have no nil guard, so a missed init
// would panic at the first Set call.
func resetMetrics() {
	replicaScalingTotal = nil
	desiredReplicas = nil
	currentReplicas = nil
	desiredRatio = nil
	saturationUtilization = nil
	spareCapacity = nil
	requiredCapacity = nil
	kvCacheTokensUsed = nil
	kvCacheTokensCapacity = nil
	saturationMetricsUp = nil
	controllerInstance = ""
	replicaSeriesMu.Lock()
	replicaSeriesAccel = map[string]string{}
	replicaSeriesMu.Unlock()
}

func gatherMetric(registry *prometheus.Registry, name string) *dto.MetricFamily {
	families, err := registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

func gaugeValue(mf *dto.MetricFamily, labels map[string]string) (float64, bool) {
	for _, m := range mf.GetMetric() {
		match := true
		for k, v := range labels {
			found := false
			for _, lp := range m.GetLabel() {
				if lp.GetName() == k && lp.GetValue() == v {
					found = true
					break
				}
			}
			if !found {
				match = false
				break
			}
		}
		if match && m.GetGauge() != nil {
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

var _ = Describe("RecordSaturationMetrics", func() {

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

	It("should record all saturation metrics with correct values", func() {
		emitter.RecordSaturationMetrics(ctx,
			"variant-a", "test-ns", "meta-llama/Llama-3.1-8B", "nvidia-a100", constants.UnitContinuous,
			0.75, 0.25, 5000.0,
			100000, 200000,
		)

		accelLabels := map[string]string{
			constants.LabelVariantName:     "variant-a",
			constants.LabelNamespace:       "test-ns",
			constants.LabelModelName:       "meta-llama/Llama-3.1-8B",
			constants.LabelAcceleratorType: "nvidia-a100",
		}
		modelLabels := map[string]string{
			constants.LabelVariantName: "variant-a",
			constants.LabelNamespace:   "test-ns",
			constants.LabelModelName:   "meta-llama/Llama-3.1-8B",
		}
		requiredLabels := map[string]string{
			constants.LabelVariantName: "variant-a",
			constants.LabelNamespace:   "test-ns",
			constants.LabelModelName:   "meta-llama/Llama-3.1-8B",
			constants.LabelUnit:        constants.UnitContinuous,
		}

		// saturation_utilization
		mf := gatherMetric(registry, constants.WVASaturationUtilization)
		Expect(mf).NotTo(BeNil())
		val, ok := gaugeValue(mf, accelLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(0.75))

		// spare_capacity (carries unit label, same label set as required_capacity)
		mf = gatherMetric(registry, constants.WVASpareCapacity)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, requiredLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(0.25))

		// required_capacity (carries unit label)
		mf = gatherMetric(registry, constants.WVARequiredCapacity)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, requiredLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(5000.0))

		// kv_cache_tokens_used
		mf = gatherMetric(registry, constants.WVAKvCacheTokensUsed)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, modelLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(100000.0))

		// kv_cache_tokens_total
		mf = gatherMetric(registry, constants.WVAKvCacheTokensCapacity)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, modelLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(200000.0))
	})

	It("should distinguish binary and continuous required_capacity series via unit label", func() {
		// Same variant, same namespace, different units → two series
		emitter.RecordSaturationMetrics(ctx,
			"variant-multi", "ns", "model-x", "h100", constants.UnitBinary,
			0.5, 0.5, 1.0, 100, 200,
		)
		emitter.RecordSaturationMetrics(ctx,
			"variant-multi", "ns", "model-x", "h100", constants.UnitContinuous,
			0.5, 0.5, 8000.0, 100, 200,
		)

		mf := gatherMetric(registry, constants.WVARequiredCapacity)
		Expect(mf).NotTo(BeNil())

		v1Labels := map[string]string{
			constants.LabelVariantName: "variant-multi",
			constants.LabelNamespace:   "ns",
			constants.LabelModelName:   "model-x",
			constants.LabelUnit:        constants.UnitBinary,
		}
		v2Labels := map[string]string{
			constants.LabelVariantName: "variant-multi",
			constants.LabelNamespace:   "ns",
			constants.LabelModelName:   "model-x",
			constants.LabelUnit:        constants.UnitContinuous,
		}

		v1Val, ok := gaugeValue(mf, v1Labels)
		Expect(ok).To(BeTrue())
		Expect(v1Val).To(Equal(1.0))

		v2Val, ok := gaugeValue(mf, v2Labels)
		Expect(ok).To(BeTrue())
		Expect(v2Val).To(Equal(8000.0))
	})

	It("should include controller_instance label when env var is set", func() {
		// Re-init with controller instance set
		resetMetrics()
		Expect(os.Setenv(ControllerInstanceEnvVar, "controller-1")).To(Succeed())
		DeferCleanup(os.Unsetenv, ControllerInstanceEnvVar)

		registry = prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		emitter = NewMetricsEmitter()

		emitter.RecordSaturationMetrics(ctx,
			"variant-b", "prod-ns", "ibm/granite-13b", "nvidia-h100", constants.UnitContinuous,
			0.90, 0.10, 10000.0,
			500000, 600000,
		)

		// Verify controller_instance + model_name on accel-scoped metric
		mf := gatherMetric(registry, constants.WVASaturationUtilization)
		Expect(mf).NotTo(BeNil())
		val, ok := gaugeValue(mf, map[string]string{
			constants.LabelVariantName:        "variant-b",
			constants.LabelNamespace:          "prod-ns",
			constants.LabelModelName:          "ibm/granite-13b",
			constants.LabelAcceleratorType:    "nvidia-h100",
			constants.LabelControllerInstance: "controller-1",
		})
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(0.90))

		// Verify controller_instance + model_name + unit on required_capacity
		mf = gatherMetric(registry, constants.WVARequiredCapacity)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, map[string]string{
			constants.LabelVariantName:        "variant-b",
			constants.LabelNamespace:          "prod-ns",
			constants.LabelModelName:          "ibm/granite-13b",
			constants.LabelUnit:               constants.UnitContinuous,
			constants.LabelControllerInstance: "controller-1",
		})
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(10000.0))
	})

	It("should handle zero values correctly", func() {
		emitter.RecordSaturationMetrics(ctx,
			"variant-c", "ns", "model-z", "amd-mi300x", constants.UnitBinary,
			0.0, 0.0, 0.0,
			0, 0,
		)

		accelLabels := map[string]string{
			constants.LabelVariantName:     "variant-c",
			constants.LabelNamespace:       "ns",
			constants.LabelModelName:       "model-z",
			constants.LabelAcceleratorType: "amd-mi300x",
		}
		modelLabels := map[string]string{
			constants.LabelVariantName: "variant-c",
			constants.LabelNamespace:   "ns",
			constants.LabelModelName:   "model-z",
		}
		requiredLabels := map[string]string{
			constants.LabelVariantName: "variant-c",
			constants.LabelNamespace:   "ns",
			constants.LabelModelName:   "model-z",
			constants.LabelUnit:        constants.UnitBinary,
		}

		// saturation_utilization is per-variant (accelerator-labeled).
		mf := gatherMetric(registry, constants.WVASaturationUtilization)
		Expect(mf).NotTo(BeNil(), "metric %s not found", constants.WVASaturationUtilization)
		val, ok := gaugeValue(mf, accelLabels)
		Expect(ok).To(BeTrue(), "gauge not found for %s", constants.WVASaturationUtilization)
		Expect(val).To(Equal(0.0))

		// spare_capacity is model/role-level (unit-labeled, same set as required_capacity).
		mf = gatherMetric(registry, constants.WVASpareCapacity)
		Expect(mf).NotTo(BeNil(), "metric %s not found", constants.WVASpareCapacity)
		val, ok = gaugeValue(mf, requiredLabels)
		Expect(ok).To(BeTrue(), "gauge not found for %s", constants.WVASpareCapacity)
		Expect(val).To(Equal(0.0))

		mf = gatherMetric(registry, constants.WVARequiredCapacity)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, requiredLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(0.0))

		for _, metricName := range []string{
			constants.WVAKvCacheTokensUsed,
			constants.WVAKvCacheTokensCapacity,
		} {
			mf := gatherMetric(registry, metricName)
			Expect(mf).NotTo(BeNil(), "metric %s not found", metricName)
			val, ok := gaugeValue(mf, modelLabels)
			Expect(ok).To(BeTrue(), "gauge not found for %s", metricName)
			Expect(val).To(Equal(0.0), "expected 0 for %s", metricName)
		}
	})

})

var _ = Describe("EmitReplicaScalingMetrics", func() {

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

	It("should emit replica scaling metrics with ActionScaleUp and DecisionReasonV2", func() {
		va := &llmdOptv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-variant",
				Namespace: "test-namespace",
			},
		}

		err := emitter.EmitReplicaScalingMetrics(ctx, va, domain.ActionScaleUp, domain.DecisionReasonV2)
		Expect(err).NotTo(HaveOccurred())

		// Gather the metric and verify the counter was incremented
		mf := gatherMetric(registry, constants.WVAReplicaScalingTotal)
		Expect(mf).NotTo(BeNil())

		// Find the metric with the expected labels
		labels := map[string]string{
			constants.LabelVariantName: "test-variant",
			constants.LabelNamespace:   "test-namespace",
			constants.LabelDirection:   string(domain.ActionScaleUp),
			constants.LabelReason:      string(domain.DecisionReasonV2),
		}

		var found bool
		var counterValue float64
		for _, m := range mf.GetMetric() {
			match := true
			for k, expectedVal := range labels {
				labelFound := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == k && lp.GetValue() == expectedVal {
						labelFound = true
						break
					}
				}
				if !labelFound {
					match = false
					break
				}
			}
			if match && m.GetCounter() != nil {
				found = true
				counterValue = m.GetCounter().GetValue()
				break
			}
		}

		// found==true already verifies the metric carries reason="V2" + direction
		// labels (the match loop above compares every entry in `labels`, which
		// includes LabelReason=string(DecisionReasonV2), against the gathered series).
		Expect(found).To(BeTrue(), "metric with reason=\"V2\" and direction=\"scale-up\" labels should exist")
		Expect(counterValue).To(Equal(1.0), "counter should be incremented to 1")
	})

})

var _ = Describe("RecordSaturationFreshness", func() {
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

	It("emits 1.0 on a fresh cycle and 0.0 on a stale cycle", func() {
		emitter.RecordSaturationFreshness(ctx, "variant-fresh", "ns-a", true)
		emitter.RecordSaturationFreshness(ctx, "variant-stale", "ns-a", false)

		mf := gatherMetric(registry, constants.WVASaturationMetricsUp)
		Expect(mf).NotTo(BeNil(), "wva_saturation_metrics_up not registered")

		freshLabels := map[string]string{
			constants.LabelVariantName: "variant-fresh",
			constants.LabelNamespace:   "ns-a",
		}
		val, ok := gaugeValue(mf, freshLabels)
		Expect(ok).To(BeTrue(), "fresh series not found")
		Expect(val).To(Equal(1.0))

		staleLabels := map[string]string{
			constants.LabelVariantName: "variant-stale",
			constants.LabelNamespace:   "ns-a",
		}
		val, ok = gaugeValue(mf, staleLabels)
		Expect(ok).To(BeTrue(), "stale series not found")
		Expect(val).To(Equal(0.0))
	})

	It("flips an existing series between fresh and stale on subsequent cycles", func() {
		// Cycle 1: fresh.
		emitter.RecordSaturationFreshness(ctx, "variant-a", "ns-a", true)
		mf := gatherMetric(registry, constants.WVASaturationMetricsUp)
		labels := map[string]string{
			constants.LabelVariantName: "variant-a",
			constants.LabelNamespace:   "ns-a",
		}
		val, _ := gaugeValue(mf, labels)
		Expect(val).To(Equal(1.0))

		// Cycle 2: same VA, no fresh decision — the gauge must drop to 0,
		// not retain its previous 1.0 value (that's the whole point of the
		// freshness signal vs. relying on Prometheus' implicit staleness).
		emitter.RecordSaturationFreshness(ctx, "variant-a", "ns-a", false)
		mf = gatherMetric(registry, constants.WVASaturationMetricsUp)
		val, _ = gaugeValue(mf, labels)
		Expect(val).To(Equal(0.0))
	})

	It("uses only {variant_name, namespace} labels — no accelerator_type or model_name", func() {
		emitter.RecordSaturationFreshness(ctx, "variant-a", "ns-a", true)

		mf := gatherMetric(registry, constants.WVASaturationMetricsUp)
		Expect(mf).NotTo(BeNil())
		Expect(mf.GetMetric()).To(HaveLen(1))

		wantLabels := map[string]bool{
			constants.LabelVariantName: true,
			constants.LabelNamespace:   true,
		}
		for _, lp := range mf.GetMetric()[0].GetLabel() {
			Expect(wantLabels).To(HaveKey(lp.GetName()),
				"unexpected label %q on freshness gauge (must be the smallest cardinality shared across the five saturation gauges)", lp.GetName())
		}
		Expect(mf.GetMetric()[0].GetLabel()).To(HaveLen(len(wantLabels)))
	})

	It("adds controller_instance label when CONTROLLER_INSTANCE is set", func() {
		resetMetrics()
		t := GinkgoT()
		t.Setenv(ControllerInstanceEnvVar, "primary")
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		emitter := NewMetricsEmitter()

		emitter.RecordSaturationFreshness(ctx, "variant-a", "ns-a", true)

		mf := gatherMetric(registry, constants.WVASaturationMetricsUp)
		Expect(mf).NotTo(BeNil())
		val, ok := gaugeValue(mf, map[string]string{
			constants.LabelVariantName:        "variant-a",
			constants.LabelNamespace:          "ns-a",
			constants.LabelControllerInstance: "primary",
		})
		Expect(ok).To(BeTrue(), "controller_instance label not on series")
		Expect(val).To(Equal(1.0))
	})
})
