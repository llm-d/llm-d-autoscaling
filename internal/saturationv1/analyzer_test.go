package saturationv1

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

func init() {
	// Initialize logger for tests
	logging.NewTestLogger()
}

func TestAnalyzeModelSaturation_ScaleUp(t *testing.T) {
	analyzer := NewAnalyzer()
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	tests := []struct {
		name                string
		replicaMetrics      []domain.ReplicaMetrics
		expectScaleUp       bool
		expectScaleUpReason string
	}{
		{
			name: "scale up due to low KV spare Saturation",
			replicaMetrics: []domain.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.75, QueueLength: 2},
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.76, QueueLength: 2},
			},
			expectScaleUp: true, // avg spare KV = 0.045 < 0.1
		},
		{
			name: "scale up due to low queue spare Saturation",
			replicaMetrics: []domain.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 3},
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 3},
			},
			expectScaleUp: true, // avg spare queue = 2 < 3
		},
		{
			name: "no scale up - healthy Saturation",
			replicaMetrics: []domain.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 1},
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 1},
			},
			expectScaleUp: false, // avg spare KV = 0.30, avg spare queue = 4
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysis, err := analyzer.AnalyzeModelSaturation(
				context.Background(),
				"test-model",
				"test-ns",
				tt.replicaMetrics,
				config,
			)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if analysis.ShouldScaleUp != tt.expectScaleUp {
				t.Errorf("expected ShouldScaleUp=%v, got %v (reason: %s)",
					tt.expectScaleUp, analysis.ShouldScaleUp, analysis.ScaleUpReason)
			}
		})
	}
}

func TestAnalyzeModelSaturation_ScaleDownSafety(t *testing.T) {
	analyzer := NewAnalyzer()
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	tests := []struct {
		name                string
		replicaMetrics      []domain.ReplicaMetrics
		expectScaleDownSafe bool
	}{
		{
			name: "scale down safe - adequate headroom",
			replicaMetrics: []domain.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.20, QueueLength: 1},
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.30, QueueLength: 1},
				{PodName: "pod-3", VariantName: "v1", KvCacheUsage: 0.25, QueueLength: 1},
			},
			expectScaleDownSafe: true,
		},
		{
			name: "scale down unsafe - insufficient headroom",
			replicaMetrics: []domain.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.70, QueueLength: 2},
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.75, QueueLength: 2},
			},
			expectScaleDownSafe: false,
		},
		{
			name: "scale down unsafe - only one non-saturated replica",
			replicaMetrics: []domain.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 2},
			},
			expectScaleDownSafe: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysis, err := analyzer.AnalyzeModelSaturation(
				context.Background(),
				"test-model",
				"test-ns",
				tt.replicaMetrics,
				config,
			)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if analysis.ScaleDownSafe != tt.expectScaleDownSafe {
				t.Errorf("expected ScaleDownSafe=%v, got %v",
					tt.expectScaleDownSafe, analysis.ScaleDownSafe)
			}
		})
	}
}

func TestAnalyzeModelSaturation_MultiVariant(t *testing.T) {
	analyzer := NewAnalyzer()
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	// Test with metrics from multiple variants
	replicaMetrics := []domain.ReplicaMetrics{
		// Variant 1
		{PodName: "v1-pod-1", VariantName: "variant-1", ModelID: "model-a", KvCacheUsage: 0.70, QueueLength: 2},
		{PodName: "v1-pod-2", VariantName: "variant-1", ModelID: "model-a", KvCacheUsage: 0.75, QueueLength: 3},
		// Variant 2
		{PodName: "v2-pod-1", VariantName: "variant-2", ModelID: "model-a", KvCacheUsage: 0.60, QueueLength: 1},
		{PodName: "v2-pod-2", VariantName: "variant-2", ModelID: "model-a", KvCacheUsage: 0.65, QueueLength: 2},
	}

	analysis, err := analyzer.AnalyzeModelSaturation(
		context.Background(),
		"model-a",
		"test-ns",
		replicaMetrics,
		config,
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify aggregation across variants
	if analysis.TotalReplicas != 4 {
		t.Errorf("expected TotalReplicas=4, got %d", analysis.TotalReplicas)
	}

	if analysis.NonSaturatedCount != 4 {
		t.Errorf("expected NonSaturatedCount=4, got %d", analysis.NonSaturatedCount)
	}

	if len(analysis.VariantAnalyses) != 2 {
		t.Errorf("expected 2 variant analyses, got %d", len(analysis.VariantAnalyses))
	}

	// Verify per-variant breakdown
	for _, va := range analysis.VariantAnalyses {
		if va.ReplicaCount != 2 {
			t.Errorf("expected ReplicaCount=2 for variant %s, got %d", va.VariantName, va.ReplicaCount)
		}
	}
}

func TestAnalyzeModelSaturation_EmptyMetrics(t *testing.T) {
	analyzer := NewAnalyzer()
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	analysis, err := analyzer.AnalyzeModelSaturation(
		context.Background(),
		"test-model",
		"test-ns",
		[]domain.ReplicaMetrics{},
		config,
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if analysis.TotalReplicas != 0 {
		t.Errorf("expected TotalReplicas=0, got %d", analysis.TotalReplicas)
	}

	if analysis.ShouldScaleUp {
		t.Errorf("expected ShouldScaleUp=false for empty metrics")
	}

	if analysis.ScaleDownSafe {
		t.Errorf("expected ScaleDownSafe=false for empty metrics")
	}
}

func TestAnalyzeVariant_SaturatedReplicas(t *testing.T) {
	analyzer := &Analyzer{}
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	metrics := []domain.ReplicaMetrics{
		{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.85, QueueLength: 2}, // Saturated (KV)
		{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 6}, // Saturated (Queue)
		{PodName: "pod-3", VariantName: "v1", KvCacheUsage: 0.60, QueueLength: 2}, // Not saturated
	}

	analysis := analyzer.analyzeVariant(context.Background(), "v1", metrics, config)

	if analysis.ReplicaCount != 3 {
		t.Errorf("expected ReplicaCount=3, got %d", analysis.ReplicaCount)
	}

	if analysis.NonSaturatedCount != 1 {
		t.Errorf("expected NonSaturatedCount=1, got %d", analysis.NonSaturatedCount)
	}

	if len(analysis.SaturatedReplicas) != 2 {
		t.Errorf("expected 2 saturated replicas, got %d", len(analysis.SaturatedReplicas))
	}

	// Verify saturated pods are tracked
	saturatedSet := make(map[string]bool)
	for _, pod := range analysis.SaturatedReplicas {
		saturatedSet[pod] = true
	}

	if !saturatedSet["pod-1"] || !saturatedSet["pod-2"] {
		t.Errorf("expected pod-1 and pod-2 to be saturated, got: %v", analysis.SaturatedReplicas)
	}
}

func TestAnalyzeModelSaturation_AllSaturated(t *testing.T) {
	analyzer := NewAnalyzer()
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	// All replicas are saturated
	replicaMetrics := []domain.ReplicaMetrics{
		{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.85, QueueLength: 2}, // Saturated (KV)
		{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 6}, // Saturated (Queue)
		{PodName: "pod-3", VariantName: "v1", KvCacheUsage: 0.90, QueueLength: 7}, // Saturated (both)
	}

	analysis, err := analyzer.AnalyzeModelSaturation(
		context.Background(),
		"test-model",
		"test-ns",
		replicaMetrics,
		config,
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// When all replicas are saturated
	if analysis.TotalReplicas != 3 {
		t.Errorf("expected TotalReplicas=3, got %d", analysis.TotalReplicas)
	}

	if analysis.NonSaturatedCount != 0 {
		t.Errorf("expected NonSaturatedCount=0, got %d", analysis.NonSaturatedCount)
	}

	// With no non-saturated replicas, average spare Saturation should be 0
	if analysis.AvgSpareKvCapacity != 0 {
		t.Errorf("expected AvgSpareKvSaturation=0, got %.3f", analysis.AvgSpareKvCapacity)
	}

	if analysis.AvgSpareQueueLength != 0 {
		t.Errorf("expected AvgSpareQueueLength=0, got %.1f", analysis.AvgSpareQueueLength)
	}

	// Should scale up when all replicas are saturated (0 spare Saturation < triggers)
	if !analysis.ShouldScaleUp {
		t.Errorf("expected ShouldScaleUp=true when all saturated (urgently needs more Saturation)")
	}

	// Scale-down should be unsafe
	if analysis.ScaleDownSafe {
		t.Errorf("expected ScaleDownSafe=false when all saturated")
	}
}

func TestAnalyzeModelSaturation_TimestampSet(t *testing.T) {
	analyzer := NewAnalyzer()
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	before := time.Now()

	replicaMetrics := []domain.ReplicaMetrics{
		{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 2, Cost: 10},
	}

	analysis, err := analyzer.AnalyzeModelSaturation(
		context.Background(),
		"test-model",
		"test-ns",
		replicaMetrics,
		config,
	)

	after := time.Now()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify timestamp is set and within reasonable range
	if analysis.AnalyzedAt.IsZero() {
		t.Errorf("expected AnalyzedAt to be set, but it's zero")
	}

	if analysis.AnalyzedAt.Before(before) || analysis.AnalyzedAt.After(after) {
		t.Errorf("AnalyzedAt timestamp %v is outside expected range [%v, %v]",
			analysis.AnalyzedAt, before, after)
	}
}

// Tests for two-step decision logic (CalculatesaturationTargets + ArbitrateWithModelBased)

func TestCalculatesaturationTargets_ScaleUpCheapest(t *testing.T) {
	analyzer := NewAnalyzer()

	saturationAnalysis := &domain.ModelSaturationAnalysis{
		ModelID:       "test-model",
		Namespace:     "test-ns",
		ShouldScaleUp: true,
		ScaleUpReason: "KV spare Saturation low",
		VariantAnalyses: []domain.VariantSaturationAnalysis{
			{VariantName: "v1-expensive", Cost: 20, ReplicaCount: 2},
			{VariantName: "v2-cheap", Cost: 5, ReplicaCount: 2},
			{VariantName: "v3-medium", Cost: 15, ReplicaCount: 2},
		},
	}

	variantStates := []domain.VariantReplicaState{
		{VariantName: "v1-expensive", CurrentReplicas: 2, DesiredReplicas: 0},
		{VariantName: "v2-cheap", CurrentReplicas: 2, DesiredReplicas: 0},
		{VariantName: "v3-medium", CurrentReplicas: 2, DesiredReplicas: 0},
	}

	targets := analyzer.CalculateSaturationTargets(context.Background(), saturationAnalysis, variantStates)

	// Should scale up cheapest variant (v2-cheap)
	if targets["v2-cheap"] != 3 {
		t.Errorf("expected v2-cheap target=3, got %d", targets["v2-cheap"])
	}

	// Others should remain at current
	if targets["v1-expensive"] != 2 {
		t.Errorf("expected v1-expensive target=2, got %d", targets["v1-expensive"])
	}
	if targets["v3-medium"] != 2 {
		t.Errorf("expected v3-medium target=2, got %d", targets["v3-medium"])
	}
}

func TestCalculatesaturationTargets_ScaleDownMostExpensive(t *testing.T) {
	analyzer := NewAnalyzer()

	saturationAnalysis := &domain.ModelSaturationAnalysis{
		ModelID:       "test-model",
		Namespace:     "test-ns",
		ShouldScaleUp: false,
		ScaleDownSafe: true,
		VariantAnalyses: []domain.VariantSaturationAnalysis{
			{VariantName: "v1-expensive", Cost: 20, ReplicaCount: 2},
			{VariantName: "v2-cheap", Cost: 5, ReplicaCount: 2},
			{VariantName: "v3-medium", Cost: 15, ReplicaCount: 2},
		},
	}

	variantStates := []domain.VariantReplicaState{
		{VariantName: "v1-expensive", CurrentReplicas: 2, DesiredReplicas: 0},
		{VariantName: "v2-cheap", CurrentReplicas: 2, DesiredReplicas: 0},
		{VariantName: "v3-medium", CurrentReplicas: 2, DesiredReplicas: 0},
	}

	targets := analyzer.CalculateSaturationTargets(context.Background(), saturationAnalysis, variantStates)

	// Should scale down most expensive variant (v1-expensive)
	if targets["v1-expensive"] != 1 {
		t.Errorf("expected v1-expensive target=1, got %d", targets["v1-expensive"])
	}

	// Others should remain at current
	if targets["v2-cheap"] != 2 {
		t.Errorf("expected v2-cheap target=2, got %d", targets["v2-cheap"])
	}
	if targets["v3-medium"] != 2 {
		t.Errorf("expected v3-medium target=2, got %d", targets["v3-medium"])
	}
}

func TestCalculatesaturationTargets_ModelLevelTransitionBlocking(t *testing.T) {
	analyzer := NewAnalyzer()

	saturationAnalysis := &domain.ModelSaturationAnalysis{
		ModelID:       "test-model",
		Namespace:     "test-ns",
		ShouldScaleUp: true,
		ScaleUpReason: "KV spare Saturation low",
		VariantAnalyses: []domain.VariantSaturationAnalysis{
			{VariantName: "v1-expensive", Cost: 20, ReplicaCount: 2},
			{VariantName: "v2-cheap", Cost: 5, ReplicaCount: 2},
		},
	}

	// v1 has desired > current (previous optimizer wanted to scale up)
	// This puts the MODEL in transition state, blocking all scaling decisions
	variantStates := []domain.VariantReplicaState{
		{VariantName: "v1-expensive", CurrentReplicas: 2, DesiredReplicas: 4},
		{VariantName: "v2-cheap", CurrentReplicas: 2, DesiredReplicas: 0},
	}

	targets := analyzer.CalculateSaturationTargets(context.Background(), saturationAnalysis, variantStates)

	// v1 should preserve its desired replicas (transition in progress)
	if targets["v1-expensive"] != 4 {
		t.Errorf("expected v1-expensive target=4 (preserved desired), got %d", targets["v1-expensive"])
	}

	// v2 should NOT be scaled up because model is in transition (v1 is transitioning)
	// Model-level transition protection blocks all scaling decisions
	if targets["v2-cheap"] != 2 {
		t.Errorf("expected v2-cheap target=2 (blocked by model transition), got %d", targets["v2-cheap"])
	}
}

func TestCalculatesaturationTargets_PendingReplicasBlocksScaling(t *testing.T) {
	analyzer := NewAnalyzer()

	saturationAnalysis := &domain.ModelSaturationAnalysis{
		ModelID:       "test-model",
		Namespace:     "test-ns",
		ShouldScaleUp: true,
		ScaleUpReason: "KV spare Saturation low",
		VariantAnalyses: []domain.VariantSaturationAnalysis{
			{VariantName: "v1-expensive", Cost: 20, ReplicaCount: 2},
			{VariantName: "v2-cheap", Cost: 5, ReplicaCount: 2},
		},
	}

	// v1 has a pending replica (3 current, only 2 ready) - scale-up in progress.
	// This puts the MODEL in transition state, blocking all scaling decisions.
	variantStates := []domain.VariantReplicaState{
		{VariantName: "v1-expensive", CurrentReplicas: 3, DesiredReplicas: 0, PendingReplicas: 1},
		{VariantName: "v2-cheap", CurrentReplicas: 2, DesiredReplicas: 0},
	}

	targets := analyzer.CalculateSaturationTargets(context.Background(), saturationAnalysis, variantStates)

	// v1 should stay at current replicas (pods still starting)
	if targets["v1-expensive"] != 3 {
		t.Errorf("expected v1-expensive target=3 (current, pods pending), got %d", targets["v1-expensive"])
	}

	// v2 should NOT be scaled up because model is in transition (v1 has pending replicas)
	if targets["v2-cheap"] != 2 {
		t.Errorf("expected v2-cheap target=2 (blocked by model transition), got %d", targets["v2-cheap"])
	}
}

// TestCalculatesaturationTargets_LWSGroupSizeDoesNotBlock is the regression test
// for #1302: a LeaderWorkerSet variant with group size > 1 reports more metric
// pods (ReplicaCount) than it has replicas/groups (CurrentReplicas). The old
// metrics(ReplicaCount) != current(CurrentReplicas) transition check treated this
// as a permanent transition and blocked all scaling. With the pending-based
// check, a stable LWS variant scales normally and targets are in group units.
func TestCalculatesaturationTargets_LWSGroupSizeDoesNotBlock(t *testing.T) {
	analyzer := NewAnalyzer()

	saturationAnalysis := &domain.ModelSaturationAnalysis{
		ModelID:       "test-model",
		Namespace:     "test-ns",
		ShouldScaleUp: true,
		ScaleUpReason: "KV spare Saturation low",
		VariantAnalyses: []domain.VariantSaturationAnalysis{
			// LWS group size 2: 1 group, 2 pods reporting metrics.
			{VariantName: "lws-variant", Cost: 5, ReplicaCount: 2},
		},
	}

	// 1 group (CurrentReplicas), fully ready (PendingReplicas 0). ReplicaCount (2
	// metric pods) deliberately differs from CurrentReplicas (1 group).
	variantStates := []domain.VariantReplicaState{
		{VariantName: "lws-variant", CurrentReplicas: 1, DesiredReplicas: 0, PendingReplicas: 0},
	}

	targets := analyzer.CalculateSaturationTargets(context.Background(), saturationAnalysis, variantStates)

	// Not blocked: scales up by one group, in group units (1 -> 2), NOT pods (2 -> 3).
	if targets["lws-variant"] != 2 {
		t.Errorf("expected lws-variant target=2 (1 group scaled up by 1, in group units), got %d", targets["lws-variant"])
	}
}

func TestAnalyzeVariant_AvgKvCacheUsage(t *testing.T) {
	analyzer := &Analyzer{}
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	tests := []struct {
		name               string
		metrics            []domain.ReplicaMetrics
		expectedAvgKvUsage float64
		expectedMaxKvUsage float64
	}{
		{
			name: "uniform KV cache usage",
			metrics: []domain.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 2},
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 2},
				{PodName: "pod-3", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 2},
			},
			expectedAvgKvUsage: 0.50,
			expectedMaxKvUsage: 0.50,
		},
		{
			name: "mixed KV cache usage",
			metrics: []domain.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.30, QueueLength: 2},
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.60, QueueLength: 2},
				{PodName: "pod-3", VariantName: "v1", KvCacheUsage: 0.90, QueueLength: 2},
			},
			expectedAvgKvUsage: 0.60, // (0.30 + 0.60 + 0.90) / 3
			expectedMaxKvUsage: 0.90,
		},
		{
			name: "mixed saturated and non-saturated",
			metrics: []domain.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.85, QueueLength: 2}, // Saturated
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.40, QueueLength: 2}, // Not saturated
				{PodName: "pod-3", VariantName: "v1", KvCacheUsage: 0.55, QueueLength: 2}, // Not saturated
			},
			expectedAvgKvUsage: 0.60, // (0.85 + 0.40 + 0.55) / 3 - includes ALL replicas
			expectedMaxKvUsage: 0.85,
		},
		{
			name:               "empty metrics",
			metrics:            []domain.ReplicaMetrics{},
			expectedAvgKvUsage: 0.0,
			expectedMaxKvUsage: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysis := analyzer.analyzeVariant(context.Background(), "v1", tt.metrics, config)

			if analysis.AvgKvCacheUsage != tt.expectedAvgKvUsage {
				t.Errorf("expected AvgKvCacheUsage=%.2f, got %.2f", tt.expectedAvgKvUsage, analysis.AvgKvCacheUsage)
			}

			if analysis.MaxKvCacheUsage != tt.expectedMaxKvUsage {
				t.Errorf("expected MaxKvCacheUsage=%.2f, got %.2f", tt.expectedMaxKvUsage, analysis.MaxKvCacheUsage)
			}
		})
	}
}

// TestAnalyzeVariant_MissingMetricsTreatedAsSaturated is a regression test for
// issue #360: a replica whose KV cache or queue metric was not scraped this
// cycle reports KvCacheUsage/QueueLength as 0 (the collector's placeholder).
// Before the fix, that 0 was indistinguishable from a genuinely idle replica,
// so a saturated pod suffering a temporary scrape failure was misread as
// having full spare capacity. The analyzer must instead treat a missing
// metric as saturated (fail conservative) and must exclude the placeholder 0
// from the KV usage average/max so it isn't dragged down by data that was
// never really collected.
func TestAnalyzeVariant_MissingMetricsTreatedAsSaturated(t *testing.T) {
	analyzer := &Analyzer{}
	cfg := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	tests := []struct {
		name                    string
		metrics                 []domain.ReplicaMetrics
		expectSaturatedPods     []string
		expectNonSaturatedCount int
		expectedAvgKvUsage      float64
		expectedMaxKvUsage      float64
	}{
		{
			name: "pod with missing KV metric is saturated despite reading 0",
			metrics: []domain.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0, KvCacheUsageMissing: true, QueueLength: 1},
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.40, QueueLength: 1},
			},
			expectSaturatedPods:     []string{"pod-1"},
			expectNonSaturatedCount: 1,
			expectedAvgKvUsage:      0.40, // pod-1's placeholder 0 is excluded
			expectedMaxKvUsage:      0.40,
		},
		{
			name: "pod with missing queue metric is saturated despite reading 0",
			metrics: []domain.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.30, QueueLength: 0, QueueLengthMissing: true},
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.30, QueueLength: 1},
			},
			expectSaturatedPods:     []string{"pod-1"},
			expectNonSaturatedCount: 1,
			expectedAvgKvUsage:      0.30, // pod-1's KV reading was real, so it still counts
			expectedMaxKvUsage:      0.30,
		},
		{
			name: "all replicas missing metrics yields zero-sample average, not zero usage",
			metrics: []domain.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0, KvCacheUsageMissing: true, QueueLength: 0, QueueLengthMissing: true},
			},
			expectSaturatedPods:     []string{"pod-1"},
			expectNonSaturatedCount: 0,
			expectedAvgKvUsage:      0.0, // no real sample exists to average
			expectedMaxKvUsage:      0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysis := analyzer.analyzeVariant(context.Background(), "v1", tt.metrics, cfg)

			if len(analysis.SaturatedReplicas) != len(tt.expectSaturatedPods) {
				t.Fatalf("expected SaturatedReplicas=%v, got %v", tt.expectSaturatedPods, analysis.SaturatedReplicas)
			}
			for i, pod := range tt.expectSaturatedPods {
				if analysis.SaturatedReplicas[i] != pod {
					t.Errorf("expected SaturatedReplicas[%d]=%s, got %s", i, pod, analysis.SaturatedReplicas[i])
				}
			}
			if analysis.NonSaturatedCount != tt.expectNonSaturatedCount {
				t.Errorf("expected NonSaturatedCount=%d, got %d", tt.expectNonSaturatedCount, analysis.NonSaturatedCount)
			}
			if analysis.AvgKvCacheUsage != tt.expectedAvgKvUsage {
				t.Errorf("expected AvgKvCacheUsage=%.2f, got %.2f", tt.expectedAvgKvUsage, analysis.AvgKvCacheUsage)
			}
			if analysis.MaxKvCacheUsage != tt.expectedMaxKvUsage {
				t.Errorf("expected MaxKvCacheUsage=%.2f, got %.2f", tt.expectedMaxKvUsage, analysis.MaxKvCacheUsage)
			}
		})
	}
}

// TestAnalyzeModelSaturation_MissingMetricsBlockUnsafeScaleDown is a
// regression test for issue #360 at the model-level decision: before the fix,
// a saturated pod with a temporarily missing KV metric was counted as
// non-saturated with full spare capacity, which could report scale-down as
// safe when it was not.
func TestAnalyzeModelSaturation_MissingMetricsBlockUnsafeScaleDown(t *testing.T) {
	analyzer := NewAnalyzer()
	cfg := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	// Two replicas: one genuinely healthy, one actually saturated but its KV
	// metric failed to scrape this cycle (mamy-CS's scenario from #360).
	metrics := []domain.ReplicaMetrics{
		{PodName: "healthy", VariantName: "v1", KvCacheUsage: 0.30, QueueLength: 1},
		{PodName: "saturated-but-unscraped", VariantName: "v1", KvCacheUsage: 0, KvCacheUsageMissing: true, QueueLength: 0, QueueLengthMissing: true},
	}

	analysis, err := analyzer.AnalyzeModelSaturation(context.Background(), "test-model", "test-ns", metrics, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if analysis.ScaleDownSafe {
		t.Errorf("expected ScaleDownSafe=false when a replica's metrics are missing, got true")
	}
}
