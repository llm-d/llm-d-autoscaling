# Operational Dashboard

## Overview
For observability, WVA records a number of metrics which are scraped by Prometheus. This document shows how to enable the operational dashboard in your Kubernetes cluster. Once installed, you can view these metrics through the provided dashboard with Grafana.

## Installation
The operational dashboard is installed by default. To uninstall, set the environment variable `DEPLOY_OPERATIONAL_DASHBOARD` to `false` and run or re-run the installation. Following is an example for installation using `Make` method:
```console
export DEPLOY_OPERATIONAL_DASHBOARD=false
make deploy-wva-on-k8s
```

## Access Operational Dashboard
Once the operational dashboard is enabled, Grafana is installed, configured, and ready to display the dashboard. Here are the next steps:

- Forward Grafana port so the dashboard can be accessed locally:
  ```console
  $ kubectl port-forward -n workload-variant-autoscaler-monitoring svc/kube-prometheus-stack-grafana 3000:80
  ```
- Get Grafana `admin` password:
  ```console
  $ kubectl get secret -n workload-variant-autoscaler-monitoring   kube-prometheus-stack-grafana   -o jsonpath="{.data.admin-password}" | base64 -d;echo
  ```
- Point browser to `http://localhost:3000/`, login with username `admin` and the password obtained in previous step.

- Browse to "Connections/Data sources", you should see a Prometheus data source `https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local:9090`. Click on `Test` button to test the data source.

- Browse to "Dashboards", you should see a dashboard called `WVA Operational Dashboard`.


## Import Operational Dashboard
The pre-installed `WVA Operational Dashboard` is read-only. You can import `WVA Operational Dashboard` to a new dashboard so you can update the dashboard as follows:
- Browse to "Dashboards", you should see a dashboard called `WVA Operational Dashboard/New/Import`.
- Copy and paste the content of `deploy/grafana/operational-dashboard.json`.
- Name the new dashboard such as `My WVA Operational Dashboard`.
- Your new dashboard now is the same as `WVA Operational Dashboard` except that you can edit and save.


## Understanding Namespace Labels in Metrics

### The `namespace` vs `exported_namespace` Issue

When viewing per-variant metrics in the operational dashboard, you may notice labels named `namespace` and `exported_namespace`. Understanding the difference is important for correctly interpreting dashboard data.

**Root Cause: Prometheus `honorLabels` Setting**

WVA's metrics include a `namespace` label indicating which namespace each variant is running in. However, when Prometheus scrapes these metrics via a ServiceMonitor, the `honorLabels` configuration (default: `false`) affects how labels are handled:

- **With `honorLabels: false` (default)**:
  - Prometheus **renames** WVA's original `namespace` label to `exported_namespace`
  - Prometheus adds its own `namespace` label containing the **controller's pod namespace** (e.g., `workload-variant-autoscaler-system`)
  - **Result**: Per-variant panels show `exported_namespace` for the variant's actual namespace

- **With `honorLabels: true`**:
  - WVA's original `namespace` label is **preserved** as-is
  - The `namespace` label contains the **variant's namespace**
  - **Result**: Per-variant panels use `namespace` for the variant's actual namespace

**Dashboard Configuration**

The operational dashboard includes a template variable `$namespace_label` that can be toggled between:
- `exported_namespace` (default) - for `honorLabels: false` configurations
- `namespace` - for `honorLabels: true` configurations

If you see incorrect namespace groupings in per-variant panels (e.g., all variants showing the controller's namespace instead of their actual namespaces), toggle the `$namespace_label` dropdown at the top of the dashboard.

**Why the Default is `honorLabels: false`**

Setting `honorLabels: false` is a security best practice that prevents scraped applications from spoofing infrastructure labels. For example, it prevents a pod from claiming to be in a different namespace via its exported metrics.

## Distributed Tracing

Metrics answer "how long do optimization cycles take?"; traces answer "why was *this* cycle slow, and what did it decide?". WVA can emit an OpenTelemetry trace per optimization cycle, covering collection, analysis, optimization, limiting, enforcement, and actuation.

Tracing is **off by default**. When it is off, no tracer provider is installed, spans are non-recording no-ops, and nothing is exported.

### Enabling

Configuration uses the standard `OTEL_*` environment variables, so WVA behaves like any other OpenTelemetry application.

| Variable | Default | Description |
|----------|---------|-------------|
| `OTEL_TRACES_EXPORTER` | `none` | `none` disables tracing. `otlp` exports over OTLP gRPC. `console` prints spans to stdout for local debugging. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | Collector endpoint, e.g. `otel-collector.observability:4317`. `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` takes precedence when both are set. |
| `OTEL_EXPORTER_OTLP_INSECURE` | `false` | Set to `true` to disable transport security (in-cluster collector without TLS). |
| `OTEL_SERVICE_NAME` | `llm-d-wva` | Reported as `service.name`. |
| `OTEL_SERVICE_VERSION` | — | Reported as `service.version` when set. |
| `OTEL_TRACES_SAMPLER` | `parentbased_always_on` | One of `always_on`, `always_off`, `traceidratio`, `parentbased_always_off`, `parentbased_traceidratio`. |
| `OTEL_TRACES_SAMPLER_ARG` | `1.0` | Sampling ratio for the ratio-based samplers. |

Add them to the controller Deployment:

```yaml
env:
  - name: OTEL_TRACES_EXPORTER
    value: "otlp"
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: "otel-collector.observability.svc.cluster.local:4317"
  - name: OTEL_EXPORTER_OTLP_INSECURE
    value: "true"
  - name: OTEL_TRACES_SAMPLER
    value: "parentbased_traceidratio"
  - name: OTEL_TRACES_SAMPLER_ARG
    value: "0.1"
```

To check the wiring without a collector, set `OTEL_TRACES_EXPORTER=console` and read the spans from the controller's logs.

Each trace carries the controller's identity as resource attributes: `service.name`, `service.version`, `k8s.namespace.name` (from `POD_NAMESPACE`), and `k8s.pod.name` / `service.instance.id` (from `POD_NAME`, falling back to the hostname).

### Trace Structure

One optimization cycle produces one trace, rooted at `reconcile`:

```
reconcile                  one optimization cycle
├── collect                metrics and inventory collection
│   └── prometheus_query   one PromQL query (repeated)
├── analyze                saturation / queueing-model analysis, per model
├── optimize               cost-aware or greedy-by-score optimizer
├── limit                  GPU limiter clamping
├── enforce                scale-to-zero and minimum-replica enforcement
└── actuate                VA status updates and HPA/KEDA signals
```

Not every stage appears in every cycle. A cycle with no active VariantAutoscalings, or one where no metrics are available, ends after collection — which is itself the useful signal when scaling appears stuck.

Span names are bare snake_case verbs. What emitted a span is carried by its **instrumentation scope**, which is the emitting package's import path, so the name does not repeat it:

| Scope | Spans |
|-------|-------|
| `llm-d-wva/internal/engines/saturation` | `reconcile`, `analyze`, `optimize`, `actuate` |
| `llm-d-wva/internal/engines/pipeline` | `optimize`, `limit`, `enforce` |
| `llm-d-wva/internal/collector` | `collect` |
| `llm-d-wva/internal/collector/source/prometheus` | `prometheus_query` |

This matches `llm-d-router`'s convention (`llm-d-router/pkg/...` scopes with names like `prefill`, `score_prefix_cache`), so traces spanning both projects group consistently. When filtering, use scope **and** name together — `optimize` is emitted by two packages, since the V1 path builds targets in the engine while V2 runs the optimizer pipeline.

Spans carry the decision context as `wva.*` attributes: `wva.model_id`, `wva.namespace`, `wva.variant_autoscaling`, `wva.mode`, `wva.analyzer`, `wva.optimizer`, `wva.limiter`, `wva.current_replicas`, `wva.desired_replicas`, `wva.decision_count`, `wva.limited_decision_count`, and `wva.query` / `wva.query_type` on Prometheus query spans.

All attribute values are integers, booleans, or strings from a set the controller itself defines (analyzer, optimizer, and limiter names, Kubernetes object names, and PromQL built from registered templates with escaped parameters). Free-form user input is never attached to a span.

### Not Yet Covered

TLS and authentication for the OTLP exporter are not configurable yet — an in-cluster collector reached over plaintext (`OTEL_EXPORTER_OTLP_INSECURE=true`) is the supported deployment for now. Log lines do not yet carry `trace_id` / `span_id`, so jumping from a log to its trace is manual.

## Troubleshooting
### services "kube-prometheus-stack-grafana" not found
  - Make sure to install WVA cluster with `DEPLOY_OPERATIONAL_DASHBOARD=true` (default) as this would install Grafana.
  
### No Data
  - Hover over the information (`i`) icon in the panel. The information may show cases where data is not available.
  - Check the datasource by browse to "Connections/Data sources", you should see a Prometheus data source `https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local:9090`. Click on `Test` button to test the data source.
  - Check data source which is defined in `kube-prometheus-stack-grafana-datasource` configmap:
    ```console
      $ oc get cm kube-prometheus-stack-grafana-datasource -n workload-
    variant-autoscaler-monitoring -o yaml
    apiVersion: v1
    data:
      datasource.yaml: |-
        apiVersion: 1
        datasources:
        - name: "Prometheus"
          type: prometheus
          uid: prometheus
          url: http://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring:9090/
          access: proxy
          isDefault: true
          jsonData:
            httpMethod: POST
            timeInterval: 30s
        - name: "Alertmanager"
          type: alertmanager
          uid: alertmanager
          url: http://kube-prometheus-stack-alertmanager.workload-variant-autoscaler-monitoring:9093/
          access: proxy
    ...
    ```
  ### Dashboard Not Found
  - Dashboard is stored in `wva-operation-dashboard` configmap. Check configmap as follows:

    ```console
    $ oc get cm wva-operation-dashboard -n workload-variant-autoscaler-monitoring -o yaml
    apiVersion: v1
    data:
      operational-dashboard.json: |
        {
          "annotations": {
            "list": [
              {
                "builtIn": 1,
                "datasource": {
                  "type": "grafana",
                  "uid": "-- Grafana --"
                },
                "enable": true,
                "hide": true,
                "iconColor": "rgba(0, 211, 255, 1)",
                "name": "Annotations & Alerts",
                "type": "dashboard"
              }
            ]
        ...
    ```