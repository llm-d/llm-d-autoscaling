package saturation

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
)

// The Enabled tag runAnalyzersAndScore puts on each ballot entry is what decides
// which analyzers vote in the combine math: votingResults prunes the ballot to the
// enabled subset, and the anchor is derived from what survives. The three
// configurations below are the ones the developer guide documents as supported, so
// they are pinned here at the layer that actually derives Enabled from config.
// Pipeline-level tests hand-construct NamedAnalyzerResult.Enabled and so cannot
// catch a regression in this wiring.

// noAnalyzerListCfg is the default configuration: no explicit analyzer list, which
// is the shipped single-analyzer (saturation-only) shape.
var noAnalyzerListCfg = config.SaturationScalingConfig{
	ScaleUpThreshold:  0.85,
	ScaleDownBoundary: 0.70,
}

// bothAnalyzersCfg names saturation and throughput explicitly, so both vote.
var bothAnalyzersCfg = config.SaturationScalingConfig{
	ScaleUpThreshold:  0.85,
	ScaleDownBoundary: 0.70,
	Analyzers: []config.AnalyzerScoreConfig{
		{Name: domain.SaturationAnalyzerName},
		{Name: throughput.AnalyzerName},
	},
}

// saturationSilencedWarnings returns the warning emitted when an explicit analyzer
// list leaves saturation out, isolating it from the routine per-analyzer INFO lines.
func saturationSilencedWarnings(logs *observer.ObservedLogs) []observer.LoggedEntry {
	var out []observer.LoggedEntry
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "cannot veto scale-down") {
			out = append(out, e)
		}
	}
	return out
}

// Default config (no analyzer list): saturation is the sole analyzer and votes.
func TestRunAnalyzersAndScore_DefaultConfigSaturationVotes(t *testing.T) {
	ctx, logs := zapObserverCtx(t)
	e := demandLivenessEngine(informativeSat(), throughputAnalyzer(1000))

	results, err := e.runAnalyzersAndScore(ctx, "m", "ns", nil, noAnalyzerListCfg, nil, nil, nil, nil, 0)
	require.NoError(t, err)

	byName := namedByName(results)
	assert.True(t, byName[domain.SaturationAnalyzerName].Enabled,
		"saturation must vote in the default no-analyzer-list configuration")
	// Throughput is not opted in, so it is not run and contributes no entry.
	assert.NotContains(t, byName, throughput.AnalyzerName,
		"an analyzer absent from the list must not be run")
	assert.Empty(t, saturationSilencedWarnings(logs),
		"the default configuration must not warn about saturation being silenced")
}

// Throughput-only list: saturation stays on the ballot as the identity carrier but
// does not vote, and the loss of its scale-down veto is called out in the log.
func TestRunAnalyzersAndScore_ThroughputOnlySilencesSaturationVote(t *testing.T) {
	ctx, logs := zapObserverCtx(t)
	e := demandLivenessEngine(informativeSat(), throughputAnalyzer(1000))

	results, err := e.runAnalyzersAndScore(ctx, "m", "ns", nil, enabledThroughputCfg, nil, nil, nil, nil, 0)
	require.NoError(t, err)

	byName := namedByName(results)
	require.Contains(t, byName, domain.SaturationAnalyzerName,
		"saturation must remain on the ballot as the identity/(a) carrier even when it does not vote")
	assert.False(t, byName[domain.SaturationAnalyzerName].Enabled,
		"saturation must not vote when an explicit analyzer list omits it")
	assert.True(t, byName[throughput.AnalyzerName].Enabled,
		"a listed analyzer must vote")
	assert.Len(t, saturationSilencedWarnings(logs), 1,
		"silencing saturation's vote must be logged: it removes the scale-down veto")
}

// Both analyzers listed: both vote.
func TestRunAnalyzersAndScore_BothAnalyzersVote(t *testing.T) {
	ctx, logs := zapObserverCtx(t)
	e := demandLivenessEngine(informativeSat(), throughputAnalyzer(1000))

	results, err := e.runAnalyzersAndScore(ctx, "m", "ns", nil, bothAnalyzersCfg, nil, nil, nil, nil, 0)
	require.NoError(t, err)

	byName := namedByName(results)
	assert.True(t, byName[domain.SaturationAnalyzerName].Enabled,
		"saturation must vote when it is named in the analyzer list")
	assert.True(t, byName[throughput.AnalyzerName].Enabled,
		"throughput must vote when it is named in the analyzer list")
	assert.Empty(t, saturationSilencedWarnings(logs),
		"saturation is voting here, so nothing should warn about its veto")
}
