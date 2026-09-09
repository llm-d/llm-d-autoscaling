package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/tracing"
)

// withRecordingTracer installs an in-memory span exporter as the global tracer
// provider for the duration of the test and returns the exporter.
func withRecordingTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()

	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		require.NoError(t, provider.Shutdown(context.Background()))
	})
	return exporter
}

// requireSpan returns the single recorded span with the given name.
func requireSpan(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()

	var found []tracetest.SpanStub
	for _, s := range spans {
		if s.Name == name {
			found = append(found, s)
		}
	}
	require.Len(t, found, 1, "expected exactly one %q span", name)
	return found[0]
}

// attrsOf renders a span's attributes as a string map for assertions.
func attrsOf(span tracetest.SpanStub) map[string]string {
	out := map[string]string{}
	for _, kv := range span.Attributes {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// assertChildOfRoot checks that child belongs to root's trace and is its direct child.
func assertChildOfRoot(t *testing.T, root, child tracetest.SpanStub) {
	t.Helper()
	assert.Equal(t, root.SpanContext.SpanID(), child.Parent.SpanID(),
		"%q must be a direct child of the cycle root", child.Name)
	assert.Equal(t, root.SpanContext.TraceID(), child.SpanContext.TraceID(),
		"%q must share the cycle's trace", child.Name)
}

// tracingModelRequest builds a minimal single-variant scale-up request.
func tracingModelRequest() ModelScalingRequest {
	return ModelScalingRequest{
		ModelID:   "model-a",
		Namespace: "default",
		Priority:  1,
		AnalyzerResults: []NamedAnalyzerResult{{
			Name: domain.SaturationAnalyzerName,
			Result: &domain.AnalyzerResult{
				RequiredCapacity: 1,
				VariantCapacities: []domain.VariantCapacity{{
					VariantName:        "variant-a",
					PerReplicaCapacity: 1,
					Cost:               1,
					AcceleratorName:    "A100",
				}},
			},
			Score:     1,
			Remaining: 1,
		}},
		VariantStates: []domain.VariantReplicaState{{
			VariantName:     "variant-a",
			CurrentReplicas: 1,
		}},
	}
}

func TestOptimizersEmitOptimizeSpanUnderCycleRoot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		optimizer ScalingOptimizer
	}{
		{"cost-aware", NewCostAwareOptimizer()},
		{"greedy-by-score", NewGreedyByScoreOptimizer()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exporter := withRecordingTracer(t)

			ctx, root := tracing.Tracer(tracerScope).Start(context.Background(), tracing.SpanReconcile)
			tc.optimizer.Optimize(ctx, []ModelScalingRequest{tracingModelRequest()}, nil)
			root.End()

			spans := exporter.GetSpans()
			rootSpan := requireSpan(t, spans, tracing.SpanReconcile)
			optimizeSpan := requireSpan(t, spans, tracing.SpanOptimize)

			assertChildOfRoot(t, rootSpan, optimizeSpan)

			attrs := attrsOf(optimizeSpan)
			assert.Equal(t, tc.optimizer.Name(), attrs[tracing.AttrOptimizer])
			assert.Equal(t, "1", attrs[tracing.AttrModelCount])
			assert.Contains(t, attrs, tracing.AttrDecisionCount)
		})
	}
}

func TestDefaultLimiterEmitsLimitSpanUnderCycleRoot(t *testing.T) {
	exporter := withRecordingTracer(t)

	limiter := NewDefaultLimiter("test-limiter",
		newMockInventory("test-inventory", map[string]int{"A100": 10}),
		&mockAlgorithm{name: "test-algo"})

	decisions := []*domain.VariantDecision{{
		VariantName:     "variant-a",
		ModelID:         "model-a",
		Namespace:       "default",
		AcceleratorName: "A100",
		GPUsPerReplica:  1,
		CurrentReplicas: 1,
		TargetReplicas:  2,
		Action:          domain.ActionScaleUp,
	}}

	ctx, root := tracing.Tracer(tracerScope).Start(context.Background(), tracing.SpanReconcile)
	require.NoError(t, limiter.Limit(ctx, decisions))
	root.End()

	spans := exporter.GetSpans()
	rootSpan := requireSpan(t, spans, tracing.SpanReconcile)
	limitSpan := requireSpan(t, spans, tracing.SpanLimit)

	assertChildOfRoot(t, rootSpan, limitSpan)

	attrs := attrsOf(limitSpan)
	assert.Equal(t, "test-limiter", attrs[tracing.AttrLimiter])
	assert.Equal(t, "1", attrs[tracing.AttrDecisionCount])
	assert.Contains(t, attrs, tracing.AttrLimitedCount)
}

func TestLimiterEmitsNoSpanForEmptyDecisions(t *testing.T) {
	exporter := withRecordingTracer(t)

	limiter := NewDefaultLimiter("test-limiter",
		newMockInventory("test-inventory", map[string]int{"A100": 10}),
		&mockAlgorithm{name: "test-algo"})

	ctx, root := tracing.Tracer(tracerScope).Start(context.Background(), tracing.SpanReconcile)
	require.NoError(t, limiter.Limit(ctx, nil))
	root.End()

	for _, s := range exporter.GetSpans() {
		assert.NotEqual(t, tracing.SpanLimit, s.Name,
			"an empty decision set does no work and must not open a stage span")
	}
}

func TestEnforcerEmitsEnforceSpanUnderCycleRoot(t *testing.T) {
	exporter := withRecordingTracer(t)

	// A positive request count keeps decisions as they are; this test is about
	// the span, not the enforcement outcome.
	enforcer := NewEnforcer(func(context.Context, string, string, time.Duration) (float64, error) {
		return 1, nil
	})

	decisions := []domain.VariantDecision{{
		VariantName:     "variant-a",
		ModelID:         "model-a",
		Namespace:       "default",
		Cost:            1,
		CurrentReplicas: 1,
		TargetReplicas:  2,
		Action:          domain.ActionScaleUp,
	}}

	ctx, root := tracing.Tracer(tracerScope).Start(context.Background(), tracing.SpanReconcile)
	enforcer.EnforcePolicyOnDecisions(ctx, "model-a", "default", decisions,
		config.ScaleToZeroConfigData{}, nil, "cost-aware")
	root.End()

	spans := exporter.GetSpans()
	rootSpan := requireSpan(t, spans, tracing.SpanReconcile)
	enforceSpan := requireSpan(t, spans, tracing.SpanEnforce)

	assertChildOfRoot(t, rootSpan, enforceSpan)

	attrs := attrsOf(enforceSpan)
	assert.Equal(t, "model-a", attrs[tracing.AttrModelID])
	assert.Equal(t, "default", attrs[tracing.AttrNamespace])
	assert.Equal(t, "1", attrs[tracing.AttrDecisionCount])
	assert.Equal(t, "cost-aware", attrs[tracing.AttrOptimizer])
}

func TestStagesEmitNoSpansWhenTracingDisabled(t *testing.T) {
	// No provider installed: the global no-op provider is what a disabled
	// deployment runs with. Nothing may be recorded.
	exporter := tracetest.NewInMemoryExporter()

	ctx, root := tracing.Tracer(tracerScope).Start(context.Background(), tracing.SpanReconcile)
	NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{tracingModelRequest()}, nil)
	NewEnforcer(func(context.Context, string, string, time.Duration) (float64, error) {
		return 1, nil
	}).EnforcePolicyOnDecisions(ctx, "model-a", "default", nil,
		config.ScaleToZeroConfigData{}, nil, "cost-aware")
	root.End()

	assert.False(t, root.SpanContext().IsValid(), "spans must not record while tracing is disabled")
	assert.Empty(t, exporter.GetSpans())
}
