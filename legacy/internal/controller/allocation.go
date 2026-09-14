package controller

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// BuildAllocationFromMetrics assembles an Allocation struct from raw optimizer metrics
// and Kubernetes resources. This delegates to utils.BuildAllocationFromMetrics.
func BuildAllocationFromMetrics(
	metrics domain.OptimizerMetrics,
	va *variant.VariantAutoscaling,
	scaleTarget scaletarget.ScaleTargetAccessor,
	acceleratorCost float64,
) (domain.Allocation, error) {
	return utils.BuildAllocationFromMetrics(metrics, va, scaleTarget, acceleratorCost)
}
