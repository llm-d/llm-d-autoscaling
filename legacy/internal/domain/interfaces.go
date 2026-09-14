package domain

import (
	"context"

	llmdOptv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

type Actuator interface {
	// EmitMetrics publishes metrics for external autoscalers (e.g., HPA, KEDA).
	// This includes real-time current state and Inferno's optimization targets.
	EmitMetrics(
		ctx context.Context,
		VariantAutoscalings *llmdOptv1alpha1.VariantAutoscaling,
	) error
}
