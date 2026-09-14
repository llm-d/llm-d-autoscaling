# Metrics Health Monitoring

## Overview

The Workload Variant Autoscaler (WVA) includes a comprehensive metrics health monitoring system that validates vLLM metrics availability and provides clear status feedback through Kubernetes conditions. This feature helps operators quickly diagnose issues with ServiceMonitor configuration and Prometheus scraping.

## Status Conditions

WVA now exposes two status conditions on each `VariantAutoscaling` resource:

### 1. MetricsAvailable

Indicates whether vLLM metrics are available from Prometheus for the variant.

**Status Values:**
- `True`: Metrics are available and up-to-date
- `False`: Metrics are missing, stale, or Prometheus query failed

**Reasons:**
- `MetricsFound`: Metrics successfully retrieved and up-to-date
- `MetricsMissing`: No vLLM metrics found (likely ServiceMonitor misconfiguration)
- `MetricsStale`: Metrics exist but are outdated (>5 minutes old)
- `PrometheusError`: Error querying Prometheus API

### 2. OptimizationReady

Indicates whether the optimization engine can run successfully.

**Status Values:**
- `True`: Optimization completed successfully
- `False`: Optimization cannot run or failed

**Reasons:**
- `OptimizationSucceeded`: Optimization completed and replicas calculated
- `OptimizationFailed`: Optimization engine failed
- `MetricsUnavailable`: Cannot optimize without valid metrics

## Viewing Status Conditions

### Using kubectl

```bash
# View all VariantAutoscaling resources with metrics status
kubectl get variantautoscaling -A

# Example output:
# NAME              MODEL                    ACCELERATOR  CURRENTREPLICAS  OPTIMIZED  METRICSREADY  AGE
# llama-variant     meta-llama/Llama-3-8b    A100         2                3          True          5m
# mistral-variant   mistralai/Mistral-7B     A100         1                2          False         3m
```

### Detailed Condition Information

```bash
# Get detailed status for a specific VariantAutoscaling
kubectl describe variantautoscaling <name> -n <namespace>

# Or use jsonpath to extract conditions
kubectl get variantautoscaling <name> -n <namespace> -o jsonpath='{.status.conditions}' | jq
```

Example output:
```json
[
  {
    "type": "MetricsAvailable",
    "status": "False",
    "reason": "MetricsMissing",
    "message": "No vLLM metrics found for model 'meta-llama/Llama-3-8b' in namespace 'default'. Ensure:\n1. ServiceMonitor is created in the monitoring namespace\n2. ServiceMonitor selector matches vLLM service labels\n3. vLLM pods are running and exposing /metrics endpoint\n4. Prometheus is scraping the monitoring namespace",
    "lastTransitionTime": "2025-01-15T10:30:00Z",
    "observedGeneration": 1
  },
  {
    "type": "OptimizationReady",
    "status": "False",
    "reason": "MetricsUnavailable",
    "message": "Cannot optimize without metrics: No vLLM metrics found...",
    "lastTransitionTime": "2025-01-15T10:30:00Z",
    "observedGeneration": 1
  }
]
```

## Graceful Degradation

When metrics are unavailable, WVA implements graceful degradation:

1. **Skips optimization** for affected variants (no scaling decisions)
2. **Maintains current replica count** (doesn't scale to zero or make random changes)
3. **Updates status conditions** with actionable error messages
4. **Continues monitoring** and retries on next reconciliation interval
5. **Other variants continue to optimize** if their metrics are available

## Architecture

### Metrics Validation Flow

```
1. Controller reconciles VariantAutoscaling
2. For each variant:
   a. Validate metrics availability (ValidateMetricsAvailability)
   b. Set MetricsAvailable condition
   c. If metrics unavailable:
      - Set OptimizationReady=False
      - Update status
      - Skip optimization (graceful degradation)
      - Continue to next variant
   d. If metrics available:
      - Collect metrics
      - Continue with optimization
3. Run optimization for all variants with valid metrics
4. Set OptimizationReady condition based on optimization result
5. Update status for all variants
```

### Key Components

- **`collector.ValidateMetricsAvailability()`**: Validates metrics and returns structured result
- **`api/v1alpha1.SetCondition()`**: Helper to set status conditions
- **Controller**: Integrates validation and updates conditions
- **CRD**: Includes conditions field and MetricsReady printcolumn

## Best Practices

1. **Monitor the MetricsReady column** in your operational dashboards
2. **Set up alerts** for prolonged MetricsAvailable=False conditions
3. **Review condition messages** for troubleshooting guidance
4. **Validate ServiceMonitor** configuration during initial deployment
5. **Test metrics flow** before relying on WVA for production autoscaling

## Related Documentation

- [Prometheus Integration (Custom Metrics)](./integrations/prometheus.md)
- [ServiceMonitor Configuration](../config/prometheus)

