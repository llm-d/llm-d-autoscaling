# Static signal analysis: coord-disaggregation/decode-slot-guard

<!-- markdownlint-disable MD013 -->

| | |
| --- | --- |
| Scenario | `benchmark/config/scenarios/staging/coord-disaggregation/decode-slot-guard.yaml` |
| Backend overlay | Coordinator PD-only, vLLM H100 BF16 TP2 DP1 |
| Rendered with | `llmdbenchmark plan --dry-run` and `standup --dry-run -t kustomize`, llm-d-benchmark at `61e9e47b3c6df6e26c1575539be84d81e12e9f71` |
| Load | Open-loop 8192:256, 2048:2048, 256:4096, equal-weight blend, and a shared-prefix shift. At offered rate q, token rates are q × ISL and q × OSL, including cache hits. The shared-prefix test must report actual prefix hits. |
| Date | 2026-11-26 |
| Verdict | **Accepted with conditions** |

<!-- markdownlint-enable MD013 -->

Step 1 of the [evaluation protocol](../../README.md). See the
[Step 2 verdict](decode-slot-guard-step2.md),
[Step 3 status](decode-slot-guard-step3.md) and
[Step 5 plan](decode-slot-guard-step5.md).

The guard can request capacity when shared KV blocks keep occupancy low and the
running list approaches its scheduler cap. Its value depends on reaching that
cap before another resource or the latency SLO binds.

## Deployed versions

The [coordinator-kv verdict](coordinator-kv.md) pins the base
EPP configuration, guide, LWS and KEDA. The guide uses vLLM v0.31.0
(`db9527a46873454610df6dbedf79a36d6bf1a7f6`). Its
[engine argument
defaults](https://github.com/vllm-project/vllm/blob/db9527a46873454610df6dbedf79a36d6bf1a7f6/vllm/engine/arg_utils.py)
select 1024 sequences and an 8192-token batch budget for the OpenAI API
server on H100 80 GB. The rendered GPU decode container is TP2 and has
no sequence, active-sequence, batch-token, performance-mode or data-parallel
override. The argument resolver clamps the default sequence count to the
token budget, which leaves 1024 under these settings. The scheduler uses
the resolved value for runner slots and defaults running admission to
that value unless `max_num_active_seqs` is set. This is a source and
manifest check. Verify both effective limits at engine startup on each arm.

The guide render uses llm-d at `5eb338c505fb60f1bb0a5cc76881fd8136d534aa`
with the [PD-only overlay](https://github.com/llm-d/llm-d/pull/3111) at
`5433661c71ec80af2d8122c25a7ce3e3a09315f3` and the
[model-server PodMonitors](https://github.com/llm-d/llm-d/pull/3114) at
`3f1cc1a3d12a8e2612129579d33bf8ae55b15203`. Use a guide checkout with
both changes for either arm. For matched experiments, the control also
needs the [pool selectors from #1637][pool-selectors]
at `2d1bba790df7a95e7bfc92336c6d77a1eb9d0aa9`. The candidate uses the
same inherited selectors.

The guard reads `kube_pod_info` and `kube_pod_status_ready` from
kube-state-metrics. Enable both metrics and verify scrape intervals and
inventory before starting the workload. The age limit for each source
is 75 s.

## ScaledObject `coord-pd-prefill`

The prefill driver and its EPP plugin contract are assessed in
[coordinator-kv](coordinator-kv.md#scaledobject-coord-pd-prefill).

## ScaledObject `coord-pd-decode`

The rendered `27_keda-scaledobjects.yaml` sets a minimum of 1, maximum
of 4 and polling interval of 15 s. Up to four additional pods per
60 s. Scale-down uses 300 s stabilization and one pod per 300 s. Fallback
holds the current count after the consecutive failure counter exceeds 3. The
inherited
[KV driver and waiting alarm](coordinator-kv.md#scaledobject-coord-pd-decode)
retain the 0.45 and 5 targets.

### Trigger `decode-running-slots`

The [scenario
query](../../../config/scenarios/staging/coord-disaggregation/decode-slot-guard.yaml)
sums fresh engine gauges for Ready pods. It requires a fresh readiness
condition for each observed scheduled decode pod UID and a fresh
inventory entry for each observed decode readiness UID. An unscheduled
pod may lack a readiness condition and contributes no capacity. A Ready
pod without a fresh
engine gauge also makes the query return an empty vector.

`AverageValue`, target 716.8 (0.7 × 1024), activation threshold 0,
`ignoreNullValues: false`. The historical trigger used 48 (0.75 × 64) in
the colocated Qwen3-14B screen, chosen before the 64 replays from the
configured cap. The 32 sensitivity target used 0.5 × 64. A 0.7 ratio on
that same substitute plant is 44.8.

The [vLLM
scheduler](https://github.com/vllm-project/vllm/blob/db9527a46873454610df6dbedf79a36d6bf1a7f6/vllm/v1/core/sched/scheduler.py)
reports `len(self.running)` and the
[metrics
logger](https://github.com/vllm-project/vllm/blob/db9527a46873454610df6dbedf79a36d6bf1a7f6/vllm/v1/metrics/loggers.py)
publishes it per engine. This counts admitted engine work items, including
items carried across iterations. Parallel output sampling can create multiple
engine requests from one client request. Token-budget limits, preemption and
a lower running admission cap can prevent the gauge reaching max-num-seqs.
The gauge is a capped signal used as a sequence-slot guard.

<!-- markdownlint-disable MD013 -->

| Property | Result | Evidence | Tag |
| --- | --- | --- | --- |
| P1 proportionality | n/a for guard | Admitted work is capped and can flatten while offered load rises. | source |
| P2 actuation coupling | n/a for guard | Running occupancy varies with scheduling and residency. It need not be inversely proportional to replica count. | source |
| P3 fixed busyness | n/a for guard, fails as driver | Running varied about 4.4x at the four knees. | prior sim |
| P4 aggregation | pass under DP1 | Max deduplicates scrape copies by namespace, pod and engine. The sum is extensive. | config |
| P5 cap | conditional | 716.8 < 1024/1.1. At one status replica and the default HPA tolerance, the integer gauge first clears the scale-up tolerance at 789. | config |
| P6 zero | n/a with min 1 | The query returns an empty vector before any pod reports Ready. | config |
| P7 visible failure | conditional | A fresh zero is valid. A missing or stale row on either side of the inventory join faults if the other metric still reports that pod. Simultaneous omission from both can undercount. | config |
| P8 lag and noise | needs measurement | Gauge has no averaging window. With 30 s scrapes and 15 s HPA sync, worst nominal observation delay is about 45 s before actuation and transport. The 75 s age gate limits accepted sample age. | config / measure |
| P9 semantics | conditional | One engine per scaling unit. Verify resolved cap, running admission and slot-to-gauge correlation. | source / measure |
| P10 isolation | conditional | The guard matches the namespace and LWS v1.0.0 decode pod name structure, including the eight-digit hex revision. Run each arm in a dedicated namespace. Verify membership on the deployed stack. | config / measure |
| P11 EPP configuration | pass for guard | The guard reads vLLM and kube-state-metrics. The inherited prefill trigger still needs its configured prefix producer. | config |

<!-- markdownlint-enable MD013 -->

**Failure case.** One Ready pod at 1024 slots and one Pending pod yield
ceil(1024/716.8) = 2 before HPA tolerance. The pending pod contributes no
service. The guard can hold at two while one remains saturated. A valid
waiting proposal can still request a third pod.

## Combination

- The KV driver should win on KV-bound mixes. The waiting alarm can win
  after backlog accumulates. The HPA applies tolerance to each external metric
  using the scale target's status replica count. Outside tolerance, the
  `AverageValue` proposal is `ceil(pool_total / target)`. It takes the
  maximum valid proposal before replica bounds and scaling behavior.
- The guard can also delay scale-in. A failing slot metric can inhibit
  scale-down. An independent valid scale-up can proceed.
- The selector handles duplicate engine scrapes, separates roles and
  namespaces, and assumes one matching Coordinator pool per namespace.
  Compare the Prometheus inventory with the Kubernetes pod list to detect joint
  omissions.

<!-- markdownlint-disable MD013 -->

| Mix | Decode knee req/s | KV at knee | Guard prediction |
| --- | ---: | ---: | --- |
| 8192:256 | 3.078 | 0.677 | Long prompt occupancy consumes KV before slots. |
| 2048:2048 | 1.000 | 0.844 | Longer residency raises running count and KV. KV should win. |
| 256:4096 | 0.678 | 0.795 | Long generations raise running count and per-request KV. KV should win. |
| Equal blend | 1.165 | 0.782 | KV should win at the knee. Measure transient guard proposals after shifts. |
| Shared prefix | needs measurement | needs measurement | Shared blocks can lower KV per active item enough for slots to bind. Verify whether this happens at 1024 slots while the SLO is still attainable. |

<!-- markdownlint-enable MD013 -->

Knees come from the published [control simulation](coordinator-kv-step3.md).
It did not record the running gauge in its archived HPA traces. Maintainer
discussion gives the 4.4× spread, not an absolute trace for each mix.
[Maintainer discussion][issue-discussion]

## Conditions for evaluation

1. Reconcile the readiness series against the decode pod inventory and verify
   that the real monitoring stack has both sources. Run the control-path
   checks on the exact guide topology. The supplemental checks are in Step 2.
2. Measure guard crossings, running cap and prefix reuse on the four regular
   mixes and shared-prefix load. Require the guard to stay below the winning
   control proposal on KV-bound steady stages.
3. Measure first trigger crossing, replica request, Ready and backlog drain
   separately at 120 s startup. Evaluate client latency during each interval.

[pool-selectors]: https://github.com/llm-d/llm-d-autoscaling/pull/1637
[issue-discussion]: https://github.com/llm-d/llm-d-autoscaling/issues/1595#issuecomment-7135000949
