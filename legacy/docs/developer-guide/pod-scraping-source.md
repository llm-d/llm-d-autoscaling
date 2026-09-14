# PodScrapingSource Usage Guide

## Overview

`PodScrapingSource` is a generic metrics source implementation that scrapes Prometheus-format metrics directly from Kubernetes pods via HTTP requests. It implements the `MetricsSource` interface and can be used as an alternative to Prometheus queries when you need to collect metrics directly from pods.

### Key Features

- **Direct Pod Scraping**: Scrapes metrics from pod IPs via HTTP (no Prometheus required)
- **Service-Based Discovery**: Automatically discovers pods using Kubernetes Service selectors
- **Concurrent Scraping**: Scrapes multiple pods concurrently with configurable limits
- **Caching**: Built-in caching with configurable TTL
- **Authentication Support**: Optional Bearer token authentication
- **Prometheus Format**: Parses standard Prometheus text format metrics

## Prerequisites

1. **Kubernetes Cluster Access**: Valid kubeconfig and cluster connectivity
2. **Service with Pod Selector**: A Kubernetes Service that selects the target pods
3. **Metrics Endpoint**: Pods must expose a `/metrics` endpoint (or custom path) in Prometheus format
   - For EPP pods: Port `9090` (default)
4. **RBAC Permissions**: The client needs:
   - `get`, `list`, `watch` on `services`
   - `get`, `list`, `watch` on `pods`
   - `get` on `secrets` (if using authentication)
   - For EPP metrics: `get` on non-resource URLs `/metrics`
5. **Authentication Secret** (optional): If metrics endpoint requires authentication
   - For EPP: Typically `inference-gateway-sa-metrics-reader-secret` (see [official authentication guide](https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/site-src/guides/metrics-and-observability.md#scrape-metrics--pprof-profiles))

## Basic Usage

### Import the Package

```go
import (
    "context"
    "time"
    
    "sigs.k8s.io/controller-runtime/pkg/client"
    
    "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/pod"
)
```

### Create a PodScrapingSource Instance

```go
ctx := context.Background()
k8sClient := // your Kubernetes client

config := pod.PodScrapingSourceConfig{
    // Required: Service identification
    ServiceName:      "my-service",
    ServiceNamespace: "my-namespace",
    
    // Required: Metrics endpoint
    MetricsPort: 9090,
    
    // Optional: Customize defaults
    MetricsPath:   "/metrics",  // default: "/metrics"
    MetricsScheme: "http",      // default: "http"
}

source, err := pod.NewPodScrapingSource(ctx, k8sClient, config)
if err != nil {
    // handle error
}
```

### Use Default Configuration

```go
config := pod.DefaultPodScrapingSourceConfig()
config.ServiceName = "my-service"
config.ServiceNamespace = "my-namespace"
config.MetricsPort = 9090

source, err := pod.NewPodScrapingSource(ctx, k8sClient, config)
```

## Configuration Reference

### PodScrapingSourceConfig

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `ServiceName` | `string` | Yes | - | Kubernetes Service name that selects target pods |
| `ServiceNamespace` | `string` | Yes | - | Namespace of the service |
| `MetricsPort` | `int32` | Yes | - | Port number where pods expose metrics |
| `MetricsPath` | `string` | No | `"/metrics"` | HTTP path for metrics endpoint |
| `MetricsScheme` | `string` | No | `"http"` | URL scheme (`"http"` or `"https"`) |
| `MetricsReaderSecretName` | `string` | No | `""` | Secret name containing Bearer token (optional) |
| `MetricsReaderSecretNamespace` | `string` | No | `ServiceNamespace` | Namespace where the secret is located (defaults to service namespace) |
| `MetricsReaderSecretKey` | `string` | No | `"token"` | Key in secret containing the token |
| `BearerToken` | `string` | No | `""` | Direct Bearer token (overrides secret) |
| `ScrapeTimeout` | `time.Duration` | No | `5s` | HTTP request timeout per pod |
| `MaxConcurrentScrapes` | `int` | No | `10` | Maximum concurrent pod scrapes |
| `DefaultTTL` | `time.Duration` | No | `30s` | Cache TTL for scraped metrics |

### Configuration Examples

#### Minimal Configuration (Required Fields Only)

```go
config := pod.PodScrapingSourceConfig{
    ServiceName:      "epp-service",
    ServiceNamespace: "default",
    MetricsPort:      9090,
}
```

#### With Authentication (Secret in Same Namespace)

```go
config := pod.PodScrapingSourceConfig{
    ServiceName:             "epp-service",
    ServiceNamespace:        "default",
    MetricsPort:             9090,
    MetricsReaderSecretName: "metrics-reader-secret", 
    MetricsReaderSecretKey:  "token",
}
```

#### With Authentication (Secret in Different Namespace)

```go
config := pod.PodScrapingSourceConfig{
    ServiceName:                  "epp-service",
    ServiceNamespace:             "default",
    MetricsPort:                  9090,
    MetricsReaderSecretName:      "wva-epp-metrics-token",
    MetricsReaderSecretNamespace: "workload-variant-autoscaler-system", // Secret in controller namespace
    MetricsReaderSecretKey:       "token",
}
```

#### With Custom Timeouts and Concurrency

```go
config := pod.PodScrapingSourceConfig{
    ServiceName:          "epp-service",
    ServiceNamespace:     "default",
    MetricsPort:          9090,
    ScrapeTimeout:        10 * time.Second,
    MaxConcurrentScrapes: 5,
    DefaultTTL:           60 * time.Second,
}
```

#### With Direct Bearer Token

```go
config := pod.PodScrapingSourceConfig{
    ServiceName:      "epp-service",
    ServiceNamespace: "default",
    MetricsPort:      9090,
    BearerToken:      "your-token-here",
}
```

## Using PodScrapingSource

### Refresh Metrics

`Refresh()` discovers pods, scrapes metrics from all ready pods, and updates the cache.

```go
results, err := source.Refresh(ctx, source.RefreshSpec{
    Queries: []string{"all_metrics"}, // Empty = refresh all registered queries
    Params:  map[string]string{},     // Query parameters (not used for pod scraping)
})
if err != nil {
    // handle error
}

// Access results
result := results["all_metrics"]
if result != nil {
    for _, value := range result.Values {
        fmt.Printf("Metric: %s, Value: %f, Pod: %s\n",
            value.Labels["__name__"],
            value.Value,
            value.Labels["pod"],
        )
    }
}
```

### Get Cached Metrics

`Get()` retrieves cached metrics without scraping. Returns `nil` if not cached or expired.

```go
cached := source.Get("all_metrics", nil)
if cached != nil && !cached.IsExpired() {
    result := cached.Result
    // Use cached metrics
} else {
    // Cache miss or expired - call Refresh()
    results, err := source.Refresh(ctx, source.RefreshSpec{})
    // ...
}
```

### Query List

`QueryList()` returns the query registry. PodScrapingSource registers a default `"all_metrics"` query.

```go
registry := source.QueryList()
queries := registry.List()
// queries = ["all_metrics"]
```

## Understanding Results

### MetricResult Structure

```go
type MetricResult struct {
    QueryName   string        // Query name (e.g., "all_metrics")
    Values      []MetricValue // All metric values
    CollectedAt time.Time     // When metrics were scraped
    Error       error         // Error if query failed
}
```

### MetricValue Structure

```go
type MetricValue struct {
    Value     float64            // Metric value
    Timestamp time.Time          // When metric was sampled
    Labels    map[string]string  // Metric labels
}
```

### Label Structure

Each `MetricValue` includes labels:

- `__name__`: The metric name (e.g., `"inference_pool_average_queue_size"`)
- `pod`: The pod name that provided this metric
- Additional labels from the original Prometheus metric (e.g., `model_name`, `target_model_name`, `name` for inference pool)

### Example Result

```go
result := results["all_metrics"]
// result.Values contains:
// [
//   {
//     Value: 5.0,
//     Timestamp: 2025-01-15T10:30:00Z,
//     Labels: {
//       "__name__": "inference_pool_average_queue_size",
//       "pod": "epp-pod-abc123",
//       "name": "my-inference-pool"
//     }
//   },
//   {
//     Value: 0.85,
//     Timestamp: 2025-01-15T10:30:00Z,
//     Labels: {
//       "__name__": "inference_pool_average_kv_cache_utilization",
//       "pod": "epp-pod-abc123",
//       "name": "my-inference-pool"
//     }
//   },
//   {
//     Value: 10.0,
//     Timestamp: 2025-01-15T10:30:00Z,
//     Labels: {
//       "__name__": "inference_objective_running_requests",
//       "pod": "epp-pod-abc123",
//       "model_name": "llama-3-8b"
//     }
//   },
//   // ... more metrics from other pods
// ]
```

### Available EPP Metrics (For scraping EPP Metrics)

When scraping EPP pods, you'll find metrics from the [Gateway API Inference Extension](https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/site-src/guides/metrics-and-observability.md). Common metrics include:

**Request Metrics:**
- `inference_objective_request_total` (Counter) - Total requests per model
- `inference_objective_request_error_total` (Counter) - Request errors per model
- `inference_objective_running_requests` (Gauge) - Number of running requests per model
- `inference_objective_request_duration_seconds` (Distribution) - Request latency distribution

**Inference Pool Metrics:**
- `inference_pool_average_queue_size` (Gauge) - Average queue size for an inference pool
- `inference_pool_per_pod_queue_size` (Gauge) - Per-pod queue size
- `inference_pool_average_kv_cache_utilization` (Gauge) - Average KV cache utilization
- `inference_pool_ready_pods` (Gauge) - Number of ready pods in the pool

**Flow Control Metrics (Experimental):**
- `inference_extension_flow_control_queue_size` (Gauge) - Flow control queue size
- `inference_extension_flow_control_request_queue_duration_seconds` (Distribution) - Queue wait time

## Integration Examples

### Register in SourceRegistry

```go
import (
    "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
    "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/pod"
)

// Create source
config := pod.PodScrapingSourceConfig{
    ServiceName:      "epp-service",
    ServiceNamespace: "default",
    MetricsPort:      9090,
}
podSource, err := pod.NewPodScrapingSource(ctx, k8sClient, config)
if err != nil {
    return err
}

// Register in registry
registry := source.NewSourceRegistry()
if err := registry.Register("pod-scraping-epp", podSource); err != nil {
    return err
}

// Use from registry
source := registry.Get("pod-scraping-epp")
results, err := source.Refresh(ctx, source.RefreshSpec{})
```

### Use in Engine Example

```go
type MyEngine struct {
    source source.MetricsSource
}

func (e *MyEngine) CheckPendingRequests(ctx context.Context) (bool, error) {
    results, err := e.source.Refresh(ctx, source.RefreshSpec{})
    if err != nil {
        return false, err
    }
    
    result := results["all_metrics"]
    for _, value := range result.Values {
        // Check for pending requests using queue size metrics
        metricName := value.Labels["__name__"]
        if (metricName == "inference_pool_average_queue_size" ||
            metricName == "inference_extension_flow_control_queue_size") &&
            value.Value > 0 {
            return true, nil
        }
    }
    
    return false, nil
}
```

## Additional Notes

### Pod Discovery

- PodScrapingSource discovers pods using the Service's `spec.selector`
- Only **Ready** pods are scraped (pod has `Ready` condition with `status=True`)
- If service has no selector (headless service), no pods are discovered

### Concurrency

- Multiple pods are scraped concurrently (up to `MaxConcurrentScrapes`)
- Each scrape uses `ScrapeTimeout` for the HTTP request
- Results are aggregated into a single `MetricResult`

### Caching

- Metrics are cached with `DefaultTTL` (default: 30s)
- Cache is shared across all queries
- Use `Get()` to retrieve cached values without scraping

### Authentication

- Authentication is **optional** - if neither `BearerToken` nor `MetricsReaderSecretName` is provided, no auth header is sent
- If secret doesn't exist, authentication is skipped (no error)
- `BearerToken` takes precedence over secret-based auth
- **Secret Namespace**: By default, secrets are looked up in the service's namespace (`ServiceNamespace`). Use `MetricsReaderSecretNamespace` to specify a different namespace (e.g., when the authentication secret is in a different namespace than the target service)

### Cross-Namespace Authentication

When the authentication secret is in a different namespace than the target service, you must:

1. Set `MetricsReaderSecretNamespace` to the namespace containing the secret
2. Ensure the controller has RBAC permissions to read secrets in that namespace

Example:

```go
config := pod.PodScrapingSourceConfig{
    ServiceName:                  "epp-service",
    ServiceNamespace:             "llm-d",  // EPP service namespace
    MetricsPort:                  9090,
    MetricsReaderSecretName:      "metrics-reader-token",
    MetricsReaderSecretNamespace: "auth-namespace",  // Secret namespace
    MetricsReaderSecretKey:       "token",
}
```
