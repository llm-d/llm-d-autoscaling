package saturationv1

// Saturation analyzer constants
const (
	// MinNonSaturatedReplicasForScaleDown is the minimum number of non-saturated replicas
	// required before scale-down is considered safe. With fewer replicas, the system
	// cannot safely redistribute load without risking saturation.
	MinNonSaturatedReplicasForScaleDown = 2

	// DefaultVariantCost is the fallback cost used when variant cost is not specified
	// in the VariantAutoscaling CR. This should match the cost of the cheapest accelerator
	// to avoid biasing decisions toward unknown-cost variants.
	DefaultVariantCost = 10.0
)
