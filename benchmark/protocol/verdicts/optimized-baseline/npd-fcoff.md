# Static signal analysis: optimized-baseline/npd-fcoff

Step 1 of the [evaluation protocol](../../README.md), produced with the
`static-signal-analysis` skill. The properties P1-P9 are defined in
[step 1](../../1-static-signal-analysis.md) §1.2. Scenario paths are at
`llm-d/llm-d-autoscaling@4716fb91`; router paths at
`llm-d/llm-d-router@b9393d1a` (`v1.0.0`); vLLM paths at
`vllm-project/vllm@db9527a4` (`v0.31.0`).

| | |
|---|---|
| Scenario | `benchmark/config/scenarios/staging/optimized-baseline/npd-fcoff.yaml` |
| Backend overlay | `benchmark/config/cluster-configs/ocp/vllm-npd.yaml` |
| Rendered with | `llmdbenchmark standup --dry-run`, llm-d-benchmark@`3c5aa40`. The template on llm-d-benchmark `main` (`a180aad`) differs only by optional fields this scenario does not set (#2180), so the rendered object is the same |
| Load | `stair.yaml.in` replaying `gpu-stair` (`hack/npd-fcoff/gen_traces.py:36-37`): open loop, Poisson arrivals, `q` = 0.8, 1.6, 2.4, 3.2, 4.0 req/s in 300 s stages, ISL 8192, OSL 128, no prefix reuse. `λ_in` = 6,554 to 32,768 tokens/s, `λ_out` = 102 to 512 tokens/s. Also read against `gpu-mixshift` (4.9 req/s of 8192:128, then 23.22 req/s of 128:512) and `gpu-overload-hard` (2.9 then 16 req/s of 8192:128) |
| Date | 2026-11-17 |
| Verdict | **Accepted with conditions** |

Accepted with conditions: the one trigger is an offered work rate counted at the
router before any queue (flow control is off), so it is proportional to load and
does not move when pods are added (P1, P2, P4 pass from the source). It is the
right kind of driver. The conditions are a threshold measured on the deployed
plant (the shipped default is the research plant's, 0.80 rather than 0.70 of the
capacity the README measured on this overlay, P3), a visible failure mode for an
EPP outage (P7 fails: the query reads 0 and the pool drains to 1), and three
query details that turn edge cases into wrong values: the ratio denominators,
prefix caching being on, and the namespace-only selector (P8, P9). Predicted:
replicas step at `ceil(ν / threshold)` on the staircase and hold across the
mix shift, as the scenario's validation reports. An EPP scrape outage removes
one pod per 300 s while traffic continues. Short bursts after an idle minute,
and any window with no completions, inflate the ratio terms by up to 1,000×.

## Deployed versions

| Component | Version | Source of the version |
|---|---|---|
| EPP | `ghcr.io/llm-d/llm-d-router-endpoint-picker:v1.0.0` | llm-d-benchmark@`3c5aa40`:`config/templates/values/defaults.yaml:119` (same on `main` `a180aad`); resolved in the rendered `config.yaml:22`. The README's GPU knee was measured on EPP `v0.11.0` |
| Router chart | `v1.0.0` | llm-d-benchmark `defaults.yaml:115` |
| vLLM | `docker.io/vllm/vllm-openai:v0.31.0@sha256:a4a4c043…` | `cluster-configs/ocp/vllm-npd.yaml:77` (pinned by digest) |
| KEDA | 2.20 or later, not pinned | `benchmark/README.md`; trap T7 *unverified* for the installed release |
| Prometheus | OpenShift Thanos Querier, bearer auth; EPP scraped every 5 s, vLLM every 5 s | `vllm-npd.yaml` `keda.prometheus`; scenario `:155` (`router.monitoring.interval: "5s"`), `:196` (`monitoring.scrapeInterval: "5s"`) |

## ScaledObject `npd-fcoff`

- `minReplicaCount: 1`, `maxReplicaCount: 6` (`27_keda-scaledobjects.yaml:15-16`).
- `pollingInterval`: not rendered, KEDA default 30 s (scenario `:205`).
- `behavior`: scale-up stabilization 0 s, at most +1 pod per 180 s; scale-down stabilization 300 s, at most -1 pod per 300 s (`27_keda-scaledobjects.yaml:44-58`).
- No `fallback`, no `ignoreNullValues` (not set in the scenario; KEDA default `true`, trap T7).

### Trigger `epp-offered-work-rate`

```promql
(
  (sum(rate(llm_d_epp_request_total{namespace="<ns>"}[1m])) or vector(0))
  + (sum(rate(llm_d_epp_request_error_total{namespace="<ns>",
                                           error_code="ServiceUnavailable"}[1m])) or vector(0))
)
*
(
  ((sum(rate(vllm:prefix_cache_queries_total{namespace="<ns>"}[1m]))
    / clamp_min(sum(rate(vllm:request_success_total{namespace="<ns>"}[1m])), 0.001)
   ) or vector(8192))
  + 3.3 *
  ((sum(rate(vllm:generation_tokens_total{namespace="<ns>"}[1m]))
    / clamp_min(sum(rate(vllm:request_success_total{namespace="<ns>"}[1m])), 0.001)
   ) or vector(128))
)
```

`metricType: AverageValue`, `threshold: "14619"`, `activationThreshold: "0"`
(`27_keda-scaledobjects.yaml:20`, `:42-43`).

The query is `ν = (D1 + D15) × (L̄_p + 3.3 × L̄_out)`, in work tokens per second.

- **What it counts:**
  - `D1`, `llm_d_epp_request_total`: a counter, incremented once `director.HandleRequest` returns without error, which is after admission, endpoint lookup and scheduling (`pkg/epp/handlers/server.go#L614`, `#L651`). With flow control off, admission is `LegacyAdmissionController`, which rejects only sheddable requests (`priority < 0`) on a saturated pool (`pkg/epp/requestcontrol/admission.go#L118-L137`, `pkg/epp/util/request/sheddable.go#L20-L22`). The EPP holds no queue, so in this plant `D1` is the arrival rate of requests that found an endpoint. `source`.
  - `D15`, `llm_d_epp_request_error_total{error_code="ServiceUnavailable"}`: a counter, incremented in the request's deferred error path (`server.go#L461-L463`). `ServiceUnavailable` is returned when no endpoint candidate exists or screeners remove them all (`pkg/epp/requestcontrol/director.go#L279-L294`). So `D1 + D15` is the arrival rate, minus requests that fail parsing (`BadRequest`) or scheduling (`ResourceExhausted`, `director.go#L327`). `source`.
  - `L̄_p` numerator, `vllm:prefix_cache_queries`: a counter of the prompt tokens looked up in the prefix cache when the scheduler first places a request (`vllm/v1/core/kv_cache_manager.py#L253-L262`, incremented at `vllm/v1/metrics/loggers.py#L1052-L1054`). Cached and uncached tokens both count. Re-lookups of preempted requests go to separate fields and are not counted (`vllm/v1/metrics/stats.py#L132-L143`). Nothing is recorded when prefix caching is off (`kv_cache_manager.py#L255-L256`, via `prefix_cache_lookup_enabled`, `#L249-L251`; default on, `vllm/config/cache.py#L142`). `source`.
  - `L̄_out` numerator, `vllm:generation_tokens`: a counter of tokens generated per engine step (`loggers.py#L1134-L1136`), including tokens of requests later aborted. `source`.
  - Denominator, `vllm:request_success`: a counter, incremented per finished request in `process_outputs` (`loggers.py#L1153-L1156`, via `vllm/v1/engine/output_processor.py#L766`). Client aborts go through `abort_requests` (`output_processor.py#L517-L578`), which does not record finished-request stats, so they are not in the denominator. `source`.
- **Kind:** offered work rate, counted before any queue, token-weighted (§1.3). **Role it can play:** driver. **Role it plays here:** driver, the only trigger.

| Property | Result | Evidence | Tag |
|---|---|---|---|
| P1 proportional | pass | `D1 + D15` is the arrival rate, and at a fixed mix `L̄_p` and `L̄_out` are constants, so `ν = q × 8,614.4` (`8,192 + 3.3 × 128`). Doubling `q` doubles `ν`: `ε = 1` at every utilization, including past capacity, since the EPP does not queue with flow control off. Staircase: `ν` = 6,892, 13,783, 20,675, 27,566, 34,458 | source |
| P2 coupled to actuation | pass | `D1 + D15` is counted at the router and the ratios are pool-wide means; neither depends on `R` under open-loop load. At 2.4 req/s, `U = 20,675` at 2 pods and at 3; desired `ceil(20,675 / 14,619) = 2` both times. `η = −1`. Under the closed-loop `agentic` workload, `q` itself moves with `R` (§1.1) | source |
| P3 target means a fixed busyness | fail as shipped; pass at a fixed mix with a plant-measured threshold | **Constant:** `thresholdWork: "14619"` (scenario `:68`) is 0.70 of the research plant's knee. The README measured this overlay's knee at 18,331 work tok/s (`workloads/npd-fcoff/README.md:238`): `14,619 / 18,331 = 0.80`, so the default leaves `1 / 0.80 − 1 = 25 %` spare, not 43 %. **Mix:** `V = 1` only if 3.3 is the true decode-to-prefill cost ratio on the deployed engine for every mix; the scenario calls it a model and hardware fit (`:69-71`). **Hit rate:** `L̄_p` counts cached tokens too. At a 75 % hit rate an 8192:128 request computes `0.25 × 8,192 + 422.4 = 2,470` work tokens while the signal counts 8,614: if capacity follows computed work, the threshold stands for `8,614 / 2,470 = 3.5×` less busyness. That direction over-provisions | config / measure |
| P4 metric type matches aggregation | pass | `sum(rate(...))` totals times ratios of totals: doubling pods at the same per-pod load doubles `D1 + D15` and leaves the ratios unchanged, so the value is extensive. Rendered `metricType: AverageValue` (`:20`) | config |
| P5 bounded the right way | pass | A rate of arrivals: neither capped nor integrating. At `gpu-overload-hard`, `16 × 8,614.4 = 137,830`, desired `ceil(137,830 / 14,619) = 10`, capped at 6 by `maxReplicaCount`; one pod per 180 s takes the pool from 3 to 6 in 540 s. When every request times out, completions stop, and the denominators' `clamp_min(…, 0.001)` makes the ratios explode too (see P8). That happens only when the need already exceeds `maxReplicaCount`, so it is harmless here | source / config |
| P6 defined at zero replicas | pass (`minReplicaCount: 1`) | At `R = 0`, `D1 = 0` (the request fails in `HandleRequest` before `server.go#L651`), `D15` = arrival rate, and the vLLM series are absent, so the defaults apply: `ν = q × 8,614.4 > 0 = activationThreshold`. The configuration could scale from zero. The scenario's kind `bootstrap` arm reports 0 → 1 | source |
| P7 fails visibly | fail | `or vector(0)` on both router terms (`27_keda-scaledobjects.yaml:28-30`), no `fallback`, `ignoreNullValues` default `true`. If the EPP stops being scraped, the first factor reads 0 and `ν = 0`. If the vLLM series vanish, the ratios silently become 8192 and 128 | config |
| P8 bounded lag and noise | pass on lag, marginal; fail on noise at one or two replicas; fail at the idle edge | **Lag:** `5 + 30 + 30 = 65 s` (EPP scrape, half the `[1m]` window, default `pollingInterval`), against a ~95 s H100 cold start (README `:200`): 68 % of the cold start, not well below it. **Noise:** at one pod on the threshold, `q = 14,619 / 8,614.4 = 1.70 req/s`, 102 arrivals per 60 s window, so Poisson noise `1 / √102 = 9.9 %`, equal to the 10 % tolerance. At 4 pods, 407 arrivals and 5.0 %. **Idle edge:** after a minute with no completions the denominators are 0 while the counters still exist, so `clamp_min` divides by 0.001 instead of falling back. `n` 8192-token requests admitted before the first completes give `L̄_p ≈ n × (8,192 / 60) / 0.001 = n × 136,533` and `ν ≈ (n / 60) × n × 136,533 = n² × 2,276`; three requests give 20,480, desired 2 | config / measure |
| P9 measures what it claims | pass, with conditions | Offered work, as the driver needs: `D1 + D15` counts arrivals in this flow-control-off plant. Traps: **T1** (vLLM has no arrival counter) was verified on `v0.27.0`/`v0.30.0`, and `v0.31.0` is outside that range; re-read at `v0.31.0`, the trap holds, and the query takes arrivals from the router, not vLLM. **T8** (`request_total` is counted after admission and scheduling) was verified on `main`; at `v1.0.0` the placement is the same (`server.go#L614` → `#L651`). Here the undercounts it names are a pool with no endpoint (covered by `D15`) and scheduler failures (`ResourceExhausted`, not covered). **T7** unverified. Conditions: (a) `L̄_p` reads 0, not the default, if prefix caching is turned off; (b) the selector is `namespace` only, and `request_total` carries no pool label (model labels plus `fairness_id` and `priority`, `pkg/epp/metrics/llm_d_router_metrics.go#L35`), so a second EPP or pool in the namespace is summed in; (c) under overload, aborted requests stay in the numerators but leave the denominator, which biases both ratios upward | source / config |

**Failure cases:**

- **P7, EPP outage.** `gpu-mixshift` phase 1 at 3 pods: 4.9 req/s of 8192:128, `ν = 42,211`, desired 3. The EPP's scrape fails for 10 minutes while Envoy keeps serving. The query reads `0 × (…) = 0`, desired `max(1, 0) = 1`. After the 300 s window the pool goes 3 → 2 at 300 s and 2 → 1 at 600 s. One pod is then offered `42,211 / 18,331 = 2.3×` its capacity, and requests time out.
- **P7, vLLM series lost.** `gpu-mixshift` phase 2: 23.22 req/s of 128:512, true `ν = 23.22 × (128 + 3.3 × 512) = 42,205`. If the vLLM PodMonitor stops (a relabel, a scrape config change), the ratios become 8192 and 128: `ν = 23.22 × 8,614.4 = 200,026`, desired 14, capped at 6 when the need is 3.
- **P3, default threshold.** On this overlay, at 4.0 req/s of 8192:128, the default asks for `ceil(34,458 / 14,619) = 3` pods; each runs at `34,458 / 3 / 18,331 = 63 %` of the measured knee. At 3.2 req/s it asks for 2: `27,566 / 2 / 18,331 = 75 %`, past the 70 % the law is designed for.
- **P8, idle edge.** One minute without traffic, then three requests arrive within a scrape interval. Until one completes (about 1.4 s for 8192:128 at low load, plus up to 5 s scrape), `ν ≈ 20,480`, desired `ceil(20,480 / 14,619) = 2`. With scale-up stabilization 0, one KEDA poll in that window adds a pod, which stays for at least 600 s.

## Combination

- **Driver in normal operation:** `epp-offered-work-rate`, the only trigger.
- **Who decides where:** the one trigger, everywhere. No guard covers KV-cache pressure, which a long-output mix could reach before `ν` says the pool is busy (within the reach of P3's mix question).
- **Scale-in:** driven by `ν` falling. It cannot go below need in normal operation, since `ν` does not depend on `R` (P2). It goes below need on an EPP outage (P7).
- **behavior and bounds:** +1 pod per 180 s bounds the P8 spikes to one extra pod each, and makes a step that needs +2 pods take 360 s. -1 pod per 300 s after a 300 s window drains slowly (6 → 1 in 1,800 s). `maxReplicaCount: 6` binds at `gpu-overload-hard`, where the need is 11 at the measured threshold (`137,830 / 12,832 = 10.7`).

**Predicted behavior:** at low and moderate load the pool holds 1 replica, with an occasional extra pod from a burst after idle. On the staircase, replicas step one at a time at `ceil(ν / threshold)`, about 65 s plus a 95 s cold start after each step. On the mix shift they hold at 3 while `q` rises 4.74×. Past capacity the trigger keeps rising and the pool pins at 6. At constant load the count is stable from 3 pods up; at 1-2 pods near the threshold, Poisson noise alone can add a pod.

## Needs measurement

1. This plant's knee in work tokens per second for at least two mixes (8192:128 and 128:512), to test whether 3.3 makes `V ≈ 1`; needed by P3 (mix).
2. Knee at prefix hit rates 0, 50 % and 75 % on a shared-prefix mix, to size the over-provisioning from counting cached tokens; needed by P3 (hit rate).
3. The trigger's sample-to-sample swing at steady load on one and two replicas near the threshold, and how often a single sample crosses `1.1 × threshold`; needed by P8 (noise).
4. Trigger value in the first 10 s after a burst of 1, 3 and 8 requests that follows ≥ 60 s of idle; needed by P8 (idle edge).
5. Cold start of one replica of the pinned `v0.31.0` image on the target GPU; needed by P8 (lag).
6. Whether the default plugin set ever returns `ResourceExhausted` from the scheduler under overload with flow control off (a gap in `D1 + D15`); needed by P9.
7. Trigger against offered `ν` during `gpu-overload-hard`, to size the abort bias in the ratios; needed by P9 (c) and P5.
8. The `job` and `service` labels of the EPP's `up` series under the chart's ServiceMonitor, for the P7 fix; needed by P7.

## Fixes

| Fails | Fix | Effect |
|---|---|---|
| trigger, P3 (constant) | Default `npdFcoff.thresholdWork` to the knee measured on this overlay, `"12832"` (README `:238-250`), or fail standup until it is overridden | Each pod's target becomes 70 % of the measured capacity, 43 % spare |
| trigger, P7 | Add `and on() (max(up{namespace="<ns>", job="<epp job>"}) == 1)` to the query, set `ignoreNullValues: false` on the trigger, and add `fallback: {failureThreshold: 3, replicas: 1, behavior: currentReplicas}`. llm-d-benchmark renders all three since #2180 (`8264f5b`) | An EPP outage makes the query empty, the scaler errors, and the pool holds its size instead of draining. The per-term `or vector(0)` stays, so an idle pool with no `request_total` series yet still reads 0 |
| trigger, P8 (idle edge), P5 (collapse) | Replace `/ clamp_min(sum(rate(vllm:request_success_total{…}[1m])), 0.001)` with `/ (sum(rate(vllm:request_success_total{…}[1m])) > 0)` in both ratios | No completions in the window makes the ratio empty, so `or vector(<default>)` applies, as the scenario header (`:27-28`) intends |
| trigger, P8 (noise) | Set scale-up `stabilizationWindowSeconds: 60`, so a scale-up needs two consecutive KEDA polls above target; or widen the router terms to `[2m]` (`1 / √204 = 7.0 %` at one pod) | One noisy sample no longer adds a pod. Either costs about 30-60 s of extra lag on a real step; choose after measurement 3 |
| trigger, P9 (a) | State in the scenario's prerequisites that prefix caching must stay on (vLLM's default), or take `L̄_p` from the EPP's dispatch-side `llm_d_epp_prefix_prompt_tokens` histogram (`_sum / _count`, `pkg/epp/framework/plugins/requestcontrol/dataproducer/prefixmetrics/metrics.go#L78-L88`), which does not depend on vLLM's cache setting. Whether the default plugin set emits it *needs measurement* | `L̄_p` cannot silently read 0 |
| trigger, P9 (b) | Add the EPP's `service` label (and the vLLM pods' pool label) to every selector before this law moves into a guide | A second pool in the namespace no longer adds its traffic |
