package saturation

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

func zapObserverCtx(t *testing.T) (context.Context, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)))
	return ctx, logs
}

func TestLogAnalyzerResult_EmitsRequiredFields(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	nr := pipeline.NamedAnalyzerResult{
		Name:              "saturation",
		ScaleUpThreshold:  1.2,
		ScaleDownBoundary: 0.7,
		Result: &domain.AnalyzerResult{
			TotalSupply:      100000,
			TotalDemand:      80000,
			Utilization:      0.8,
			RequiredCapacity: 0,
			SpareCapacity:    20000,
			VariantCapacities: []domain.VariantCapacity{
				{
					VariantName:        "primary",
					PerReplicaCapacity: 50000,
					Cost:               10,
					Role:               domain.RoleDecode,
					Reason:             "P2-hist",
				},
			},
		},
	}

	logAnalyzerResult(ctx, "mymodel", "ns", nr)

	require.Equal(t, 1, logs.Len())
	entry := logs.All()[0]
	assert.Equal(t, "analyzer-result", entry.Message)

	fields := entry.ContextMap()
	for _, key := range []string{"modelID", "namespace", "analyzer", "supply", "demand", "util", "rc", "sc", "scaleUpThreshold", "scaleDownBoundary", "variants"} {
		assert.Contains(t, fields, key, "missing field %q", key)
	}
	assert.Equal(t, "mymodel", fields["modelID"])
	assert.Equal(t, "ns", fields["namespace"])
	assert.Equal(t, "saturation", fields["analyzer"])

	// Verify variants entries contain "reason" but not "cost".
	b, err := json.Marshal(fields["variants"])
	require.NoError(t, err)
	variantsJSON := string(b)
	assert.Contains(t, variantsJSON, `"reason"`, "variants entry must include reason field")
	assert.Contains(t, variantsJSON, "P2-hist", "label value must be present")
	assert.NotContains(t, variantsJSON, `"cost"`, "cost must not appear in variants entry")

	// V2 charges waiting requests by P/D role, so demand is not interpretable
	// without the resolved role on this line.
	assert.Contains(t, variantsJSON, `"role"`, "variants entry must include role field")
	assert.Contains(t, variantsJSON, domain.RoleDecode, "resolved role value must be present")
}

// An analyzer that leaves Role unset is treated as "both" downstream, so the log
// line must say "both" rather than omitting the field — that is precisely the
// case a reader needs to distinguish from an explicitly-roled variant.
func TestLogAnalyzerResult_DefaultRoleRendersAsBoth(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	logAnalyzerResult(ctx, "mymodel", "ns", pipeline.NamedAnalyzerResult{
		Name: "saturation",
		Result: &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "primary", PerReplicaCapacity: 50000, Role: ""},
			},
		},
	})

	require.Equal(t, 1, logs.Len())
	b, err := json.Marshal(logs.All()[0].ContextMap()["variants"])
	require.NoError(t, err)
	assert.Contains(t, string(b), `"role":"both"`, "unset role must render as the canonical %q", domain.RoleBoth)
}

func TestLogAnalyzerResult_NilResultSkipped(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	logAnalyzerResult(ctx, "m", "ns", pipeline.NamedAnalyzerResult{
		Name:   "saturation",
		Result: nil,
	})

	assert.Equal(t, 0, logs.Len(), "nil result should emit no log line")
}

func TestLogAnalyzerResult_EmptyVariants(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	nr := pipeline.NamedAnalyzerResult{
		Name: "throughput",
		Result: &domain.AnalyzerResult{
			TotalSupply:       0,
			TotalDemand:       0,
			RequiredCapacity:  15000,
			VariantCapacities: []domain.VariantCapacity{},
		},
	}

	logAnalyzerResult(ctx, "m", "ns", nr)

	require.Equal(t, 1, logs.Len())
	entry := logs.All()[0]
	assert.Equal(t, "analyzer-result", entry.Message)
	assert.Contains(t, entry.ContextMap(), "variants")
}

func TestLogScalingDecisions_EmitsPerModel(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	requests := []pipeline.ModelScalingRequest{
		{ModelID: "model-a", Namespace: "ns"},
		{ModelID: "model-b", Namespace: "ns"},
	}
	decisions := []domain.VariantDecision{
		{ModelID: "model-a", Namespace: "ns", VariantName: "v1", CurrentReplicas: 1, TargetReplicas: 2, Action: domain.ActionScaleUp},
		{ModelID: "model-a", Namespace: "ns", VariantName: "v2", CurrentReplicas: 1, TargetReplicas: 1, Action: domain.ActionNoChange},
		{ModelID: "model-b", Namespace: "ns", VariantName: "v1", CurrentReplicas: 2, TargetReplicas: 1, Action: domain.ActionScaleDown},
	}

	logScalingDecisions(ctx, requests, decisions)

	require.Equal(t, 2, logs.Len(), "expected one log line per model")
	msgs := map[string]bool{}
	for _, e := range logs.All() {
		assert.Equal(t, "scaling-decision", e.Message)
		modelID, _ := e.ContextMap()["modelID"].(string)
		msgs[modelID] = true
	}
	assert.True(t, msgs["model-a"], "expected log for model-a")
	assert.True(t, msgs["model-b"], "expected log for model-b")
}

func TestLogScalingDecisions_NoDecisionsSkipsModel(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	requests := []pipeline.ModelScalingRequest{
		{ModelID: "model-a", Namespace: "ns"},
		{ModelID: "model-b", Namespace: "ns"},
	}
	// Only model-a has a decision; model-b has none.
	decisions := []domain.VariantDecision{
		{ModelID: "model-a", Namespace: "ns", VariantName: "v1", Action: domain.ActionNoChange},
	}

	logScalingDecisions(ctx, requests, decisions)

	require.Equal(t, 1, logs.Len(), "model with no decisions should emit no log line")
	assert.Equal(t, "model-a", logs.All()[0].ContextMap()["modelID"])
}
