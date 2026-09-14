package metrics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	llmdOptv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
	"github.com/prometheus/client_golang/prometheus"
)

// ControllerInstanceEnvVar is the environment variable name for controller instance label
const ControllerInstanceEnvVar = "CONTROLLER_INSTANCE"

// replicaSeriesAccel tracks the last accelerator_type label emitted for each VA
// (keyed by variant/namespace) on the replica scaling gauges. It lets
// EmitReplicaMetrics evict a stale series only when the accelerator label
// actually changes, instead of delete-and-reset every cycle (which would expose
// a zero-value gap to a concurrent scrape of the scaling signal).
//
// TODO: entries are not pruned on VariantAutoscaling deletion, so the map grows
// by one small entry per (variant, namespace) ever seen. This matches the
// pre-existing behavior of the gauge series themselves (also not deleted on VA
// removal — they expire via Prometheus staleness). Wire a deletion hook if/when
// per-VA metric cleanup is added.
var (
	replicaSeriesMu    sync.Mutex
	replicaSeriesAccel = map[string]string{}
)

// replicaSeriesKey identifies a VA's replica-gauge series independent of
// accelerator_type. controllerInstance is process-global (set once at
// InitMetrics) so it is intentionally not part of the key.
func replicaSeriesKey(variantName, namespace string) string {
	return variantName + "\x00" + namespace
}

var (
	replicaScalingTotal *prometheus.CounterVec
	desiredReplicas     *prometheus.GaugeVec
	currentReplicas     *prometheus.GaugeVec
	desiredRatio        *prometheus.GaugeVec
	errorsTotal         *prometheus.CounterVec

	optimizationDuration *prometheus.HistogramVec
	modelsProcessedGauge *prometheus.GaugeVec

	// pipeline stage visibility metrics
	decisionsLimitedTotal               *prometheus.CounterVec
	availableGpus                       *prometheus.GaugeVec
	gpuDiscoveryUp                      *prometheus.GaugeVec
	enforcerModificationsTotal          *prometheus.CounterVec
	optimizerActive                     *prometheus.GaugeVec
	configInfoGauge                     *prometheus.GaugeVec
	configKvSpareThresholdGauge         *prometheus.GaugeVec
	configQueueSpareThresholdGauge      *prometheus.GaugeVec
	configOptimizationIntervalSecsGauge *prometheus.GaugeVec
	metricsCollectionDuration           *prometheus.HistogramVec
	metricsCollectionErrors             *prometheus.CounterVec
	metricsPodsDiscovered               *prometheus.GaugeVec
	metricsFreshnessStatus              *prometheus.GaugeVec
	podMappingMissTotal                 *prometheus.CounterVec

	// Saturation and capacity metrics
	saturationUtilization *prometheus.GaugeVec
	spareCapacity         *prometheus.GaugeVec
	requiredCapacity      *prometheus.GaugeVec
	kvCacheTokensUsed     *prometheus.GaugeVec
	kvCacheTokensCapacity *prometheus.GaugeVec
	saturationMetricsUp   *prometheus.GaugeVec

	// controllerInstance stores the optional controller instance identifier.
	// When set, it's added as a label to all emitted metrics.
	controllerInstance string
)

// GetControllerInstance returns the configured controller instance label value
// Returns empty string if not configured
func GetControllerInstance() string {
	return controllerInstance
}

// InitMetrics registers all custom metrics with the provided registry.
// This function should be called once during application startup from main().
// It reads CONTROLLER_INSTANCE from the environment to optionally add
// controller instance isolation labels to all emitted metrics.
func InitMetrics(registry prometheus.Registerer) error {
	// Read controller instance from environment
	controllerInstance = os.Getenv(ControllerInstanceEnvVar)

	// Fresh registry => fresh gauges; drop any stale replica-series tracking so
	// it never references series from a previous registry.
	replicaSeriesMu.Lock()
	replicaSeriesAccel = map[string]string{}
	replicaSeriesMu.Unlock()

	// Build label sets based on whether controller_instance is configured.
	// Existing replica metrics (baseLabels, scalingLabels) are unchanged.
	// New saturation metrics carry an additional model_name label so dashboards
	// can group/filter by the model a variant serves.
	baseLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelAcceleratorType}
	scalingLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelDirection, constants.LabelReason}
	errorLabels := []string{constants.LabelComponent, constants.LabelErrorType}
	// satAccelLabels: per-variant per-accelerator saturation metrics.
	satAccelLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelModelName, constants.LabelAcceleratorType}
	// satModelLabels: per-variant model-level saturation metrics (no accelerator_type).
	satModelLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelModelName}
	// requiredCapacityLabels: satModelLabels + "unit" to disambiguate V1 (binary 0/1)
	// vs V2 (continuous token demand) values of the wva_required_capacity gauge.
	requiredCapacityLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelModelName, constants.LabelUnit}
	// satFreshnessLabels: smallest cardinality shared across the five
	// saturation/capacity gauges. Freshness is a per-VA property, so
	// model_name / accelerator_type / unit don't need to be on this series.
	satFreshnessLabels := []string{constants.LabelVariantName, constants.LabelNamespace}

	if controllerInstance != "" {
		baseLabels = append(baseLabels, constants.LabelControllerInstance)
		scalingLabels = append(scalingLabels, constants.LabelControllerInstance)
		errorLabels = append(errorLabels, constants.LabelControllerInstance)
		satAccelLabels = append(satAccelLabels, constants.LabelControllerInstance)
		satModelLabels = append(satModelLabels, constants.LabelControllerInstance)
		requiredCapacityLabels = append(requiredCapacityLabels, constants.LabelControllerInstance)
		satFreshnessLabels = append(satFreshnessLabels, constants.LabelControllerInstance)
	}

	replicaScalingTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAReplicaScalingTotal,
			Help: "Total number of replica scaling operations",
		},
		scalingLabels,
	)
	desiredReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVADesiredReplicas,
			Help: "Desired number of replicas for each variant",
		},
		baseLabels,
	)
	currentReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVACurrentReplicas,
			Help: "Current number of replicas for each variant",
		},
		baseLabels,
	)
	desiredRatio = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVADesiredRatio,
			Help: "Ratio of the desired number of replicas and the current number of replicas for each variant",
		},
		baseLabels,
	)
	errorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAErrorsTotal,
			Help: "Total number of errors by component",
		},
		errorLabels,
	)
	saturationUtilization = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVASaturationUtilization,
			Help: "Per-variant utilization ratio (0.0-1.0) from saturation analysis. V1 path: mean of per-replica KV-cache-usage fractions (matches the per-replica threshold V1 checks). V2 path: TotalDemand / TotalCapacity from the analyzer result. Numerically equivalent for uniform-capacity replicas; V2 is capacity-weighted for mixed-capacity cases.",
		},
		satAccelLabels,
	)
	spareCapacity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVASpareCapacity,
			Help: fmt.Sprintf("Spare capacity; >0 indicates safe scale-down headroom (per-role for P/D-disaggregated models, model-level otherwise). The %q label (shared with wva_required_capacity) interprets the value: %q → token surplus from the Token-based analyzer, max(0, TotalSupply - TotalDemand/scaleDownBoundary); empty → a 0.0-1.0 threshold-relative fraction from the Percentage-based analyzer.", constants.LabelUnit, constants.UnitContinuous),
		},
		requiredCapacityLabels,
	)
	requiredCapacity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVARequiredCapacity,
			Help: fmt.Sprintf("Required capacity; >0 indicates scale-up needed (per-role for P/D-disaggregated models, model-level otherwise). Use the %q label to interpret the value: %q → token demand from the Token-based analyzer, %q → 0/1 scale-up signal from the Percentage-based analyzer.", constants.LabelUnit, constants.UnitContinuous, constants.UnitBinary),
		},
		requiredCapacityLabels,
	)
	kvCacheTokensUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAKvCacheTokensUsed,
			Help: "Total KV cache tokens currently in use across all replicas of a variant (sum of per-replica TokensInUse).",
		},
		satModelLabels,
	)
	kvCacheTokensCapacity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAKvCacheTokensCapacity,
			Help: "Total KV cache token capacity across all replicas of a variant (sum of per-replica TotalKvCapacityTokens).",
		},
		satModelLabels,
	)
	saturationMetricsUp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVASaturationMetricsUp,
			Help: "Per-VA freshness signal for the saturation/capacity gauges: 1.0 in cycles where the optimizer produced a fresh decision for the variant, 0.0 in cycles where the analyzer was aware of the variant but did not refresh those gauges. Pairs with wva_saturation_utilization, wva_spare_capacity, wva_required_capacity, wva_kv_cache_tokens_used, and wva_kv_cache_tokens_capacity so dashboards can gate alerts on this gauge instead of relying on Prometheus' implicit staleness marker.",
		},
		satFreshnessLabels,
	)

	optimizationDurationLabels := []string{constants.LabelStatus}
	if controllerInstance != "" {
		optimizationDurationLabels = append(optimizationDurationLabels, constants.LabelControllerInstance)
	}
	optimizationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    constants.WVAOptimizationDurationSeconds,
			Help:    "Duration of optimization loop cycles in seconds",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		optimizationDurationLabels,
	)
	modelsProcessedLabels := []string{}
	if controllerInstance != "" {
		modelsProcessedLabels = append(modelsProcessedLabels, constants.LabelControllerInstance)
	}
	modelsProcessedGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAModelsProcessed,
			Help: "Number of models processed in the last optimization cycle",
		},
		modelsProcessedLabels,
	)

	decisionsLimitedLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelLimiterName}
	if controllerInstance != "" {
		decisionsLimitedLabels = append(decisionsLimitedLabels, constants.LabelControllerInstance)
	}
	decisionsLimitedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVADecisionsLimitedTotal,
			Help: "Total number of decisions limited by the limiter",
		},
		decisionsLimitedLabels,
	)

	availableGpusLabels := []string{constants.LabelAcceleratorVendor, constants.LabelAcceleratorModel, constants.LabelAcceleratorType}
	if controllerInstance != "" {
		availableGpusLabels = append(availableGpusLabels, constants.LabelControllerInstance)
	}
	availableGpus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAAvailableGpus,
			Help: "Number of currently available GPUs when wva_gpu_discovery_up is 1. When wva_gpu_discovery_up is 0, shows the number of GPUs that were available at the last successful discovery.",
		},
		availableGpusLabels,
	)

	gpuDiscoveryUpLabels := []string{}
	if controllerInstance != "" {
		gpuDiscoveryUpLabels = append(gpuDiscoveryUpLabels, constants.LabelControllerInstance)
	}
	gpuDiscoveryUp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAGpuDiscoveryUp,
			Help: "Indicates whether GPU discovery is on or off",
		},
		gpuDiscoveryUpLabels,
	)

	enforcerModificationsLabels := []string{constants.LabelPolicyType}
	if controllerInstance != "" {
		enforcerModificationsLabels = append(enforcerModificationsLabels, constants.LabelControllerInstance)
	}
	enforcerModificationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAEnforcerModificationsTotal,
			Help: "Total number of decision modifications made by the enforcer",
		},
		enforcerModificationsLabels,
	)

	optimizerActiveLabels := []string{constants.LabelOptimizerName}
	if controllerInstance != "" {
		optimizerActiveLabels = append(optimizerActiveLabels, constants.LabelControllerInstance)
	}
	optimizerActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAOptimizerActive,
			Help: "1 for active optimizer, 0 for inactive",
		},
		optimizerActiveLabels,
	)

	// Config info metric with labels
	configInfoLabels := []string{constants.LabelAnalyzerName, constants.LabelLimiterEnabled, constants.LabelScaleToZeroEnabled}
	if controllerInstance != "" {
		configInfoLabels = append(configInfoLabels, constants.LabelControllerInstance)
	}
	configInfoGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAConfigInfo,
			Help: "WVA configuration information (value is always 1)",
		},
		configInfoLabels,
	)

	// Config threshold and interval metrics
	configLabels := []string{}
	if controllerInstance != "" {
		configLabels = append(configLabels, constants.LabelControllerInstance)
	}
	configKvSpareThresholdGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAConfigKvSpareThreshold,
			Help: "Global default (not per-model override) KV cache spare threshold configuration value",
		},
		configLabels,
	)
	configQueueSpareThresholdGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAConfigQueueSpareThreshold,
			Help: "Global default (not per-model override) queue spare threshold configuration value",
		},
		configLabels,
	)
	configOptimizationIntervalSecsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAConfigOptimizationIntervalSeconds,
			Help: "Optimization interval in seconds",
		},
		configLabels,
	)

	metricsCollectionDurationLabels := []string{constants.LabelQueryType}
	if controllerInstance != "" {
		metricsCollectionDurationLabels = append(metricsCollectionDurationLabels, constants.LabelControllerInstance)
	}
	metricsCollectionDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    constants.WVAMetricsCollectionDurationSeconds,
			Help:    "Duration of metrics collection operations in seconds",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
		},
		metricsCollectionDurationLabels,
	)

	metricsCollectionErrorsLabels := []string{constants.LabelQueryType, constants.LabelReason}
	if controllerInstance != "" {
		metricsCollectionErrorsLabels = append(metricsCollectionErrorsLabels, constants.LabelControllerInstance)
	}
	metricsCollectionErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAMetricsCollectionErrorsTotal,
			Help: "Total number of metrics collection errors",
		},
		metricsCollectionErrorsLabels,
	)

	metricsPodsDiscoveredLabels := []string{constants.LabelNamespace}
	if controllerInstance != "" {
		metricsPodsDiscoveredLabels = append(metricsPodsDiscoveredLabels, constants.LabelControllerInstance)
	}
	metricsPodsDiscovered = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAMetricsPodsDiscovered,
			Help: "Number of pods discovered for a namespace",
		},
		metricsPodsDiscoveredLabels,
	)

	metricsFreshnessStatusLabels := []string{constants.LabelVariantName, constants.LabelStatus}
	if controllerInstance != "" {
		metricsFreshnessStatusLabels = append(metricsFreshnessStatusLabels, constants.LabelControllerInstance)
	}
	metricsFreshnessStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAMetricsFreshnessStatus,
			Help: "Freshness status of metrics for each variant",
		},
		metricsFreshnessStatusLabels,
	)

	podMappingMissLabels := []string{constants.LabelNamespace, constants.LabelReason}
	if controllerInstance != "" {
		podMappingMissLabels = append(podMappingMissLabels, constants.LabelControllerInstance)
	}
	podMappingMissTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAPodMappingMissTotal,
			Help: "Total number of pods whose metrics could not be attributed to a managed scaler",
		},
		podMappingMissLabels,
	)

	// Register metrics with the registry
	if err := registry.Register(replicaScalingTotal); err != nil {
		return fmt.Errorf("failed to register replicaScalingTotal metric: %w", err)
	}
	if err := registry.Register(desiredReplicas); err != nil {
		return fmt.Errorf("failed to register desiredReplicas metric: %w", err)
	}
	if err := registry.Register(currentReplicas); err != nil {
		return fmt.Errorf("failed to register currentReplicas metric: %w", err)
	}
	if err := registry.Register(desiredRatio); err != nil {
		return fmt.Errorf("failed to register desiredRatio metric: %w", err)
	}
	if err := registry.Register(errorsTotal); err != nil {
		return fmt.Errorf("failed to register errorsTotal metric: %w", err)
	}
	if err := registry.Register(optimizationDuration); err != nil {
		return fmt.Errorf("failed to register optimizationDuration metric: %w", err)
	}
	if err := registry.Register(modelsProcessedGauge); err != nil {
		return fmt.Errorf("failed to register modelsProcessedGauge metric: %w", err)
	}
	if err := registry.Register(decisionsLimitedTotal); err != nil {
		return fmt.Errorf("failed to register decisionsLimitedTotal metric: %w", err)
	}
	if err := registry.Register(availableGpus); err != nil {
		return fmt.Errorf("failed to register availableGpus metric: %w", err)
	}
	if err := registry.Register(gpuDiscoveryUp); err != nil {
		return fmt.Errorf("failed to register gpuDiscoveryUp metric: %w", err)
	}
	if err := registry.Register(enforcerModificationsTotal); err != nil {
		return fmt.Errorf("failed to register enforcerModificationsTotal metric: %w", err)
	}
	if err := registry.Register(optimizerActive); err != nil {
		return fmt.Errorf("failed to register optimizerActive metric: %w", err)
	}
	if err := registry.Register(configInfoGauge); err != nil {
		return fmt.Errorf("failed to register configInfoGauge metric: %w", err)
	}
	if err := registry.Register(configKvSpareThresholdGauge); err != nil {
		return fmt.Errorf("failed to register configKvSpareThresholdGauge metric: %w", err)
	}
	if err := registry.Register(configQueueSpareThresholdGauge); err != nil {
		return fmt.Errorf("failed to register configQueueSpareThresholdGauge metric: %w", err)
	}
	if err := registry.Register(configOptimizationIntervalSecsGauge); err != nil {
		return fmt.Errorf("failed to register configOptimizationIntervalSecsGauge metric: %w", err)
	}
	if err := registry.Register(metricsCollectionDuration); err != nil {
		return fmt.Errorf("failed to register metricsCollectionDuration metric: %w", err)
	}
	if err := registry.Register(metricsCollectionErrors); err != nil {
		return fmt.Errorf("failed to register metricsCollectionErrors metric: %w", err)
	}
	if err := registry.Register(metricsPodsDiscovered); err != nil {
		return fmt.Errorf("failed to register metricsPodsDiscovered metric: %w", err)
	}
	if err := registry.Register(podMappingMissTotal); err != nil {
		return fmt.Errorf("failed to register podMappingMissTotal metric: %w", err)
	}
	if err := registry.Register(metricsFreshnessStatus); err != nil {
		return fmt.Errorf("failed to register metricsFreshnessStatus metric: %w", err)
	}
	if err := registry.Register(saturationUtilization); err != nil {
		return fmt.Errorf("failed to register saturationUtilization metric: %w", err)
	}
	if err := registry.Register(spareCapacity); err != nil {
		return fmt.Errorf("failed to register spareCapacity metric: %w", err)
	}
	if err := registry.Register(requiredCapacity); err != nil {
		return fmt.Errorf("failed to register requiredCapacity metric: %w", err)
	}
	if err := registry.Register(kvCacheTokensUsed); err != nil {
		return fmt.Errorf("failed to register kvCacheTokensUsed metric: %w", err)
	}
	if err := registry.Register(kvCacheTokensCapacity); err != nil {
		return fmt.Errorf("failed to register kvCacheTokensCapacity metric: %w", err)
	}
	if err := registry.Register(saturationMetricsUp); err != nil {
		return fmt.Errorf("failed to register saturationMetricsUp metric: %w", err)
	}

	return nil
}

// InitMetricsAndEmitter registers metrics with Prometheus and creates a metrics emitter
// This is a convenience function that handles both registration and emitter creation
func InitMetricsAndEmitter(registry prometheus.Registerer) (*MetricsEmitter, error) {
	if err := InitMetrics(registry); err != nil {
		return nil, err
	}
	return NewMetricsEmitter(), nil
}

// MetricsEmitter handles emission of custom metrics
type MetricsEmitter struct{}

// NewMetricsEmitter creates a new metrics emitter
func NewMetricsEmitter() *MetricsEmitter {
	return &MetricsEmitter{}
}

// EmitReplicaScalingMetrics emits metrics related to replica scaling
func (m *MetricsEmitter) EmitReplicaScalingMetrics(ctx context.Context, va *llmdOptv1alpha1.VariantAutoscaling, direction domain.SaturationAction, reason domain.DecisionReason) error {
	labels := prometheus.Labels{
		constants.LabelVariantName: va.Name,
		constants.LabelNamespace:   va.Namespace,
		constants.LabelDirection:   string(direction),
		constants.LabelReason:      string(reason),
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	// These operations are local and should never fail, but we handle errors for debugging
	if replicaScalingTotal == nil {
		return errors.New("replicaScalingTotal metric not initialized")
	}

	replicaScalingTotal.With(labels).Inc()
	return nil
}

// ObserveOptimizationDuration records the duration of an optimization cycle with the given status.
// Status should be one of: "success", "error".
func ObserveOptimizationDuration(durationSeconds float64, status string) {
	if optimizationDuration == nil {
		return
	}
	labels := prometheus.Labels{constants.LabelStatus: status}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	optimizationDuration.With(labels).Observe(durationSeconds)
}

// SetModelsProcessed sets the gauge to the number of models processed in the last optimization cycle.
func SetModelsProcessed(count int) {
	if modelsProcessedGauge == nil {
		return
	}
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	modelsProcessedGauge.With(labels).Set(float64(count))
}

// EmitReplicaMetrics emits current and desired replica metrics — the scaling
// signal consumed by HPA/KEDA. These gauges are emitted regardless of whether
// the accelerator type is resolved: the signal must never be withheld just
// because the accelerator is unknown. When the type is unresolved the
// accelerator_type label carries the bounded UnresolvedAcceleratorType value
// (never the internal "unknown" sentinel).
//
// The HPA/KEDA query selects a VA by variant_name + namespace, NOT by
// accelerator_type, so scaling must not depend on that label. To keep that true
// across an accelerator transition (e.g. unresolved -> a real type) without
// exposing a gap on the steady-state path, we set the current series first and
// then — only when the accelerator label actually changed since the last emit —
// delete the now-stale prior series. Steady state (no label change) is therefore
// a plain idempotent Set: no gap, no per-cycle churn, exactly one series. Only
// during an accelerator transition do the old and new series briefly coexist
// (set-before-delete), which is preferable to a zero-value gap.
func (m *MetricsEmitter) EmitReplicaMetrics(ctx context.Context, va *llmdOptv1alpha1.VariantAutoscaling, current, desired int32, acceleratorType string) error {
	// These operations are local and should never fail, but we handle errors for debugging
	if currentReplicas == nil || desiredReplicas == nil || desiredRatio == nil {
		return errors.New("replica metrics not initialized")
	}

	if !constants.IsAcceleratorResolved(acceleratorType) {
		acceleratorType = constants.UnresolvedAcceleratorType
	}

	labelsFor := func(accel string) prometheus.Labels {
		l := prometheus.Labels{
			constants.LabelVariantName:     va.Name,
			constants.LabelNamespace:       va.Namespace,
			constants.LabelAcceleratorType: accel,
		}
		if controllerInstance != "" {
			l[constants.LabelControllerInstance] = controllerInstance
		}
		return l
	}

	// Set the current series first so a concurrent scrape never sees a gap.
	baseLabels := labelsFor(acceleratorType)
	currentReplicas.With(baseLabels).Set(float64(current))
	desiredReplicas.With(baseLabels).Set(float64(desired))
	// Avoid division by 0 if current replicas is zero: set the ratio to the desired replicas.
	// Going 0 -> N is treated by using `desired_ratio = N`.
	if current == 0 {
		desiredRatio.With(baseLabels).Set(float64(desired))
	} else {
		desiredRatio.With(baseLabels).Set(float64(desired) / float64(current))
	}

	// If the accelerator label changed since the last emit for this VA, evict the
	// stale prior series (done after Set, so there is no zero-value window).
	key := replicaSeriesKey(va.Name, va.Namespace)
	replicaSeriesMu.Lock()
	prev, had := replicaSeriesAccel[key]
	replicaSeriesAccel[key] = acceleratorType
	replicaSeriesMu.Unlock()
	if had && prev != acceleratorType {
		stale := labelsFor(prev)
		currentReplicas.Delete(stale)
		desiredReplicas.Delete(stale)
		desiredRatio.Delete(stale)
	}
	return nil
}

// RecordOptimizerActiveMetric records which optimizer is currently active.
// Only one optimizer should be active at a time (isActive=true), while others are inactive (isActive=false).
func (m *MetricsEmitter) RecordOptimizerActiveMetric(optimizerName string, isActive bool) {
	if optimizerActive == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelOptimizerName: optimizerName,
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	v := float64(0)
	if isActive {
		v = 1
	}
	optimizerActive.With(labels).Set(v)
}

// RecordError records an error metric for the specified component and error type.
// Callers MUST invoke InitMetrics before this method (the package-level
// metric vars are nil otherwise, and the Set calls below would panic).
// InitMetricsAndEmitter is the recommended construction path because it
// performs the registration before returning the emitter.
func RecordError(component, errorType string) {
	if errorsTotal == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelComponent: component,
		constants.LabelErrorType: errorType,
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	errorsTotal.With(labels).Inc()
}

// RecordEnforcerMetric records a decision modification made by the enforcer.
// The policyType identifies which enforcement policy (e.g., "scale_to_zero", "minimum_replicas") was applied.
func (m *MetricsEmitter) RecordEnforcerMetric(policyType string) {
	if enforcerModificationsTotal == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelPolicyType: policyType,
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	enforcerModificationsTotal.With(labels).Inc()
}

// SetGpuDiscoveryUp sets the GPU discovery status metric.
// status should be 1 when GPU discovery is enabled, 0 when it is disabled.
func SetGpuDiscoveryUp(status float64) {
	if gpuDiscoveryUp == nil {
		return
	}
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	gpuDiscoveryUp.With(labels).Set(status)
}

// RecordAvailableGPUsMetric records the number of available GPUs for a given accelerator type. acceleratorModel is
// accelerator full name, acceleratorType is short name.
// This metric is updated during GPU discovery and reflects cluster GPU capacity.
func (m *MetricsEmitter) RecordAvailableGPUsMetric(vendor, acceleratorModel, acceleratorType string, count int32) {
	if availableGpus == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelAcceleratorVendor: vendor,
		constants.LabelAcceleratorModel:  acceleratorModel,
		constants.LabelAcceleratorType:   acceleratorType,
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	availableGpus.With(labels).Set(float64(count))
}

// RecordDecisionsLimitedTotalMetric records when a scaling decision was constrained by a limiter.
// This tracks how often the limiter prevents scaling actions due to resource constraints.
func (m *MetricsEmitter) RecordDecisionsLimitedTotalMetric(variantName, namespace, limiterName string) {
	if decisionsLimitedTotal == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelVariantName: variantName,
		constants.LabelNamespace:   namespace,
		constants.LabelLimiterName: limiterName,
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	decisionsLimitedTotal.With(labels).Inc()
}

// SetConfigInfo sets the config info metric with the given analyzer name and feature flags.
// The metric value is always set to 1 (info-style metric).
func SetConfigInfo(analyzerName string, limiterEnabled, scaleToZeroEnabled bool) {
	if configInfoGauge == nil {
		return
	}
	configInfoGauge.Reset()
	labels := prometheus.Labels{
		constants.LabelAnalyzerName:       analyzerName,
		constants.LabelLimiterEnabled:     strconv.FormatBool(limiterEnabled),
		constants.LabelScaleToZeroEnabled: strconv.FormatBool(scaleToZeroEnabled),
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	configInfoGauge.With(labels).Set(1)
}

// SetConfigKvSpareThreshold sets the KV spare threshold configuration value.
func SetConfigKvSpareThreshold(threshold float64) {
	if configKvSpareThresholdGauge == nil {
		return
	}
	configKvSpareThresholdGauge.Reset()
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	configKvSpareThresholdGauge.With(labels).Set(threshold)
}

// SetConfigQueueSpareThreshold sets the queue spare threshold configuration value.
func SetConfigQueueSpareThreshold(threshold float64) {
	if configQueueSpareThresholdGauge == nil {
		return
	}
	configQueueSpareThresholdGauge.Reset()
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	configQueueSpareThresholdGauge.With(labels).Set(threshold)
}

// SetConfigOptimizationInterval sets the optimization interval in seconds.
func SetConfigOptimizationInterval(intervalSeconds float64) {
	if configOptimizationIntervalSecsGauge == nil {
		return
	}
	configOptimizationIntervalSecsGauge.Reset()
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	configOptimizationIntervalSecsGauge.With(labels).Set(intervalSeconds)
}

// ObserveMetricsCollectionDuration records the duration of a metrics collection operation.
func ObserveMetricsCollectionDuration(durationSeconds float64, queryType string) {
	if metricsCollectionDuration == nil {
		return
	}
	labels := prometheus.Labels{constants.LabelQueryType: queryType}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	metricsCollectionDuration.With(labels).Observe(durationSeconds)
}

// IncMetricsCollectionErrors increments the metrics collection error counter.
func IncMetricsCollectionErrors(queryType, reason string) {
	if metricsCollectionErrors == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelQueryType: queryType,
		constants.LabelReason:    reason,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	metricsCollectionErrors.With(labels).Inc()
}

// IncPodMappingMiss increments the pod-to-managed-scaler mapping miss counter.
func IncPodMappingMiss(namespace, reason string) {
	if podMappingMissTotal == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelReason:    reason,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	podMappingMissTotal.With(labels).Inc()
}

// SetMetricsPodsDiscovered sets the number of pods discovered in a namespace.
func SetMetricsPodsDiscovered(namespace string, count int) {
	if metricsPodsDiscovered == nil {
		return
	}
	labels := prometheus.Labels{constants.LabelNamespace: namespace}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	metricsPodsDiscovered.With(labels).Set(float64(count))
}

// SetMetricsFreshnessStatus sets the freshness status count for a variant's metrics.
// status should be one of: "fresh", "stale", "missing", "unavailable".
// count is the number of metrics with this status for the variant.
func SetMetricsFreshnessStatus(variantName, status string, count int) {
	if metricsFreshnessStatus == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelVariantName: variantName,
		constants.LabelStatus:      status,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	metricsFreshnessStatus.With(labels).Set(float64(count))
}

// RecordSaturationMetrics records saturation analysis and KV cache capacity
// metrics. The "Record" naming reflects the fact that the controller does not
// actively push these metrics — Prometheus scrapes them.
//
// modelID is exposed as the model_name label so dashboards can group/filter by
// the model a variant serves. requiredCapacityUnit ("binary" or "continuous")
// is used as the "unit" label on both wva_required_capacity and wva_spare_capacity
// (they share the same label set) to describe how the value should be interpreted.
//
// Callers MUST invoke InitMetrics before this method (the package-level
// metric vars are nil otherwise, and the Set calls below would panic).
// InitMetricsAndEmitter is the recommended construction path because it
// performs the registration before returning the emitter.
func (m *MetricsEmitter) RecordSaturationMetrics(
	ctx context.Context,
	variantName, namespace, modelID, acceleratorType, requiredCapacityUnit string,
	utilization, spare, required float64,
	kvTokensUsed, kvTokensCapacity int64,
) {
	accelLabels := prometheus.Labels{
		constants.LabelVariantName:     variantName,
		constants.LabelNamespace:       namespace,
		constants.LabelModelName:       modelID,
		constants.LabelAcceleratorType: acceleratorType,
	}
	modelLabels := prometheus.Labels{
		constants.LabelVariantName: variantName,
		constants.LabelNamespace:   namespace,
		constants.LabelModelName:   modelID,
	}
	// capacityLabels are shared by wva_required_capacity and wva_spare_capacity: both
	// are model/role-level signals carrying the unit label (no accelerator_type).
	capacityLabels := prometheus.Labels{
		constants.LabelVariantName: variantName,
		constants.LabelNamespace:   namespace,
		constants.LabelModelName:   modelID,
		constants.LabelUnit:        requiredCapacityUnit,
	}

	if controllerInstance != "" {
		accelLabels[constants.LabelControllerInstance] = controllerInstance
		modelLabels[constants.LabelControllerInstance] = controllerInstance
		capacityLabels[constants.LabelControllerInstance] = controllerInstance
	}

	saturationUtilization.With(accelLabels).Set(utilization)
	spareCapacity.With(capacityLabels).Set(spare)
	requiredCapacity.With(capacityLabels).Set(required)
	kvCacheTokensUsed.With(modelLabels).Set(float64(kvTokensUsed))
	kvCacheTokensCapacity.With(modelLabels).Set(float64(kvTokensCapacity))
}

// RecordSaturationFreshness publishes the per-VA freshness signal for the
// five saturation/capacity gauges recorded by RecordSaturationMetrics.
//
// fresh=true means the optimizer just produced a fresh decision for the
// variant this cycle (the other gauges have just been refreshed). fresh=false
// means the analyzer was aware of the variant but did not refresh those
// gauges this cycle, so dashboards see an explicit "stale" signal rather
// than relying on Prometheus' 5-minute implicit staleness marker.
//
// Unlike RecordSaturationMetrics, this method runs on every variant every
// cycle (not gated on hasDecision), so it must tolerate tests that haven't
// called InitMetrics — nil-guarded the same way as SetMetricsFreshnessStatus.
// In production InitMetrics runs at startup from main.go and the guard is a
// no-op fast path.
func (m *MetricsEmitter) RecordSaturationFreshness(ctx context.Context, variantName, namespace string, fresh bool) {
	if saturationMetricsUp == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelVariantName: variantName,
		constants.LabelNamespace:   namespace,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	value := 0.0
	if fresh {
		value = 1.0
	}
	saturationMetricsUp.With(labels).Set(value)
}
