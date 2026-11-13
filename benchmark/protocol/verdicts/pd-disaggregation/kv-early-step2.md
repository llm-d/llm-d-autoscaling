# Step 2: Verification with a simulator — pd-disaggregation/kv-early

Step 2 of the [evaluation protocol](../README.md), following the
[procedure](../2-simulator-verification.md). Builds on the step 1 verdict
[`verdicts/pd-disaggregation/kv-early.md`](kv-early.md).

| | |
|---|---|
| Scenario | `benchmark/config/scenarios/staging/pd-disaggregation/kv-early.yaml` |
| Backend overlay | `benchmark/config/cluster-configs/ocp/model-sim-qwen3-32b.yaml` |
| Simulator | `vllm-model-sim` (real vLLM on CPU, physics latency model) |
| Kubernetes context | `default/api-rits-roce-fmaas-res-ibm-com:6443/villard@us.ibm.com` |
| Namespace | `step2-kv-early-20261113` |
| Cluster version | OpenShift `4.19.19`, Kubernetes server `v1.32.9` |
| KEDA | `2.19.0` (`openshift-keda`, `custom-metrics-autoscaler-rhel9`). **Below** the `>= 2.20` this repo's README assumes; trap T7 is unverified at this exact release — see Simulator gaps |
| vLLM image | `docker.io/vllm/vllm-openai-cpu:v0.31.0` (prefill and decode) |
| Router/EPP image | `ghcr.io/llm-d/llm-d-router-endpoint-picker:v1.0.0` |
| Date | 2026-11-13 |
| Overall result | step 1's verdict **holds** |

## Deployment note (not a step 2 finding, but required to get here)

Standing this scenario up surfaced two defects unrelated to the KEDA triggers
themselves, fixed before any claim below could be tested:

1. **EPP config incompatible with the pinned router image** (fixed in this
   repo, commit `a1437da8`). All four `staging/pd-disaggregation/*.yaml`
   scenarios used the pre-`v0.11.0` EPP plugin chain
   (`disagg-headers-handler`, `deciderPluginName`), which the pinned
   `llm-d-router-endpoint-picker:v1.0.0` image no longer registers
   ([llm-d-router#2815](https://github.com/llm-d/llm-d-router/pull/2815)).
   The EPP container crash-looped with `plugin type
   'disagg-headers-handler' is not registered` on every restart. Fixed by
   dropping the plugin and moving to the nested `deciders:` form, matching
   upstream's `guides/pd-disaggregation.yaml`.
2. **Per-namespace Prometheus auth secret goes stale across a
   teardown/standup cycle.** The overlay's `prometheus-auth` secret holds a
   bearer token for the `wva-prometheus-auth` ServiceAccount; a
   teardown/standup cycle recreates that ServiceAccount, invalidating the
   token already in the secret. Both ScaledObjects read `READY=False` and
   the HPA showed `<unknown>` targets (`403` from Thanos) until the secret
   was recreated. Not a repo bug — the overlay already documents the
   one-time creation command — but it needs repeating after every
   teardown/standup, which the overlay's comment doesn't say.

Both are now cleared: pods Ready, inference test passing, both
ScaledObjects `READY=True`, HPAs reporting non-`<unknown>` targets, per the
`run-benchmark` skill's Step 7 gate.

## Claims

From the step 1 verdict, every trigger failed P9 (the `role` label matches
no series) and, as a consequence, P1/P2/P4 (no driver; the pool is predicted
to sit at `minReplicaCount: 1` regardless of load). This step tests the P9
claim directly, with its downstream effect on the whole scaling pipeline, at
idle and under load.

| Step 1 says | Observation that confirms it | Outcome |
|---|---|---|
| P9: `role="prefill"` / `role="decode"` match no series on any of the four triggers | `count by (role)(vllm:num_requests_waiting{model_name="Qwen/Qwen3-32B"})` returns one series with an **empty label set** (`"metric":{}`), holding all 9 matching series cluster-wide. The exact trigger queries, `max(vllm:num_requests_waiting{model_name="Qwen/Qwen3-32B",role="prefill"})` and the KV equivalent, return `"result":[]` at idle. Reading the unfiltered series directly confirms why: every series carries `model_name`, `engine`, `pod`, `namespace`, `job`, `instance`, `container`, `endpoint` — no `role` label | **confirmed** |
| P7 (downstream of P9) + predicted combination: "both pools stay at `minReplicaCount: 1` whatever the load" | Drove the prefill pod with the `guide_pd-disaggregation_1` workload (45 req/s, 5000-token prompts, 250-token outputs, 120 s) while polling both the ground-truth queue (no `role` filter, read straight off the prefill pod) and the exact trigger query every 15 s. The ground-truth queue climbed from 0 to 3126 waiting requests over 4.4 minutes with no sign of draining; the trigger query returned an empty result at every single sample; the HPA (`keda-hpa-pd-disaggregation-prefill`) stayed at `0/20 (avg)` and the replica count never left 1 | **confirmed** |

### Plot

![P9: trigger reads 0 while the real queue grows](kv-early-step2/p9-trigger-vs-queue.svg)

Data: [`kv-early-step2/p1-p9-queue.csv`](kv-early-step2/p1-p9-queue.csv)
(18 samples, 15 s apart, read directly from Thanos during the run described
above). Script: [`kv-early-step2/plot_p9.py`](kv-early-step2/plot_p9.py).

The solid line is `sum(vllm:num_requests_waiting{pod="<prefill-pod>"})`, read
straight off the prefill pod with no `role` filter — the real backlog the
engine sees. The dashed line is the ScaledObject's own query,
`max(vllm:num_requests_waiting{model_name="Qwen/Qwen3-32B",role="prefill"})`;
it returns an empty result at every sample, which trap T7's
`ignoreNullValues: true` default turns into 0, so it is flat at 0 for the
whole run. Load starts at the dashed vertical line (≈140 s); the real queue
starts climbing within one 15 s sample and reaches over 3000 waiting
requests by the last sample, while the trigger the ScaledObject actually
reads never moves. This is the queue backlog's shape predicted in step 1 (no
dead band is visible here because load starts well past the engine's
capacity) combined with the label miss: the signal that should eventually
drive scale-up never reaches KEDA at all.

Other properties (P1–P5, P8 on the queue; the KV guard; the decode pool)
were not separately tested: with the trigger query itself empty, there is no
non-zero signal whose proportionality, coupling, or lag could be measured on
this stack. Testing those properties requires first applying the step 1 fix
(select by pod name or add a `role` relabeling) and re-running step 2 against
the corrected scenario — tracked as a follow-up, not attempted here.

## Simulator gaps

- **KV-cache usage exceeded 1.0** (`sum(vllm:kv_cache_usage_perc{pod=...})`
  read `1.84` at peak, see the CSV's last column) against the metric's
  documented range of 0–1. `sum()` over this pod's series should not exceed
  the per-engine value when there is exactly one engine reporting, so this
  looks like a `vllm-model-sim` artifact (an engine shard, or the simulated
  KV-cache accounting, not clamping under extreme queueing) rather than a
  property this scenario's KV trigger would see on real hardware. *Needs
  measurement*: re-check the KV reading on a tuned real-hardware run before
  treating this as a general fact about the KV guard; not folded into the
  step 1 verdict.
- **EPP OOMKilled under sustained overload.** At 45 req/s against a single,
  uninflatable prefill replica, the EPP container (`resources.limits.memory:
  1Gi`, the router chart's default) was OOMKilled after about 18 minutes of
  sustained load — well after the data above was captured, so it does not
  affect the P9 claim, but it means this backend cannot sustain a long
  overload run without also raising the EPP memory limit or shortening the
  load window. Not investigated further here: this is about the EPP's own
  resource sizing under backlog, not the KEDA triggers this scenario is
  being evaluated for.
- Qwen3-32B on H100 SXM5 is a tuned profile (`beta: [3.119229, 1.182904,
  0.146612]` in the overlay); no retuning was needed for this claim, since
  P9 depends only on which labels the metric carries, not on the latency
  model.

## Needs measurement (carried over from step 1, still open)

1. Per-replica capacity and KV usage at that capacity (step 1 items 2–3):
   not reachable until the `role` label is fixed and a real driver exists to
   test against.
2. Cold start of a prefill/decode pod (step 1 item 4): not tested here.
3. The exact KEDA release in trap T7's verified range: now narrowed to
   `2.19.0` on `openshift-custom-metrics-autoscaler`, which is **outside**
   the `>= 2.20` this repository's README states as the requirement, and
   outside the commit T7 was verified against. `ignoreNullValues` behaved as
   T7 describes in this run, so the trap's effect holds on `2.19.0`, but
   `traps.md` should get this version added to its verified range.

## Teardown

Standup, the EPP config fix, the auth secret refresh, and the P9 run above
all happened in `step2-kv-early-20261113`. The namespace was **not** torn
down at the end of this session — it is left up for the P1–P5 follow-up
against the corrected scenario (§ Claims). Tear down with:

```bash
llmdbenchmark teardown --spec benchmark/config/specification/staging/pd-disaggregation/kv-early.yaml.j2 \
  --cluster-config benchmark/config/cluster-configs/ocp/model-sim-qwen3-32b.yaml \
  --workspace benchmark/results -p step2-kv-early-20261113
kubectl delete namespace step2-kv-early-20261113
kubectl delete clusterrolebinding allow-thanos-querier-api-access-step2-kv-early-20261113
```
