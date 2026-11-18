<!-- markdownlint-disable MD013 -- wide tables -->

# What an llm-d-benchmark run leaves for this figure

Two states of llm-d-benchmark are described:

- **stock**: upstream `main` at `4c08b9f9` (2026-11-12), with inference-perf `v0.7.0` (the pin in
  `build/Dockerfile`). Runs made before the next entry landed look like this.
- **main**: upstream `main` at `0fb187fb` (2026-11-18, merge of
  [#2192](https://github.com/llm-d/llm-d-benchmark/pull/2192), "Patches for load-vs-supply data"),
  which closes both required gaps below and keeps the per-request file under
  `--data-collect results`. inference-perf is still pinned at `v0.7.0`.

Code read only; nothing here was run against a cluster. Re-check before relying on a line number.

## What each series needs, and where a run keeps it

| Series | Stock | Main | Condition |
| --- | --- | --- | --- |
| Offered load (open loop) | `per_request_lifecycle_metrics.json` (inference-perf) or `profile_export.jsonl` (aiperf) | same | profile has `report.request_lifecycle.per_request: true`; collected with `--data-collect default` or `fast`; on main `results` also keeps it (`KEEP_PLAIN` in `llmdbenchmark/utilities/archive.py#L28-L35`), on stock it drops it |
| Offered load (closed loop) | the same records give the send rate; the load itself is a calculation from the `load.type: concurrent` settings | same | the measured send rate is not the load |
| Latencies for SLO attainment (optional) | inference-perf `computed_metrics` per request, written only with `per_request_fields.computed_metrics: true` (default false); otherwise derived from `start_time`, `end_time` and token times. aiperf: per-request metrics | same | the profile setting, or accept the raw derivation (not tokenizer-corrected) |
| Clock anchor | `inference-perf.partial.stage_N.yaml` `run.time.start` (epoch) | same | inference-perf times are `perf_counter`; the bridge's error is one inter-arrival gap, so bins of 30 s or more |
| Ready, requested | `metrics/processed/replica_status_timeseries.json`, **empty on a default stack** (gap 1) | same file, populated; a failed `kubectl get` is recorded as `collection_error` | `run --monitoring`; `--data-collect default` or `fast` |
| HPA desired replicas (optional) | not recorded | `metrics/processed/autoscaler_timeseries.json`: `hpa.<name>.desiredReplicas` per tick, read by the skill | as above |
| What KEDA read | **not recorded** (gap 2) | per-tick `hpa.<name>.currentMetrics`: for an External `AverageValue` metric this is the per-pod value (trigger value ÷ current replicas), one entry per trigger named `s<N>-<type>-<metric>`. Not read by the skill yet | stock: a Prometheus range query of each trigger expression (`keda_scaler_metrics_value`) |
| Threshold levels (optional) | `threshold:` and `metricType:` in the rendered ScaledObject (`plan/<stack>/30_keda-scaledobject.yaml` in the workspace) | same | `--scaledobject`, or `--trigger FILE=THRESHOLD` |

## Required gaps

A gap is listed only if, without it fixed, a core panel cannot be drawn from the run itself.
Gaps with a working workaround inside the run, or that touch only an optional element, are left
out: `--data-collect results` (a choice; the default keeps the files), the `perf_counter` clock
(bridged), the 15 s tick (coarser, not wrong), threshold levels (in the rendered manifest).

| # | Gap | Panel | Evidence (stock) | Status on main | Until fixed |
| --- | --- | --- | --- | --- | --- |
| 1 | Harness role cannot list Deployments or StatefulSets | replicas | `config/templates/jinja/05_namespace_sa_rbac_secret.yaml.j2#L22-L56`: `deployments` only inside the `fma` block, `get,patch`. `workload/harnesses/collect_metrics.sh#L123` then falls back to `{"items":[]}` silently | **closed** (`0fb187fb`): Role grants `apps/{deployments,statefulsets}`, `autoscaling/horizontalpodautoscalers`, `keda.sh/scaledobjects` get,list outside the `fma` block (`05_namespace_sa_rbac_secret.yaml.j2#L50-L60`); a failed `kubectl get` is recorded as `collection_error` (`collect_metrics.sh#L123-L127`). Verify on a cluster with `kubectl auth can-i list deployments --as=system:serviceaccount:<ns>:<sa>` | `--replicas-json`: Prometheus range query of `kube_deployment_status_replicas_ready` |
| 2 | What the autoscaler read is not recorded | trigger | `collect_metrics.sh#L123` is the only per-tick `kubectl get`; `llmdbenchmark/run/steps/step_09a_capture_cluster_state.py#L174`, `#L201` snapshot the HPA once, after the run | **closed upstream-side** (`0fb187fb`): `collect_autoscaler_state` (`collect_metrics.sh#L223`) records each HPA's `currentMetrics` per tick. **Open in the skill**: it does not read `currentMetrics` yet | `--trigger FILE`: Prometheus range query of each trigger expression, while retention lasts |
