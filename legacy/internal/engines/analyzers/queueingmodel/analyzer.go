package queueingmodel

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/queueingmodel/tuner"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/queueing/analyzer"
	ctrl "sigs.k8s.io/controller-runtime"
)

// QueueingModelAnalyzer implements domain.Analyzer.
// It performs SLO-driven capacity analysis by:
//  1. Learning model parameters (alpha, beta, gamma) online via Kalman filter
//  2. Using queueing model to predict max request rate that meets TTFT/ITL SLOs
//  3. Computing capacity signals for scaling decisions

type QueueingModelAnalyzer struct {
	// modelsParameterStore stores learned parameters of variants for all models
	modelsParameterStore map[string]*ParameterStore // key: modelKey (namespace/modelID)
}

// NewQueueingModelAnalyzer creates a new queueing model analyzer instance.
func NewQueueingModelAnalyzer() *QueueingModelAnalyzer {
	return &QueueingModelAnalyzer{
		modelsParameterStore: make(map[string]*ParameterStore),
	}
}

// Name implements domain.Analyzer.
func (a *QueueingModelAnalyzer) Name() string {
	return domain.QueueingModelAnalyzerName
}

// Update deletes non-existing models from paramStore[models]
// and adds new models to the store
func (a *QueueingModelAnalyzer) Update(currentModels map[string]bool) {
	// delete non-existing models
	deletedModels := []string{}
	for modelKey := range a.modelsParameterStore {
		if _, exists := currentModels[modelKey]; !exists {
			deletedModels = append(deletedModels, modelKey)
		}
	}
	for _, modelKey := range deletedModels {
		delete(a.modelsParameterStore, modelKey)
	}

	// add new models
	for modelKey := range currentModels {
		if _, exists := a.modelsParameterStore[modelKey]; !exists {
			a.modelsParameterStore[modelKey] = NewParameterStore()
		}
	}
}

// get parameters for a given model, namespace, and variant (nil if does not exist)
func (a *QueueingModelAnalyzer) getParams(modelID, namespace, variantName string) (params *LearnedParameters) {
	modelKey := MakeModelKey(namespace, modelID)
	if pStore, exists := a.modelsParameterStore[modelKey]; exists {
		params = pStore.Get(namespace, variantName)
	}
	return params
}

// set parameters for a given model, namespace, and variant
func (a *QueueingModelAnalyzer) setParams(modelID, namespace, variantName string, params *LearnedParameters) {
	modelKey := MakeModelKey(namespace, modelID)
	pStore := a.modelsParameterStore[modelKey]
	// this shouldn't happen as Update() makes sure that there are entries for all current models
	if pStore == nil {
		a.modelsParameterStore[modelKey] = NewParameterStore()
		pStore = a.modelsParameterStore[modelKey]
	}
	pStore.Set(namespace, variantName, params)
}

// Analyze implements domain.Analyzer.
// Called for each model.
//
// If we fail to analyze a model (bad config, no learned parameters, no
// variant capacities), Analyze returns an error. The caller is expected
// to retry on subsequent reconcile cycles; the error persists until the
// underlying condition is resolved (e.g., tuner succeeds, metrics become
// available). The one exception is missing SLO targets — that yields an
// empty result rather than an error, since capacity cannot be defined
// without an SLO but the situation is not necessarily erroneous.
func (a *QueueingModelAnalyzer) Analyze(
	ctx context.Context,
	input domain.AnalyzerInput,
) (*domain.AnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)
	modelID := input.ModelID
	namespace := input.Namespace

	// Extract configuration
	qConfig, ok := input.Config.(*QMConfig)
	if !ok {
		return nil, fmt.Errorf("expected *QMConfig, got %T", input.Config)
	}

	// Get variant names and group metrics by variant
	variantNames := getVariantNames(input.ReplicaMetrics)
	variantMetrics := groupMetricsByVariant(input.ReplicaMetrics)

	// Update parameters (tuner) for all variants associated with the model
	if qConfig.TuningEnabled {
		a.updateVariantParameters(ctx, namespace, modelID, variantNames, variantMetrics, qConfig)
	}

	// Get SLO targets
	// TODO: store the time series for SLO and smooth the SLO target.
	sloTarget := a.getSLOTarget(ctx, namespace, modelID, qConfig, variantNames, input.ReplicaMetrics)
	if sloTarget == nil {
		logger.Info("No SLO targets", "modelID", modelID)
		return nil, fmt.Errorf("failed to analyze variants due to lack of SLO targets for model %q", modelID)
	}

	// Compute capacities
	variantCapacities := a.computeAllVariantCapacities(
		ctx, namespace, modelID, variantMetrics, input.VariantStates, sloTarget,
	)
	if len(variantCapacities) == 0 {
		return nil, fmt.Errorf("could not compute variant capacities for model %q", modelID)
	}

	// Aggregate and build result
	totalSupply, totalDemand := aggregateCapacities(variantCapacities)
	utilization := 0.0
	if totalSupply > 0 {
		utilization = totalDemand / totalSupply
	}

	return &domain.AnalyzerResult{
		AnalyzerName:      a.Name(),
		ModelID:           modelID,
		Namespace:         namespace,
		AnalyzedAt:        time.Now(),
		VariantCapacities: variantCapacities,
		TotalSupply:       totalSupply,
		TotalDemand:       totalDemand,
		Utilization:       utilization,
		RequiredCapacity:  math.Max(0, totalDemand-totalSupply),
		SpareCapacity:     math.Max(0, totalSupply-totalDemand),
	}, nil
}

// updateVariantParameters runs the model tuner and updates the parameters in the store
func (a *QueueingModelAnalyzer) updateVariantParameters(
	ctx context.Context,
	namespace string,
	modelID string,
	variantNames []string,
	variantMetrics map[string][]domain.ReplicaMetrics,
	config *QMConfig,
) {
	logger := ctrl.LoggerFrom(ctx)

	// Run tuner for each variant
	// (use variant names from slice instead of map to avoid randomness)
	for _, variantName := range variantNames {
		variantReplicaMetrics := variantMetrics[variantName]
		if variantReplicaMetrics == nil {
			logger.V(1).Info("No metric for variant", "variant", variantName)
			continue
		}
		// Build environment from replica metrics
		envs, err := buildEnvironmentsFromMetrics(variantName, variantReplicaMetrics)
		if len(envs) == 0 || err != nil {
			logger.V(1).Info("Failed to build environment for variant",
				"variant", variantName,
				"namespace", namespace,
				"error", err)
			continue
		}

		// Create a variantTuner for this variant
		variantTuner, err := a.createTunerForVariant(ctx, namespace, modelID, variantName, envs[0], config.FilterConfig)
		if err != nil {
			logger.V(1).Info("Failed to get/create tuner for variant",
				"variant", variantName,
				"namespace", namespace,
				"error", err)
			continue
		}

		// Run tuner to learn parameters
		var results *tuner.TunedResults
		for _, env := range envs {
			var err error
			if err = variantTuner.UpdateEnvironment(env); err != nil {
				logger.V(1).Info("Tuner could not update environment for variant",
					"variant", variantName,
					"namespace", namespace,
					"error", err)
				continue
			}
			results, err = variantTuner.Run()
			if results.ValidationFailed {
				err = fmt.Errorf("tune validation failed: %w", err)
			}
			if err != nil {
				logger.V(1).Info("Tuner failed to run for variant",
					"variant", variantName,
					"namespace", namespace,
					"error", err)
			}
		}
		if results == nil {
			logger.V(1).Info("Failed to tune variant",
				"variant", variantName,
				"namespace", namespace)
			continue
		}

		// Store tuned parameters
		a.storeParametersFromResults(namespace, modelID, variantName, results)

		// Log tuning results
		if results.ValidationFailed {
			logger.Info("Tuner validation failed, using previous state",
				"variant", variantName,
				"namespace", namespace,
				"NIS", results.NIS)
		} else {
			logger.V(1).Info("Parameters tuned successfully",
				"variant", variantName,
				"namespace", namespace,
				"alpha", results.ServiceParms.Alpha,
				"beta", results.ServiceParms.Beta,
				"gamma", results.ServiceParms.Gamma,
				"NIS", results.NIS)
		}
	}
}

func (a *QueueingModelAnalyzer) getSLOTarget(
	ctx context.Context,
	namespace string,
	modelID string,
	config *QMConfig,
	variantNames []string,
	modelReplicaMetrics []domain.ReplicaMetrics,
) *SLOTarget {
	// First try explicit config
	if slo := config.GetSLOForModel(namespace, modelID); slo != nil {
		return slo
	}
	// Infer SLO from the queueing model and observed metrics
	return a.guessSLOFromMetrics(ctx, namespace, modelID, config, variantNames, modelReplicaMetrics)
}

// calculate capacities for all variants of a model in a given namespace
func (a *QueueingModelAnalyzer) computeAllVariantCapacities(
	ctx context.Context,
	namespace string,
	modelID string,
	variantMetrics map[string][]domain.ReplicaMetrics,
	variantStates []domain.VariantReplicaState,
	sloTarget *SLOTarget,
) []domain.VariantCapacity {
	logger := ctrl.LoggerFrom(ctx)

	// Build cost and accelerator lookup from input metrics
	variantCost := make(map[string]float64)
	variantAccel := make(map[string]string)
	for variantName, replicaMetrics := range variantMetrics {
		if len(replicaMetrics) > 0 {
			variantCost[variantName] = replicaMetrics[0].Cost
			variantAccel[variantName] = replicaMetrics[0].AcceleratorName
		}
	}

	variantCapacities := make([]domain.VariantCapacity, 0, len(variantStates))
	for _, variantState := range variantStates {
		variantName := variantState.VariantName
		readyCount := max(variantState.CurrentReplicas-variantState.PendingReplicas, 0)

		// variant capacity in case of error
		errorVariantCapacity := domain.VariantCapacity{
			VariantName:     variantName,
			AcceleratorName: variantAccel[variantName],
			Cost:            variantCost[variantName],
			// Role must be carried: consumers canonicalize a blank role to
			// domain.RoleBoth, which on a disaggregated model collapses prefill and
			// decode into one bucket and lets the optimizer shed them
			// interchangeably. This path is parked (the engine refuses to dispatch
			// it today), so this is here for whoever re-enables it.
			Role:            variantState.Role,
			ReplicaCount:    readyCount,
			PendingReplicas: variantState.PendingReplicas,

			PerReplicaCapacity: 0.0, // TODO: caller should handle variants without results, instead of relying on absolute values
			TotalCapacity:      0.0,
			TotalDemand:        0.0,
			Utilization:        0.0,
		}

		// Accumulate data over all pod replicas of the variant
		replicaMetrics := variantMetrics[variantName]
		if len(replicaMetrics) == 0 {
			logger.Info("No replicas for variant", "variant", variantName)
			vr := errorVariantCapacity
			variantCapacities = append(variantCapacities, vr)
			continue
		}
		wm := aggregateWorkloadMetrics(replicaMetrics)
		if wm.busyPods == 0 {
			logger.Info("No replicas with traffic to calculate capacity for variant", "variant", variantName)
			vr := errorVariantCapacity
			variantCapacities = append(variantCapacities, vr)
			continue
		}

		// get model parameters
		params := a.getParams(modelID, namespace, variantName)
		if params == nil {
			logger.Info("No parameters found for variant", "variant", variantName)
			vr := errorVariantCapacity
			variantCapacities = append(variantCapacities, vr)
			continue
		}

		// get max batch size
		maxBatchSize := int64(DefaultMaxBatchSize)
		for _, rm := range replicaMetrics {
			if rm.MaxBatchSize > 0 {
				maxBatchSize = rm.MaxBatchSize
				break
			}
		}

		// Create queue analyzer
		config := &analyzer.Configuration{
			MaxBatchSize: int(maxBatchSize),
			MaxQueueSize: DefaultMaxQueueSize,
			ServiceParms: &analyzer.ServiceParms{
				Alpha: params.Alpha,
				Beta:  params.Beta,
				Gamma: params.Gamma,
			},
		}

		requestSize := &analyzer.RequestSize{
			AvgInputTokens:  float32(wm.avgInputTokens),
			AvgOutputTokens: float32(wm.avgOutputTokens),
		}

		targetPerf := &analyzer.TargetPerf{
			TargetTTFT: sloTarget.TargetTTFT,
			TargetITL:  sloTarget.TargetITL,
		}

		queueAnalyzer, err := analyzer.NewQueueAnalyzer(config, requestSize)
		if err != nil {
			logger.Info("Failed to create queue analyzer for variant", "variant", variantName, "error", err)
			vr := errorVariantCapacity
			variantCapacities = append(variantCapacities, vr)
			continue
		}

		// find max request rate to achieve target SLOs
		var maxRequestRate float64
		if _, metrics, _, err := queueAnalyzer.Size(targetPerf); err != nil {
			logger.Info("Failed to calculate max request rate for variant", "variant", variantName, "error", err)
			vr := errorVariantCapacity
			variantCapacities = append(variantCapacities, vr)
			continue
		} else {
			maxRequestRate = float64(metrics.Throughput)
		}

		if maxRequestRate == 0 {
			logger.Info("Failed to calculate max request rate for variant", "variant", variantName)
			vr := errorVariantCapacity
			variantCapacities = append(variantCapacities, vr)
			continue
		}

		totalArrivalRate := wm.avgArrivalRate * float64(wm.busyPods)
		desiredNumReplicas := math.Ceil(totalArrivalRate / maxRequestRate)
		if desiredNumReplicas == 0 {
			desiredNumReplicas = 1
		}
		arrivalRatePerReplica := totalArrivalRate / desiredNumReplicas

		variantCapacity := domain.VariantCapacity{
			VariantName:     variantName,
			AcceleratorName: variantAccel[variantName],
			Cost:            variantCost[variantName], // TODO: multiply by numReplicas?
			Role:            variantState.Role,        // see errorVariantCapacity above
			ReplicaCount:    readyCount,
			PendingReplicas: variantState.PendingReplicas,

			PerReplicaCapacity: maxRequestRate,
			TotalCapacity:      desiredNumReplicas * maxRequestRate,
			TotalDemand:        totalArrivalRate,
			Utilization:        arrivalRatePerReplica / maxRequestRate,
		}
		variantCapacities = append(variantCapacities, variantCapacity)
	}

	return variantCapacities
}

// createTunerForVariant creates a new tuner instance for a variant.
// If parameters exist in the store, uses the stored state and covariance.
// Otherwise, attempts to guess initial state from environment metrics.
func (a *QueueingModelAnalyzer) createTunerForVariant(
	ctx context.Context,
	namespace string,
	modelID string,
	variantName string,
	env *tuner.Environment,
	filterConfig *tuner.FilterData,
) (*tuner.Tuner, error) {
	logger := ctrl.LoggerFrom(ctx)

	// Check if we have existing parameters
	existingParams := a.getParams(modelID, namespace, variantName)

	// Get base tuner config (uses user config or defaults)
	tunerConfig := tuner.CreateTunerConfigFromData(filterConfig, env)

	if existingParams != nil {
		// Restore state and covariance from previous tuning cycle
		logger.V(1).Info("Restoring tuner state from parameter store",
			"variant", variantName,
			"namespace", namespace,
			"alpha", existingParams.Alpha,
			"beta", existingParams.Beta,
			"gamma", existingParams.Gamma)

		tunerConfig.ModelData.InitState = ParamsToStateVector(
			float64(existingParams.Alpha),
			float64(existingParams.Beta),
			float64(existingParams.Gamma))

		flatCov := flattenCovariance(existingParams.Covariance)
		if flatCov != nil {
			tunerConfig.ModelData.InitCovarianceMatrix = flatCov
		}
	} else {
		// No existing parameters - attempt to guess initial state from metrics
		logger.V(1).Info("No existing parameters found, attempting to guess initial state",
			"variant", variantName,
			"namespace", namespace)

		state, err := guessInitState(env)
		if err != nil {
			logger.V(1).Info("Failed to guess initial state, using defaults",
				"variant", variantName,
				"namespace", namespace,
				"error", err)
			// tunerConfig already has default InitState, so we can proceed
		} else {
			alpha, beta, gamma := StateVectorToParams(state)
			logger.V(1).Info("Using guessed initial state",
				"variant", variantName,
				"namespace", namespace,
				"alpha", alpha,
				"beta", beta,
				"gamma", gamma)
			tunerConfig.ModelData.InitState = state
			// Update bounds based on guessed state
			tunerConfig.ModelData.MinState = tuner.GetFactoredSlice(state, tuner.DefaultMinStateFactor)
			tunerConfig.ModelData.MaxState = tuner.GetFactoredSlice(state, tuner.DefaultMaxStateFactor)
		}
	}

	// Create new tuner instance with environment
	t, err := tuner.NewTuner(tunerConfig, env)
	if err != nil {
		return nil, fmt.Errorf("failed to create tuner: %w", err)
	}

	return t, nil
}

// guessSLOFromMetrics infers SLO targets from the queueing model when no
// explicit SLO configuration is provided.
//
// The SLO is defined using the idle-latency multiplier approach: the queueing
// delay (T_iter) is allowed to inflate by a fixed multiplier k relative to the
// idle baseline α, while deterministic work components remain at their true cost.
//
// Formulas (all values in milliseconds):
//
//	TargetTTFT = k×α + (β+γ)×i_l
//	TargetITL  = k×α + β + γ×(i_l + (o_l+1)/2)
//
// This gives exact utilization correspondence: ρ = 1 - 1/k.
// k is a fixed constant (not dependent on system state) because SLOs are contracts.
//
// When learned parameters are unavailable, falls back to observed latencies
// with a headroom multiplier, capped at reasonable maximums.
func (a *QueueingModelAnalyzer) guessSLOFromMetrics(
	ctx context.Context,
	namespace string,
	modelID string,
	config *QMConfig,
	variantNames []string,
	modelReplicaMetrics []domain.ReplicaMetrics,
) *SLOTarget {
	logger := ctrl.LoggerFrom(ctx)

	// Aggregate workload characteristics
	wm := aggregateWorkloadMetrics(modelReplicaMetrics)
	if wm.busyPods == 0 {
		return nil
	}

	// Get the SLO multiplier (default if not configured)
	k := config.SLOMultiplier
	if k <= 1.0 {
		k = DefaultSLOMultiplier
	}

	// Try theory-based SLO: take the max of SLO targets over variants with learned parameters
	var SLOTargetForModel *SLOTarget
	for _, variantName := range variantNames {
		params := a.getParams(modelID, namespace, variantName)
		if params == nil || params.Alpha <= 0 || params.Beta <= 0 || params.Gamma <= 0 {
			continue
		}

		alpha := float64(params.Alpha)
		beta := float64(params.Beta)
		gamma := float64(params.Gamma)

		// T_iter at SLO utilization: k × α = α/(1-ρ) where ρ = 1-1/k
		tIterSLO := k * alpha

		// Deterministic work — NOT inflated by k
		prefillWork := (beta + gamma) * wm.avgInputTokens
		decodeWork := beta + gamma*(wm.avgInputTokens+(wm.avgOutputTokens+1.0)/2.0)

		ttftSLO := tIterSLO + prefillWork
		itlSLO := tIterSLO + decodeWork

		logger.V(1).Info("Inferred SLO from queueing model",
			"variant", variantName,
			"k", k,
			"alpha", alpha, "beta", beta, "gamma", gamma,
			"avgInputTokens", wm.avgInputTokens,
			"avgOutputTokens", wm.avgOutputTokens,
			"TargetTTFT_ms", ttftSLO,
			"TargetITL_ms", itlSLO,
		)

		SLOTargetForVariant := &SLOTarget{
			TargetTTFT: float32(ttftSLO),
			TargetITL:  float32(itlSLO),
		}

		if SLOTargetForModel == nil {
			SLOTargetForModel = SLOTargetForVariant
		} else {
			SLOTargetForModel.Max(SLOTargetForVariant)
		}
	}

	// Fallback: use observed latencies with headroom if none of the variants have
	// learned parameters (e.g. cold start / early tuning cycles)
	if SLOTargetForModel == nil {
		return fallbackSLOFromObservations(ctx, wm)
	}
	return SLOTargetForModel
}

// fallbackSLOFromObservations creates SLO targets from observed TTFT/ITL
// with a headroom multiplier and reasonable caps. Used during cold start
// before the Kalman filter has learned hardware parameters.
func fallbackSLOFromObservations(
	ctx context.Context,
	wm *workloadMetrics,
) *SLOTarget {
	if wm.avgTTFT <= 0 || wm.avgITL <= 0 {
		return nil
	}

	logger := ctrl.LoggerFrom(ctx)

	// Convert seconds → milliseconds and apply headroom
	ttft := math.Min(wm.avgTTFT*1000.0*DefaultFallbackHeadroom, DefaultMaxFallbackTTFT)
	itl := math.Min(wm.avgITL*1000.0*DefaultFallbackHeadroom, DefaultMaxFallbackITL)

	logger.V(1).Info("Using fallback SLO from observations",
		"observedTTFT_s", wm.avgTTFT,
		"observedITL_s", wm.avgITL,
		"TargetTTFT_ms", ttft,
		"TargetITL_ms", itl,
	)

	return &SLOTarget{
		TargetTTFT: float32(ttft),
		TargetITL:  float32(itl),
	}
}

// storeParametersFromResults saves tuned results to the parameter store.
func (a *QueueingModelAnalyzer) storeParametersFromResults(
	namespace, modelID, variantName string,
	results *tuner.TunedResults,
) {
	// Extract covariance matrix
	covariance := matrixToSlice2D(results.Covariance)

	params := &LearnedParameters{
		Alpha:       results.ServiceParms.Alpha,
		Beta:        results.ServiceParms.Beta,
		Gamma:       results.ServiceParms.Gamma,
		NIS:         results.NIS,
		Covariance:  covariance,
		LastUpdated: time.Now(),
	}

	a.setParams(modelID, namespace, variantName, params)
}

// buildEnvironmentsFromMetrics creates Environments for the tuner, depending on
// the setting of TuningByAggregatingPodsForVariant.
// true: single environment from aggregating all pods, representing the variant's
// current operating state.
// false: multiple environments, one per pod
// Returns error if required metrics are unavailable.
func buildEnvironmentsFromMetrics(
	variantName string,
	variantReplicaMetrics []domain.ReplicaMetrics,
) ([]*tuner.Environment, error) {
	if len(variantReplicaMetrics) == 0 {
		return nil, fmt.Errorf("no replica metrics for variant %s", variantName)
	}

	// MaxBatchSize is per-deployment (same for all replicas of a variant),
	// so we extract it once from the first replica that has it.
	maxBatchSize := int64(DefaultMaxBatchSize)
	for _, rm := range variantReplicaMetrics {
		if rm.MaxBatchSize > 0 {
			maxBatchSize = rm.MaxBatchSize
			break
		}
	}

	envs := []*tuner.Environment{}
	if TuningByAggregatingPodsForVariant {
		// create an environment by aggregating all server pods into an equivalent server
		wm := aggregateWorkloadMetrics(variantReplicaMetrics)
		if wm.busyPods == 0 {
			return nil, fmt.Errorf("no replicas with traffic for variant %s", variantName)
		}

		env := &tuner.Environment{
			Lambda:        float32(wm.avgArrivalRate * 60), // Convert reqs/sec to reqs/min for tuner
			AvgInputToks:  float32(wm.avgInputTokens),
			AvgOutputToks: float32(wm.avgOutputTokens),
			MaxBatchSize:  int(maxBatchSize),
			AvgTTFT:       float32(wm.avgTTFT * 1000.0), // convert secs to msecs for tuner
			AvgITL:        float32(wm.avgITL * 1000.0),  // convert secs to msecs for tuner
		}

		if !env.Valid() {
			return nil, fmt.Errorf("invalid environment for variant %s: %v", variantName, env)
		}
		envs = append(envs, env)
	} else {
		// create environments for server pods with valid data
		for _, rm := range variantReplicaMetrics {
			if rm.ArrivalRate <= 0 {
				continue
			}
			env := &tuner.Environment{
				Lambda:        float32(rm.ArrivalRate * 60), // Convert reqs/sec to reqs/min for tuner
				MaxBatchSize:  int(maxBatchSize),
				AvgInputToks:  float32(rm.AvgInputTokens),
				AvgOutputToks: float32(rm.AvgOutputTokens),
				AvgTTFT:       float32(rm.AvgTTFT * 1000), // Convert from microseconds to milliseconds for tuner
				AvgITL:        float32(rm.AvgITL * 1000),  // Convert from microseconds to milliseconds for tuner
			}
			if env.Valid() {
				envs = append(envs, env)
			}
		}
		if len(envs) == 0 {
			return nil, fmt.Errorf("no replicas with traffic for variant %s", variantName)
		}
	}
	return envs, nil
}

// guessInitState makes an initial guess of the state estimates based on observed metrics.
// Uses the queueing model from the paper to derive parameters alpha, beta, gamma from observed TTFT and ITL.
//
// From the queueing model:
//
//	T_p (TTFT) = T_iter + (beta + gamma) × i_l                    ... (eq 12)
//	T_g (ITL)  = T_iter + beta + gamma × (i_l + (o_l + 1)/2)     ... (eq 13)
//
// Where:
//   - alpha: baseline iteration overhead (embedded in T_iter)
//   - beta: compute time per token
//   - gamma: KV cache memory access time per token
//   - i_l: average input tokens
//   - o_l: average output tokens
func guessInitState(env *tuner.Environment) ([]float64, error) {
	// Validate environment
	if env == nil || !env.Valid() {
		return nil, errors.New("invalid environment for guessing initial state")
	}

	// Extract observed metrics
	ttft := float64(env.AvgTTFT)             // T_p in paper
	itl := float64(env.AvgITL)               // T_g in paper
	inputToks := float64(env.AvgInputToks)   // i_l in paper
	outputToks := float64(env.AvgOutputToks) // o_l in paper

	// Validate inputs
	if ttft <= 0 || itl <= 0 || inputToks <= 0 || outputToks <= 0 {
		return nil, fmt.Errorf("invalid metrics: TTFT=%.2f, ITL=%.2f, inputToks=%.2f, outputToks=%.2f",
			ttft, itl, inputToks, outputToks)
	}

	// Step 1: Estimate alpha (baseline iteration overhead) as a fraction of ITL
	// The iteration time T_iter is embedded in both TTFT and ITL observations.
	// At light-to-moderate load, T_iter is approximately alpha + small_overhead
	// We use ITL as a proxy since it includes T_iter plus minimal decode work
	alpha := tuner.BaseFactor * itl // BaseFactor ≈ 0.9

	// Step 2: From TTFT equation (eq 12), solve for (beta + gamma)
	// TTFT = T_iter + (beta + gamma) × i_l
	// Assuming T_iter ≈ α at the observed load:
	// (beta + gamma) = (TTFT - alpha) / i_l
	sumBetaGamma := (ttft - alpha) / inputToks

	if sumBetaGamma < 0 {
		return nil, fmt.Errorf("invalid derived sum(beta+gamma)=%.6f < 0, check BaseFactor or metrics", sumBetaGamma)
	}

	// Step 3: From ITL equation (eq 13), solve for the beta and gamma relationship
	// ITL = T_iter + beta + gamma × (i_l + (o_l + 1)/2)
	// Assuming T_iter is approximately alpha:
	// beta + gamma × (i_l + (o_l + 1)/2) = ITL - alpha
	//
	// Substitute beta = sumBetaGamma - gamma:
	// (sumBetaGamma - gamma) + gamma × (i_l + (o_l + 1)/2) = ITL - alpha
	// sumBetaGamma + gamma × (i_l + (o_l + 1)/2 - 1) = ITL - alpha
	//
	// Solve for gamma:
	denominator := inputToks + (outputToks+1)/2 - 1
	if denominator <= 0 {
		return nil, fmt.Errorf("invalid denominator=%.6f for gamma calculation", denominator)
	}

	gamma := ((itl - alpha) - sumBetaGamma) / denominator

	// Step 4: Solve for beta
	beta := sumBetaGamma - gamma

	// Validate results: all parameters must be positive
	if alpha <= 0 {
		return nil, fmt.Errorf("derived alpha=%.6f <= 0 (ITL=%.2f, BaseFactor=%.2f)",
			alpha, itl, tuner.BaseFactor)
	}
	if beta <= 0 {
		return nil, fmt.Errorf("derived beta=%.6f <= 0 (TTFT=%.2f, ITL=%.2f, i_l=%.2f, o_l=%.2f)",
			beta, ttft, itl, inputToks, outputToks)
	}
	if gamma <= 0 {
		return nil, fmt.Errorf("derived gamma=%.6f <= 0 (TTFT=%.2f, ITL=%.2f, i_l=%.2f, o_l=%.2f)",
			gamma, ttft, itl, inputToks, outputToks)
	}

	return ParamsToStateVector(alpha, beta, gamma), nil
}

// aggregateCapacities calculates the sum of supply and demand over all variants
func aggregateCapacities(capacities []domain.VariantCapacity) (supply, demand float64) {
	for _, c := range capacities {
		supply += c.TotalCapacity
		demand += c.TotalDemand
	}
	return
}

// groupMetricsByVariant groups replica metrics by variant name.
func groupMetricsByVariant(modelReplicaMetrics []domain.ReplicaMetrics) map[string][]domain.ReplicaMetrics {
	grouped := make(map[string][]domain.ReplicaMetrics)
	for _, replicaMetric := range modelReplicaMetrics {
		grouped[replicaMetric.VariantName] = append(grouped[replicaMetric.VariantName], replicaMetric)
	}
	return grouped
}

// getVariantNames returns the variant names, derived from replicaMetrics,
// in the order they appear in the slice
func getVariantNames(replicaMetrics []domain.ReplicaMetrics) []string {
	names := []string{}
	namesMap := make(map[string]bool)
	for _, replicaMetric := range replicaMetrics {
		variantName := replicaMetric.VariantName
		if !namesMap[variantName] {
			namesMap[variantName] = true
			names = append(names, variantName)
		}
	}
	return names
}

// workloadMetrics holds workload characteristics for a server.
type workloadMetrics struct {
	avgArrivalRate  float64 // req/sec
	avgInputTokens  float64
	avgOutputTokens float64
	avgTTFT         float64 // seconds
	avgITL          float64 // seconds
	busyPods        int
}

// aggregateWorkloadMetrics averages token sizes and latencies across replicas
// that have active traffic. Returns zero metrics if no replicas have traffic.
func aggregateWorkloadMetrics(replicaMetrics []domain.ReplicaMetrics) *workloadMetrics {
	var totalArrivalRate float64
	var totalInputToks, totalOutputToks float64
	var totalTTFT, totalITL float64

	// Aggregate per-pod traffic metrics across replicas
	// TODO: option 1: metrics weighted based on arrival rates (below)
	// TODO: option 2: metrics added across pods
	// TODO: option 3, just take any one pod (maybe the oldest?) that is a good representative of a variant load statistics.

	busyPods := 0
	for _, rm := range replicaMetrics {
		if rm.ArrivalRate <= 0 {
			continue
		}
		totalArrivalRate += rm.ArrivalRate
		totalInputToks += rm.ArrivalRate * rm.AvgInputTokens
		totalOutputToks += rm.ArrivalRate * rm.AvgOutputTokens
		totalTTFT += rm.ArrivalRate * rm.AvgTTFT
		totalITL += rm.ArrivalRate * rm.AvgITL
		busyPods++
	}

	if busyPods == 0 {
		return &workloadMetrics{}
	}

	return &workloadMetrics{
		avgArrivalRate:  totalArrivalRate / float64(busyPods),
		avgInputTokens:  totalInputToks / totalArrivalRate,
		avgOutputTokens: totalOutputToks / totalArrivalRate,
		avgTTFT:         totalTTFT / totalArrivalRate,
		avgITL:          totalITL / totalArrivalRate,
		busyPods:        busyPods,
	}
}
