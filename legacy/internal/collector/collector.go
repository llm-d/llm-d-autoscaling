// Package collector provides metrics collection functionality.
//
// The collector package provides a pluggable metrics collection system with support for
// multiple backends (Prometheus, EPP). Use factory.NewMetricsCollector() to create collector
// instances, and the MetricsCollector interface from internal/domain to interact with them.
//
// Note: Some legacy functions in this package (ValidateMetricsAvailability, AddMetricsToOptStatus)
// are deprecated. See individual function documentation for details.
package collector

// This file contains deprecated compatibility functions that delegate to the new
// MetricsCollector interface. These functions are kept for backward compatibility
// but should not be used in new code.

import (
	"context"
	"errors"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/discovery"
)

type AcceleratorModelInfo = discovery.AcceleratorModelInfo

// The WVA currently operates in unlimited mode only, where each variant receives
// optimal allocation independently without cluster capacity constraints.
// Limited mode support requires integration with the llmd stack and additional
// design work to handle degraded mode operations without violating SLOs.
// Future work: Implement CollectInventoryK8S and capacity-aware allocation for limited mode.

// CollectInventoryK8S provides accelerator inventory using the discovery mechanism.
func CollectInventoryK8S(ctx context.Context, r interface{}) (map[string]map[string]AcceleratorModelInfo, error) {
	c, ok := r.(client.Client)
	if !ok {
		return nil, errors.New("invalid client type: expected client.Client")
	}

	// Use the K8sWithGpuOperator discovery mechanism
	// TODO: Make this configurable or dynamic based on environment
	disc := &discovery.K8sWithGpuOperator{Client: c}
	return disc.Discover(ctx)
}
