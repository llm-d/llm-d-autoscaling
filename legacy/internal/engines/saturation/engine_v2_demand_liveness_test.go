package saturation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// demandLivenessEngine builds a minimal Engine with a saturation entry plus an
// enabled throughput entry, suitable for driving runAnalyzersAndScore. Config
// is left nil so the liveness/demand threshold falls back to the 30s default
// (analyzerLivenessStaleCycles=3 → 90s window).
func demandLivenessEngine(sat, tp domain.Analyzer) *Engine {
	return &Engine{
		saturationV2Analyzer: sat,
		analyzersSnapshot: []analyzerEntry{
			{name: domain.SaturationAnalyzerName, analyzer: sat},
			{name: throughput.AnalyzerName, analyzer: tp},
		},
		started: true,
	}
}

// informativeSat is a saturation analyzer whose result is informative (a
// non-sentinel Reason), so the saturation entry is always live and never the
// subject of the demand detector.
func informativeSat() domain.Analyzer {
	return &fakeAnalyzerWithResult{
		analyzerName: domain.SaturationAnalyzerName,
		result: &domain.AnalyzerResult{
			AnalyzedAt:        time.Now(),
			VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Reason: "P1-obs"}},
		},
	}
}

// throughputAnalyzer builds a throughput fake with a fresh, informative supply
// signal and the given model-level demand.
func throughputAnalyzer(totalDemand float64) domain.Analyzer {
	return &fakeAnalyzerWithResult{
		analyzerName: throughput.AnalyzerName,
		result: &domain.AnalyzerResult{
			AnalyzedAt:        time.Now(),
			TotalDemand:       totalDemand,
			VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Reason: "P1-obs"}},
		},
	}
}

// enabledThroughputCfg opts the throughput analyzer into the scaling decision so
// runAnalyzersAndScore includes it (Enabled nil ⇒ present, participates).
var enabledThroughputCfg = config.SaturationScalingConfig{
	ScaleUpThreshold:  0.85,
	ScaleDownBoundary: 0.70,
	Analyzers:         []config.AnalyzerScoreConfig{{Name: throughput.AnalyzerName}},
}

// demandWarnings returns the demand-liveness warning entries emitted by the
// detector, isolating them from the routine per-analyzer INFO lines.
func demandWarnings(logs *observer.ObservedLogs) []observer.LoggedEntry {
	var out []observer.LoggedEntry
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "reported no demand") {
			out = append(out, e)
		}
	}
	return out
}

// Case 1: supply informative + demand > 0 every cycle → demand latch keeps
// pace, no warning.
func TestDetectDemandLiveness_HealthyNoWarn(t *testing.T) {
	ctx, logs := zapObserverCtx(t)
	e := demandLivenessEngine(informativeSat(), throughputAnalyzer(1000))

	results, err := e.runAnalyzersAndScore(ctx, "m", "ns", nil, enabledThroughputCfg, nil, nil, nil, nil, 0)
	require.NoError(t, err)

	assert.True(t, namedByName(results)[throughput.AnalyzerName].Live, "fresh throughput supply must be live")
	assert.Empty(t, demandWarnings(logs), "demand keeping pace must not warn")
}

// Case 2: supply informative but demand absent for longer than the staleness
// window → warn fired. Crucially the detector must not affect the decision:
// throughput stays live (its supply is fresh), proving the warning is pure
// telemetry and did not veto anything.
func TestDetectDemandLiveness_SupplyLiveDemandStaleWarns(t *testing.T) {
	ctx, logs := zapObserverCtx(t)
	e := demandLivenessEngine(informativeSat(), throughputAnalyzer(0))

	// Pre-seed the demand latch older than the 90s staleness window so a single
	// cycle with fresh supply but zero demand crosses the threshold.
	modelKey := utils.GetNamespacedKey("ns", "m")
	demandKey := throughput.AnalyzerName + demandLatchSuffix
	e.lastGoodAnalysis = map[string]map[string]time.Time{
		modelKey: {demandKey: time.Now().Add(-95 * time.Second)},
	}

	results, err := e.runAnalyzersAndScore(ctx, "m", "ns", nil, enabledThroughputCfg, nil, nil, nil, nil, 0)
	require.NoError(t, err)

	warns := demandWarnings(logs)
	require.Len(t, warns, 1, "live supply with stale demand must warn exactly once")
	assert.Equal(t, throughput.AnalyzerName, warns[0].ContextMap()["analyzer"])

	assert.True(t, namedByName(results)[throughput.AnalyzerName].Live,
		"demand warning must not flip throughput Live — it is telemetry only")
}

// Case 3: cold start — supply live for the first cycle, demand zero that cycle
// only → no warn, because the demand latch is seeded to the current supply
// timestamp so the gap is still 0 (< threshold).
func TestDetectDemandLiveness_ColdStartNoWarn(t *testing.T) {
	ctx, logs := zapObserverCtx(t)
	e := demandLivenessEngine(informativeSat(), throughputAnalyzer(0)) // fresh engine, no pre-seed

	results, err := e.runAnalyzersAndScore(ctx, "m", "ns", nil, enabledThroughputCfg, nil, nil, nil, nil, 0)
	require.NoError(t, err)

	assert.Empty(t, demandWarnings(logs), "cold start (demand absent one cycle) must not warn")
	assert.True(t, namedByName(results)[throughput.AnalyzerName].Live)
}

// The synthetic demand key must never make an analyzer live: the Live/veto path
// reads lastGoodAnalysis only via keyed lookups on real analyzer names, never
// the synthetic demand key. A model whose only entry is the demand key, with a
// non-informative supply result, stays non-live.
func TestDetectDemandLiveness_SyntheticKeyNeverFlipsLive(t *testing.T) {
	e := &Engine{started: true}
	modelKey := utils.GetNamespacedKey("ns", "m")
	demandKey := throughput.AnalyzerName + demandLatchSuffix
	e.lastGoodAnalysis = map[string]map[string]time.Time{
		modelKey: {demandKey: time.Now()},
	}

	results := []pipeline.NamedAnalyzerResult{{
		Name: throughput.AnalyzerName,
		Result: &domain.AnalyzerResult{
			AnalyzedAt:        time.Now(),
			VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Reason: pipeline.ReasonNoData}},
		},
	}}
	e.updateLivenessAndSetLive(context.Background(), "ns", "m", results)

	assert.False(t, results[0].Live, "synthetic demand key must never make an analyzer live")
}
