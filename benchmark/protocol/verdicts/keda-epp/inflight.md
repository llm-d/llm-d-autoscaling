# Static signal analysis: keda-epp/inflight

Step 1 of the [evaluation protocol](../../README.md), produced with the
`static-signal-analysis` skill, on the autoscaling configuration llm-d ships by
default. The properties P1-P9 are defined in
[step 1](../../1-static-signal-analysis.md) §1.2. The configuration lives in
llm-d, not in this repository: cited llm-d paths are at `llm-d/llm-d@edb6405b`,
router paths at `llm-d/llm-d-router@647ca7ca`.

| | |
|---|---|
| Scenario | llm-d guide `guides/workload-autoscaling/keda-epp`, `SIGNAL=inflight`: `optimized-baseline/overlays/k8s/inflight/scaledobject.yaml` at `llm-d/llm-d@edb6405b`. The `ocp/inflight` overlay has the same trigger, with a Thanos `serverAddress` and `authModes: bearer` |
| Backend overlay | none: the guide deploys on llm-d `optimized-baseline` (GPU vLLM, Qwen3-32B) with its router unchanged, flow control off |
| Rendered with | by hand: the file is a plain manifest, `${NAMESPACE}` / `${TARGET_DEPLOYMENT}` substituted by `envsubst` |
| Date | 2026-11-12 |
| Verdict | **Accepted with conditions** |

Accepted with conditions: the one trigger counts requests from dispatch to last
token, which is close to proportional below saturation and correctly aggregated,
so it can drive the pool for a stable output-length mix; but its threshold of 16
has no measured basis, an empty query reads 0 with no fallback, and the 300 s
scale-up stabilization delays every scale-up by 5 minutes. Predicted: tracks
load at a fixed mix with a ~325 s reaction lag plus cold start, mis-sizes the
pool when the output-length mix moves, and drops to 1 replica if the EPP is down
for more than 300 s while the gateway keeps serving.

## Deployed versions

| Component | Version | Source of the version |
|---|---|---|
| EPP | `ghcr.io/llm-d/llm-d-router-endpoint-picker:main`, `pullPolicy: Always`: **floating**, resolved when the container starts | `llm-d/llm-d-router@647ca7ca:config/charts/routerlib/values.yaml#L78-L82`; no image override in the guide's router values |
| Router chart | `llm-d-router-standalone` `v0`: **floating** | `guides/env.sh:36` (`ROUTER_CHART_VERSION:-v0`) |
| vLLM | `docker.io/vllm/vllm-openai:v0.31.0` | `guides/optimized-baseline/modelserver/gpu/vllm/base/kustomization.yaml:9` -> `recipes/modelserver/components/images/gpu-vllm/release` |
| KEDA | not pinned; the README links KEDA 2.20 docs | `guides/workload-autoscaling/keda-epp/README.md:178`; trap T7 *unverified* for the installed release |
| Prometheus | kube-prometheus-stack (k8s) / OpenShift Thanos Querier (ocp); EPP scraped every 10 s | `guides/recipes/router/features/monitoring.values.yaml:6` (`interval: "10s"`) |

The trigger reads no vLLM series, so the vLLM traps (T1, T13) do not apply.

Floating EPP: the findings below are read at `llm-d/llm-d-router@647ca7ca`
(`main`, 2026-11-12). As of that date the `:main` tag carries #3439
(2026-10-30), so T3b does not apply; a deployment that pins a release (`v1.0.0`,
`v0.11.0`) does have T3b. The tag can move, so resolve the running digest before
relying on this.

## ScaledObject `optimized-baseline-keda-epp`

- `minReplicaCount: 1`, `maxReplicaCount: 8` (`scaledobject.yaml:30-31`).
- `pollingInterval: 15` (`:28`), which KEDA applies to scaling from 0 to 1; from
  1 to N the HPA polls on its sync period (15 s by default). `cooldownPeriod:
  300` (`:29`) is only used for scale to zero, which `minReplicaCount: 1`
  excludes.
- Scale-up: `stabilizationWindowSeconds: 300`, 100 % per 15 s (`:36-41`).
- Scale-down: `stabilizationWindowSeconds: 300`, 100 % per 15 s (`:42-47`).
- No `fallback`, no `ignoreNullValues`, no `activationThreshold`.

### Trigger `epp-inflight-requests`

```promql
sum(llm_d_epp_inflight_requests{namespace="<NAMESPACE>",producer_name="inflight-load-producer"})
```

`metricType: AverageValue`, `threshold: "16"`, `activationThreshold` unset
(`scaledobject.yaml:51-56`).

- **What it counts:** a gauge per endpoint, created and incremented in
  `PreRequest` after scheduling, before the request reaches vLLM, and
  decremented at `EndOfStream` (or when the request is deleted). It therefore
  counts each request from dispatch to its last token, including time queued
  inside vLLM. `source`:
  `llm-d/llm-d-router@647ca7ca:pkg/epp/framework/plugins/requesthandling/dataproducer/inflightload/producer.go#L471-L473`
  (`WithLabelValues(...)`, `requestGauge.Inc()`), `#L621-L633` (`EndOfStream` ->
  `PluginState.Delete`), `#L202-L211` (`OnEvicted` -> `requestGauge.Dec()`);
  metric defined in `metrics.go#L32-L38` (labels `endpoint_name, namespace,
  producer_name, fairness_id, priority`).
  - Held to the last token here: the early release at `StartOfStream`
    (`producer.go#L588-L600`) frees the request count only for a profile named
    `prefill`; optimized-baseline's router has one profile, `default`.
  - Not dropped early: the producer binds the request's liveness
    (`producer.go#L499`), so the 5-minute PluginState reaper skips requests
    whose context is alive
    (`pkg/epp/framework/interface/plugin/plugin_state.go#L239-L253`).
  - Not double-counted across EPP replicas: the gauge is local to the EPP that
    dispatched the request, and each request passes through one EPP.
    `CrossReplicaState` (`producer.go#L313-L335`) shares scheduling attributes,
    not the metric.
  - No series before the first request: an EPP that has served nothing since it
    started exports no `llm_d_epp_inflight_requests` series, so the query is
    empty, not 0 (matters for P7).
- **Kind:** requests in progress (send to last token). **Role it can play:**
  driver, only for a stable output-length mix
  ([step 1](../../1-static-signal-analysis.md) §1.3). **Role it plays here:**
  driver, the only trigger.

| Property | Result | Evidence | Tag |
|---|---|---|---|
| P1 proportional | pass below saturation | By Little's law the total is arrival rate × time from dispatch to last token. Below saturation that time is mostly decode and changes little with load; [step 1](../../1-static-signal-analysis.md) §1.3 gives `ε` 1.0-1.24 for this kind | source |
| P2 coupled to actuation | pass below saturation; fail above | Below saturation, adding a pod spreads the same requests over more pods (`η ≈ -1`). Above saturation the total includes requests waiting in vLLM, which drain when a pod is added, so the total falls (`η < -1`) | source |
| P3 fixed busyness | fail across output-length mixes | The threshold is a per-pod count, `16`, with no capacity in the query. A replica at capacity holds `μ × E2E` requests, and E2E scales with output length, so 16 is a different fraction of capacity per mix; [step 1](../../1-static-signal-analysis.md) §1.3 gives `V` up to 31 across output lengths. Where 16 came from is not recorded in the guide; its value for any mix *needs measurement* | config, measure |
| P4 metric type | pass | `sum(...)` is extensive, under `AverageValue` (`:51`, `:55`): `desired = ceil(total / 16)` | config |
| P5 bounds | pass, overshoot size unknown | Not capped at the EPP: under open-loop arrivals, requests queued in vLLM keep adding to it past saturation (integrating). Only `maxReplicaCount: 8` and the scale-up policy bound the overshoot; how far it overshoots during a cold start *needs measurement* | config, measure |
| P6 zero replicas | n/a | `minReplicaCount: 1` | config |
| P7 fails visibly | fail | No `or vector(0)`, but `ignoreNullValues` is not set and defaults to `true` (the guide's `README.md:536` says so; trap [T7](../../traps.md), release not verified), and no `fallback` is set, so an empty result reads 0. The router fails open (`guides/recipes/router/base.values.yaml:18`, `failureMode: "FailOpen"`): with the EPP down the gateway keeps serving, so the pool carries load while its signal reads 0. The README's "No `fallback` is configured, so autoscaling fails visibly" (`README.md:221`) holds for scaler errors (auth), not for an empty result | config, source, measure |
| P8 lag and noise | fail on lag; noise needs measurement | Lag: `≤ 10 (scrape) + 0 (no window) + ≤ 15 (HPA sync)`, then `scaleUp.stabilizationWindowSeconds: 300`: for scale-up the HPA uses the lowest recommendation of the last 300 s, so a rising signal is acted on ~300 s late, about `10 + 15 + 300 ≈ 325 s` after a step, before any cold start. Noise: at `R` pods one request moves the ratio by `1 / (16 R)`, but the sample-to-sample swing of an in-flight count at steady load can exceed the 10 % tolerance at small `R`; the 300 s windows then turn it into bias (scale-up takes the minimum, scale-down the maximum) | config, measure |
| P9 measures what it claims | pass, with conditions | Counts dispatch to last token, as the role needs. T3b (negative residue after scale-in) does not apply on `main` after #3439; it applies if the EPP is pinned to `v1.0.0` or `v0.11.0`. Requests rejected before scheduling are not counted (`PreRequest` is never reached); with flow control off these are errors, not load the pool could serve. Requests that fail open around the EPP are not counted (P7). The query selects by `namespace` and `producer_name` only, so a second pool's EPP in the same namespace, or a second scrape of the same EPP, adds into the sum | source |

**Failure case (P2):** load at 1.5 replicas' worth on 1 pod, open loop. vLLM's
queue grows, so the total keeps rising and asks for more pods than 1.5 replicas'
worth; once they are Ready the queue drains and the total falls back toward `λ ×
E2E`. The pool overshoots, up to `maxReplicaCount: 8`, then holds the extra pods
for the 300 s scale-down window before shrinking.

**Failure case (P3):** illustrative numbers, to be replaced by measurement. If
one replica at the SLO holds 40 concurrent long-output requests but only 8
short-output ones, threshold 16 is `16 / 40 = 40 %` of capacity for the long mix
(the pool runs 2.5× larger than needed) and `16 / 8 = 200 %` for the short mix
(the pool runs at 2× its capacity while the trigger reads on target).

**Failure case (P7):** 6 replicas serving; the EPP pod crashes or is evicted and
stays down for more than 300 s. The gateway fails open and keeps sending traffic
to the vLLM pods, while the EPP's series disappear. The query returns empty,
KEDA reports 0, `desired = ceil(0 / 16) = 0`, clamped to `minReplicaCount: 1`.
After the 300 s scale-down window, the 100 %-per-15 s policy (`:45-47`) takes
the pool from 6 to 1 in one step while it is still serving 6 replicas' worth.

**Failure case (P8):** load steps from 2 to 6 replicas' worth at `t = 0`. The
total takes about one request's E2E to fill to `λ × E2E`, then the HPA's
recommendation reaches 6; the scale-up window holds the count at 2 until about
`t = 325 s`, and the 100 %-per-15 s policy then goes 2 → 4 → 6 over two syncs.
The new pods then need a cold start (*needs measurement*) before they serve. For
that whole interval 2 replicas carry 6 replicas' worth of load.

## Combination

- **Driver in normal operation:** `epp-inflight-requests`, the only trigger.
- **Who decides where:** this trigger at every load level. There is no guard (KV
  cache) and no scale-in veto.
- **Scale-in:** at fixed load below saturation the trigger reads `λ × E2E`,
  which does not collapse after a scale-up, so it does not drive the pool below
  need. After a load drop the 300 s window holds the highest recommendation,
  then the 100 % policy can remove every pod above the new need in one step. A
  transient overshoot built while vLLM was queuing is released the same way.
- **`behavior` and bounds:** the scale-up window trades lag (P8) for damping the
  overshoot of a signal that inflates while new pods start, and it filters
  upward noise; the guide's comment says the damping is the intent.
  `maxReplicaCount: 8` caps any overshoot.

**Predicted behavior:** at a fixed output-length mix and below saturation, the
pool tracks load at the density set by the threshold, reacting about 325 s after
each step plus cold start. Past saturation it overshoots and then holds the
extra pods for the scale-down window. When the mix shifts to longer outputs the
pool grows more than capacity requires; to shorter outputs it stays too small
while the trigger reads on target. An EPP outage of over 300 s takes the pool to
1 while the gateway keeps serving.

## Needs measurement

1. Concurrent requests one replica sustains at the TTFT and ITL SLOs, for each
   output-length mix the guide targets (Qwen3-32B, vLLM v0.31.0, the guide's
   hardware). Needed by P3, to set or replace the threshold of 16.
   llm-d-benchmark's `make calibrate-serving-capacity`
   (llm-d/llm-d-benchmark#2108) measures serving capacity.
2. How much the output-length mix moves in the target workload. Needed by P3, to
   decide whether a request count can be the driver or a token-weighted rate is
   needed.
3. Cold start of one replica (model download, load, warm-up) on the guide's
   stack. Needed by P5 and P8, to size the overshoot and to set the scale-up
   window or policy.
4. Sample-to-sample swing of the trigger value at steady load, at 1 and at
   several replicas. Needed by P8, to decide whether a scale-up window shorter
   than 300 s still filters the noise.
5. The KEDA release installed. Needed by P7, to settle T7 and to know whether
   `fallback.behavior` is available.
6. The EPP digest running. Needed by P9, to decide whether T3b applies.

## Fixes

| Fails | Fix | Effect |
|---|---|---|
| P2, P5 | No change to the trigger: the overshoot past saturation is what this kind does (§1.3). Bound it with the scale-up policy from the P8 fix, and keep `maxReplicaCount` at the largest pool the workload needs | the overshoot is limited by the policy rate and the cap, not by luck |
| P3 | Set the threshold from measurement 1, as `0.7 ×` the measured per-replica concurrency for the mix; if measurement 2 shows the mix moves, move the driver to a token-weighted offered rate divided by measured capacity | the target means the same busyness for the measured mix |
| P7 | Add `ignoreNullValues: "false"` to the trigger metadata and `fallback: {failureThreshold: 3, replicas: 1, behavior: currentReplicas}`. Not a fixed `replicas` sized for peak: an EPP that has not served yet has no series, so a static fallback would scale an idle pool to that size | an empty result becomes a scaler error, and KEDA holds the current replica count instead of reading 0 |
| P8 | Set the scale-up window from measurements 3 and 4, well below cold start but long enough to filter the noise; if the window is shortened to damp less, bound the overshoot with a `Pods` policy sized to the largest step expected. Trade-off: the 300 s window is deliberate (the guide's comment says it damps the overshoot while new pods start), and a one-pod-per-cold-start limit alone would take 4 cold starts to go from 2 to 6 | the first pod starts well before a cold start has passed; the overshoot stays bounded by the policy |
