package utils

import (
	"strconv"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// BuildAllocationFromMetrics assembles an Allocation struct from raw optimizer metrics
// and Kubernetes resources. This is responsible for:
// - Converting raw metrics (seconds -> milliseconds, formatting strings)
// - Extracting K8s information (replicas, accelerator, cost calculation)
// - Assembling the final Allocation struct
//
// This function is placed in utils to avoid import cycles between collector and controller packages.
func BuildAllocationFromMetrics(
	metrics domain.OptimizerMetrics,
	va *variant.VariantAutoscaling,
	scaleTarget scaletarget.ScaleTargetAccessor,
	acceleratorCost float64,
) (domain.Allocation, error) {
	// Extract K8s information
	// Number of replicas
	var numReplicas int
	if scaleTarget.GetReplicas() != nil {
		numReplicas = int(*scaleTarget.GetReplicas())
	} else {
		numReplicas = int(constants.SpecReplicasFallback)
	}

	// Accelerator type - extract from deployment/LWS nodeSelector/nodeAffinity or VA labels.
	// When unresolved, GetAcceleratorNameFromScaleTarget returns the sentinel value
	// ("unknown") so that metrics collection can proceed for deployments without
	// nodeSelector (common in homogeneous GPU clusters). The GPU limiter resolves
	// the sentinel to the real type in homogeneous clusters before it reaches
	// status or metrics.
	acc := accelerator.GetAcceleratorNameFromScaleTarget(va, scaleTarget)

	// Calculate variant cost
	// VariantCost removed from Status as it is duplicated from Spec (per-replica cost)
	// or derived (total cost).

	// Max batch size - TODO: collect value from server
	maxBatch := 256

	// Convert metrics and format values to meet CRD validation regex '^\\d+(\\.\\d+)?$'
	// Convert seconds to milliseconds for TTFT and ITL
	ttftMilliseconds := metrics.TTFTSeconds * 1000
	itlMilliseconds := metrics.ITLSeconds * 1000

	ttftAverageStr := strconv.FormatFloat(ttftMilliseconds, 'f', 2, 64)
	itlAverageStr := strconv.FormatFloat(itlMilliseconds, 'f', 2, 64)
	arrivalRateStr := strconv.FormatFloat(metrics.ArrivalRate, 'f', 2, 64)
	avgInputTokensStr := strconv.FormatFloat(metrics.AvgInputTokens, 'f', 2, 64)
	avgOutputTokensStr := strconv.FormatFloat(metrics.AvgOutputTokens, 'f', 2, 64)

	// Build Allocation struct
	allocation := domain.Allocation{
		Accelerator: acc,
		NumReplicas: numReplicas,
		MaxBatch:    maxBatch,
		// VariantCost removed from Status
		TTFTAverage: ttftAverageStr,
		ITLAverage:  itlAverageStr,
		Load: domain.LoadProfile{
			ArrivalRate:     arrivalRateStr,
			AvgInputTokens:  avgInputTokensStr,
			AvgOutputTokens: avgOutputTokensStr,
		},
	}

	return allocation, nil
}
