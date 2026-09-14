package saturation

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// runV2AnalysisOnly runs the V2 saturation analyzer and returns the raw AnalyzerResult
// without building targets or converting to V1 types. The optimizer will handle
// target building across all models.
func (e *Engine) runV2AnalysisOnly(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []domain.ReplicaMetrics,
	config config.SaturationScalingConfig,
	variantStates []domain.VariantReplicaState,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	schedulerQueue *domain.SchedulerQueueMetrics,
	arrivalRate float64,
) (*domain.AnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)

	// 1. Pre-populate capacity store with scale target-derived params
	for _, va := range variantAutoscalings {
		key := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
		scaleTarget := scaleTargets[key]
		if scaleTarget == nil {
			logger.V(logging.DEBUG).Info("No scale target found for VA, skipping capacity store pre-population",
				"variant", va.Name, "scaleTargetKey", key)
			continue
		}
		// Get accelerator name from scale target nodeSelector/nodeAffinity or VA label
		accelerator := accelerator.GetAcceleratorNameFromScaleTarget(va, scaleTarget)
		gpuCount := scaleTarget.GetTotalGPUsPerReplica()
		e.capacityStore.LoadFromScaleTarget(namespace, modelID, va.Name, accelerator, gpuCount, scaleTarget)
		logger.V(logging.DEBUG).Info("Pre-populated capacity store from scale target",
			"variant", va.Name, "accelerator", accelerator, "gpuCount", gpuCount)
	}

	// 2. Build AnalyzerInput
	input := domain.AnalyzerInput{
		ModelID:        modelID,
		Namespace:      namespace,
		ReplicaMetrics: replicaMetrics,
		VariantStates:  variantStates,
		Config:         &config,
		SchedulerQueue: schedulerQueue,
		ArrivalRate:    arrivalRate,
	}

	// 3. Run V2 analyzer
	result, err := e.saturationV2Analyzer.Analyze(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("V2 saturation analysis failed: %w", err)
	}

	// Analysis results are logged by the caller (runAnalyzersAndScore) after the
	// applyUniversalThreshold post-step, so the single Info line can include the
	// real RequiredCapacity/SpareCapacity. They are left zero here, so logging
	// them at this point would always report 0 and be misleading.
	return result, nil
}

// analyzerLivenessStaleCycles is the number of optimization cycles an
// analyzer's last informative result may age before it is treated as stale
// (non-live) for the scale-down veto gate. Fixed for now; revisit as a
// per-deployment config field if operators need to tune it.
// TODO: make configurable if needed.
const analyzerLivenessStaleCycles = 3

// runAnalyzersAndScore runs the V2 saturation analyzer, applies the universal
// threshold post-step to every analyzer's result (using per-analyzer config
// overrides where set), and computes the weighted composite score from
// saturation's signal and the model's priority.
//
// The engine applies applyUniversalThreshold to every analyzer (saturation and
// all registered non-saturation analyzers) and collects the calibrated results
// into a per-analyzer slice returned to the optimizer. Each entry is tagged with
// its enablement (Enabled — whether it votes in the combine RC/SC math this
// cycle) and liveness (Live). Order is not significant: the anchor is derived on
// demand from the ballot (bindingAnchor), and combine math consumes only the
// voting subset (votingResults). Saturation is always appended as the
// identity/(a) carrier — it supplies per-variant metadata (Cost, AcceleratorName,
// Role) for every configured variant — but it votes only when enabled.
func (e *Engine) runAnalyzersAndScore(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []domain.ReplicaMetrics,
	config config.SaturationScalingConfig,
	variantStates []domain.VariantReplicaState,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	schedulerQueue *domain.SchedulerQueueMetrics,
	arrivalRate float64,
) ([]pipeline.NamedAnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)

	// Run saturation analyzer (always needed for PerReplicaCapacity).
	baseResult, err := e.runV2AnalysisOnly(ctx, modelID, namespace, replicaMetrics, config,
		variantStates, scaleTargets, variantAutoscalings, schedulerQueue, arrivalRate)
	if err != nil {
		return nil, err
	}

	// Universal threshold post-step for saturation: recalibrate RC/SC using the
	// resolved threshold for the saturation entry (per-analyzer override over global).
	satUp, satDown := resolveThresholds(domain.SaturationAnalyzerName, config)
	applyUniversalThreshold(baseResult, satUp, satDown)

	// Build AnalyzerInput once; shared by all non-saturation analyzers.
	// Note: &config has had saturation's per-entry threshold overrides applied
	// (the loop above). Non-saturation analyzers therefore receive the
	// saturation-adjusted config rather than the original. This is harmless
	// on this branch (their results are discarded), and the clean fix —
	// engine applies thresholds universally after each analyzer runs —
	// is tracked on multi-analyzer-threshold (PR #1228).
	input := domain.AnalyzerInput{
		ModelID:        modelID,
		Namespace:      namespace,
		ReplicaMetrics: replicaMetrics,
		VariantStates:  variantStates,
		Config:         &config,
		SchedulerQueue: schedulerQueue,
		ArrivalRate:    arrivalRate,
	}

	// Whether saturation votes in the combine (RC/SC) math this cycle. It always
	// votes in the default single-analyzer config (no explicit analyzer list) and
	// otherwise votes only when its name is enabled. When it does not vote it is
	// still appended below as the identity/(a) carrier — present in the ballot,
	// but pruned from the voting subset by votingResults.
	satVotes := len(config.Analyzers) == 0 || effectiveEnabled(domain.SaturationAnalyzerName, config)

	// An explicit analyzer list that omits saturation is a supported configuration,
	// but it is also a real change in safety posture and easy to arrive at without
	// meaning to. Saturation stops contributing to the combine, so it can no longer
	// veto a scale-down: a demand-driven analyzer reading zero demand (EPP not
	// scraped for a window, say) will shed replicas with nothing holding it back,
	// even while saturation itself sees KV cache near capacity. Say so plainly
	// rather than leaving it to the docs.
	if !satVotes {
		logger.Info("saturation analyzer is absent from the configured analyzer list: it will not vote and cannot veto scale-down for this model",
			"modelID", modelID,
			"namespace", namespace,
		)
	}

	// Collect per-analyzer results. Each entry carries its Enabled tag; order is
	// not significant. Saturation is appended first purely as a code artifact (it
	// is the (a) carrier), tagged Enabled: satVotes; each enabled non-saturation
	// analyzer is run, calibrated with its resolved thresholds, and appended
	// tagged Enabled: true.
	namedResults := []pipeline.NamedAnalyzerResult{{
		Name:              domain.SaturationAnalyzerName,
		Result:            baseResult,
		Score:             scoreForAnalyzer(domain.SaturationAnalyzerName, config),
		Remaining:         baseResult.RequiredCapacity,
		Spare:             baseResult.SpareCapacity,
		ScaleUpThreshold:  satUp,
		ScaleDownBoundary: satDown,
		Enabled:           satVotes,
	}}
	for _, entry := range e.analyzersSnapshot {
		if entry.name == domain.SaturationAnalyzerName {
			// Reuse guard, not a decision gate: saturation is already appended
			// above (as the (a) carrier). Its vote is decided by satVotes there,
			// not by skipping it here.
			continue
		}
		if !effectiveEnabled(entry.name, config) {
			continue
		}
		result := runRegisteredAnalyzer(ctx, logger, entry, modelID, input)
		if result == nil {
			continue
		}
		up, down := resolveThresholds(entry.name, config)
		applyUniversalThreshold(result, up, down)
		namedResults = append(namedResults, pipeline.NamedAnalyzerResult{
			Name:              entry.name,
			Result:            result,
			Score:             scoreForAnalyzer(entry.name, config),
			Remaining:         result.RequiredCapacity,
			Spare:             result.SpareCapacity,
			ScaleUpThreshold:  up,
			ScaleDownBoundary: down,
			Enabled:           true,
		})
	}
	e.updateLivenessAndSetLive(ctx, namespace, modelID, namedResults)

	for _, nr := range namedResults {
		logAnalyzerResult(ctx, modelID, namespace, nr)
	}
	return namedResults, nil
}

// updateLivenessAndSetLive refreshes e.lastGoodAnalysis for this model with
// every informative result's AnalyzedAt (or the current time, as a fail-safe,
// when an informative result carries a zero-valued AnalyzedAt), then sets
// nr.Live on each entry in place based on whether its last good analysis (if
// any) is within the staleness window. Applies uniformly to every analyzer,
// including saturation — no name-based exemption. After the liveness pass it
// runs the warn-only demand-liveness detector (see detectDemandLiveness).
func (e *Engine) updateLivenessAndSetLive(
	ctx context.Context,
	namespace, modelID string,
	namedResults []pipeline.NamedAnalyzerResult,
) {
	if e.lastGoodAnalysis == nil {
		e.lastGoodAnalysis = make(map[string]map[string]time.Time)
	}
	modelKey := utils.GetNamespacedKey(namespace, modelID)
	if e.lastGoodAnalysis[modelKey] == nil {
		e.lastGoodAnalysis[modelKey] = make(map[string]time.Time)
	}
	perAnalyzer := e.lastGoodAnalysis[modelKey]

	// Config is nil in unit tests that construct a minimal Engine directly
	// (bypassing NewEngine, which panics on a nil Config in production). A present
	// Config returning a non-positive interval falls back to the same 30s default —
	// a zero/negative interval would otherwise zero the threshold below and latch
	// every analyzer non-live, blocking all scale-down.
	interval := 30 * time.Second
	if e.Config != nil && e.Config.OptimizationInterval() > 0 {
		interval = e.Config.OptimizationInterval()
	}
	now := time.Now()
	threshold := analyzerLivenessStaleCycles * interval

	for i := range namedResults {
		nr := &namedResults[i]
		if pipeline.ResultIsInformative(*nr) {
			at := nr.Result.AnalyzedAt
			if at.IsZero() {
				// Fail-safe: an informative result observed this cycle is current by
				// definition, so a forgotten AnalyzedAt cannot silently disarm the veto.
				at = now
			}
			perAnalyzer[nr.Name] = at
		}
		lastGood, ok := perAnalyzer[nr.Name]
		nr.Live = ok && now.Sub(lastGood) <= threshold
	}

	e.detectDemandLiveness(ctx, modelID, namespace, namedResults, perAnalyzer, now, threshold)
}

// demandLatchSuffix is appended to an analyzer name to form the synthetic inner
// key under which a demand latch is stored in lastGoodAnalysis. The NUL byte
// guarantees the key cannot collide with any real analyzer name (analyzer names
// are printable identifiers), so the Live/veto path — which only reads the map
// via keyed lookups on real analyzer names (perAnalyzer[nr.Name]) and never
// ranges over it in the decision path — can never observe this key and can
// never have an nr.Live flipped by it.
//
// DEFERRED (future, per-pod demand): when demand becomes per-replica, the demand
// latch key extends with a pod component (analyzerName + demandLatchSuffix +
// "\x00" + podID) and a per-replica latch is tracked alongside the model-level
// one. Not built now — 0.9 demand is model-level only.
const demandLatchSuffix = "\x00demand"

// detectDemandLiveness emits a warn-only observability signal when the
// throughput analyzer has a live capacity/supply signal but has reported no
// demand (Result.TotalDemand > 0) for at least the staleness window. It never
// sets nr.Live, never touches RoleSpare, and never gates any decision.
//
// Why warn-only, and why a veto here would be wrong:
//   - Zero demand is a legitimate state, not necessarily a fault. With no
//     served-rate floor, arrival→0 correctly drives TotalDemand→0, which only
//     permits scale-down and never forces a scale action. A missing or zero
//     arrival signal can therefore never cause a spurious scale-up or a spurious
//     veto, so vetoing on "demand looks absent" would defeat the very scale-down
//     the liveness gate exists to enable.
//   - The veto path is already covered by the supply/capacity liveness gate: an
//     analyzer with no capacity signal is excluded from the scale-down vote via
//     nr.Live. This detector is strictly additive telemetry pointing a human at
//     a likely-broken arrival query.
//
// The detector pairs two latches in the same per-model map:
//   - Supply latch: perAnalyzer[throughput.AnalyzerName], the analyzer's
//     last-informative timestamp (maintained by the liveness loop above; the
//     throughput analyzer resolves per-replica capacity regardless of arrival).
//   - Demand latch: perAnalyzer[throughput.AnalyzerName+demandLatchSuffix],
//     stamped with AnalyzedAt each cycle TotalDemand > 0.
//
// The signal is a timestamp gap, not a boolean, so a cold-start EPP scrape lag
// (supply comes up a cycle or two before the first arrival scrape) does not
// false-positive: on the first cycle the demand latch is seeded to the current
// supply timestamp, so the gap starts at zero and only reaches the staleness
// window after demand has genuinely been absent for that long.
func (e *Engine) detectDemandLiveness(
	ctx context.Context,
	modelID, namespace string,
	namedResults []pipeline.NamedAnalyzerResult,
	perAnalyzer map[string]time.Time,
	now time.Time,
	threshold time.Duration,
) {
	var tp *pipeline.NamedAnalyzerResult
	for i := range namedResults {
		if namedResults[i].Name == throughput.AnalyzerName {
			tp = &namedResults[i]
			break
		}
	}
	if tp == nil {
		return // throughput not participating this cycle
	}

	demandKey := throughput.AnalyzerName + demandLatchSuffix

	// Stamp the demand latch whenever demand is observed.
	if tp.Result != nil && tp.Result.TotalDemand > 0 {
		perAnalyzer[demandKey] = tp.Result.AnalyzedAt
	}

	// Only meaningful while supply is live now; otherwise the supply liveness
	// gate already covers the situation and there is nothing to seed or compare.
	supplyTS, hasSupply := perAnalyzer[throughput.AnalyzerName]
	if !hasSupply || now.Sub(supplyTS) > threshold {
		return
	}

	// Seed the demand latch to the current supply timestamp the first time we
	// see live supply, so a fresh model / cold start starts with a zero gap.
	demandTS, hasDemand := perAnalyzer[demandKey]
	if !hasDemand {
		perAnalyzer[demandKey] = supplyTS
		return
	}

	if supplyTS.Sub(demandTS) >= threshold {
		logger := ctrl.LoggerFrom(ctx)
		logger.Info("throughput analyzer has a live capacity signal but has reported no demand "+
			"for at least the staleness window; the request-arrival query is likely misconfigured "+
			"or EPP is not reporting arrivals — scale-up will not trigger until arrivals are observed",
			"modelID", modelID,
			"namespace", namespace,
			"analyzer", throughput.AnalyzerName,
			"noDemandFor", supplyTS.Sub(demandTS).String(),
		)
	}
}

// pruneLastGoodAnalysis evicts liveness state for models that are no longer
// active. activeKeys is the set of currently-active model keys (as produced by
// utils.GetNamespacedKey(namespace, modelID)); any outer key in
// e.lastGoodAnalysis absent from activeKeys belongs to a departed model (its
// VariantAutoscalings are gone) and is deleted, bounding the map to live models
// rather than letting it grow unboundedly across the controller's lifetime.
//
// Guards against an empty active set: a cycle that enumerates no models (e.g.
// saturation config not loaded yet) must not wipe accumulated state, so pruning
// is skipped when activeKeys is empty. This also means a genuine all-models-removed
// state is indistinguishable from a transient empty cycle here: those entries leak
// until a model reappears. Accepted, bounded leak — distinguishing the two cases
// would need a separate "models were present last cycle" signal.
func (e *Engine) pruneLastGoodAnalysis(activeKeys map[string]bool) {
	if len(activeKeys) == 0 || e.lastGoodAnalysis == nil {
		return
	}
	for modelKey := range e.lastGoodAnalysis {
		if !activeKeys[modelKey] {
			delete(e.lastGoodAnalysis, modelKey)
		}
	}
}

// scoreForAnalyzer returns the AnalyzerScoreConfig.Score for the named analyzer,
// defaulting to 1.0 when the analyzer has no explicit entry in cfg.Analyzers.
// This value is the per-analyzer weight used by GreedyByScoreOptimizer for
// fair-share priority ordering across models.
func scoreForAnalyzer(analyzerName string, cfg config.SaturationScalingConfig) float64 {
	for _, aw := range cfg.Analyzers {
		if aw.EffectiveType() == analyzerName {
			if aw.Score > 0 {
				return aw.Score
			}
			return 1.0
		}
	}
	return 1.0
}

func resolveThresholds(analyzerName string, cfg config.SaturationScalingConfig) (scaleUp, scaleDown float64) {
	for _, aw := range cfg.Analyzers {
		if aw.EffectiveType() == analyzerName {
			return aw.EffectiveScaleUpThreshold(cfg.ScaleUpThreshold),
				aw.EffectiveScaleDownBoundary(cfg.ScaleDownBoundary)
		}
	}
	return cfg.ScaleUpThreshold, cfg.ScaleDownBoundary
}

// effectiveEnabled reports whether the named analyzer should participate in this
// cycle's scaling decision. An analyzer is opt-in: it participates only when it has
// an explicit entry in cfg.Analyzers whose Enabled is true (or nil, i.e. present but
// not yet defaulted). An analyzer registered in code but ABSENT from cfg.Analyzers
// does NOT participate — this prevents a registered-but-unconfigured analyzer (e.g.
// throughput) from returning SpareCapacity=0 and silently vetoing scale-down.
// Saturation is handled separately: it is always appended as the identity/(a)
// carrier, and whether it votes is decided by satVotes, which consults this
// function for the saturation name. The per-analyzer loop skips the saturation
// name as a reuse guard (so it is not appended twice), not as a decision gate.
func effectiveEnabled(analyzerName string, cfg config.SaturationScalingConfig) bool {
	for _, aw := range cfg.Analyzers {
		if aw.EffectiveType() == analyzerName {
			if aw.Enabled != nil {
				return *aw.Enabled
			}
			return true // present, not yet defaulted → participates
		}
	}
	return false // absent → opt-in: does not participate
}

// runRegisteredAnalyzer invokes a single non-saturation analyzer's Analyze
// method, isolating the call from the rest of the cycle. Errors are logged
// and nil is returned; panics are recovered and logged. Returns the result
// so the caller can apply the universal threshold post-step before discarding.
func runRegisteredAnalyzer(
	ctx context.Context,
	logger logr.Logger,
	entry analyzerEntry,
	modelID string,
	input domain.AnalyzerInput,
) (result *domain.AnalyzerResult) {
	defer func() {
		if r := recover(); r != nil {
			// Plugin failure is non-fatal; log at debug to avoid spamming
			// operator logs every optimize cycle.
			logger.V(logging.DEBUG).Info("registered analyzer panicked; result discarded",
				"name", entry.name, "modelID", modelID, "panic", fmt.Sprintf("%v", r))
			result = nil
		}
	}()
	var err error
	result, err = entry.analyzer.Analyze(ctx, input)
	if err != nil {
		// Plugin failure is non-fatal; log at debug to avoid spamming
		// operator logs every optimize cycle.
		logger.V(logging.DEBUG).Info("registered analyzer failed; result discarded",
			"name", entry.name, "modelID", modelID, "error", err)
		return nil
	}
	return result
}

// applyUniversalThreshold recalibrates RequiredCapacity and SpareCapacity in
// place for the analyzer result and every RoleCapacity entry using the pure
// formula:
//
//	RC = max(0, TotalDemand/scaleUp − TotalAnticipatedSupply)
//	SC = max(0, TotalSupply    − TotalDemand/scaleDown)
//
// TotalAnticipatedSupply is read as-is — zero is a literal value, not a
// sentinel. The analyzer is responsible for populating it correctly (see
// internal/engines/aggregation for shared helpers). The asymmetry preserves
// the conservative "don't double-scale while replicas are launching, don't
// count pending as removable" stance.
//
// The same formula and the same (scaleUp, scaleDown) are applied at every
// scope — model-level and each RoleCapacity entry. There are no per-role
// threshold overrides. A non-positive scaleUp or scaleDown leaves the
// corresponding signal unchanged.
func applyUniversalThreshold(r *domain.AnalyzerResult, scaleUp, scaleDown float64) {
	if r == nil {
		return
	}

	if scaleUp > 0 {
		rc := r.TotalDemand/scaleUp - r.TotalAnticipatedSupply
		if rc < 0 {
			rc = 0
		}
		r.RequiredCapacity = rc
	}
	if scaleDown > 0 {
		sc := r.TotalSupply - r.TotalDemand/scaleDown
		if sc < 0 {
			sc = 0
		}
		r.SpareCapacity = sc
	}

	for role, rc := range r.RoleCapacities {
		if scaleUp > 0 {
			v := rc.TotalDemand/scaleUp - rc.TotalAnticipatedSupply
			if v < 0 {
				v = 0
			}
			rc.RequiredCapacity = v
		}
		if scaleDown > 0 {
			v := rc.TotalSupply - rc.TotalDemand/scaleDown
			if v < 0 {
				v = 0
			}
			rc.SpareCapacity = v
		}
		r.RoleCapacities[role] = rc
	}
}

// computeCurrentGPUUsage iterates over model scaling requests to compute the
// current GPU usage per accelerator type. Used to provide current usage to
// the ConstraintProvider when building GPU constraints for the optimizer.
func computeCurrentGPUUsage(requests []pipeline.ModelScalingRequest) map[string]int {
	usage := make(map[string]int)
	for _, req := range requests {
		// The saturation result is the per-variant identity carrier (VariantName,
		// AcceleratorName, topology) and is present in every ballot even when
		// saturation does not vote. Current physical GPU usage is a topology lookup
		// (CurrentReplicas × GPUs-per-replica), not a voting or ordering decision, so
		// it reads that carrier directly by name.
		var satCarrier *domain.AnalyzerResult
		for _, e := range req.AnalyzerResults {
			if e.Name == domain.SaturationAnalyzerName {
				satCarrier = e.Result
				break
			}
		}
		if satCarrier == nil {
			continue
		}
		stateMap := make(map[string]domain.VariantReplicaState, len(req.VariantStates))
		for _, s := range req.VariantStates {
			stateMap[s.VariantName] = s
		}
		for _, vc := range satCarrier.VariantCapacities {
			state := stateMap[vc.VariantName]
			gpusPerReplica := state.GPUsPerReplica
			if gpusPerReplica <= 0 {
				gpusPerReplica = 1
			}
			usage[vc.AcceleratorName] += state.CurrentReplicas * gpusPerReplica
		}
	}
	return usage
}

// computeCurrentGPUUsageByNamespace mirrors computeCurrentGPUUsage but buckets
// usage by namespace, then accelerator type. Every request's namespace is
// represented (with at least an empty per-type map) so namespaces carrying a
// quota but zero current usage are still surfaced as active namespaces to the
// constraint providers (and therefore still constrained).
func computeCurrentGPUUsageByNamespace(requests []pipeline.ModelScalingRequest) map[string]map[string]int {
	usage := make(map[string]map[string]int)
	for _, req := range requests {
		perType, ok := usage[req.Namespace]
		if !ok {
			perType = make(map[string]int)
			usage[req.Namespace] = perType
		}
		// Topology lookup of the always-present saturation identity carrier (see
		// computeCurrentGPUUsage); not a voting or ordering decision.
		var satCarrier *domain.AnalyzerResult
		for _, e := range req.AnalyzerResults {
			if e.Name == domain.SaturationAnalyzerName {
				satCarrier = e.Result
				break
			}
		}
		if satCarrier == nil {
			continue
		}
		stateMap := make(map[string]domain.VariantReplicaState, len(req.VariantStates))
		for _, s := range req.VariantStates {
			stateMap[s.VariantName] = s
		}
		for _, vc := range satCarrier.VariantCapacities {
			state := stateMap[vc.VariantName]
			gpusPerReplica := state.GPUsPerReplica
			if gpusPerReplica <= 0 {
				gpusPerReplica = 1
			}
			perType[vc.AcceleratorName] += state.CurrentReplicas * gpusPerReplica
		}
	}
	return usage
}

// gpuConstraintProviders returns the ConstraintProvider(s) backing the GPU
// limiter for the V2 optimizer path. A limiter that is itself a
// ConstraintProvider (a *DefaultLimiter) contributes itself; a CompositeLimiter
// contributes each constituent that is a ConstraintProvider, so multi-entry
// quota configs are all consulted. Other limiter shapes (e.g. NoOpLimiter)
// contribute nothing.
func gpuConstraintProviders(l pipeline.Limiter) []pipeline.ConstraintProvider {
	switch lim := l.(type) {
	case pipeline.ConstraintProvider:
		return []pipeline.ConstraintProvider{lim}
	case *pipeline.CompositeLimiter:
		var providers []pipeline.ConstraintProvider
		for _, c := range lim.Constituents() {
			if cp, ok := c.(pipeline.ConstraintProvider); ok {
				providers = append(providers, cp)
			}
		}
		return providers
	}
	return nil
}

// collectV2ModelRequest performs V2 analysis for a single model and returns
// a ModelScalingRequest for the optimizer, or nil if analysis should be skipped.
func (e *Engine) collectV2ModelRequest(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []domain.ReplicaMetrics,
	config config.SaturationScalingConfig,
	variantStates []domain.VariantReplicaState,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	schedulerQueue *domain.SchedulerQueueMetrics,
	arrivalRate float64,
) (*pipeline.ModelScalingRequest, error) {
	namedResults, err := e.runAnalyzersAndScore(ctx, modelID, namespace, replicaMetrics, config,
		variantStates, scaleTargets, variantAutoscalings, schedulerQueue, arrivalRate)
	if err != nil {
		return nil, fmt.Errorf("collecting V2 model request for %s/%s: %w", namespace, modelID, err)
	}

	// Detect P/D disaggregation: true when any variant has role != domain.RoleBoth
	disaggregated := false
	for _, vs := range variantStates {
		if vs.Role != "" && vs.Role != domain.RoleBoth {
			disaggregated = true
			break
		}
	}

	return &pipeline.ModelScalingRequest{
		ModelID:         modelID,
		Namespace:       namespace,
		AnalyzerResults: namedResults,
		VariantStates:   variantStates,
		Priority:        config.Priority,
		Disaggregated:   disaggregated,
	}, nil
}

// logAnalyzerResult emits one INFO "analyzer-result" line for a single named
// analyzer result. Called for every analyzer that ran in a model's reconcile
// cycle, after the universal threshold post-step has been applied.
func logAnalyzerResult(ctx context.Context, modelID, namespace string, nr pipeline.NamedAnalyzerResult) {
	if nr.Result == nil {
		return
	}
	logger := ctrl.LoggerFrom(ctx)

	type variantEntry struct {
		Name string  `json:"name"`
		PRC  float64 `json:"prc"`
		// Role is included because V2 charges waiting requests by P/D role, so
		// demand is not interpretable without knowing which role was resolved —
		// a missing llm-d.ai/role label reads as "both" and changes the charge.
		// Emitted unconditionally: an analyzer that leaves Role unset (the
		// queueing model does) is treated as "both" downstream, so the value is
		// canonicalized below and the field always carries a meaningful role
		// rather than being absent.
		Role   string `json:"role"`
		Reason string `json:"reason,omitempty"`
	}
	variants := make([]variantEntry, 0, len(nr.Result.VariantCapacities))
	for _, vc := range nr.Result.VariantCapacities {
		role := vc.Role
		if role == "" {
			role = domain.RoleBoth
		}
		variants = append(variants, variantEntry{
			Name:   vc.VariantName,
			PRC:    vc.PerReplicaCapacity,
			Role:   role,
			Reason: vc.Reason,
		})
	}

	logger.Info("analyzer-result",
		"modelID", modelID,
		"namespace", namespace,
		"analyzer", nr.Name,
		"supply", nr.Result.TotalSupply,
		"demand", nr.Result.TotalDemand,
		"util", nr.Result.Utilization,
		"rc", nr.Result.RequiredCapacity,
		"sc", nr.Result.SpareCapacity,
		"scaleUpThreshold", nr.ScaleUpThreshold,
		"scaleDownBoundary", nr.ScaleDownBoundary,
		"variants", variants,
	)
}

// logScalingDecisions emits one INFO "scaling-decision" line per model after
// the optimizer has produced per-variant decisions.
func logScalingDecisions(
	ctx context.Context,
	modelRequests []pipeline.ModelScalingRequest,
	decisions []domain.VariantDecision,
) {
	logger := ctrl.LoggerFrom(ctx)

	type modelKey struct{ ns, modelID string }
	type decisionEntry struct {
		Name   string `json:"name"`
		Curr   int    `json:"curr"`
		Tgt    int    `json:"tgt"`
		Action string `json:"action"`
	}

	grouped := make(map[modelKey][]decisionEntry, len(modelRequests))
	for _, d := range decisions {
		k := modelKey{d.Namespace, d.ModelID}
		grouped[k] = append(grouped[k], decisionEntry{
			Name:   d.VariantName,
			Curr:   d.CurrentReplicas,
			Tgt:    d.TargetReplicas,
			Action: string(d.Action),
		})
	}

	for _, req := range modelRequests {
		k := modelKey{req.Namespace, req.ModelID}
		entries := grouped[k]
		if len(entries) == 0 {
			continue
		}
		logger.Info("scaling-decision",
			"modelID", req.ModelID,
			"namespace", req.Namespace,
			"decisions", entries,
		)
	}
}
