# Static signal analysis: pd-disaggregation/kv-early

Step 1 of the [evaluation protocol](../../README.md), produced with the
`static-signal-analysis` skill. The properties P1–P9 are defined in
[step 1](../../1-static-signal-analysis.md) §1.2. Cited files
`27_keda-scaledobjects.yaml` and `ms-values.yaml` are from the rendered plan.

| | |
|---|---|
| Scenario | `benchmark/config/scenarios/staging/pd-disaggregation/kv-early.yaml` |
| Backend overlay | `benchmark/config/cluster-configs/ocp/model-sim-qwen3-32b.yaml` (the scenario names none; this is the README's example backend) |
| Rendered with | `llmdbenchmark standup --dry-run`, llm-d-benchmark@`bd7d768`. The Jinja plan rendered; the Helm step failed offline, so the modelservice values (`helm/ms-values.yaml`) were read before chart rendering |
| Load | `sanity_random.yaml` (llm-d-benchmark@`bd7d768`): open loop, `constant` at `q` = 1 req/s for 30 s; random prompts, mean ISL 384 and OSL 50, `ignore_eos: true`. So `λ_in = 1 × 384 = 384` and `λ_out = 1 × 50 = 50` tokens/s. Random prompts share no prefix, so capacity does not depend on a cache hit rate |
| Date | 2026-11-11; revised 2026-11-12 for the load definition of step 1 §1.1 |
| Verdict | **Rejected** |

Rejected: no trigger is proportional to load, and every query filters on a
`role` label that, per the source, no series carries, so on this stack all four
triggers most likely read 0. Predicted: both pools stay at `minReplicaCount: 1`
whatever the load. If the label does exist, the KV guard can take a pool to 2
replicas and no further, and only the queue backlog can go beyond, late and
with oscillation.

kv-early differs from `baseline.yaml` only in the KV threshold, 0.95 → 0.80
(scenario lines 205 and 238). That change fixes one check, P5 on the KV
trigger, and leaves the rest of the baseline's failures in place.

## Deployed versions

| Component | Version | Source of the version |
|---|---|---|
| vLLM (both roles) | `vllm/vllm-openai-cpu:v0.27.0-x86_64` | overlay `model-sim-qwen3-32b.yaml:73-74` |
| llm-d-modelservice chart (PodMonitors) | `v0.4.16` (`6965740`) | llm-d-benchmark `defaults.yaml:116` |
| llm-d-benchmark (renderer) | `bd7d768` | the clone used to render |
| Prometheus endpoint | OpenShift Thanos querier, bearer auth | `27_keda-scaledobjects.yaml:24-25` |
| KEDA | 2.20 or later, release not known | `benchmark/README.md`; trap T7 is *unverified* for it |

The EPP and the gateway are not read by any trigger.

## ScaledObject `pd-disaggregation-prefill`

- `minReplicaCount: 1`, `maxReplicaCount: 4` (`27_keda-scaledobjects.yaml:15-16`).
- `pollingInterval` not rendered: KEDA's default, 30 s.
- Scale-up: one pod per 180 s, no stabilization (`:51-56`). Scale-down: one pod
  per 300 s after a 300 s window (`:45-50`).
- No `fallback`, no `ignoreNullValues`: the template cannot render them.
- vLLM scrape interval 15 s (`ms-values.yaml:564`), no relabelings
  (`ms-values.yaml:569-571`).

### Trigger `vllm-queue-size`

```promql
max(vllm:num_requests_waiting{model_name="Qwen/Qwen3-32B",role="prefill"})
```

`metricType: AverageValue`, `threshold: "20"`, `activationThreshold: "0"`
(`27_keda-scaledobjects.yaml:19-29`).

- **What it counts:** a gauge, set every scheduler step to the length of the
  engine's waiting queue: requests that have reached vLLM and are not yet
  admitted to the running batch. Labels `model_name` and `engine` only.
  `source`: `vllm-project/vllm@4bdc8a7` (`v0.27.0`)
  `vllm/v1/core/sched/scheduler.py#L2521` (`num_waiting_reqs=len(self.waiting)`),
  `vllm/v1/metrics/loggers.py#L468` (labels), `#L503-L508` (definition),
  `#L1116` (`set`).
- **Kind:** queue backlog. **Role it can play:** alarm. **Role it plays here:**
  driver: it is the only trigger that can ask for more than 2 pods.

| Property | Result | Evidence | Tag |
|---|---|---|---|
| P1 proportional | fail | A request is counted only while vLLM cannot admit it, so the gauge stays near 0 until a pod's batch is full, then grows with the excess: a dead band, then integration | source |
| P2 coupled to actuation | fail | `max` over pods: adding a pod drains queues and lowers the reading at fixed load | source |
| P3 fixed busyness | fail | 20 waiting requests is a different overload for every prompt and output length; no capacity in the query | config |
| P4 metric type | fail | `max(...)` is intensive under `AverageValue` (`:20`): the HPA computes `ceil(max / 20)` whatever the pod count | config |
| P5 bounds | pass | integrating, but `maxReplicaCount: 4` and one pod per 180 s bound the overshoot | config |
| P6 defined at zero | pass | `minReplicaCount: 1` (`27_keda-scaledobjects.yaml:15`). At zero pods it would read 0, so scaling from zero would not work: the gauge is exported by the prefill vLLM pods (`loggers.py#L503-L508`) and disappears with the last one, and `ignoreNullValues` defaults to true (T7) | config |
| P7 fails visibly | fail | an empty result reads 0 (`ignoreNullValues` defaults to true, no fallback); see P9 for why it is empty | config |
| P8 lag and noise | pass | lag `15 + 0 + 30 = 45 s` (scrape, no window, polling); one request moves it by `1 / 20 = 5 %` of the threshold | config |
| P9 measures what it claims | fail | `role="prefill"` matches no series: vLLM `v0.27.0` labels its gauges `model_name` and `engine` only (`loggers.py#L468`), and the rendered PodMonitor values add no relabeling (`ms-values.yaml:569-571`). The chart puts `llm-d.ai/role` on the pod (`llm-d-modelservice@6965740:charts/llm-d-modelservice/templates/_helpers.tpl#L69`), but a PodMonitor copies pod labels onto series only through `relabelings` or `podTargetLabels`, and neither is set. Whether anything else in the cluster adds it *needs measurement* | source |

**Failure case (P4):** 4 prefill pods, each with 30 requests waiting. The HPA
computes `ratio = 30 / (20 × 4) = 0.375`, outside the tolerance, and asks for
`ceil(30 / 20) = 2`: a scale-in while every pod is overloaded. To grow from
`R` to `R + 1` pods, the worst pod's queue must exceed `20 × 1.1 × R`: 22, 44,
then 66 waiting requests.

**Failure case (P9):** any load. If the series is empty, the trigger reads 0 and
asks for `ceil(0 / 20) = 0`, clamped to `minReplicaCount: 1`.

### Trigger `vllm-kv-utilization`

```promql
max(vllm:kv_cache_usage_perc{model_name="Qwen/Qwen3-32B",role="prefill"})
```

`metricType: AverageValue`, `threshold: "0.80"`, `activationThreshold: "0"`
(`27_keda-scaledobjects.yaml:30-41`).

- **What it counts:** a gauge, the engine's KV-cache usage, 1 meaning full; set
  every scheduler step. Same labels. `source`: `vllm-project/vllm@4bdc8a7`
  `vllm/v1/metrics/loggers.py#L561-L566` (definition), `#L1123` (`set`),
  `vllm/v1/core/sched/scheduler.py#L2523`.
- **Kind:** capped saturation, ceiling 1. **Role it can play:** guard. **Role
  it plays here:** guard.

| Property | Result | Evidence | Tag |
|---|---|---|---|
| P4 metric type | fail | `max(...)` under `AverageValue` (`:32`): the ask is `ceil(max / 0.80)`, at most `ceil(1 / 0.80) = 2` at any pod count | config |
| P5 bounds | pass | `1 / 0.80 = 1.25 > 1.1`: one pod at KV ≥ `0.80 × 1.1 = 0.88` asks for `ceil(0.88 / 0.80) = 2`. This is the check kv-early fixes; at 0.95, `1 / 0.95 = 1.05` never left the tolerance | config |
| P8 lag | pass | `15 + 0 + 30 = 45 s` | config |
| P9 measures what it claims | fail | same `role` label as above | source |

Whether 0.80 marks the binding resource for the benchmark's mix is not decided
here: *needs measurement*.

**Failure case (P4):** 2 pods, both with full KV caches. The ratio is
`1.0 / (0.80 × 2) = 0.625`, and the ask is `ceil(1.0 / 0.80) = 2`: the guard
can never take the pool past 2, however many pods are full.

## ScaledObject `pd-disaggregation-decode`

Same bounds, `behavior`, defaults and scrape interval
(`27_keda-scaledobjects.yaml:71-112`, `ms-values.yaml:296-301`). Its two
triggers are the prefill ones with `role="decode"` (`:73-97`), so every row and
failure case above applies unchanged. One addition for the KV guard: the
`token-aware.yaml` comment at line 346 records decode KV usage of 0.72–0.98 in
an 8,192-token run while decode was only waiting on prefill's KV transfers. At
0.80 that guard would add decode pods that were not the bottleneck. *Needs
measurement* on this stack.

## Combination

- **Driver in normal operation:** none.
- **Who decides where:** if the `role` label is absent, nobody: both triggers
  read 0 and both pools hold 1 replica. If it is present, in each pool:
  - below saturation, the queue reads about 0 and the KV guard decides; it
    adds one pod once any pod's KV reaches 0.88, and stops at 2;
  - past saturation, the queue decides, one pod per 180 s, each step needing
    `20 × 1.1 × R` waiting requests on the worst pod.
- **Scale-in:** the queue collapses as soon as the new pod drains it, and the
  KV guard asks for at most 2. After the 300 s window the pool loses a pod
  every 300 s, down to 2 or fewer, while load is unchanged.
- **`behavior` and bounds:** one pod per 180 s bounds the swing;
  `maxReplicaCount: 4` caps it.
- **Disaggregated pools:** each pool reads its own engines' gauges, so neither
  carries the other pool's work, apart from decode KV filling while it waits on
  prefill (above).

**Predicted behavior:** without the `role` label, a fixed pool of 1 prefill and
1 decode replica, so a run of kv-early would show the same scaling as a run of
the baseline: none. With the label, each pool sits at 1–2 replicas until it is
already overloaded, then climbs one pod per 3 minutes, and at constant load
cycles between 2 and the overloaded count as the queue drains and refills.

## Needs measurement

1. Do the vLLM series carry a `role` label on the target cluster? Run
   `count by (role) (vllm:num_requests_waiting{model_name="Qwen/Qwen3-32B"})`
   against the Thanos querier. Needed by P9 and P7 of all four triggers.
2. Per-replica capacity `μ_SLO` of each role at the TTFT and ITL SLOs, for the
   benchmark's mix and cache setup: prefill in prompt tokens/s, decode in
   output tokens/s. Needed to size a proportional driver (fix 3).
3. KV-cache usage of each role at that capacity. Needed to tell whether 0.80
   guards the binding resource (KV P5, decode note).
4. Cold start of a prefill and of a decode pod. Needed to judge the 45 s lag
   and the one-pod-per-180 s policy.
5. The KEDA release on the cluster. Needed to settle trap T7.

## Fixes

| Fails | Fix | Effect |
|---|---|---|
| All four triggers, P9 | Select by pod name, as `token-aware.yaml:365` does: `role="prefill"` → `pod=~".*-prefill-.*"`, and the same for decode. Or add to both PodMonitors: `relabelings: [{sourceLabels: [__meta_kubernetes_pod_label_llm_d_ai_role], targetLabel: role}]` | the queries match series; P9 passes on the label |
| KV guard, P4 | `avg(vllm:kv_cache_usage_perc{…})` with `metricType: Value`, or `sum(…)` with `AverageValue`, threshold 0.80 | the ask scales with the pod count; the guard can go past 2 |
| No driver; queue P1–P4 | Add a driver per pool from a counter of offered work, divided by the measured per-replica capacity, with `AverageValue` and target 0.7. At the router, the offered-rate counter depends on the EPP version (trap T8): on `main`, `sum(rate(llm_d_epp_fairness_id_resolved_total[1m]))` needs no gate. On this stack's `v0.11.0`, the only option is `llm_d_epp_flow_control_requests_total` over every `outcome`, which needs the `flowControl` gate; no scenario here turns it on, and doing so makes the vLLM queue a bounded sawtooth (T4). Either way it is a request rate. On prefill, weight it by prompt tokens to read `λ_in`, and divide by `μ_SLO` in prompt tokens/s. On decode the load is `λ_out`, but output tokens are not known at arrival: a request rate × the mix's mean OSL is as mix-sensitive as a request rate (P3) | a proportional trigger decides in normal operation; a counter at the router exists with no vLLM pod, so scaling from zero could work, provided its series survives zero endpoints (check against T5) |
| Queue as alarm | Keep it, as `sum(...)` under `AverageValue`, with a threshold that only fires past saturation | it stays out of the result in normal operation and adds pods fast under overload |
| All drivers, P7 | Change llm-d-benchmark's `27_keda-scaledobjects.yaml.j2` to render `ignoreNullValues` and `fallback`; then set `ignoreNullValues: "false"` and a `fallback` sized for peak | an outage becomes a scaler error, not a reading of 0 |
