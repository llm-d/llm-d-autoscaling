# Prometheus Integration

WVA integrates with Prometheus to collect metrics from different sources such as vLLM inference servers, and expose internal as well as custom autoscaling metrics. This guide covers Prometheus configuration, metric collection, and security best practices.

## Configuration

WVA supports two methods for configuring Prometheus connectivity:

### 1. Environment Variables (Recommended)

Set Prometheus configuration via environment variables in the WVA deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller-manager
spec:
  template:
    spec:
      containers:
      - name: manager
        env:
        # Required: Prometheus server URL
        - name: PROMETHEUS_BASE_URL
          value: "https://prometheus-k8s.monitoring.svc.cluster.local:9091"

        # Optional: TLS configuration
        - name: PROMETHEUS_TLS_INSECURE_SKIP_VERIFY
          value: "false"  # Set to "true" only for testing/development

        - name: PROMETHEUS_CA_CERT_PATH
          value: "/etc/prometheus-certs/ca.crt"

        - name: PROMETHEUS_CLIENT_CERT_PATH
          value: "/etc/prometheus-certs/client.crt"

        - name: PROMETHEUS_CLIENT_KEY_PATH
          value: "/etc/prometheus-certs/client.key"

        - name: PROMETHEUS_SERVER_NAME
          value: "prometheus-k8s.monitoring.svc.cluster.local"

        # Optional: Bearer token authentication
        - name: PROMETHEUS_BEARER_TOKEN
          valueFrom:
            secretKeyRef:
              name: prometheus-token
              key: token
```

**Environment Variable Reference:**

| Variable | Required | Description | Default |
|----------|----------|-------------|---------|
| `PROMETHEUS_BASE_URL` | Yes | Prometheus server URL (HTTPS only in production) | - |
| `PROMETHEUS_ALLOW_HTTP` | No | Allow a plain `http://` `PROMETHEUS_BASE_URL` (dev/test only; cannot be combined with TLS settings or bearer token auth) | `false` |
| `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` | No | Skip TLS certificate verification (dev/test only) | `false` |
| `PROMETHEUS_CA_CERT_PATH` | No | Path to CA certificate for TLS verification | - |
| `PROMETHEUS_CLIENT_CERT_PATH` | No | Path to client certificate for mutual TLS | - |
| `PROMETHEUS_CLIENT_KEY_PATH` | No | Path to client private key for mutual TLS | - |
| `PROMETHEUS_SERVER_NAME` | No | Expected server name in TLS certificate | - |
| `PROMETHEUS_BEARER_TOKEN` | No | Bearer token for Prometheus authentication | - |

### 2. ConfigMap Configuration

Alternatively, configure Prometheus via the controller's ConfigMap:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-manager-config
  namespace: workload-variant-autoscaler-system
data:
  PROMETHEUS_BASE_URL: "https://prometheus-k8s.monitoring.svc.cluster.local:9091"
  PROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "false"
  PROMETHEUS_CA_CERT_PATH: "/etc/prometheus-certs/ca.crt"
  PROMETHEUS_CLIENT_CERT_PATH: "/etc/prometheus-certs/client.crt"
  PROMETHEUS_CLIENT_KEY_PATH: "/etc/prometheus-certs/client.key"
  PROMETHEUS_SERVER_NAME: "prometheus-k8s.monitoring.svc.cluster.local"
  PROMETHEUS_BEARER_TOKEN: "your-bearer-token"  # Not recommended - use Secret instead
```
**Configuration Priority:**
1. Environment variables (checked first)
2. ConfigMap values (fallback)
3. Error if neither provides `PROMETHEUS_BASE_URL`
   
### Metrics Endpoint
The metrics are exposed at the `/metrics` endpoint on port 8080 (HTTP).

### ServiceMonitor Configuration

WVA metrics are exposed on port 8080 (HTTP):
```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: workload-variant-autoscaler
  namespace: workload-variant-autoscaler-system
  labels:
    release: kube-prometheus-stack
spec:
  selector:
    matchLabels:
      control-plane: controller-manager
  endpoints:
  - port: http
    scheme: http
    interval: 30s
    path: /metrics
```



## Security Considerations

### TLS Configuration

**Production Deployments:**
- Always use HTTPS endpoints (`https://`)
- Provide CA certificate via `PROMETHEUS_CA_CERT_PATH`
- Never set `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true` in production

**Development/Testing:**
- You may set `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true` for local clusters
- Example (port-forwarding to Prometheus):
  ```bash
  # Terminal 1: Port forward Prometheus
  kubectl port-forward -n monitoring svc/prometheus-k8s 9091:9091

  # Terminal 2: Set environment for local development
  export PROMETHEUS_BASE_URL=https://127.0.0.1:9091
  export PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true
  ```
- If your Prometheus is only reachable over plain HTTP (e.g. `kube-prometheus-stack`'s
  default in-cluster service), set `PROMETHEUS_ALLOW_HTTP=true` and use an
  `http://` `PROMETHEUS_BASE_URL` instead of standing up a TLS-terminating proxy.
  This cannot be combined with `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY`,
  `PROMETHEUS_SERVER_NAME`, any `PROMETHEUS_CA_CERT_PATH`/client cert settings, or
  bearer token auth — WVA refuses to start if those are set alongside a plain HTTP
  URL. Note that credentials and metrics are sent in cleartext, so use this only
  on a trusted network (dev/test or a secured in-cluster path).

### PromQL Injection Prevention

WVA implements security measures to prevent PromQL injection attacks:

1. **Parameter Escaping**: All query parameters (namespace, model ID, variant name) are automatically escaped:
   - Backslashes are escaped: `\` → `\\`
   - Double quotes are escaped: `"` → `\"`

2. **Namespace Validation**: Namespace values are validated before use in PromQL queries to prevent malicious label matchers

**Example - Safe Query Construction:**
```go
// User input (potentially malicious)
namespace := `prod",malicious="value`

// WVA automatically escapes the value
escapedNamespace := EscapePromQLValue(namespace)
// Result: `prod\",malicious=\"value`

// Safe PromQL query
query := fmt.Sprintf(`vllm_kv_cache_usage{namespace="%s"}`, escapedNamespace)
// Result: vllm_kv_cache_usage{namespace="prod\",malicious=\"value"}
// Prometheus treats this as a literal string, preventing injection
```

**Why This Matters:**
- Prevents unauthorized access to metrics from other namespaces
- Blocks label injection attacks that could manipulate query results
- Ensures multi-tenant deployments remain isolated



## WVA Metrics

WVA exposes metrics providing insights into autoscaling behavior and optimization performance. These metrics are exposed via Prometheus at the `/metrics` endpoint.

### Notes on **name_space**s in metrics
With WVA metrics, the value for the label `namespace` is the WVA controller namespace, not the VA's namespace. The VA namespace has the label `exported_namespace`. Here's an example:
```
{
  "metric": "wva_desired_replicas",
  "labels": {
    "accelerator_type": "A100",
    "container": "manager",
    "endpoint": "https",
    "exported_namespace": "llm-d-sim",    <==== VA namespace
    "instance": "10.244.0.73:8443",
    "job": "workload-variant-autoscaler-metrics",
    "namespace": "workload-variant-autoscaler-system",  <=== WVA controller namespace
    "pod": "workload-variant-autoscaler-controller-manager-75b45dd7c-89g5s",
    "service": "workload-variant-autoscaler-metrics",
    "variant_name": "workload-variant-autoscaler-va"
  },
  "value": "2"
}
```

### Configuration Metrics

### `wva_config_info`
- **Type**: Gauge
- **Description**: WVA configuration information (value is always 1)
- **Labels**:
  - `analyzer_name`: Name of the saturation analyzer in use
  - `limiter_enabled`: Whether the limiter is enabled (`true`, `false`)
  - `scale_to_zero_enabled`: Whether scale-to-zero is enabled (`true`, `false`)
- **Use Case**: Info-style metric to expose WVA configuration via labels for monitoring and debugging
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_config_info",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "limiter_enabled": "false",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "scale_to_zero_enabled": "false",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### `wva_config_kv_spare_threshold`
- **Type**: Gauge
- **Description**: KV cache spare threshold configuration value
- **Labels**: None (global configuration)
- **Use Case**: Monitor the configured KV cache spare threshold used in scaling decisions
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_config_kv_spare_threshold",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778846184.925,
      "0.1"
    ]
  }
  ```

### `wva_config_queue_spare_threshold`
- **Type**: Gauge
- **Description**: Queue spare threshold configuration value
- **Labels**: None (global configuration)
- **Use Case**: Monitor the configured queue spare threshold used in scaling decisions
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_config_queue_spare_threshold",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778846184.925,
      "3"
    ]
  }
  ```

### `wva_config_optimization_interval_seconds`
- **Type**: Gauge
- **Description**: Optimization interval in seconds
- **Labels**: None (global configuration)
- **Use Case**: Track how frequently the optimization loop runs
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_config_optimization_interval_seconds",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778846184.925,
      "30"
    ]
  }
  ```
### Metrics Collection Observability

### `wva_metrics_collection_duration_seconds`
- **Type**: Histogram
- **Description**: Duration of metrics collection operations in seconds
- **Labels**:
  - `query_type`: Type of metrics query being executed
- **Buckets**: 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5
- **Use Case**: Monitor metrics collection performance and identify slow queries
- ***Example**:
  ```
  {
    "metric": {
      "__name__": "wva_metrics_collection_duration_seconds_bucket",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "le": "0.001",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "query_type": "cache_config",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778846184.925,
      "0"
    ]
  }
  ```

### `wva_metrics_collection_errors_total`
- **Type**: Counter
- **Description**: Total number of metrics collection errors
- **Labels**:
  - `query_type`: Type of metrics query that failed
  - `reason`: Reason for the error
- **Use Case**: Track metrics collection failures and identify problematic queries
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_metrics_collection_errors_total",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.59:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-f9fdb95df-wvd9p",
      "query_type": "kv_cache",
      "reason": "bad_data",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778854211.669,
      "3"
    ]
  }
  ```

### `wva_metrics_pods_discovered`
- **Type**: Gauge
- **Description**: Number of pods discovered for a namespace
- **Labels**:
  - `namespace`: Kubernetes namespace
- **Use Case**: Monitor pod discovery to ensure all replicas are being tracked
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_metrics_pods_discovered",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### `wva_metrics_freshness_status`
- **Type**: Gauge
- **Description**: Freshness status of metrics for each variant
- **Labels**:
  - `variant_name`: Name of the variant
  - `status`: Status of metrics freshness (`fresh`, `stale`, `missing`, `unavailable`)
- **Use Case**: Track metric staleness to ensure autoscaling decisions are based on current data
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_metrics_freshness_status",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
      "status": "fresh",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "2"
    ]
  }
  ```

### `wva_pod_mapping_miss_total`
- **Type**: Counter
- **Description**: Total number of pods whose metrics could not be attributed to a managed scaler — neither the `llm-d.ai/variant` label nor the pod locator (ownerReference walk) resolved an owning HPA/ScaledObject. Makes the otherwise-silent skip observable.
- **Labels**:
  - `namespace`: Kubernetes namespace of the unattributed pod
  - `reason`: Why the pod was unattributed (currently always `unresolved`)
- **Use Case**: Alert on a rising rate of unattributed pods — usually a missing `llm-d.ai/variant` label, a scaler missing the `llm-d.ai/managed` annotation, or a shadow-pod layout without the label
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_pod_mapping_miss_total",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.59:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-f9fdb95df-wvd9p",
      "exported_namespace": "llm-d-sim",
      "reason": "unresolved",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778854211.669,
      "2"
    ]
  }
  ```

### Optimization Metrics

### `wva_optimization_duration_seconds`
- **Type**: Histogram
- **Description**: Duration of optimization loop cycles in seconds
- **Labels**:
  - `status`: Status of the optimization cycle (`success`, `error`)
- **Buckets**: 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10
- **Use Case**: Monitor optimization loop performance and identify slow optimization cycles
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_optimization_duration_seconds_bucket",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "le": "0.01",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
      "status": "success"
    },
    "value": [
      1778846184.925,
      "191"
    ]
  }
  ```

### `wva_models_processed`
- **Type**: Gauge
- **Description**: Number of models processed in the last optimization cycle
- **Labels**: None (global metric)
- **Use Case**: Track how many models are being processed per optimization cycle to understand workload
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_models_processed",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### Saturation and Capacity Metrics

### `wva_saturation_utilization`
- **Type**: Gauge
- **Description**: Per-variant utilization ratio from saturation analysis. V1 path: mean of per-replica KV-cache-usage fractions, bounded 0.0-1.0 (matches the per-replica threshold V1 checks). V2 path: TotalDemand / TotalCapacity from the analyzer result — **unbounded above**, where > 1.0 means demand exceeds supply. Numerically equivalent for uniform-capacity replicas; V2 is capacity-weighted for mixed-capacity cases.
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `model_name`: Model name served by the variant
  - `accelerator_type`: Type of accelerator being used
- **Use Case**: Monitor KV cache utilization to understand saturation levels and trigger scaling decisions
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_saturation_utilization",
      "accelerator_type": "H100",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "model_name": "unsloth/Meta-Llama-3.1-8B",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "0"
    ]
  }
  ```

### `wva_spare_capacity`
- **Type**: Gauge
- **Description**: Spare capacity; >0 indicates scale-down headroom (per-role for P/D-disaggregated models, model-level otherwise). Use the `unit` label to interpret the value: `continuous` → **token surplus** from the **Token-based analyzer** (`max(0, TotalSupply - TotalDemand/scaleDownBoundary)`); empty → a 0.0-1.0 threshold-relative fraction from the **Percentage-based analyzer** (`kvCacheThreshold - avg KV usage`).
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `model_name`: Model name served by the variant
  - `unit`: `continuous` (Token-based analyzer — a token magnitude) or empty (Percentage-based analyzer — 0.0-1.0 fraction)
- **Use Case**: Track available capacity headroom to prevent saturation and optimize resource allocation
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_spare_capacity",
      "unit": "continuous",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "model_name": "unsloth/Meta-Llama-3.1-8B",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "12000"
    ]
  }
  ```

### `wva_required_capacity`
- **Type**: Gauge
- **Description**: Required capacity; >0 indicates scale-up needed (per-role for P/D-disaggregated models, model-level otherwise). Use the `unit` label to interpret the value: `continuous` → **token demand** from the **Token-based analyzer**; `binary` → 0/1 scale-up signal from the **Percentage-based analyzer**.
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `model_name`: Model name served by the variant
  - `unit`: `continuous` (Token-based analyzer — a token magnitude) or `binary` (Percentage-based analyzer — 0/1 signal)
- **Use Case**: Identify when additional capacity is needed and understand the magnitude of demand
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_required_capacity",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "model_name": "unsloth/Meta-Llama-3.1-8B",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "0"
    ]
  }
  ```

### `wva_kv_cache_tokens_used`
- **Type**: Gauge
- **Description**: Total KV cache tokens currently in use across all replicas of a variant (sum of vLLM TokensInUse).
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `model_name`: Model name served by the variant
- **Use Case**: Monitor absolute KV cache token usage across variant replicas
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_kv_cache_tokens_used",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "model_name": "unsloth/Meta-Llama-3.1-8B",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "0"
    ]
  }
  ```

### Pipeline Stage Visibility Metrics

### `wva_decisions_limited_total`
- **Type**: Counter
- **Description**: Total number of scaling decisions constrained by the limiter. This tracks how often the limiter prevents scaling actions due to resource constraints.
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `limiter_name`: Name of the limiter that constrained the decision
- **Use Case**: Monitor how frequently resource limiters are constraining scaling decisions to understand capacity bottlenecks
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_decisions_limited_total",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.60:8443",
      "job": "workload-variant-autoscaler-metrics",
      "limiter_name": "gpu-limiter",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-659d5c9dcf-w6cw7",
      "service": "workload-variant-autoscaler-metrics",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778855539.730,
      "1"
    ]
  }
  ```

### `wva_gpu_discovery_up`
- **Type**: Gauge
- **Description**: Indicates whether GPU discovery is on (1) or off (0). GPU discovery is enabled when the limiter configuration is active (e.g., `enableLimiter` is `true`). This metric helps operators understand whether WVA is actively discovering GPU resources.
- **Labels**: None (global metric, optional `controller_instance` label when multi-instance deployment is used)
- **Use Case**: Monitor GPU discovery status to ensure resource discovery is functioning when expected
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_gpu_discovery_up",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### `wva_available_gpus`
- **Type**: Gauge
- **Description**: Number of currently available GPUs grouped by accelerator type (e.g., "H100", "A100"). When `wva_gpu_discovery_up` is 1, this shows the number of currently available GPUs. When `wva_gpu_discovery_up` is 0, this metric shows the number of GPUs that were available at the last successful discovery. Only available in clusters such as OpenShift where WVA can iterate over node objects. There are no exclusions such as tainted nodes or GPUs operating in different modes such as MIG.
- **Labels**:
  - `accelerator_vendor`: Name of the GPU vendor
  - `accelerator_model`: Full name of the accelerator
  - `accelerator_type`: Type of accelerator (short name of the accelerator)
- **Use Case**: Track the number of GPUs discovered by WVA and available for allocation
- **Example**:
  ```
  {
      "metric": {
        "__name__": "wva_available_gpus",
        "accelerator_model": "NVIDIA-H100-SXM5-80GB",
        "accelerator_type": "H100",
        "accelerator_vendor": "nvidia.com",
        "container": "manager",
        "endpoint": "https",
        "instance": "10.244.2.55:8443",
        "job": "workload-variant-autoscaler-metrics",
        "namespace": "workload-variant-autoscaler-system",
        "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
        "service": "workload-variant-autoscaler-metrics"
      },
      "value": [
        1778847371.083,
        "4"
      ]
    }
  ```

### `wva_enforcer_modifications_total`
- **Type**: Counter
- **Description**: Total number of decision modifications made by the enforcer. The enforcer applies policy constraints (e.g., "scale_to_zero", "minimum_replicas") to scaling decisions.
- **Labels**:
  - `policy_type`: Type of enforcement policy applied
- **Use Case**: Monitor how often the enforcer modifies scaling decisions to enforce policies and understand policy impact
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_enforcer_modifications_total",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.62:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-c8b9c74b5-82t2c",
      "policy_type": "scale_to_zero",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778859222.859,
      "1"
    ]
  }
  ```

### `wva_optimizer_active`
- **Type**: Gauge
- **Description**: Indicates which optimizer is currently active. Value is 1 for the active optimizer and 0 for inactive optimizers. Only one optimizer should be active at a time. If the label `optimizer_name` is not in the metric, this means V1 saturation optimizer is currently active.
- **Labels**:
  - `optimizer_name`: Name of the optimizer
- **Use Case**: Track which optimization strategy is currently in use for scaling decisions
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_optimizer_active",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "optimizer_name": "cost-aware",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### Replica Management Metrics

### `wva_current_replicas`
- **Type**: Gauge
- **Description**: Current number of replicas for each variant
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `accelerator_type`: Type of accelerator being used
- **Use Case**: Monitor current number of replicas per variant
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_current_replicas",
      "accelerator_type": "H100",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### `wva_desired_replicas`
- **Type**: Gauge
- **Description**: Desired number of replicas for each variant
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `accelerator_type`: Type of accelerator being used
- **Use Case**: Expose the desired optimized number of replicas per variant
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_desired_replicas",
      "accelerator_type": "H100",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### `wva_desired_ratio`
- **Type**: Gauge
- **Description**: Ratio of the desired number of replicas and the current number of replicas for each variant
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `accelerator_type`: Type of accelerator being used
- **Use Case**: Compare the desired and current number of replicas per variant, for scaling purposes
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_desired_ratio",
      "accelerator_type": "H100",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### `wva_replica_scaling_total`
- **Type**: Counter
- **Description**: Total number of replica scaling operations performed, labeled by direction and reason
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `direction`: Scaling direction (`up`, `down`)
  - `reason`: Reason for the scaling operation
- **Use Case**: Detect scaling thrashing — backs the `WVAReplicaScalingThrashing` alert — and audit scaling activity per variant
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_replica_scaling_total",
      "direction": "up",
      "namespace": "workload-variant-autoscaler-system",
      "reason": "saturation",
      "variant_name": "smoke-test-va"
    },
    "value": [
      1778846184.925,
      "3"
    ]
  }
  ```

### Error Tracking

### `wva_errors_total`
- **Type**: Counter
- **Description**: Total number of errors by component. The components are "collector", "analyzer", "optimizer", "limiter", "enforcer", and "controller". Some of the components currently may not have any `wva_errors_total` metrics. They may be available in future WVA versions.
- **Labels**:
  - `component`: Component where the error occurred
  - `error_type`: Type or category of the error
- **Use Case**: Track error rates across different components to identify problematic areas and monitor system health
- **Example**:
  ```
  {
    "metric": {
      "__name__": "wva_errors_total",
      "component": "controller",
      "container": "manager",
      "endpoint": "https",
      "error_type": "Failed to parse saturation scaling config entry",
      "instance": "10.244.2.55:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778850096.639,
      "1"
    ]
  }
  ```

## Example Queries

### Basic Queries
```promql
# Current replicas by variant
wva_current_replicas

# Scaling frequency
rate(wva_replica_scaling_total[5m])

# Desired replicas by variant
wva_desired_replicas
```

### Advanced Queries
```promql
# Scaling frequency by direction
rate(wva_replica_scaling_total{direction="scale_up"}[5m])

# Replica count mismatch
abs(wva_desired_replicas - wva_current_replicas)

# Scaling frequency by reason
rate(wva_replica_scaling_total[5m]) by (reason)

# Optimization duration 95th percentile
histogram_quantile(0.95, rate(wva_optimization_duration_seconds_bucket[5m]))

# Saturation utilization by variant
wva_saturation_utilization

# KV cache utilization percentage
(wva_kv_cache_tokens_used / wva_kv_cache_tokens_total) * 100

# Variants requiring scale-up (V1 binary signal)
wva_required_capacity{unit="binary"} > 0

# High utilization — scale-up likely (V1 and V2)
wva_saturation_utilization > 0.85

# Low spare capacity, V1 fractional signal only.
# On V2 wva_spare_capacity is an absolute token count, not a 0-1 ratio — use utilization above.
wva_spare_capacity{unit=""} < 0.2

# Models processed over time
wva_models_processed

# Metrics collection duration 99th percentile by query type
histogram_quantile(0.99, rate(wva_metrics_collection_duration_seconds_bucket[5m])) by (query_type)

# Metrics collection error rate
rate(wva_metrics_collection_errors_total[5m])

# Pods discovered per namespace
wva_metrics_pods_discovered

# Stale metrics by variant
wva_metrics_freshness_status{status="stale"}

# Fresh metrics ratio
sum(wva_metrics_freshness_status{status="fresh"}) / sum(wva_metrics_freshness_status)

# WVA configuration info
wva_config_info

# Check if limiter is enabled
wva_config_info{limiter_enabled="true"}

# Current KV cache spare threshold
wva_config_kv_spare_threshold

# Current queue spare threshold
wva_config_queue_spare_threshold

# Optimization loop interval
wva_config_optimization_interval_seconds

# Total errors by component
wva_errors_total

# Error rate over time
rate(wva_errors_total[5m])

# Error rate by component
rate(wva_errors_total[5m]) by (component)

# Error rate by error type
rate(wva_errors_total[5m]) by (error_type)

# Decisions limited by limiter
wva_decisions_limited_total

# Decisions limited rate over time
rate(wva_decisions_limited_total[5m])

# Decisions limited by variant
rate(wva_decisions_limited_total[5m]) by (variant_name)

# Decisions limited by limiter type
rate(wva_decisions_limited_total[5m]) by (limiter_name)

# GPU discovery status
wva_gpu_discovery_up

# Check if GPU discovery is enabled
wva_gpu_discovery_up == 1

# Available GPUs by accelerator type
wva_available_gpus

# Total available GPUs across all types
sum(wva_available_gpus)

# Enforcer modifications total
wva_enforcer_modifications_total

# Enforcer modification rate over time
rate(wva_enforcer_modifications_total[5m])

# Enforcer modifications by policy type
rate(wva_enforcer_modifications_total[5m]) by (policy_type)

# Active optimizer
wva_optimizer_active

# Currently active optimizer (filter for value = 1)
wva_optimizer_active == 1
```

## Alerting Rules

WVA pre-defined a number of Prometheus alerting rules which can be optionally installed. These rules are defined in `config/components/prometheus-alerts/prometheusrule.yaml`.
### Alerting Rules Installation
- To install alerting rules, set environment variable `DEPLOY_ALERTING_RULES` to `true`, and run the installation, for example:
  ```bash
  export DEPLOY_ALERTING_RULES=true
  make deploy-e2e-infra
  ```
- To remove alerting rules, set environment variable `DEPLOY_ALERTING_RULES` to `false`, and run the installation, for example:
  ```bash
  export DEPLOY_ALERTING_RULES=false
  make deploy-e2e-infra
  ```

### Alerting Rules Verification
Once installed, you can verify as follows:
- Check that PrometheusRule resource has been created:
  ```bash
  kubectl get PrometheusRule -n workload-variant-autoscaler-system controller-manager-alerts
  NAME                        AGE
  controller-manager-alerts   40h
  ```

- Check Prometheus:
  - Forward Prometheus service to local host:
    ```bash
    kubectl port-forward -n workload-variant-autoscaler-monitoring svc/kube-prometheus-stack-prometheus 9090:9090
    ```

  - Browse to https://localhost:9090/alerts
  - You should see `wva.rules`: ![wva.rules](./wva.rules.png)
  - Here's an example of a triggered rule: ![wva.rules-firing](./wva.rules-firing.png)

- Get fired-rule details:
    From https://localhost:9090/alerts, if there's a rule has been fired, you can get the details as follows. Here's an example:
    ```bash
    curl -k https://localhost:9090/api/v1/alerts | jq .

    {
        "labels": {
          "alertname": "WVAHighErrorRate",
          "component": "controller",
          "error_type": "Config is nil in ConfigMapReconciler bootstrap",
          "severity": "warning"
        },
        "annotations": {
          "description": "WVA component 'controller' error_type 'Config is nil in ConfigMapReconciler bootstrap' rate is 0.03/sec (>6/min threshold) sustained for 5+ minutes. Check controller logs for error patterns.",
          "summary": "WVA error rate elevated in controller"
        },
        "state": "pending",
        "activeAt": "2026-07-02T19:35:06.642428048Z",
        "value": "3.1034482758620693e-02"
    }
    ```

### Alerting Rules Notes
- For `WVAGPUResourceExhausted` rule with `wva_available_gpus` metric, as described in [wva_available_gpus](#wva_available_gpus) this metric is not always available in which case this rule will not trigger. In other words, if there's no alert for this rule, it does not mean GPUs are available.
  
### Alerting Rules E2E Test
E2E tests for alerting rules are in `test/e2e/prometheus_alerts_test.go`. The tests cover basic install and validation. Here are some scenarios:
- should create PrometheusRule with WVA alert rules
- should have all expected alert rules defined
- should have valid alert rule structure 
- should only reference known WVA metrics in alert expressions

How to execute E2e Test:
```bash
export DEPLOY_ALERTING_RULES=true
make deploy-e2e-infra
make test-e2e-smoke FOCUS="PrometheusAlerts"
```