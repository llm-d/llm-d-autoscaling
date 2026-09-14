package throughput

import (
	"context"
	"math"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// ThroughputAnalyzer accumulates per-variant workload shape and ITL observations
// across reconcile cycles and computes a μ_dec supply vs λ_dec demand scaling signal.
// It implements domain.Analyzer.
//
// State is tracked per variant (keyed by "namespace|modelID|variantName") because
// different variants may run on different hardware with different ITL coefficients,
// and all replicas of the same variant are expected to share OL, IL, and KV_max.
type ThroughputAnalyzer struct {
	mu            sync.Mutex
	variantStates map[string]*variantState
}

// variantState holds the cross-cycle calibration state for a single variant.
type variantState struct {
	shapeTracker      *ShapeTracker
	observationWindow *ObservationWindow
	// role is the P/D disaggregation role ("prefill", "decode", "both", "").
	// Updated from VariantStates at the start of each Analyze call.
	role             string
	lastSanityReport SanityReport
	lastObservedAt   time.Time
	// lastFittedB is the B coefficient from the most recent successful Tier-1 OLS fit.
	// It is used as the pinned baseline in Tier-2 instead of DefaultBaselineITLSec,
	// because B reflects hardware/model characteristics rather than workload shape.
	// A shape change clears the observation window but must NOT clear lastFittedB.
	lastFittedB float64
	hasFittedB  bool
	// consecutiveGPSMismatches counts how many consecutive Analyze cycles have
	// produced a GPS mismatch for this variant. The observation window is cleared
	// when this reaches DefaultGPSMismatchClearThreshold. Always reset alongside
	// observationWindow.Clear() so it is bound to the current window's lifetime.
	consecutiveGPSMismatches int
	// set by Analyze() for VariantState() snapshots
	lastITLModel         ITLModel
	lastPerReplicaSupply float64
	lastTotalSupply      float64
	lastDemand           float64
}

// NewThroughputAnalyzer creates a ThroughputAnalyzer with default configuration.
func NewThroughputAnalyzer() *ThroughputAnalyzer {
	return &ThroughputAnalyzer{
		variantStates: make(map[string]*variantState),
	}
}

// Name returns the canonical name for this analyzer.
func (a *ThroughputAnalyzer) Name() string {
	return AnalyzerName
}

// Observe processes one reconcile cycle for a model. It groups metrics by
// VariantName and, for each variant:
//  1. Runs sanity checks; skips the variant if any issue is found.
//  2. Computes the variant-average IL, OL, and prefix hit rate.
//  3. Updates the shape tracker; clears the observation window on shape change.
//  4. Adds one (k, ITL) observation per replica to the window.
//  5. Prunes observations older than DefaultObservationMaxAge.
//
// Returns a map of variantName → SanityReport for logging. An empty SanityReport
// (report.OK() == true) means that variant's metrics were healthy this cycle.
func (a *ThroughputAnalyzer) Observe(
	ctx context.Context,
	now time.Time,
	modelID, namespace string,
	metrics []domain.ReplicaMetrics,
) map[string]SanityReport {
	if err := ctx.Err(); err != nil {
		return nil
	}
	byVariant := groupByVariant(metrics)
	reports := make(map[string]SanityReport, len(byVariant))

	a.mu.Lock()
	defer a.mu.Unlock()

	for variantName, variantMetrics := range byVariant {
		report := CheckModelMetrics(variantMetrics)
		reports[variantName] = report

		key := variantKey(namespace, modelID, variantName)
		state := a.getOrCreateVariantState(key)
		state.lastSanityReport = report
		state.lastObservedAt = now

		if report.Has(SanityIssueNoReplicas) {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: no replicas, skipping variant",
				"namespace", namespace,
				"modelID", modelID,
				"variant", variantName,
			)
			continue
		}
		if !report.OK() {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: sanity issues detected, some pods excluded",
				"namespace", namespace,
				"modelID", modelID,
				"variant", variantName,
				"issues", report.Issues,
				"affectedPods", report.AffectedPods,
			)
		}

		// Only healthy pods contribute to shape averaging and window observations.
		// Pods with per-replica issues (cold start, stale metrics, missing KV) are
		// excluded so one bad replica cannot block the entire variant.
		healthyMetrics := filterHealthyForShape(variantMetrics)
		if len(healthyMetrics) == 0 {
			continue
		}

		// Compute variant-average shape metrics. All replicas of the same variant
		// are expected to have the same OL and IL (same model, same config); the
		// mean handles any minor per-pod variation.
		il, ol, hitRate := averageShapeMetrics(healthyMetrics)

		shape, changed := state.shapeTracker.Observe(il, ol, hitRate)
		if changed {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: workload shape changed, clearing observation window",
				"namespace", namespace,
				"modelID", modelID,
				"variant", variantName,
				"newKVreq", shape.KVreq,
			)
			state.observationWindow.Clear()
			state.consecutiveGPSMismatches = 0
		}

		// Collect one (k*, ITL) observation per healthy replica. Per-replica variation
		// in k* provides the k-spread needed for a reliable OLS fit.
		for _, m := range healthyMetrics {
			if dropped := state.observationWindow.Add(m.KvUsageInstant, m.AvgITL, now); dropped {
				ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: observation dropped (k out of range or ITL invalid)",
					"namespace", namespace, "modelID", modelID, "variant", variantName,
					"k", m.KvUsageInstant, "itl", m.AvgITL)
			}
		}
		state.observationWindow.Prune(now)
	}

	// Evict variant states not observed for longer than twice the observation
	// max age. Prevents stale entries from deleted/recreated VAs from
	// accumulating in memory and causing false shape-change signals on recreate.
	for key, state := range a.variantStates {
		if now.Sub(state.lastObservedAt) > 2*DefaultObservationMaxAge {
			delete(a.variantStates, key)
		}
	}

	return reports
}

// Analyze implements domain.Analyzer. It calls Observe to update internal
// state, then computes a supply vs demand scaling signal for each variant using
// a two-tier ITL model resolution strategy:
//
//   - Tier 1 (OLS): observation window Ready — fit ITL(k) = A·k + B via OLS.
//   - Tier 2 (constrained OLS): window not ready — fit A with B = DefaultBaselineITLSec
//     using all replica (k*, ITL_obs) points: A = Σ((ITL_i−B)·k_i) / Σ(k_i²).
//
// Model-level decode demand is Λ_req × avgOL: the
// model-level arrival rate (input.ArrivalRate, a single sum(rate(...)) query
// with no per-pod labels — an all-or-nothing model-level signal) times avgOL, the RequestRate-weighted
// average output length across all healthy replicas of the model. This is the
// sole driver of TotalDemand's arrival component; it does not depend on
// per-variant EPP attribution.
//
// Per-variant demand (populating VariantCapacity.TotalDemand/Utilization, for
// introspection/VariantState() only — not summed into TotalDemand) is still
// estimated in priority order:
//  1. EPP primary: Σ ArrivalRate × AvgOutputTokens (when ArrivalRate > 0 on any replica).
//  2. Engine-rate fallback: RequestRate × avgOL (when EPP absent but the engine completion rate is nonzero).
//  3. k*-based local: Σ k_r* × KV_max_r / KVreq / ITL(k_r*) (scale-up only; no EPP needed).
//
// Scheduler queue demand (QueueSize / (DefaultQueueDrainFactor × ITL(k_sat))) is added
// to model-level demand after all variants are processed (non-prefill roles only).
//
// TA publishes TotalSupply, TotalAnticipatedSupply, and TotalDemand on the
// returned AnalyzerResult; RequiredCapacity and SpareCapacity are left zero.
// The engine's universal threshold post-step writes RC/SC after Analyze returns.
// PendingReplicas are included in TotalAnticipatedSupply to suppress redundant
// scale-up while pods are starting. Both the arrival decode term and scheduler
// queue demand are split across non-prefill roles via distributeDemandByRole.
//
// For P/D disaggregated models, RoleCapacities carries per-role Total* fields
// (TotalSupply, TotalAnticipatedSupply, TotalDemand); RC/SC per role are also
// left zero for the engine post-step. Prefill TotalDemand is negligible after
// the OL guard in computeLocalDemand.
func (a *ThroughputAnalyzer) Analyze(
	ctx context.Context,
	input domain.AnalyzerInput,
) (*domain.AnalyzerResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	now := time.Now()

	// Build lookup tables from VariantStates before taking any locks.
	pendingByVariant := make(map[string]int, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		pendingByVariant[vs.VariantName] = vs.PendingReplicas
	}

	// Analyze is assumed single-flight per model; concurrent VariantState() snapshots
	// may observe partial state across the lock gaps below.
	// Update variant roles so state.role is current when Observe() runs.
	a.mu.Lock()
	for _, vs := range input.VariantStates {
		key := variantKey(input.Namespace, input.ModelID, vs.VariantName)
		state := a.getOrCreateVariantState(key)
		state.role = vs.Role
	}
	a.mu.Unlock()

	// Observe updates internal state (acquires/releases a.mu internally).
	a.Observe(ctx, now, input.ModelID, input.Namespace, input.ReplicaMetrics)

	byVariant := groupByVariant(input.ReplicaMetrics)

	// EPP presence is now derived from the model-level arrival rate rather than
	// per-replica ArrivalRate: a model-level sum(rate(...)) is all-or-nothing
	// by design, so a single check here is equivalent and simpler.
	//
	// The per-replica ReplicaMetrics.RequestRate is deliberately NOT consulted here as
	// a "broken arrival" cross-check (e.g. warn when arrival == 0 while ΣRequestRate > 0).
	// RequestRate is a request completion rate, not an arrival rate: a draining engine
	// keeps RequestRate > 0 after arrivals have legitimately fallen to zero, so that
	// condition is a normal ramp-down state, not a fault — cross-checking it would warn
	// constantly during scale-down. A genuine broken-arrival signal is temporal — supply
	// live but demand never observed across a full staleness window — and is surfaced as
	// an observability-only warning in the engine liveness path, not in this per-cycle
	// demand math.
	anyEPP := input.ArrivalRate > 0

	a.mu.Lock()
	defer a.mu.Unlock()

	var (
		anyGPSMismatch    bool
		totalDecodeITLSat float64
		totalDecodeOL     float64
		totalDecodeKV     int // Σ nKV over non-prefill variants — avgOL replica-count weight
		nDecodeVariants   int
	)
	variantCapacities := make([]domain.VariantCapacity, 0, len(byVariant))

	for variantName, variantMetrics := range byVariant {
		key := variantKey(input.Namespace, input.ModelID, variantName)
		state, ok := a.variantStates[key]
		if !ok {
			continue
		}

		shape, hasShape := state.shapeTracker.Current()
		if !hasShape || shape.KVreq <= 0 {
			continue
		}

		// TODO(#1261): skip variant when state.lastSanityReport is not OK — stale/invalid
		// metrics from Observe() do not currently block demand computation here.
		// Gating demand on the sanity report is deferred to the per-analyzer status-return
		// PR; it requires the engine contract to accept an AnalyzerStatus opt-out signal.

		// Filter to healthy replicas for ITL model fitting and GPS verification.
		// Stale replicas (cold-start, missing fields) bias the tier-2 OLS slope A
		// upward → systematic under-provisioning. Supply counting (computeVariantSupply,
		// computeDemand) uses the unfiltered variantMetrics to include booting replicas.
		healthyMetrics := filterHealthyForShape(variantMetrics)
		model, reason, ok := a.resolveITLModel(ctx, state, healthyMetrics, input.Namespace, input.ModelID, variantName)
		if !ok {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: no ITL model available, skipping variant",
				"namespace", input.Namespace,
				"modelID", input.ModelID,
				"variant", variantName,
			)
			continue
		}

		itlSat := model.ITLAt(DefaultKSat)
		if itlSat <= 0 {
			continue
		}

		supply, perReplicaSupply, nKV := computeVariantSupply(variantMetrics, shape, itlSat)
		if supply == 0 {
			continue
		}

		// isEPP is no longer used here — anyEPP is now derived model-level from
		// input.ArrivalRate above — but computeDemand's own per-variant demand
		// value is kept for VariantCapacity.TotalDemand/Utilization display and
		// as the computeLocalDemand fallback trigger below (deferred, see plan).
		demand, _ := computeDemand(variantMetrics)
		// k*-based local demand: when EPP and the engine completion rate are both absent, or when
		// EPP is present but yields zero usable demand (warm-up, no completions yet),
		// derive demand from observed KV utilization so a busy replica is not
		// mis-classified as idle and spuriously scaled down.
		if demand == 0 {
			demand = computeLocalDemand(variantMetrics, shape, model)
		}

		// Update state for VariantState() snapshots.
		state.lastITLModel = model
		state.lastPerReplicaSupply = perReplicaSupply
		state.lastTotalSupply = supply
		state.lastDemand = demand

		pending := pendingByVariant[variantName]
		// Track ITL(k_sat) and avgOL across non-prefill variants: ITL(k_sat) for
		// queue demand estimation, avgOL for the model-level arrival decode term.
		// avgOL uses each variant's tracked/smoothed shape.AvgOutputTokens (from
		// state.shapeTracker, robust to a single warm-up cycle reporting
		// AvgOutputTokens==0) rather than a fresh average over live
		// input.ReplicaMetrics, which would zero out avgOL during EPP warm-up
		// (ArrivalRate>0, no completions yet) and reintroduce the spurious-
		// scale-down bug regression-tested by "EPP warm-up" below. Weighted by
		// nKV (replica count) across variants: an unweighted mean-of-means would
		// let every non-prefill variant contribute equally regardless of its
		// share of replicas/traffic,
		// diverging from the plan's specified RequestRate-weighted model-level
		// average whenever 2+ non-prefill variants have different OL profiles.
		if state.role != domain.RolePrefill {
			totalDecodeITLSat += itlSat
			totalDecodeOL += float64(nKV) * shape.AvgOutputTokens
			totalDecodeKV += nKV
			nDecodeVariants++
		}

		if checkVariantGPSMismatch(ctx, healthyMetrics, shape, model, input.Namespace, input.ModelID, variantName) {
			anyGPSMismatch = true
			state.consecutiveGPSMismatches++
			if state.consecutiveGPSMismatches >= DefaultGPSMismatchClearThreshold {
				state.observationWindow.Clear()
				state.consecutiveGPSMismatches = 0
				ctrl.LoggerFrom(ctx).Info("throughput analyzer: GPS mismatch persisted, clearing observation window for recalibration",
					"namespace", input.Namespace,
					"modelID", input.ModelID,
					"variant", variantName,
					"threshold", DefaultGPSMismatchClearThreshold,
				)
			}
		} else {
			state.consecutiveGPSMismatches = 0
		}

		// ReplicaCount is the count of KV-capable replicas (nKV), and TotalCapacity is the
		// measured supply over exactly those replicas — so TotalSupply (which the engine uses
		// for SpareCapacity) is not inflated by still-booting KV=0 replicas. Not-ready replicas
		// are already reflected in PendingReplicas (currentReplicas − readyReplicas); they count
		// toward TotalAnticipatedSupply and so still suppress RequiredCapacity during scale-out.
		// This mirrors saturation_v2 (ReplicaCount = readyCount, PendingReplicas separate) and
		// avoids double-counting booting replicas in both ReplicaCount and PendingReplicas.
		// TotalCapacity is the product ReplicaCount × PerReplicaCapacity (the VariantCapacity
		// contract, and what aggregation.SumTotalSupply recomputes); equals supply for nKV ≥ 1.
		totalCapacity := float64(nKV) * perReplicaSupply
		variantCapacities = append(variantCapacities, domain.VariantCapacity{
			VariantName:        variantName,
			Role:               state.role,
			ReplicaCount:       nKV,
			PendingReplicas:    pending,
			PerReplicaCapacity: perReplicaSupply,
			TotalCapacity:      totalCapacity,
			TotalDemand:        demand,
			Utilization:        safeDivide(demand, totalCapacity),
			Reason:             reason,
		})
	}

	// Scale-from-zero complement: the loop above keys off byVariant, which only has
	// entries for variants with live replicas this cycle, so a variant that has
	// scaled to zero contributes nothing and is not a selectable candidate. Iterate
	// the full variant list (input.VariantStates enumerates every configured variant,
	// zero-replica included) and, for a variant absent from byVariant that TA saw
	// live before, emit a PRC-only capacity carrying its persisted last-good
	// per-replica supply. That keeps a previously-live variant proactively selectable
	// for scale-from-zero without fabricating a baseline for one TA has never sized.
	// Cost, AcceleratorName, and ReplicaCount are deliberately left unset: they are
	// identity fields the anchor merge sources from the saturation entry for the same
	// variant, not values TA is entitled to emit. Role is the exception and must be
	// carried: this slice is consumed by distributeDemandByRole and
	// aggregateRoleCapacities below, before any anchor merge runs, and both
	// canonicalize an empty role to domain.RoleBoth. On a disaggregated model a
	// blank-role entry would therefore manufacture a phantom "both" bucket — which
	// dilutes each role's demand share and leaves the paired allocator unable to pick
	// a variant for it — so the persisted role is emitted, exactly as the live loop
	// above does. A never-seen variant (no persisted supply) gets nothing, so its
	// per-replica capacity stays zero and it is not proactively selectable; the
	// reactive scale-from-zero engine covers genuine cold-starts. Persisted supply
	// self-expires on the same idle window as the rest of the per-variant state, so a
	// long-idle variant degrades to the never-seen case.
	for _, vs := range input.VariantStates {
		if _, alreadyLive := byVariant[vs.VariantName]; alreadyLive {
			continue
		}
		key := variantKey(input.Namespace, input.ModelID, vs.VariantName)
		st, ok := a.variantStates[key]
		if !ok || st.lastPerReplicaSupply <= 0 {
			continue
		}
		variantCapacities = append(variantCapacities, domain.VariantCapacity{
			VariantName:        vs.VariantName,
			Role:               st.role,
			PerReplicaCapacity: st.lastPerReplicaSupply,
			Reason:             itlReasonScaleFromZero,
		})
	}

	// Model-level supply totals computed from the per-variant slice.
	// TotalAnticipatedSupply is published so the engine's post-step can compute RC/SC.
	totalSupply := aggregation.SumTotalSupply(variantCapacities)
	totalAnticipatedSupply := aggregation.SumTotalAnticipatedSupply(variantCapacities)

	// Decode demand is a model-level quantity: Λ_req × avgOL,
	// computed once from the model-level arrival rate rather than summed from each
	// variant's computeDemand result. This replaces the retired per-variant EPP
	// arrival contribution to TotalDemand (per-variant VariantCapacity.TotalDemand
	// above is unaffected — it still reflects computeDemand/computeLocalDemand for
	// per-variant introspection). avgOL is the nKV-weighted mean of tracked
	// shape.AvgOutputTokens across non-prefill variants (totalDecodeOL /
	// totalDecodeKV, accumulated in the loop above) — weighted, not a plain
	// mean-of-variant-means, so a variant with more replicas contributes
	// proportionally more. Zero when no non-prefill variant
	// currently has a resolved ITL model, matching avgDecodeITLSat's guard below.
	var totalDemand, arrivalDecodeDemand float64
	var arrivalDemandByRole map[string]float64
	// Scheduler queue demand is decode-rate-denominated and not variant-attributed.
	// Add to model-level demand and distribute across active non-prefill roles so
	// per-role TotalDemand satisfies the linearity invariant.
	var queueDemandByRole map[string]float64
	// nDecodeVariants > 0 is guaranteed here: the loop above only increments it for
	// variants that produced supply > 0 (itlSat > 0), so dividing by nDecodeVariants
	// or totalDecodeKV (both guaranteed >= 1 in that case) is safe from
	// division-by-zero.
	if nDecodeVariants > 0 {
		avgOL := totalDecodeOL / float64(totalDecodeKV)
		// Arrival rate is the demand signal. When it is zero — EPP absent, or present
		// but not yet scraped — arrivalDecodeDemand is legitimately zero and so is
		// TotalDemand. Zero demand only ever permits scale-down (still governed by the
		// multi-analyzer all-live-agree gate); it never forces a scale action and never
		// drives scale-up. So a zero/absent arrival signal is safe here and is
		// intentionally NOT floored to a served-rate proxy.
		arrivalDecodeDemand = input.ArrivalRate * avgOL
		totalDemand = arrivalDecodeDemand
		arrivalDemandByRole = distributeDemandByRole(arrivalDecodeDemand, variantCapacities)

		avgDecodeITLSat := totalDecodeITLSat / float64(nDecodeVariants)
		queueDemand := estimateQueueDemand(input.SchedulerQueue, avgDecodeITLSat, DefaultQueueDrainFactor)
		totalDemand += queueDemand
		queueDemandByRole = distributeDemandByRole(queueDemand, variantCapacities)
	}

	// TA publishes raw Total* fields; RequiredCapacity and SpareCapacity are left
	// zero — the engine's universal threshold post-step writes them after Analyze returns.
	// The GPS/EPP gate that previously suppressed SpareCapacity is dropped here; without
	// it, a missing SchedulerQueue (nil) causes TotalDemand to be under-estimated and the
	// engine may compute SC > 0 when queued requests exist — a scale-down risk.
	// Restoring the gate (conditioned on SchedulerQueue == nil || anyGPSMismatch) is
	// tracked in issue #1261 and requires a per-analyzer status-return signal in the
	// engine contract. anyEPP and anyGPSMismatch are retained as placeholders for that PR.
	_ = anyEPP
	_ = anyGPSMismatch

	return &domain.AnalyzerResult{
		AnalyzerName:           AnalyzerName,
		ModelID:                input.ModelID,
		Namespace:              input.Namespace,
		AnalyzedAt:             now,
		VariantCapacities:      variantCapacities,
		TotalSupply:            totalSupply,
		TotalAnticipatedSupply: totalAnticipatedSupply,
		TotalDemand:            totalDemand,
		Utilization:            safeDivide(totalDemand, totalSupply),
		RoleCapacities:         aggregateRoleCapacities(variantCapacities, arrivalDemandByRole, queueDemandByRole),
	}, nil
}

// VariantState returns a read-only snapshot of the per-variant calibration state.
// Returns (zero ThroughputVariantState, false) if no data has been observed yet
// for the given variant.
func (a *ThroughputAnalyzer) VariantState(modelID, namespace, variantName string) (ThroughputVariantState, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := variantKey(namespace, modelID, variantName)
	state, ok := a.variantStates[key]
	if !ok {
		return ThroughputVariantState{}, false
	}

	shape, _ := state.shapeTracker.Current()
	return ThroughputVariantState{
		Shape:            shape,
		ObservationReady: state.observationWindow.Ready(),
		KSpread:          state.observationWindow.KSpread(),
		SampleCount:      state.observationWindow.Len(),
		LastSanityReport: state.lastSanityReport,
		ITLModel:         state.lastITLModel,
		PerReplicaSupply: state.lastPerReplicaSupply,
		TotalSupply:      state.lastTotalSupply,
		Demand:           state.lastDemand,
		Role:             state.role,
		LastFittedB:      state.lastFittedB,
		HasFittedB:       state.hasFittedB,
	}, true
}

// --- helpers ---

// variantKey builds the map key for a variant. The null-byte delimiter is safe
// because neither Kubernetes resource names nor operator-provided model IDs can
// contain a null byte.
func variantKey(namespace, modelID, variantName string) string {
	return namespace + "\x00" + modelID + "\x00" + variantName
}

// getOrCreateVariantState returns the variantState for the given key, creating
// it with default configuration if it does not exist yet.
// Must be called with a.mu held.
func (a *ThroughputAnalyzer) getOrCreateVariantState(key string) *variantState {
	if state, ok := a.variantStates[key]; ok {
		return state
	}
	state := &variantState{
		shapeTracker: newShapeTracker(DefaultShapeChangeTolerance),
		observationWindow: newObservationWindow(
			DefaultWindowMaxSize,
			DefaultObservationMaxAge,
			DefaultMinSamples,
			DefaultMinKSpread,
			DefaultMinObservableK,
			DefaultMaxObservableK,
		),
	}
	a.variantStates[key] = state
	return state
}

// resolveITLModel returns the ITL model to use for a variant using a two-tier strategy:
//
//   - Tier 1: OLS fit from the observation window (when Ready).
//   - Tier 2: constrained OLS with B pinned. B is taken from the last successful Tier-1 fit
//     (state.lastFittedB) when one exists, because B reflects hardware/model characteristics
//     that survive workload-shape changes. Falls back to DefaultBaselineITLSec when no
//     prior fit exists. Only possible when at least one replica has k* > 0; replicas with
//     k* = 0 (idle) carry no ITL signal and are excluded.
//
// When replicas are present but all are idle (k* = 0), both tiers fail and we return (zero, false).
// A variant that has dropped to zero replicas is handled separately by the scale-from-zero
// complement in Analyze(), which re-emits the persisted last-good per-replica supply rather than
// re-deriving an ITL model here; resolveITLModel is only consulted for variants with live replicas.
//
// Must be called with a.mu held.
func (a *ThroughputAnalyzer) resolveITLModel(ctx context.Context, state *variantState, metrics []domain.ReplicaMetrics, namespace, modelID, variantName string) (ITLModel, string, bool) {
	// Tier 1: OLS fit.
	if state.observationWindow.Ready() {
		obs := state.observationWindow.Observations()
		if model, ok := FitITLModel(obs); ok {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: tier-1 OLS fit",
				"namespace", namespace, "modelID", modelID, "variant", variantName,
				"A", model.A, "B", model.B, "samples", len(obs),
			)
			state.lastFittedB = model.B
			state.hasFittedB = true
			return model, itlReasonT1OLS, true
		}
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: tier-1 OLS fit failed, trying tier-2",
			"namespace", namespace, "modelID", modelID, "variant", variantName,
			"samples", len(obs),
		)
	}

	// Tier 2: constrained OLS with B pinned.
	// Minimize Σ(ITL_i − A·k_i − B)² → A = Σ((ITL_i − B)·k_i) / Σ(k_i²).
	// Using per-replica (k*, ITL) directly is better than collapsing to a centroid
	// when replicas have spread k* values — it is the same least-squares criterion
	// as tier-1 OLS but with B pinned instead of fitted.
	baselineB := DefaultBaselineITLSec
	tier2Label := itlReasonT2Default
	if state.hasFittedB {
		baselineB = state.lastFittedB
		tier2Label = itlReasonT2Pinned
	}
	var numerator, sumK2 float64
	var n float64
	for _, m := range metrics {
		if m.KvUsageInstant > 0 && m.AvgITL > 0 {
			numerator += (m.AvgITL - baselineB) * m.KvUsageInstant
			sumK2 += m.KvUsageInstant * m.KvUsageInstant
			n++
		}
	}
	if n > 0 && sumK2 > 0 {
		A := numerator / sumK2
		if validITLModel(A, baselineB) {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: tier-2 constrained OLS fit",
				"namespace", namespace, "modelID", modelID, "variant", variantName,
				"A", A, "B", baselineB, "replicas", int(n),
			)
			return ITLModel{A: A, B: baselineB}, tier2Label, true
		}
	}
	return ITLModel{}, itlReasonT2Failed, false
}

// computeDemand aggregates λ_dec (decode token demand in tokens/sec) across replicas.
//
// Primary path (EPP deployed): Σ ArrivalRate_r × AvgOutputTokens_r.
// Fallback path (EPP absent): Σ RequestRate_r × AvgOutputTokens_r.
//
// Both paths use the per-replica product rather than sumRate × avgOL to avoid
// averaging-the-averages: replicas with higher throughput contribute proportionally
// more to λ_dec without requiring raw histogram sums.
//
// Returns (λ_dec, isEPP). isEPP is true when at least one replica reports ArrivalRate > 0.
// When EPP is present but yields zero usable demand (warm-up: ArrivalRate > 0 but
// AvgOutputTokens == 0), the function falls through to the engine request-rate proxy
// so the caller can use computeLocalDemand when both paths yield zero. The returned
// λ_dec only feeds this variant's own VariantCapacity.TotalDemand/Utilization
// (introspection) — Analyze's model-level TotalDemand uses input.ArrivalRate ×
// avgOL instead (the all-or-nothing model-level design), and derives anyEPP from that model-level rate
// rather than from this function's isEPP return.
func computeDemand(metrics []domain.ReplicaMetrics) (float64, bool) {
	var lambdaDec float64
	var isEPP bool
	for _, m := range metrics {
		if m.ArrivalRate > 0 {
			isEPP = true // EPP present, even if AvgOutputTokens is not yet observed (warm-up)
			if m.AvgOutputTokens > 0 {
				lambdaDec += m.ArrivalRate * m.AvgOutputTokens
			}
		}
	}
	if lambdaDec > 0 {
		return lambdaDec, isEPP // EPP present and gave usable demand
	}
	// EPP absent, OR EPP present but zero usable demand (warm-up, no completions yet):
	// fall through to the engine request-rate proxy.
	// Σ RequestRate_r × AvgOutputTokens_r mirrors the EPP formula structure and
	// correctly weights each replica's OL by its own throughput.
	var lambdaDecFallback float64
	for _, m := range metrics {
		if m.RequestRate > 0 && m.AvgOutputTokens > 0 {
			lambdaDecFallback += m.RequestRate * m.AvgOutputTokens
		}
	}
	return lambdaDecFallback, isEPP // isEPP still reflects "EPP present"
}

// computeLocalDemand estimates decode token demand from per-replica k* observations
// when the EPP ArrivalRate and the engine request rate are both unavailable.
//
//	λ_local = Σ_r (k_r* × KV_max_r / KVreq) / ITL(k_r*)
//
// Each replica's in-flight request count N_r = k_r* × KV_max_r / KVreq is divided
// by ITL(k_r*) to approximate its current throughput. Replicas with k* = 0 or
// KV_max = 0 are excluded (no meaningful signal at idle).
// This path is scale-up only: k*-based demand may undercount arriving load
// without EPP. The engine post-step determines SC from the published totals.
func computeLocalDemand(metrics []domain.ReplicaMetrics, shape WorkloadShape, model ITLModel) float64 {
	if shape.KVreq <= 0 || shape.AvgOutputTokens <= DefaultMinDecodeOLForLocalDemand {
		return 0
	}
	var total float64
	for _, m := range metrics {
		if math.IsNaN(m.KvUsageInstant) || m.KvUsageInstant <= 0 || m.TotalKvCapacityTokens <= 0 {
			continue
		}
		// KvUsageInstant is a KV-utilization fraction; values > 1 indicate a bad/over-committed
		// metric. Skip rather than clamp — a single over-range replica shouldn't inflate demand.
		if m.KvUsageInstant > 1 {
			continue
		}
		itlAtK := model.ITLAt(m.KvUsageInstant)
		if math.IsNaN(itlAtK) || itlAtK <= 0 {
			continue
		}
		total += m.KvUsageInstant * float64(m.TotalKvCapacityTokens) / shape.KVreq / itlAtK
	}
	return total
}

// estimateQueueDemand converts the scheduler queue depth into an equivalent
// decode token demand rate (tokens/sec).
//
//	drain_time = QueueDrainFactor × ITL(k_sat) × avgOL
//	λ_queue    = QueueSize × avgOL / drain_time
//	           = QueueSize / (QueueDrainFactor × ITL(k_sat))   (avgOL cancels)
//
// ITL(k_sat) is used as the reference latency so that admitted queue demand
// bounds per-request queueing time to ≤ QueueDrainFactor × ITL(k_sat) × avgOL.
func estimateQueueDemand(sq *domain.SchedulerQueueMetrics, itlSat, drainFactor float64) float64 {
	if sq == nil || sq.QueueSize <= 0 || itlSat <= 0 || drainFactor <= 0 {
		return 0
	}
	return float64(sq.QueueSize) / (drainFactor * itlSat)
}

// computeVariantSupply computes the aggregate μ_dec_sat supply for a variant.
//
// Per replica: N_dec_sat = DefaultKSat × KV_max / KVreq; μ_dec_sat = N_dec_sat / itlSat.
// Returns (totalSupply Σμ_dec_sat, perReplicaSupply mean(μ_dec_sat), nKV count of
// KV-capable replicas). All are zero when no replica has KV capacity data.
func computeVariantSupply(metrics []domain.ReplicaMetrics, shape WorkloadShape, itlSat float64) (total, perReplica float64, nKV int) {
	var sum float64
	var n int
	for _, m := range metrics {
		if m.TotalKvCapacityTokens <= 0 {
			continue
		}
		kvMax := float64(m.TotalKvCapacityTokens)
		nSat := DefaultKSat * kvMax / shape.KVreq
		sum += nSat / itlSat
		n++
	}
	if n == 0 {
		return 0, 0, 0
	}
	return sum, sum / float64(n), n
}

// groupByVariant partitions a slice of ReplicaMetrics by VariantName.
func groupByVariant(metrics []domain.ReplicaMetrics) map[string][]domain.ReplicaMetrics {
	groups := make(map[string][]domain.ReplicaMetrics)
	for _, m := range metrics {
		groups[m.VariantName] = append(groups[m.VariantName], m)
	}
	return groups
}

// averageShapeMetrics computes the RequestRate-weighted mean IL, OL, and
// prefix hit rate across a slice of replica metrics. Replicas with zero or
// negative IL or OL are excluded. When all eligible replicas have zero
// RequestRate, falls back to an unweighted mean.
func averageShapeMetrics(metrics []domain.ReplicaMetrics) (il, ol, hitRate float64) {
	var sumIL, sumOL, sumHitRate float64 // weighted accumulators
	var sumILu, sumOLu, sumHRu float64   // unweighted fallback
	var totalWeight, count float64
	for _, m := range metrics {
		if m.AvgInputTokens <= DefaultMinTokensPerRequest || m.AvgOutputTokens <= DefaultMinTokensPerRequest {
			continue
		}
		count++
		sumILu += m.AvgInputTokens
		sumOLu += m.AvgOutputTokens
		sumHRu += m.PrefixCacheHitRate
		if m.RequestRate > 0 {
			sumIL += m.RequestRate * m.AvgInputTokens
			sumOL += m.RequestRate * m.AvgOutputTokens
			sumHitRate += m.RequestRate * m.PrefixCacheHitRate
			totalWeight += m.RequestRate
		}
	}
	if count == 0 {
		return 0, 0, 0
	}
	if totalWeight == 0 {
		return sumILu / count, sumOLu / count, sumHRu / count
	}
	return sumIL / totalWeight, sumOL / totalWeight, sumHitRate / totalWeight
}

// filterHealthyForShape returns only the replicas that pass all per-replica
// sanity checks. Replicas with cold-start (ITL=0), stale metrics, or missing
// KV capacity are excluded so a single bad pod cannot block the variant.
func filterHealthyForShape(metrics []domain.ReplicaMetrics) []domain.ReplicaMetrics {
	healthy := make([]domain.ReplicaMetrics, 0, len(metrics))
	for _, m := range metrics {
		if len(checkReplicaMetrics(m)) == 0 {
			healthy = append(healthy, m)
		}
	}
	return healthy
}

// safeDivide returns num/denom, or 0 when denom is zero.
func safeDivide(num, denom float64) float64 {
	if denom == 0 {
		return 0
	}
	return num / denom
}

// checkVariantGPSMismatch compares each replica's observed GenerationTokenRate (GPS_obs,
// i.e. μ_dec^obs) against the model-predicted decode rate μ_dec(k*) = N_dec(k*) / ITL(k*).
// Returns true when any replica exceeds DefaultGPSMismatchThresholdPct at k* ≥
// DefaultGPSMinKForVerification, indicating the ITL model may be wrong.
//
// When a mismatch is detected near saturation (k* ≥ DefaultKSat − DefaultNearKSatMargin),
// additional diagnostics are logged to distinguish between two root causes:
//   - ITL model drift / bad data points: observed AvgITL deviates from ITL(k*).
//   - Shape mismatch: ITL fits well but GPS × AvgITL disagrees with KV-derived N_dec,
//     suggesting IL, OL, or prefix-hit-rate parameters are wrong.
func checkVariantGPSMismatch(
	ctx context.Context,
	metrics []domain.ReplicaMetrics,
	shape WorkloadShape,
	model ITLModel,
	namespace, modelID, variantName string,
) bool {
	if shape.KVreq <= 0 {
		return false
	}
	mismatch := false
	for _, m := range metrics {
		if m.GenerationTokenRate <= 0 || m.KvUsageInstant < DefaultGPSMinKForVerification {
			continue
		}
		if m.TotalKvCapacityTokens <= 0 {
			continue
		}
		itlAtK := model.ITLAt(m.KvUsageInstant)
		if itlAtK <= 0 {
			continue
		}
		nDec := m.KvUsageInstant * float64(m.TotalKvCapacityTokens) / shape.KVreq
		muDecModel := nDec / itlAtK
		if muDecModel <= 0 {
			continue
		}
		gpsErrPct := math.Abs(muDecModel-m.GenerationTokenRate) / m.GenerationTokenRate * 100
		if gpsErrPct <= DefaultGPSMismatchThresholdPct {
			continue
		}
		mismatch = true
		ctrl.LoggerFrom(ctx).Info("throughput analyzer: GPS mismatch detected",
			"namespace", namespace,
			"modelID", modelID,
			"variant", variantName,
			"pod", m.PodName,
			"k", m.KvUsageInstant,
			"GPSObs", m.GenerationTokenRate,
			"muDecModel", muDecModel,
			"gpsErrPct", gpsErrPct,
		)

		// Near k_sat: run deeper diagnostics to identify root cause.
		if m.KvUsageInstant < DefaultKSat-DefaultNearKSatMargin || m.AvgITL <= 0 {
			continue
		}
		itlResidual := math.Abs(m.AvgITL-itlAtK) / m.AvgITL
		if itlResidual > DefaultNearKSatITLResidualThreshold {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: near-k_sat ITL residual high (model drift or bad data)",
				"namespace", namespace,
				"modelID", modelID,
				"variant", variantName,
				"pod", m.PodName,
				"k", m.KvUsageInstant,
				"avgITLObs", m.AvgITL,
				"itlModel", itlAtK,
				"itlResidualPct", itlResidual*100,
			)
		} else {
			// ITL model matches observed ITL but GPS disagrees: N_dec derivation
			// (shape.KVreq via IL/OL/hit-rate) may be wrong.
			nDecGPS := m.GenerationTokenRate * m.AvgITL
			nDecErrPct := math.Abs(nDec-nDecGPS) / nDec * 100
			if nDecErrPct > DefaultNearKSatNDecResidualThreshold*100 {
				ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: near-k_sat N_dec mismatch (shape wrong?)",
					"namespace", namespace,
					"modelID", modelID,
					"variant", variantName,
					"pod", m.PodName,
					"k", m.KvUsageInstant,
					"nDecModel", nDec,
					"nDecGPS", nDecGPS,
					"nDecErrPct", nDecErrPct,
					"hint", "check AvgInputTokens/AvgOutputTokens/PrefixCacheHitRate",
				)
			}
		}
	}
	return mismatch
}

// distributeDemandByRole splits a model-level decode-rate-denominated demand
// quantity evenly across active non-prefill roles derived from vcs. Used for
// both the model-level arrival decode term (Λ_req × avgOL) and the scheduler
// queue-drain term — both are decode-rate-denominated and role-agnostic at the
// point they are computed, so prefill roles are excluded. Returns nil when
// demand is zero or no non-prefill roles exist.
func distributeDemandByRole(demand float64, vcs []domain.VariantCapacity) map[string]float64 {
	if demand == 0 {
		return nil
	}
	roles := make(map[string]struct{})
	for _, vc := range vcs {
		role := vc.Role
		if role == "" {
			role = domain.RoleBoth
		}
		if role != domain.RolePrefill {
			roles[role] = struct{}{}
		}
	}
	if len(roles) == 0 {
		return nil
	}
	share := demand / float64(len(roles))
	result := make(map[string]float64, len(roles))
	for role := range roles {
		result[role] = share
	}
	return result
}

// aggregateRoleCapacities groups variant capacities by P/D role and computes
// per-role raw Total* fields. TotalDemand per role is arrivalDemandByRole[role] +
// queueDemandByRole[role] (either map nil is safe — treated as zero); it no
// longer sums each role's per-variant computeDemand results (AggregateByRole's
// TotalDemand is unused here), since the model-level arrival decode term
// replaced that path — see Analyze's arrivalDecodeDemand. TotalSupply and
// TotalAnticipatedSupply are still summed per-variant, unaffected by the demand
// change. Returns nil for non-disaggregated models (all variants role "" or
// "both"). RequiredCapacity and SpareCapacity are left zero — the engine's
// universal threshold post-step writes them.
func aggregateRoleCapacities(vcs []domain.VariantCapacity, arrivalDemandByRole, queueDemandByRole map[string]float64) map[string]domain.RoleCapacity {
	byRole := aggregation.AggregateByRole(vcs)
	// Non-disaggregated: only a "both" bucket (or nothing) — no per-role breakdown.
	if _, hasBoth := byRole[domain.RoleBoth]; len(byRole) == 0 || (len(byRole) == 1 && hasBoth) {
		return nil
	}

	result := make(map[string]domain.RoleCapacity, len(byRole))
	for role, t := range byRole {
		result[role] = domain.RoleCapacity{
			Role:                   role,
			TotalSupply:            t.TotalSupply,
			TotalAnticipatedSupply: t.TotalAnticipatedSupply,
			TotalDemand:            arrivalDemandByRole[role] + queueDemandByRole[role],
		}
	}
	return result
}
