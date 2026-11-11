# Metric traps

Known cases where a scaling query does not measure what its metric's name
suggests. [Step 1](1-static-signal-analysis.md) checks each trigger against this
list (property P9).

Every trap is a fact about specific code, so each entry records the component,
the versions on which it was verified, and where in the source it lives. A trap
says nothing about versions outside its range.

## Using this list

1. Record the deployed version of each component a trigger reads: the vLLM
   image, the EPP image, the gateway, KEDA. Take them from the scenario and its
   backend overlay. A floating tag (`latest`, `main`, `auto`) has no version
   until it is resolved: read the image digest from the running pod, and tag
   the result *needs measurement* until then.
2. For each trap on a component the trigger reads:
   - deployed version inside the verified range: the trap applies;
   - deployed version outside it, or not determinable: the trap is
     *unverified*. Re-read the cited source at the deployed version, then
     record the result here.
3. When re-reading confirms a trap on a new version, extend its range. When it
   no longer holds, record the version that fixed it. Do not delete the entry:
   older deployments still have it.

## Traps

| # | Component | Trap | Verified on | Source |
|---|---|---|---|---|
| T1 | vLLM | No arrival counter. The earliest counter is `prefix_cache_queries`, recorded at admission; `http_requests_total` counts completions. A vLLM-side "demand" query reads admitted or completed work. Demand must come from the router or gateway. | `vllm-project/vllm@e1a5086` (2026-10-12) | `vllm/v1/metrics/loggers.py` |
| T2 | llm-d-router (EPP) | `llm_d_epp_average_*` gauges freeze. When no endpoint has fresh metrics, `ready_endpoints` goes to 0 but the averages keep their last value, so a trigger on them never scales in after the pool empties. | `llm-d/llm-d-router@333f331e` (`main`, 2026-11-11); no difference found for `v1.0.0` (`b9393d1a`) or `v0.11.0` (`a5cbe600`) | `pkg/epp/datalayer/logger/logger.go`, `refreshPrometheusMetrics` returns before the `average_*` writes when the pod count is 0 |
| T3a | llm-d-router (EPP) | `llm_d_epp_inflight_tokens` after a scale-in: a deleted pod's series keeps a positive residue that falls to 0 within seconds. Scale-in is delayed briefly. | tag `v0.10.0` (`71f4f09`) and earlier. Not re-verified: older than the releases audited on 2026-11-11 | `pkg/epp/framework/plugins/requestcontrol/dataproducer/inflightload/producer.go` |
| T3b | llm-d-router (EPP) | `llm_d_epp_inflight_tokens` and `llm_d_epp_inflight_requests` after a scale-in: a request still waiting for its first token when its pod is deleted re-creates the pruned series at minus its tokens (requests at −1) for the life of the EPP process, and a pod that returns with the same name inherits it. Under prefill load every scale-in lowers `sum()` for good. Workaround: intersect with `llm_d_epp_per_endpoint_queue_size` (`model_server_endpoint` = `endpoint_name`) with `and on (target_pod)` after `label_replace`, `clamp_min(…, 0)` each series, then `sum`, and pin `namespace` on both sides. | from #2577 (`75d6c85`, 2026-08-27): tags `v0.11.0` (`a5cbe600`) and `v1.0.0` (`b9393d1a`). **Fixed on `main`** by #3439 (`79c85447`, 2026-10-30), in no release yet: there the series stays pruned and plain `sum()` is correct | releases: `pkg/epp/framework/plugins/requestcontrol/dataproducer/inflightload/producer.go`; `main`: `…/requesthandling/…` (moved by #3830, `7f05d9ca`) |
| T4 | llm-d-router (EPP) | `vllm:num_requests_waiting` with EPP flow control on is a bounded sawtooth (read 8–59 while the true backlog was 120–220): it measures supply-side saturation, not demand. Applies only when the `flowControl` feature gate is on; it is off by default. | `llm-d/llm-d-router@333f331e` (`main`, 2026-11-11); no difference found for `v1.0.0` (`b9393d1a`) or `v0.11.0` (`a5cbe600`) | gate default: `cmd/epp/runner/runner.go` (`RegisterFeatureGate(flowcontrol.FeatureGate, false)`) |
| T5 | llm-d-router (EPP) | Per-stage `llm_d_epp_flow_control_pool_saturation{stage="prefill"\|"decode"}` series are deleted when that stage has no endpoints, so a trigger on them reads 0 (via `ignoreNullValues`) exactly when that pool is at zero. `{stage="effective"}` is always written. | `llm-d/llm-d-router@333f331e` (`main`, 2026-11-11); no difference found for `v1.0.0` (`b9393d1a`) or `v0.11.0` (`a5cbe600`) | `pkg/epp/flowcontrol/controller/internal/processor.go` |
| T6 | Gateway configuration | A catch-all route (`prefix: "/"`) counts every proxied path as demand, `/metrics` scrapes included (read 3.0 req/s under a 0.19 req/s trace). Route the inference paths to a dedicated Envoy cluster and read `envoy_cluster_upstream_rq_total{envoy_cluster_name=…}`. | Envoy v1.33, as configured by the llm-d guides' Envoy config (guide commit not recorded) | the gateway's route configuration, not Envoy's code: re-check the deployed route table |
| T7 | KEDA | The Prometheus scaler's `ignoreNullValues` defaults to `true`, turning an empty result into 0 rather than an error, so `fallback` does not fire. (Used by P6 and P7.) | `kedacore/keda@3447193` (2026-10-26) | `pkg/scalers/prometheus_scaler.go` |
| T8 | llm-d-router (EPP) | `llm_d_epp_request_total` is not an arrival counter: it is incremented after admission and scheduling, so shed requests, and queued requests later rejected, never appear. The router's offered rate is `llm_d_epp_flow_control_requests_total` summed over every `outcome`, which exists only with the `flowControl` gate on. | `llm-d/llm-d-router@333f331e` (`main`, 2026-11-11) | `pkg/epp/metrics/llm_d_router_metrics.go`; `docs/metrics.md` (flow-control counters) |
| T9 | llm-d-router (EPP) | Under the default `stalenessPolicy: saturated`, an endpoint with missing metrics scores saturation 1.0, inflating `llm_d_epp_flow_control_pool_saturation`. Gate saturation queries on `llm_d_epp_flow_control_stale_endpoints == 0`. | from #2492 (`f380c2c4`): `v0.11.0`, `v1.0.0`, `llm-d/llm-d-router@333f331e` (`main`, 2026-11-11) | `pkg/epp/framework/plugins/flowcontrol/saturationdetector/utilization/config.go` |
| T10 | vLLM, with the EPP's flow control | vLLM's time to first token excludes the router queue when flow control is on, so it misses where the latency went. Use `llm_d_epp_request_ttft_seconds`, measured from when the EPP received the request. | `llm-d/llm-d-router@333f331e` (`main`, 2026-11-11) | `pkg/epp/metrics/llm_d_router_metrics.go` |
| T11 | llm-d-router (EPP) | `llm_d_epp_request_input_tokens` / `_output_tokens` lag by a whole request and count only admitted work. On `main` and `v1.0.0` they are recorded once at end of stream (#3252, `1768eb5b`); on `v0.11.0`, from every chunk that carries usage. A streaming request counts only if the client sets `stream_options.include_usage`; otherwise it reads 0. | `llm-d/llm-d-router@333f331e` (`main`, 2026-11-11), `v1.0.0`, `v0.11.0` | `pkg/epp/metrics/llm_d_router_metrics.go` |
| T12 | llm-d-router (EPP) | Shed requests by reason: `llm_d_epp_request_outcome_total{outcome="rejected"}`, by `reason`, exists on `main` only (#3517, `5b3f0be6`). `v1.0.0` and `v0.11.0` have only `llm_d_epp_request_error_total` by `error_code`. Neither is offered demand; use them for availability and alerts. | `llm-d/llm-d-router@333f331e` (`main`, 2026-11-11); absent from `v1.0.0` and `v0.11.0` | `pkg/epp/metrics/llm_d_router_metrics.go` |

## Deployed versions in this repository

As of this list, the scenarios deploy, through the
[llm-d-benchmark](https://github.com/llm-d/llm-d-benchmark) defaults
(`config/templates/values/defaults.yaml` at `bd7d768`, 2026-10-09) and the
backend overlays in `benchmark/config/cluster-configs/`:

| Component | Version | Where set | Traps in range |
|---|---|---|---|
| EPP | `v0.11.0` | llm-d-benchmark defaults | T2, T3b, T4, T5, T9, T11 (per-chunk form). T8 and T10 *unverified*: verified on `main` only. T12 does not apply: no outcome counter. T3b means `sum()` over `inflight_tokens` needs the full workaround |
| vLLM | `v0.30.0` (`vllm-openai`), `v0.27.0-x86_64` (`ocp/model-sim-qwen3-32b.yaml`) | defaults; overlay | T1 *unverified*: verified on a commit, not on these tags |
| vLLM (CUDA) | `auto` (`ocp/vllm.yaml`) | overlay | floating: resolve first |
| inference-sim | `latest` (`k8s/inference-sim.yaml`) | overlay | floating: resolve first; T1 is about vLLM, so it does not carry over to the simulator |
| KEDA | 2.20 or later (`benchmark/README.md`) | cluster | T7 *unverified*: verified on a commit, not on a release |

The router entries were last audited on 2026-11-11 against `main` at
`333f331e` and the `v1.0.0` and `v0.11.0` releases. vLLM and KEDA entries are
still verified on commits, not on the tags deployed here: mapping them to
release tags is the next maintenance task for this list.
