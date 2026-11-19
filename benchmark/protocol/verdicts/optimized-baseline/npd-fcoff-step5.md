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
| Threshold in force from run 2 | `thresholdWork` 11,086, patched into the live ScaledObject at 21:23 UTC (was 14,619) |

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
| 2 | SO-active staircase | running | | | | | |
| 3 | mix-shift | pending | | | | | |
| 4a | overload under the cap | pending | | | | | |
| 4b | overload over the cap | pending | | | | | |
| 5 | agentic | pending | | | | | |
| 8 | bursty | pending | | | | | |
| 9 | short spikes | pending | | | | | |
| 10 | scale-down | pending | | | | | |
| 6 | static-replica baseline | pending | | | | | |
| 7 | scale from zero | not run | `minReplicaCount` is 1 | | | | |

## Discarded attempts

| Run | Session | Reason | Rerun |
|---|---|---|---|
| 1c, knee 256:4096 at an estimate of 0.7 req/s | `villardl-20261119-134008-873` | A = 1.000 through stage 5 (1.33 req/s) and 0.170 at stage 6 (1.75 req/s): the crossing is not between stages 2 and 5 (§5.3). The data otherwise passed the gate; the `llm-d-prism` pod, not a serving pod, was evicted for node ephemeral storage during the run | `knee-256x4096-r2`, estimate 1.2 req/s |

## Conclusions

2026-11-19 21:20 UTC, covering run 1 (all knees); runs 2 to 10 and 6 pending.

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

## Log

- 2026-11-19: standup; ScaledObject `READY=True`, HPA reads `0/14619 (avg)`.
  Smoke load (`sanity_random.yaml`): the trigger left 0 (max 283 work tok/s).
- 2026-11-19: runs 1a and 1b done. First 1c discarded (crossing outside the
  stages); rerun at an estimate of 1.2 req/s.
- 2026-11-19: run 1d done. `llmdbenchmark` exited 1 on a workstation-side
  `Bad Gateway` after the load ended; the results were copied from the PVC.
- 2026-11-19: run 1c (second attempt) done. The knees are final.
- 2026-11-19 21:17–21:19 UTC: cold-start probe (pin 2). 21:23 UTC: threshold
  patched to 11,086; runs 2 to 10 generated and staged.
