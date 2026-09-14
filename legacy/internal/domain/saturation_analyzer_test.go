package domain

import (
	"testing"
)

func TestVariantDecision_SetDecisionReason(t *testing.T) {
	tests := []struct {
		name               string
		action             SaturationAction
		decisionReason     DecisionReason
		detailedReason     string
		wantDecisionReason string
	}{
		{
			name:               "V2 scale-up",
			action:             ActionScaleUp,
			decisionReason:     DecisionReasonV2,
			detailedReason:     "V2 (optimizer: cost-aware)",
			wantDecisionReason: "V2",
		},
		{
			name:               "V2 scale-down",
			action:             ActionScaleDown,
			decisionReason:     DecisionReasonV2,
			detailedReason:     "V2 (optimizer: cost-aware)",
			wantDecisionReason: "V2",
		},
		{
			name:               "V2 steady state",
			action:             ActionNoChange,
			decisionReason:     DecisionReasonV2,
			detailedReason:     "V2",
			wantDecisionReason: "V2",
		},
		{
			name:               "V2 enforced",
			action:             ActionScaleUp,
			decisionReason:     DecisionReasonV2,
			detailedReason:     "V2 (optimizer: cost-aware, enforced)",
			wantDecisionReason: "V2",
		},
		{
			name:               "Test decision",
			action:             ActionScaleUp,
			decisionReason:     DecisionReasonTest,
			detailedReason:     "test",
			wantDecisionReason: "test",
		},
		{
			name:               "saturation-only mode",
			action:             ActionScaleUp,
			decisionReason:     DecisionReasonSaturationOnly,
			detailedReason:     "saturation-only mode: scale-up",
			wantDecisionReason: "saturation-only mode",
		},
		{
			name:               "scale-from-zero",
			action:             ActionScaleUp,
			decisionReason:     DecisionReasonScaleFromZero,
			detailedReason:     "scale-from-zero: pending request - scale-up",
			wantDecisionReason: "scale-from-zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := &VariantDecision{}
			decision.SetDecisionReason(tt.action, tt.decisionReason, tt.detailedReason)

			if decision.Action != tt.action {
				t.Errorf("SetDecisionReason() Action = %v, want %v", decision.Action, tt.action)
			}
			if string(decision.ReasonCategory()) != tt.wantDecisionReason {
				t.Errorf("SetDecisionReason() ReasonCategory() = %v, want %v", decision.ReasonCategory(), tt.wantDecisionReason)
			}
			if decision.Reason() != tt.detailedReason {
				t.Errorf("SetDecisionReason() Reason() = %v, want %v", decision.Reason(), tt.detailedReason)
			}
		})
	}
}

func TestVariantDecision_SetDecisionReason_MultipleUpdates(t *testing.T) {
	decision := &VariantDecision{}

	// First update
	decision.SetDecisionReason(ActionScaleUp, DecisionReasonV2, "V2 (optimizer: cost-aware)")
	if string(decision.ReasonCategory()) != "V2" {
		t.Errorf("First update: ReasonCategory() = %v, want %v", decision.ReasonCategory(), "V2")
	}
	if decision.Reason() != "V2 (optimizer: cost-aware)" {
		t.Errorf("First update: Reason() = %v, want %v", decision.Reason(), "V2 (optimizer: cost-aware)")
	}

	// Second update should overwrite
	decision.SetDecisionReason(ActionScaleUp, DecisionReasonV2, "V2 (optimizer: cost-aware, enforced)")
	if string(decision.ReasonCategory()) != "V2" {
		t.Errorf("Second update: ReasonCategory() = %v, want %v", decision.ReasonCategory(), "V2")
	}
	if decision.Reason() != "V2 (optimizer: cost-aware, enforced)" {
		t.Errorf("Second update: Reason() = %v, want %v", decision.Reason(), "V2 (optimizer: cost-aware, enforced)")
	}
}
