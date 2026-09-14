package saturation

import (
	"context"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

func init() {
	logging.NewTestLogger()
}

func TestConvertSaturationTargetsToDecisions_V1Utilization(t *testing.T) {
	engine := &Engine{}
	ctx := context.Background()

	saturationAnalysis := &domain.ModelSaturationAnalysis{
		ModelID:   "test-model",
		Namespace: "test-ns",
		VariantAnalyses: []domain.VariantSaturationAnalysis{
			{
				VariantName:        "variant-a",
				AcceleratorName:    "nvidia-a100",
				Cost:               10.0,
				AvgKvCacheUsage:    0.65,
				AvgSpareKvCapacity: 0.15,
			},
			{
				VariantName:        "variant-b",
				AcceleratorName:    "nvidia-h100",
				Cost:               15.0,
				AvgKvCacheUsage:    0.80,
				AvgSpareKvCapacity: 0.05,
			},
		},
	}

	saturationTargets := map[string]int{
		"variant-a": 3,
		"variant-b": 5,
	}

	variantStates := []domain.VariantReplicaState{
		{VariantName: "variant-a", CurrentReplicas: 2, DesiredReplicas: 0, GPUsPerReplica: 1},
		{VariantName: "variant-b", CurrentReplicas: 4, DesiredReplicas: 0, GPUsPerReplica: 2},
	}

	decisions := engine.convertSaturationTargetsToDecisions(ctx, saturationTargets, saturationAnalysis, variantStates)

	if len(decisions) != 2 {
		t.Fatalf("expected 2 decisions, got %d", len(decisions))
	}

	// Find decisions by variant name
	decisionMap := make(map[string]domain.VariantDecision)
	for _, d := range decisions {
		decisionMap[d.VariantName] = d
	}

	// Verify variant-a decision
	if d, ok := decisionMap["variant-a"]; ok {
		if d.Utilization != 0.65 {
			t.Errorf("variant-a: expected Utilization=0.65, got %.2f", d.Utilization)
		}
		if d.SpareCapacity != 0.15 {
			t.Errorf("variant-a: expected SpareCapacity=0.15, got %.2f", d.SpareCapacity)
		}
		if d.AcceleratorName != "nvidia-a100" {
			t.Errorf("variant-a: expected AcceleratorName=nvidia-a100, got %s", d.AcceleratorName)
		}
	} else {
		t.Error("variant-a decision not found")
	}

	// Verify variant-b decision
	if d, ok := decisionMap["variant-b"]; ok {
		if d.Utilization != 0.80 {
			t.Errorf("variant-b: expected Utilization=0.80, got %.2f", d.Utilization)
		}
		if d.SpareCapacity != 0.05 {
			t.Errorf("variant-b: expected SpareCapacity=0.05, got %.2f", d.SpareCapacity)
		}
		if d.AcceleratorName != "nvidia-h100" {
			t.Errorf("variant-b: expected AcceleratorName=nvidia-h100, got %s", d.AcceleratorName)
		}
	} else {
		t.Error("variant-b decision not found")
	}
}
