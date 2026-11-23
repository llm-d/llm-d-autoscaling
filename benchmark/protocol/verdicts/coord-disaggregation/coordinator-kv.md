# Static signal analysis: coord-disaggregation/coordinator-kv

Step 1 of the [evaluation protocol](../../README.md). The properties P1-P9 are
defined in [step 1](../../1-static-signal-analysis.md) §1.2. An earlier
candidate drove decode on the coordinator's arrival rate; that fails P3 by 3.9x
(Simulator evidence below), which is why decode reads KV-cache usage here.

Source references used below:

- **C** = `llm-d/llm-d-router@b9393d1a` (`v1.0.0`), the coordinator release the
  guide pins.
- **E** = `llm-d/llm-d-router@32346cce` (`main`, 2026-11-18), for the EPP, which
  the guide runs from the floating `main` tag.
- **G** = `llm-d/llm-d@a0f3cb6c` (`main`), the guide, plus the PD-only overlays
  of llm-d/llm-d#3111 (open).
- **V** = `vllm-project/vllm` tag `v0.31.0`, the image the guide's model servers
  run (the shared recipe image component; the guide README's prose still says
  v0.30.0).
- **W** = `kubernetes-sigs/lws@588881ca` (`v1.0.0`).
- **K** = `kedacore/keda@3f88d60a` (`v2.19.0`).
- **S** = blis-signal-lab `25f2dfb` with the step 3 tooling of
  `llm-d/llm-d-autoscaling@49302d82`: simulator numbers (Qwen3-VL-32B, H100,
  bf16, TP2 per pod, prefix-based-pd-decider at 8,192, cold start 120 s, scrape
  30 s). Tag `sim`. They are not GPU measurements; they stand in for the *needs
  measurement* items until step 5. The model is not in the catalog of the
  blis-substrate release the lab pins: the runs used a local catalog entry,
  proposed upstream as inference-sim/blis-substrate#415. The inputs to rerun
  them (BLIS scenario, router config, trigger terms, loads) are in
  [`coordinator-kv/blis/`](coordinator-kv/blis/).

| | |
|---|---|
| Scenario | `benchmark/config/scenarios/staging/coord-disaggregation/coordinator-kv.yaml`, two ScaledObjects, `coord-pd-prefill` and `coord-pd-decode` (control: `baseline.yaml`, the same ScaledObjects pinned at prefill 2, decode 4) |
| Rendered with | `llmdbenchmark standup --dry-run -t kustomize`, llm-d-benchmark@`61e9e47` (`main`, with #2199, #2200, #2201), against llm-d `main` (`a0f3cb6c`) with llm-d/llm-d#3111 and #3114 merged locally (both open). The rendered `27_keda-scaledobjects.yaml` is the object analyzed here. The plan installs the router with the PD-only values file last, builds `coordinator/pd-only`, applies the Coordinator and model-server PodMonitors, and patches the DisaggregatedSet to encode 0 with prefill and decode External; a server-side dry run admits the patched set |
| Backend overlay | the guide's PD-only variant: `coordinator/pd-only`, `router/coord-disaggregation-pd-only.values.yaml`, encode replicas 0 with prefill and decode `scaling.mode: External` (G + #3111). Decode vLLM metrics come from the model-server PodMonitors of llm-d/llm-d#3114 (`modelserver/monitoring`, from the recipe `monitoring-epd`), which the guide's monitoring step applies |
| Load | the run set's mixes (`run-set.md` §1): 8192:256, 2048:2048, 256:4096 and their blend B, random prompts, SLO TTFT 2 s, TPOT 50 ms, X = 0.95. With `nonCachedTokens: 8192`, only 8192:256 reaches prefill; the other mixes are served by decode alone |
| Date | 2026-11-23 |
| Verdict | **Accepted with conditions** |

Accepted with conditions. Each pool has a driver that measures its own pool's
binding work: prefill on the uncached prompt tokens the EPP dispatches to it (an
offered work rate), decode on the KV-cache occupancy of its pods, with a backlog
alarm for when that occupancy saturates. The decode driver is the choice that
matters: no rate known at arrival tracks decode capacity across the run set's
mixes (arrival rate varies 3.9x, output tokens 4.1x, and no `L_p + c x L_out`
weighting fits), while KV occupancy at the knee varies 1.25x (sim). The
conditions:

- Decode KV usage is a capped, carried signal used as a driver. That is sound
  only while KV is decode's binding resource, which the simulator shows for this
  plant and the step 5 plant must confirm (P3, P5).
- The KV driver is mildly convex in load (P1, P2: elasticity 1.3 to 1.8). Its
  threshold is therefore set from knee *load*, not knee KV: 0.45 puts pods at 66
  to 75 % of knee load, where 0.7 x the knee KV (0.55) would put them at 76 to
  87 %. The loop stays stable for elasticity below 2 (below).
- The KV driver lags long-output traffic by about one decode duration (P8). The
  alarm and a 4-pods-per-60 s decode scale-up policy carry the step; that policy
  departs from the 1-pod-at-a-time direction used elsewhere and needs agreement.
- Prefill capacity in tokens/s falls with prompt length under a fixed TTFT SLO
  (P3: 1.67x from 8k to 12k, sim), so its threshold is set for the longest
  prompt class served.
- The decode triggers need the model-server PodMonitors (llm-d/llm-d#3114), and
  the prefill query works only on the single-EPP topology (P9).

Predicted: each pool settles at `ceil(signal / threshold)` and holds at constant
load. A mix shift toward long outputs is seen by decode about one decode
duration late; the alarm then takes decode to the cap within one cold start.
Under overload, both pools reach `maxReplicaCount`. A metric outage holds the
current count.

## Deployed versions

| Component | Version | Source of the version |
|---|---|---|
| Coordinator | `ghcr.io/llm-d/llm-d-router-coordinator:v1.0.0` | G `guides/recipes/modelserver/components/images/coordinator/release/kustomization.yaml:5-6` |
| EPP | `ghcr.io/llm-d/llm-d-router-endpoint-picker:main`, **floating** | G `router/coord-disaggregation.values.yaml:40-43`. Every EPP finding is tied to E; the deployed digest must be read from the running pod |
| vLLM | `docker.io/vllm/vllm-openai:v0.31.0` | G, a kustomize build of `modelserver/gpu/vllm/base` (all three roles); the README's v0.30.0 is stale |
| LWS (DisaggregatedSet) | `v1.0.0` | G `guides/env.sh` `LWS_VERSION` |
| KEDA | target cluster: 2.19 (OpenShift Custom Metrics Autoscaler) | `kubectl` on the target cluster, 2026-11-19 |
| Scrape | EPP 10 s (`guides/recipes/router/features/monitoring.values.yaml:6`); encode, prefill and decode vLLM 30 s (`coord-disaggregation/modelserver/monitoring`, llm-d/llm-d#3114, from `recipes/modelserver/components/monitoring-epd`) | G |

## Scale targets

The guide's DisaggregatedSet `pd-disagg-coord` has no scale subresource. The
ScaledObjects target the `DisaggregatedSetRoleScaler` `pd-disagg-coord-prefill`
and `pd-disagg-coord-decode`, created by LWS for roles with `scaling.mode:
External` (W `api/disaggregatedset/v1/disaggregatedset_types.go`), `slices: 1`.
KEDA's service account can get and update `disaggregatedsetrolescalers/scale` on
the target cluster (2026-11-19). External prefill and decode also take those
roles out of LWS v1.0.0's all-or-nothing replicas rule (W
`disaggregatedset_types.go:220-224`), which rejects encode 0 with Static prefill
and decode (llm-d/llm-d#3112).

## ScaledObject `coord-pd-prefill`

- `minReplicaCount: 1`, `maxReplicaCount: 4`, `pollingInterval: 15`.
- `fallback`: `failureThreshold: 3`, `replicas: 1`, `behavior: currentReplicas`
  (K `apis/keda/v1alpha1/scaledobject_types.go#L62-L65`).
- Scale-up: one pod per 180 s, no stabilization. Scale-down: one pod per 300 s
  after a 300 s window.

### Trigger `prefill-uncached-prompt-tokens`

```promql
sum(rate(llm_d_epp_prefix_prompt_tokens_sum{namespace="${NAMESPACE}",endpoint_role="prefill",plugin_name="decode-prefix-producer"}[2m]))
-
sum(rate(llm_d_epp_prefix_predicted_cached_tokens_sum{namespace="${NAMESPACE}",endpoint_role="prefill",plugin_name="decode-prefix-producer"}[2m]))
```

`metricType: AverageValue`, `threshold: "7782"` (0.7 x 11,117 computed prompt
tokens/s, the prefill knee for 8192:256, `sim`), `activationThreshold: "0"`,
`ignoreNullValues: "false"`.

- **What it counts:** two histograms. Each prefill call adds its prompt's token
  count to the first and the producer's predicted cached tokens on the chosen
  prefill pod to the second, so their difference is the uncached prompt tokens
  prefill computes. Labels `plugin_name, plugin_type, endpoint_role, modality`
  (E
  `pkg/epp/framework/plugins/requesthandling/dataproducer/prefixmetrics/metrics.go#L46-L52`,
  `#L82-L88`), recorded in the producer's `PreRequest` after scheduling (E
  `.../approximateprefix/plugin.go#L589-L606`; director order
  `requestcontrol/director.go#L339-L433`).
- **The role label:** `endpoint_role="prefill"` when the result has a target in
  the profile named `prefill`, otherwise `decode` (E
  `prefixmetrics/metrics.go#L245-L256`). The `header-profile-handler` runs only
  the profile the Coordinator's `x-llm-d-epp-profile` header names, so on the
  single EPP `prefill` marks exactly the prefill calls.
- **One producer:** the single-EPP config declares two
  `approx-prefix-cache-producer`s (`decode-prefix-producer` and the
  default-named one, G `router/coord-disaggregation.values.yaml`), and both
  record on every call, so `plugin_name` is pinned. Whether both predict the
  same cached tokens on prefill calls is *needs measurement* (item 5).

 **Kind:** offered work rate, token-weighted, counted at dispatch before any
 queue (flow control off, E `cmd/epp/runner/runner.go#L895`). **Role:** driver,
 the only trigger.

| Property | Result | Evidence | Tag |
|---|---|---|---|
| P1 proportional | pass | A rate of dispatch events: `q x f_miss x u_p`, `ε = 1` at a fixed mix and hit rate | source |
| P2 coupled to actuation | pass | Open loop: dispatches do not depend on prefill `R`. A cold new prefill pod computes a shared prefix once per prefix group (at most 1.89x per request in the reference load), real and transient | source |
| P3 fixed busyness | pass for the run set, fail across prompt lengths | **Constant:** 11,117 tokens/s at the SLO for 8192:256 (`sim`); the GPU value is *needs measurement*. **Mix:** only mixes whose uncached suffix reaches 8,192 tokens reach prefill, so in the run set prefill sees one mix (`V = 1`). Across prompt lengths, capacity at the SLO falls: 6,648 tokens/s at 12288:256 (`V = 1.67`), and at 16384:256 attainment is 94 % even at 0.18 req/s (`sim`). Compute per token rises only about 7 % from 8k to 12k context; the drop is the fixed 2 s TTFT leaving less room to queue, so any work-rate signal shares it | sim / measure |
| P4 metric type | pass | `sum(...) - sum(...)`, extensive, under `AverageValue` | config |
| P5 bounds | pass | A dispatch rate, neither capped nor integrating; it keeps rising past prefill capacity | source |
| P6 defined at zero | pass | `minReplicaCount: 1`. At zero prefill pods it would read 0 (`prefill-filter` leaves no endpoint; E `pkg/epp/scheduling/scheduler_profile.go#L203`): prefill cannot scale from zero on this trigger | source |
| P7 fails visibly | pass | No `or vector(0)`, `ignoreNullValues: "false"`, fallback holds. Series are absent until the first prefill dispatch after an EPP start, so an idle fresh deploy shows `Fallback=True` at `minReplicaCount` | config / source |
| P8 lag and noise | fail at one pod | **Lag:** `10 + 60 + 15 = 85 s`, below the 120 s cold start (`sim` value; GPU cold start *needs measurement*). **Noise:** one pod at threshold receives `7,782 / 8,192 = 0.95` prefill requests/s, 114 per 120 s window, so `1 / √114 = 9.4 %` with equal lengths: at the 10 % tolerance. 6.6 % at two pods. `[3m]` would give 7.6 % at one pod for 30 s more lag | config |
| P9 measures what it claims | pass | Uncached prompt tokens dispatched to prefill. **Conditions:** (1) single-EPP topology only (the 3-EPP prefill EPP labels its series `decode`); (2) `plugin_name` pinned; (3) the guide's `token-producer` supplies engine token IDs; (4) the EPP runs `main`, so main-only producer changes apply: `indexCompletions` is opt-in on `main` (llm-d-router#3847), which lowers predicted hits for multi-turn traffic and raises this value; the offload-sized LRU (#2292) needs offloading, which the guide does not enable. No listed trap applies; T14's double scrape does not apply (one ServiceMonitor) | source |

**Failure case (P3):** a deployment whose prompts are mostly 12k tokens, with
the threshold taken from 8k prompts. Each prefill pod is asked to carry 7,782
tokens/s against a 6,648 tokens/s capacity at the SLO (117 %).

## ScaledObject `coord-pd-decode`

- `minReplicaCount: 1`, `maxReplicaCount: 4`, `pollingInterval: 15`, fallback as
  prefill.
- Scale-up: **up to 4 pods per 60 s**, no stabilization. Scale-down: one pod per
  300 s after a 300 s window.

### Trigger `decode-kv-usage-2m`

```promql
sum(max by (pod, engine) (avg_over_time(vllm:kv_cache_usage_perc{namespace="${NAMESPACE}",pod=~"pd-disagg-coord-.+-decode-.+"}[2m])))
```

`metricType: AverageValue`, `threshold: "0.45"`, `activationThreshold: "0"`,
`ignoreNullValues: "false"`. The threshold is the KV usage at 70 % of each mix's
knee load, read from the knee stages (`sim`): 0.45 gives 75 % (8192:256), 72 %
(2048:2048) and 66 % (256:4096, extrapolated). `0.7 x` the KV usage at the blend
knee (0.782), the protocol's usual rule, gives 0.55, which the convexity (P1)
turns into 76 to 87 % of knee load.

- **What it counts:** `vllm:kv_cache_usage_perc` is `1 - free / total` over the
  GPU block pool (V `vllm/v1/core/block_pool.py:880-891`, exported from
  `vllm/v1/metrics/loggers.py:577`). A block returns to the free queue when its
  reference count reaches 0, including prefix-cached blocks, which become
  eviction candidates (V `block_pool.py` `free_blocks`,
  `get_num_free_blocks:871-878`; `free_blocks:777`). So the gauge counts the KV
  that live requests
  hold; an idle prefix cache reads as free. On a decode pod that is the context
  of the requests it is decoding plus KV being received from prefill.
- **Selecting decode pods:** vLLM series carry no role label (trap T14).
  DisaggregatedSet pods are named `<set>-<slice>-<revision>-<role>-<group>` (W
  `pkg/utils/disaggregatedset/utils.go:96-98`, LWS pod suffix `-<groupIndex>`),
  so `pod=~"pd-disagg-coord-.+-decode-.+"` selects decode. `max by (pod,
  engine)` removes a second scrape of the same engine (two jobs or endpoints on
  one pod give series that differ only in `job`, `endpoint` or `instance`, T14)
  and keeps each data-parallel engine of a pod, which has its own `engine`
  label; the series carry `container, endpoint, engine, instance, job,
  model_name, namespace, pod`. `avg_over_time([2m])` averages four 30 s samples
  per engine; averaging and summing commute while the pod set is fixed. With
  data parallelism (DP > 1) each engine reports a fraction of its own KV pool,
  so
  the sum counts a DP-2 pod twice and the threshold is per engine, not per pod;
  the guide runs DP = 1.
- **Kind:** capped saturation (a ratio with a ceiling of 1 per pod). **Role it
  can play:** guard, or a driver if the cap is the binding resource. **Role it
  plays here:** driver. No offered decode work rate is available: output length
  is not known at arrival, and no `L_p + c x L_out` weighting fits the knees
  (8192:256 sends decode no prompt tokens yet has the lowest capacity per output
  token).

| Property | Result | Evidence | Tag |
|---|---|---|---|
| P1 proportional | fail (mild, convex) | KV usage per req/s rises with load below the cap: elasticity 1.33 (8192:256, 1.8 to 3.0 req/s), 1.76 (2048:2048, 0.6 to 1.0), 1.32 (256:4096, 0.6 to 0.8). Occupancy is requests in decode x their context, and decode time per token grows with the batch. No dead band and no explosion. With `u ∝ (λ/R)^ε`, the HPA's next count is `R' ∝ R^(1-ε)`, which converges for `ε < 2`: stable, with damped over- and undershoot; 2048:2048 has the least margin. Consequence for the threshold: see above (0.45, not 0.55) | sim / derived |
| P2 coupled to actuation | fail (mild) | The same mechanism: a new pod shrinks every batch, decode runs faster and total occupancy falls, so the total moves slightly against `R`. No oscillation in the staircase or the mix shifts (`sim`) | source / sim |
| P3 fixed busyness | pass (`V = 1.25`) | KV usage at the decode knee: 0.677 (8192:256), 0.844 (2048:2048), 0.795 (256:4096), 0.782 (blend). The arrival rate gives `V = 3.9`, requests in flight 4.4, output tokens 4.1. A KV-occupancy model, `(L_p + L_out / 2) x L_out x μ`, gives 1.18x, consistent with decode being KV-bound on this plant (40 in flight x about 8.4k tokens of context at the 8192:256 knee, about the KV room on two H100s after 64 GB of weights). The constant and the KV-bound claim are *needs measurement* on GPU | sim / measure |
| P4 metric type | pass | outermost `sum`, extensive, under `AverageValue` | config |
| P5 bounds | pass | Capped at 1 per pod. The cap is the binding resource (P3) and `T = 0.45 < 1 / 1.1`. Growth per cycle is bounded by `1 / 0.45 = 2.2x` (with 0.55: 1.82x; under overload the saturated total 4.0 asked for 8, `sim`). The alarm below covers the slow growth | sim / config |
| P6 defined at zero | pass | `minReplicaCount: 1`. At zero decode pods the series are absent, the query is empty and the scaler errors: decode cannot scale from zero on this trigger | source |
| P7 fails visibly | pass | No `or vector(0)`, `ignoreNullValues: "false"`, fallback holds. Without the decode PodMonitor the query is empty from the start (visible, but the pool never scales); with it, a scrape outage errors and holds | config |
| P8 lag and noise | fail on long outputs | **Lag:** `30 + 60 + 15 = 105 s`, plus the time the load takes to appear in KV: a request's context grows as it decodes, so a shift to 4,096-token outputs reaches steady occupancy after about one decode duration (2 to 3 minutes). That exceeds the 120 s cold start. **Noise:** instant, one pod: CV 21 to 38 % on 8192:256 (each request is about 2.8 % of the pool); with `[2m]`, 8 to 10 % (`sim`) | config / sim |
| P9 measures what it claims | pass | Reads the KV decode's requests hold, its binding resource here. **Conditions:** (1) the model-server PodMonitors are applied (llm-d/llm-d#3114, port `modelserver`; the scenario's monitoring step does it); (2) pod names follow the DisaggregatedSet pattern above; (3) vLLM v0.34 sizes the KV pool after warm-up by default (`vllm-extensible-kv-cache`), which changes what 1.0 means; the guide runs v0.31.0. No listed trap applies beyond T14, handled by `max by (pod, engine)` and the name selector | source |

**Failure case (P8):** decode right-sized at 1 pod for 8192:256 at 1.6 req/s;
traffic switches to 256:4096 at the same rate. KV fills over about two minutes
and saturates; without the alarm the driver adds at most 2.2x per cycle. In
`sim` with the alarm and threshold 0.55: about 300 s at 38 to 49 % attainment,
then 100 % at 4 pods.

### Trigger `decode-waiting-alarm`

```promql
sum(max by (pod, engine) (vllm:num_requests_waiting{namespace="${NAMESPACE}",pod=~"pd-disagg-coord-.+-decode-.+"}))
```

`metricType: AverageValue`, `threshold: "5"`, `activationThreshold: "0"`,
`ignoreNullValues: "false"`.

- **What it counts:** requests in vLLM's waiting queue on the decode pods. When
  KV is full, new requests wait there. On vLLM before v0.32, a decode request
  whose KV is still being fetched from prefill also counts as waiting (the
  reason llm-d-router#4016 subtracts `vllm:num_requests_kv_fetch_by_stage`,
  which v0.31.0 does not export), so disaggregated traffic adds about arrivals x
  fetch time per pod in steady state.
- **Kind:** queue backlog. **Role:** alarm.

| Property | Result | Evidence | Tag |
|---|---|---|---|
| P1, P2, P3 | fail, by kind | A backlog: near 0 below saturation, integrates above. Acceptable for an alarm that must not win in steady state: waiting at the decode knees is 0 to 2 per pod (`sim`), below the threshold of 5 | sim |
| P4 metric type | pass | `sum`, `AverageValue` | config |
| P5 bounds | integrating | Overshoot bounded by `maxReplicaCount: 4` and the 300 s scale-down window | config |
| P6 / P7 | as the KV driver | Absent series at zero pods; empty without the PodMonitor | source |
| P8 lag | pass | `30 + 15 = 45 s`, instant gauge | config |
| P9 | pass with condition | On vLLM < v0.32, KV fetches in flight count as waiting; the threshold must stay well above `arrivals x fetch time` per pod (*needs measurement* on the deployed connector) | source / measure |

## Combination

- **Prefill:** one driver, always decides.
- **Decode:** the HPA takes the maximum of the KV driver and the alarm. In `sim`
  the alarm read 0 in steady state (staircase, mix shifts below the cap) and won
  only under overload (77 to 239 against the driver's 8) and when one pod faced
  a step in long-output load. That is the sound pattern: one driver, plus an
  alarm that wins only when its condition holds.
- **Coupling between pools:** the decider splits one client load between the
  pools by the uncached suffix. In a mix shift that sends nothing to prefill,
  prefill scales down to 1 and starts cold when long prompts return (`sim`: a
  150 s dip at the start of the return phase). A longer prefill scale-down
  window or `minReplicaCount: 2` trades that dip for idle pods.
- **behavior:** decode scales up by up to 4 pods per 60 s so the alarm can act
  within one cold start; with 1 pod per 180 s a 1 to 4 decode step takes over 9
  minutes (`sim`: a mix shift at the cap never recovered). This differs from the
  1-pod-at-a-time direction set for the keda-epp queue trigger, whose concern
  was overshoot from an integrating driver; here the driver is capped and the
  alarm drains to 0 once pods arrive.
- **Not covered:** KV-transfer bandwidth between the pools, and prefill KV
  pressure from slow pulls. No trigger measures them.

**Predicted behavior:** at low load both pools sit at 1. Each pool steps to
`ceil(signal / threshold)`. Under a mix shift toward long outputs, decode reacts
about one decode duration late, then the alarm takes it to the cap within one
cold start; once caught up, the KV driver holds the count the mix needs. Under
overload both pools reach `maxReplicaCount`. At constant load both hold.

## Simulator evidence (`sim`)

Supporting runs, not the step 3 evaluation of record. Attainment X = 0.95 is the
gate.

| Run | v1 (arrival-rate decode) | this config |
|---|---|---|
| Blend staircase 0.25 / 0.5 / 0.75 / 0.5 / 0.25 x C, C = 4 x 1.165 req/s | 100 % every phase; 9,630 decode pod-s | 100 % every phase; decode 1-2, 2-3, 4, 3, 2; 9,060 pod-s at 0.55, 10,020 at 0.45 |
| Mix shift at 1.6 req/s, 8192:256 / 256:4096 / 8192:256 | settled 100 %; decode 2 where 1 suffices; 6,210 pod-s | settled 100 % at both thresholds; at 0.55 about 300 s at 38 to 49 % at the shift to long outputs; 5,820 pod-s at 0.55, 6,090 at 0.45 |
| Mix shift at 2.4 req/s (the 4-pod cap) | decode held at 3, 17 to 39 % for the rest of the phase | threshold 0.55: decode 1 in phase A, then 4 within one cold start; settled 74 % in phase B (backlog drains at the cap). Threshold 0.45: decode 2 in phase A, settled **97.6 %** in phase B; 7,110 decode pod-s against 6,750 |
| Overload, blend 0.5 C then 1.5 C | reaches the cap | reaches the cap; the alarm wins only here |

## Needs measurement

1. `μ_prefill` in computed tokens/s at the SLO on the deployed plant, and how it
   falls with prompt length (prefill P3).
2. Decode KV usage at the knee for each run-set mix on GPU, and whether KV is
   decode's binding resource there (decode P3, P5).
3. The cold start of a prefill and a decode replica (P8 on both).
4. `vllm:num_requests_waiting` on decode pods in steady state with disaggregated
   traffic on vLLM v0.31.0 (alarm P9).
5. Whether `decode-prefix-producer` and the default producer predict the same
   cached tokens on prefill calls, and the effect of `indexCompletions` on the
   floating EPP (prefill P9).
6. The EPP image digest the guide deploys.

## Fixes and open decisions

| Item | Option | Effect |
|---|---|---|
| decode P1 / P2 convexity | applied: threshold 0.45 (70 % of knee load) instead of 0.55 | +5 to 11 % decode pod-s in `sim`; the at-cap mix shift passes (97.6 % against 74 %) |
| decode P8 lag on long outputs | `minReplicaCount: 2` | one spare decode pod absorbs the first minutes of a long-output step |
| decode scale-up policy | keep 4 pods / 60 s, or return to 1 pod / 180 s | decision with the keda-epp direction; 1 pod / 180 s fails the at-cap mix shift in `sim` |
| prefill P8 noise at one pod | `[3m]` window | 7.6 % for 30 s more lag |
| prefill restart after a mix shift | longer prefill scale-down window, or `minReplicaCount: 2` | removes the cold dip when long prompts return |
| monitoring | done: llm-d/llm-d#3114 adds the model-server PodMonitors | required for the decode triggers |
