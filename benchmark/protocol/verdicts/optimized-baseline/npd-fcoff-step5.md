# GPU evaluation: optimized-baseline/npd-fcoff

Step 5 of the [evaluation protocol](../../README.md)
([`5-gpu-evaluation.md`](../../5-gpu-evaluation.md)), produced with the
`gpu-evaluation` skill. The step 1 verdict is [`npd-fcoff.md`](npd-fcoff.md);
there is no step 2 verdict.

| | |
|---|---|
| Namespace | `step5-npd-fcoff-20261119` |
| Cluster | OpenShift, Kubernetes v1.32.9 |
| Scenario | `benchmark/config/scenarios/staging/optimized-baseline/npd-fcoff.yaml` |
| Overlay | `benchmark/config/cluster-configs/ocp/vllm-npd.yaml`, with `--set accelerator.resource=nvidia.com/gpu --set accelerator.profile=nvidia` (llm-d-benchmark resolves `auto` but its step 03 reads the unresolved value) |
| llm-d-benchmark | `0fb187fb` (#2192) |
| Standup session | `villardl-20261119-115628-585` |
| Model | `Qwen/Qwen3-8B`, bf16, `--max-model-len 32768`, `--max-num-seqs 128`, `--gpu-memory-utilization 0.90` |
| GPU | NVIDIA H100 80GB HBM3, 1 per replica |
| Replica | one pod of Deployment `qwen-qwe-5e3849cf-qwen3-8b-decode`; `minReplicaCount` 1, `maxReplicaCount` 6 |
| vLLM | `docker.io/vllm/vllm-openai@sha256:a4a4c0437bf7240089da5f08aa370c4aee17ae5290f7a3b468825ee26c4c3a6b` (`v0.31.0`) |
| EPP | `ghcr.io/llm-d/llm-d-router-endpoint-picker@sha256:024188e998f516879f68532016ca24fabca00afbdcc2af9a61b17634e197df2c` (`v1.0.0`) |
| Envoy | `docker.io/envoyproxy/envoy@sha256:0d8c8b3fc3250b5f664dd93ac305738f9c4e6ff38fcb8499088cca1b827f787e` (`distroless-v1.33.2`) |
| Harness | `ghcr.io/llm-d/llm-d-benchmark:v0.8.9` |
| KEDA | OpenShift Custom Metrics Autoscaler 2.19.0 (KEDA 2.19); the scenario uses only a `prometheus` trigger |
| Prometheus | OpenShift Thanos Querier, bearer auth |
| SLO | TTFT ≤ 2 s, TPOT ≤ 0.05 s (llm-d-benchmark's `random_*` profiles); X = 0.95 |
| Mix set M | 8192:256, 2048:2048, 256:4096; blend B draws each request's mix from M with equal weight |
| Threshold rule | `thresholdWork = 0.70 × ν_max`. The rule's knee mix, 8192:128, is not in M; by decision of the evaluation's owner, the 8192:256 knee sets it before run 2 |
| Loads | written by `gen_load.py`: the knees from the estimates below; runs 2 to 10 from the knees and the lengths below, in [`index.json`](npd-fcoff-step5/index.json) |
| Cold start | 128 s, requested (21:17:31 UTC, `paused-replicas` 1 → 2) to Ready (21:19:39 UTC), read from the pod's `Ready` condition; the standup pod took 250 s with the first model load |
| Lengths (§5.3) | T = 640 s (ΔR = 2 at 180 s, cold start 128 s, polling 30 s, + 120 s); phases 1,280 s; R = 158 s; Q = 720 s; G = 150 s; H = 1,178 s; D = 1,020 s |
| Profiles of runs 2 to 10 | as `gen_load.py` writes them, except `report.request_lifecycle.summary: false` and `per_request_fields.computed_metrics: false`. inference-perf 0.7.0 re-tokenizes every streamed chunk once per report it writes; these two settings remove two of its four passes. TTFT and TPOT are derived from each record's token times with inference-perf's own formulas; on run 1d's 2,648 completed requests, that derivation equals `computed_metrics` exactly. `--wait-timeout` is 9,000 s |
| Threshold in force from run 2 | `thresholdWork` 11,086, patched into the live ScaledObject after the loads were generated (21:21 UTC) and before run 2 (was 14,619) |

## μ_SLO

Knee estimates (inputs to run 1, not measurements): 8192:256 2.4 req/s,
2048:2048 1.2 req/s, 256:4096 0.7 req/s, blend 1.1 req/s.

| Mix | μ_SLO (req/s) | Source |
|---|---|---|
| 8192:256 | 1.75 | [run 1a](npd-fcoff-step5/knee-8192x256/report.md): A 0.980 at 1.68 req/s, 0.681 at 2.40 req/s |
| 2048:2048 | 1.74 | [run 1b](npd-fcoff-step5/knee-2048x2048/report.md): A 1.000 at 1.68 req/s, 0.535 at 2.28 req/s |
| 256:4096 | 1.26 | [run 1c](npd-fcoff-step5/knee-256x4096/report.md): A 1.000 at 1.20 req/s, 0.591 at 1.68 req/s |
| blend | 1.59 | [run 1d](npd-fcoff-step5/knee-blend/report.md): A 0.998 at 1.54 req/s, 0.482 at 2.09 req/s; C = 6 × 1.591 = 9.55 req/s |

## Runs

| # | Run | Status | Outcome | A | Errors | GPU-s | Report |
|---|---|---|---|---|---|---|---|
| 1a | knee 8192:256 | done | as expected; μ_SLO 1.75 req/s | 0.222 (whole run) | 3,449 timeout, 2 without latency | 1,800 | [report](npd-fcoff-step5/knee-8192x256/report.md) |
| 1b | knee 2048:2048 | done | as expected; μ_SLO 1.74 req/s | 0.569 (whole run) | 380 timeout | 1,800 | [report](npd-fcoff-step5/knee-2048x2048/report.md) |
| 1c | knee 256:4096 | done (second attempt) | as expected; μ_SLO 1.26 req/s | 0.367 (whole run) | 870 timeout, 2 without latency | 1,800 | [report](npd-fcoff-step5/knee-256x4096/report.md) |
| 1d | knee blend | done | as expected; μ_SLO 1.59 req/s | 0.561 (whole run) | 1 without latency | 1,800 | [report](npd-fcoff-step5/knee-blend/report.md) |
| 2 | SO-active staircase | done (third attempt) | problem shown: A 0.444 at 0.9 × C with 6 Ready (the need); Envoy CPU-throttled, vLLM not saturated | 0.800 | 69 `ClientConnectorError`, 19 HTTP 503, 8 without latency | 14,210 | [report](npd-fcoff-step5/stair/report.md) |
| 3 | mix-shift | done | problem shown: phase 3 settled at 6 against a headroom need of 5; A ≥ 0.997 once settled | 0.945 | 1 `ClientConnectorError`, 10 without latency | 16,699 | [report](npd-fcoff-step5/mixshift/report.md) |
| 4a | overload under the cap | done | problem shown: A 0.824 at 6 Ready in the 0.8 × C phase, Envoy CPU-throttled; a scale-down and scale-up 30 s apart failed 12 requests | 0.651 | 108 `ClientConnectorError`, 12 `ClientPayloadError`, 11 HTTP 503, 4 without latency | 10,440 | [report](npd-fcoff-step5/overload-under/report.md) |
| 4b | overload over the cap | done (second attempt) | problem shown: trigger and HPA held the cap, but the router's EPP was OOMKilled 4 times and Envoy killed once; A 0.026 in the 1.5 × C phase | 0.141 | 2,372 timeout, 2,037 `ClientPayloadError`, 1,632 HTTP 400, 435 `ClientConnectorError`, 6 HTTP 503, 11 without latency | 10,104 | [report](npd-fcoff-step5/overload-over/report.md) |
| 5 | agentic | done | as expected: A ≥ 0.997, 0 errors, no context overflow; prompts lighter than planned | 0.997 | 0 | 5,215 | [report](npd-fcoff-step5/agentic/report.md) |
| 8 | bursty | done | problem shown: each of 3 scale-downs cut in-flight streams (18 in total); quiet phases A ≥ 0.985; each spike's replica Ready after the spike | 0.961 | 18 `ClientPayloadError`, 1 HTTP 503, 1 `ClientConnectorError`, 3 without latency | 11,142 | [report](npd-fcoff-step5/bursty/report.md) |
| 9 | short spikes | done | problem shown: A 0.843 in the first gap; the pool ratcheted to the cap of 6 (need 5 in spikes, 2 in gaps); 11 streams cut at 2 scale-downs | 0.976 | 11 `ClientPayloadError`, 2 without latency | 10,053 | [report](npd-fcoff-step5/short-spikes/report.md) |
| 10 | scale-down | done (second attempt) | problem shown: no step ended at its need (6/4, 4/2, 2/1); 25 streams cut at 4 scale-downs; A ≥ 0.990 after each step; Envoy killed in the 0.9 × C hold | 0.560 | 1,528 timeout, 1,435 `ClientPayloadError`, 608 `ClientConnectorError`, 102 `ServerDisconnectedError`, 41 `ClientOSError`, 6 HTTP 503, 7 without latency | 19,080 | [report](npd-fcoff-step5/scale-down/report.md) |
| 6 | static-replica baseline | done | cost reference: 23,040 GPU-s at 6 replicas, A 0.999 | 0.999 | 5 without latency | 23,040 | [report](npd-fcoff-step5/baseline/report.md) |
| 7 | scale from zero | not run | `minReplicaCount` is 1 (§5.3: only with `minReplicaCount: 0`) | | | | |

## Discarded attempts

| Run | Session | Reason | Rerun |
|---|---|---|---|
| 1c, knee 256:4096 at an estimate of 0.7 req/s | `villardl-20261119-134008-873` | A = 1.000 through stage 5 (1.33 req/s) and 0.170 at stage 6 (1.75 req/s): the crossing is not between stages 2 and 5 (§5.3). The data otherwise passed the gate; the `llm-d-prism` pod, not a serving pod, was evicted for node ephemeral storage during the run | `knee-256x4096-r2`, estimate 1.2 req/s |
| 2, SO-active staircase | `villardl-20261119-162317-913` | No per-request records: inference-perf was still writing its reports 43 min after the load ended (22:18 UTC) when `llmdbenchmark`'s 4,800 s wait expired, and the harness pod was deleted with `per_request_lifecycle_metrics.json` at 0 bytes. A tool fault, outside the configuration | the same trace, with the profile changes below |
| 2, SO-active staircase, second attempt | `villardl-20261119-180813-648` | The 3 Gi workload PVC filled with the earlier runs' results: `per_request_lifecycle_metrics.json` stopped at 583,008,256 bytes with `Disk quota exceeded`. The harness then relaunched inference-perf, which replayed part of the trace from 00:19 to 00:49 UTC until its pod was deleted. The PVC was grown to 30 Gi. A tool fault, outside the configuration | third attempt, from 00:53 UTC |
| 4b, overload over the cap | `villardl-20261120-000706-273` | The load generator fell behind: from about 1,960 s into the run (phase 2, 14.3 req/s) the send delay reached 122 s, and no request was sent between 2,140 and 2,200 s. The harness pod ran at its 2-CPU limit, throttled in 46 to 80 % of CFS periods from 05:37 to 05:49 UTC. The run was not open loop (§5.6). A tool limit, outside the configuration | rerun from 06:08 UTC with `--set harness.resources.cpu=8 --set harness.resources.memory=32Gi` |
| 10, scale-down | `villardl-20261120-110530-312` | The token KEDA uses for Thanos, minted at standup with a 24 h lifetime, expired at 16:56:44 UTC, about 2,730 s into the run: from then KEDA got HTTP 401, the ScaledObject was `Ready=False` and the HPA read `<unknown>` with the pool frozen at 4. At 16:59 UTC the workstation's copy of the cluster credentials also expired; `llmdbenchmark` could not list the results and deleted the harness pod about 3,400 s into the 4,238 s schedule. A fault outside the configuration | rerun after a new 72 h token was written to the `prometheus-auth` Secret at 17:17 UTC |

## Run 3 against run 6

The same trace (`mixshift.csv`, byte-identical), autoscaled from 1 replica
(run 3) and pinned at run 3's peak of 6 (run 6). GPU-seconds are requested
replica-seconds × 1 GPU.

| Phase | Mix | A, run 3 | A after settling, run 3 | A, run 6 | GPU-s, run 3 | GPU-s, run 6 | Run 3 / run 6 |
|---|---|---|---|---|---|---|---|
| 1 | 8192:256 | 0.852 | 0.998 | 0.999 | 4,352 | 7,680 | 0.57 |
| 2 | 2048:2048 | 0.999 | 0.999 | 1.000 | 5,120 | 7,680 | 0.67 |
| 3 | 256:4096 | 0.983 | 0.997 | 0.997 | 7,228 | 7,680 | 0.94 |
| all | | 0.945 | | 0.999 | 16,699 | 23,040 | 0.72 |

Run 3 used 28 % fewer GPU-seconds. Its attainment cost was the ramp from one
replica in phase 1 (A 0.852 against 0.999). Once settled, the two runs differ
by at most 0.001 in A (n = 1 each).

## Conclusions

2026-11-20 20:45 UTC, final: covering every run; run 7 not run
(`minReplicaCount` is 1).

**Verdict.** On one H100 per replica, the npd-fcoff work-rate trigger
measures the offered load and sizes the pool to it, with the configured
headroom. Under every load the GPU pool can serve, attainment holds once the
pool has settled. Run 3 used 28 % fewer GPU-seconds than a static fleet at its
peak, and lost attainment only during its ramp. Three properties of the
evaluated stack cost requests:

1. **Scale-downs cut in-flight requests.** All 11 scale-downs observed under
   load (runs 4a, 4b, 8, 9, 10) failed the streams the removed pod was serving,
   1 to 14 per removal (80 in total).
2. **The scenario's router is the bottleneck above about 0.7 × C.** Envoy is
   CPU-throttled at its 500m limit from 6.7 req/s (runs 2, 4a, 10); at
   1.5 × C the EPP is OOMKilled at its 1 Gi limit (run 4b). Run 2's and run
   4a's high-load phases failed the SLO with the GPU pool at the need and
   vLLM unsaturated.
3. **The HPA behavior is slow both ways for this load.** +1 pod per 180 s
   needs about 1,000 s to reach the cap; spikes shorter than that are missed
   (run 8), and short spikes ratchet the pool to the cap because the gaps are
   shorter than the 300 s scale-down window (run 9). −1 pod per 300 s leaves
   2,000 to 3,000 replica-seconds above the need per load step (run 10).

**Against step 1** ([`npd-fcoff.md`](npd-fcoff.md)): the threshold needed
re-measuring on this plant (P3; 11,086 against 14,619); the ratio terms
inflate by about 4,000 × with no completions in the window (P8; run 1a, and
one sample after the idle arm in run 3). A trigger reading 0 during router
restarts (P7) was seen only in a discarded attempt of run 4b.

Detail by run:

- μ_SLO(8192:256) = 1.75 req/s on one H100. TPOT reaches its limit before
  TTFT does.
- The rule gives `thresholdWork = 0.70 × 1.752 × (8192 + 3.3 × 256) = 11,086`
  work tok/s, 24 % below the shipped 14,619. It is patched into the live
  ScaledObject before run 2.
- The trigger read 0.90 to 1.07 × the offered ν while requests completed. With
  no completions in its 1 m window it read about 4,000 × the offered ν, the
  inflation the step 1 verdict predicted (P8).
- μ_SLO(2048:2048) = 1.74 req/s. There TTFT binds, with vLLM at 128 running
  sequences and a full KV cache. The work rate at that knee, 15,363 work tok/s,
  is 3 % below the 8192:256 knee's 15,836.
- μ_SLO(256:4096) = 1.26 req/s, the lowest of the three mixes; TTFT binds.
- μ_SLO(B) = 1.59 req/s, so C = 9.55 req/s of the blend at 6 replicas. The
  trigger read 0.98 to 1.15 × the blend's offered ν through the knee.
- Run 2: the pool followed the staircase, one replica per 180 s, to the cap
  of 6 at 0.7 × C, and held A ≥ 0.992 through 0.7 × C. It ran 1 to 2 replicas
  above `ceil(q / μ_SLO(B))`: the threshold comes from the 8192:256 knee,
  whose ν per request is below the blend's. At 0.9 × C, A fell to 0.444 with
  6 replicas Ready and vLLM at most 72 of 128 running per pod: the router's
  Envoy was CPU-throttled at the scenario's 500m limit.
- Run 3: at 3.78 req/s the trigger read 0.98 to 1.01 × the offered work rate
  in each mix, and the pool held 4, 4, then 6 replicas, against 4, 4 and 5
  with the configuration's 0.70 headroom. A once settled was 0.997 or more in
  every phase. A one-sample spike to 4.3 × 10⁶ followed the idle arm (P8).
- Run 4a: at 0.8 × C the pool reached its cap and A after settling was 0.824,
  with Envoy throttled as in run 2. At 1.91 req/s the trigger hovered around
  the 2-replica boundary; one scale-down was undone 30 s later, and the
  removed pod's 12 in-flight streams failed.
- Run 4b: at 1.5 × C the trigger read 1.05 × the offered work rate and the
  pool held the cap of 6. The router did not hold: its EPP was OOMKilled at
  the scenario's 1 Gi limit four times and Envoy was killed once, and A was
  0.026. The phase-1 flap of run 4a recurred and cut 14 streams.
- Run 5: multi-turn sessions up to 32,391-token prompts completed without an
  error, A ≥ 0.997, on 1 to 2 replicas; the trigger read 0.91 to 1.02 × the
  offered work rate.
- Run 8: every scale-down under load cut the removed pod's in-flight streams
  (4, 8 and 6), as in runs 4a and 4b. Each spike's added replica was Ready
  only after the spike ended. Quiet phases held A ≥ 0.985, one replica above
  `ceil(ν / threshold)`.
- Run 9: with gaps shorter than the 300 s scale-down window, each short spike
  added a replica and none was released: the pool reached its cap of 6 for
  spikes that need 5 and gaps that need 2, and held 6 for 337 s after the
  last spike. A fell to 0.843 in the first gap; the two scale-downs that
  followed cut 11 streams.
- Run 10: capacity is released one pod per 300 s after a 300 s window, slower
  than the 1,020 s steps: the pool ended the steps at 6, 4 and 2 replicas
  against needs of 4, 2 and 1, about 2,000 to 3,000 replica-seconds above the
  need per step. A held at 0.990 or more. All four scale-downs cut in-flight
  streams (25 in total). Envoy was killed once in the 0.9 × C hold.
- Across runs 4a, 4b, 8, 9 and 10, every scale-down observed under load (11)
  cut the streams the removed pod was serving.
- Run 6: the static fleet of 6 held A 0.999 on run 3's trace for 23,040
  GPU-seconds, against run 3's 16,699.

## Log

- 2026-11-19: standup; ScaledObject `READY=True`, HPA reads `0/14619 (avg)`.
  Smoke load (`sanity_random.yaml`): the trigger left 0 (max 283 work tok/s).
- 2026-11-19: runs 1a and 1b done. First 1c discarded (crossing outside the
  stages); rerun at an estimate of 1.2 req/s.
- 2026-11-19: run 1d done. `llmdbenchmark` exited 1 on a workstation-side
  `Bad Gateway` after the load ended; the results were copied from the PVC.
- 2026-11-19: run 1c (second attempt) done. The knees are final.
- 2026-11-19 21:17–21:19 UTC: cold-start probe (pin 2). then: threshold
  patched to 11,086; runs 2 to 10 generated and staged.
- 2026-11-19: run 2 discarded (no per-request records, see Discarded
  attempts); profiles of runs 2 to 10 changed; run 2 restarted at 23:06 UTC.
- 2026-11-20: run 2's second attempt discarded (PVC full). Its series show
  the router's Envoy at its 0.50-core CPU limit and throttled in 99–100 % of
  CFS periods from 23:33 UTC (stage 4, 6.68 req/s), a readiness-probe timeout
  of the router pod at about 23:58 UTC, and 132 `ConnectionRefusedError`
  between 23:52 and 00:02 UTC. During the knees, Envoy used at most
  0.39 core, throttled in at most 16 % of periods. The scenario sets that
  limit (`router.proxy.resources.limits.cpu: 500m`); the evaluation keeps it
  (no answer to the question whether to raise it). 00:53 UTC: run 2 restarted.
- 2026-11-20: runs 2, 3 and 4a done. Run 4b's first attempt discarded (the
  load generator fell behind). Before it fell behind, its trigger read 0 in
  five consecutive 15 s samples, 05:40:50–05:41:50 UTC, with the EPP scrape
  target up and `rate(llm_d_epp_request_total)` at 0. These numbers come from
  a discarded run and are not used in any report.
- 2026-11-20 12:30 UTC, correction to the entry above: the router pod's `epp`
  container was OOMKilled at 05:37 UTC and its `envoy-proxy` restarted at
  05:40, 05:44, 05:46, 05:49 and 05:52 UTC
  (`kube_pod_container_status_restarts_total`). The zero readings fall in
  that window; they are not shown to be the admission undercount of trap T8.
- 2026-11-20: run 4b's second attempt ran 06:11–06:56 UTC; `llmdbenchmark`
  exited 1 on collection, and the results were copied from the PVC. The
  session was then idle until 12:19 UTC. At 11:00:09 UTC the Deployment went
  from 1 to 0 replicas and back to 1 at 11:00:28, with no run active and no
  action from this evaluation: unexplained.
- 2026-11-20: run 5 done; the question on the router limits again had no
  answer, so runs 8 to 10 and 6 keep the scenario's limits.
- 2026-11-20: runs 8 and 9 done. Run 10's first attempt discarded: KEDA's
  Thanos token expired at 16:56:44 UTC (24 h after standup). A new token for
  the same ServiceAccount (`keda-prometheus-auth`, 72 h) was written to the
  `prometheus-auth` Secret at 17:17 UTC; the ScaledObject was `Ready=True` and
  the HPA read `0/11086` 75 s later. Runs 1 to 9 ended before the expiry.
- 2026-11-20: run 10 (second attempt) and run 6 done. 20:43 UTC: the plant
  pinned at 1 replica. Evaluation closed.
- 2026-11-20 20:50 UTC: the plant is kept, pinned at 1 replica
  (`autoscaling.keda.sh/paused-replicas: "1"`, 1 H100); teardown awaits its
  owner's decision. KEDA's Thanos token in `prometheus-auth` expires
  2026-11-23 17:17 UTC.
