# Step 3, run 1d: knee blend, decode pool (coord-disaggregation/coordinator-kv)

Part of the [step 3 summary](../../../coordinator-kv-step3.md). Simulator numbers: blis priced this plant; they are not GPU measurements.

| | |
|---|---|
| Plant | blis-signal-lab `25f2dfb` (branch `signals-hpa`); blis-substrate `v0.14.0` catalog plus the `Qwen/Qwen3-VL-32B-Instruct` entry of inference-sim/blis-substrate#415 (open), from a local copy |
| BLIS scenario | [`coord-pd-qwen3-vl-32b-h100-bf16-tp2.yaml`](../../coord-pd-qwen3-vl-32b-h100-bf16-tp2.yaml): a `prefill` and a `decode` pool, H100, bf16, TP2 per role, as the guide's overlay; router [`router/pd-prefix-8192.yaml`](../../router/pd-prefix-8192.yaml) (prefix decider, `nonCachedTokens` 8192, the guide's PD-only value) |
| Cold start | 120 s (`loading_delay`): the protocol's default; this overlay has no GPU measurement |
| Metrics | scrape 30 s (the model-server PodMonitors set no interval; the stack's default and llm-d-benchmark's rendered PodMonitor are 30 s). The EPP's ServiceMonitor scrapes every 10 s; blis uses one interval for every series. `metrics_delay_s` 0 (raw series, no recording rule). HPA sync 15 s, `pollingInterval` 15 s |
| Seed | blis's default (no `--seed`); the load's seed is `schedule.seed` in `blis-run.json` |
| Scenario | `benchmark/config/scenarios/staging/coord-disaggregation/coordinator-kv.yaml`, rendered with `standup --dry-run`; policy [`policy.json`](policy.json), translated from it ([`terms.json`](../../terms.json)) |
| Model | `Qwen/Qwen3-VL-32B-Instruct`, 2 H100 per replica (TP2), both roles |
| SLO | TTFT ≤ 2 s, TPOT ≤ 0.05 s; X = 0.95 |
| Run | 1d, knee blend, decode pool |
| Load | four 300 s stages at 0.6, 0.8, 1.0 and 1.25 × an estimate of 1.155 req/s ([`knee-index.json`](knee-index.json)) |
| Arm | pinned: prefill 4, decode 1 (the other pool large enough not to bind) |
| Raw results | `benchmark/results/step3-coordinator-kv-20261124/decode-knee-blend/` (not committed) |

![load vs supply, decode pool](load-vs-supply-decode.svg)

blend at four rates on one pinned decode pod. A crosses 0.95 between two stages; μ_SLO = 1.165 req/s. The trigger panels are the HPA's own reading at each sync, from blis's decision log, not a Prometheus query. Each run is one sample (n = 1).

## Per stage, decode pool

| stage | s | set req/s | offered req/s | λ_in tok/s | λ_out tok/s | sent | errors | A | TTFT p50/p90/p99 s | TPOT p50/p90/p99 ms | Ready | need | requested replica-s | Ready replica-s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 300 | 0.693 | 0.700 | 2,449 | 1,493 | 210 | 0 | 100.0 % | 0.21/0.59/0.61 | 30.9/32.0/33.8 | 1–1 | – | 300 | 300 |
| 2 | 300 | 0.924 | 0.930 | 3,254 | 1,984 | 279 | 0 | 100.0 % | 0.22/0.60/0.87 | 33.7/39.1/40.6 | 1–1 | – | 300 | 300 |
| 3 | 300 | 1.155 | 1.160 | 4,058 | 2,475 | 348 | 0 | 96.6 % | 0.24/0.64/3.46 | 42.1/45.8/47.2 | 1–1 | – | 300 | 300 |
| 4 | 300 | 1.444 | 1.450 | 5,073 | 3,093 | 435 | 0 | 8.3 % | 42.38/89.70/96.57 | 48.0/49.5/52.3 | 1–1 | – | 300 | 300 |

- run: 1,272 sent, A 67.7 %, errors 0, requested 1,200 replica-s = 2,400 GPU-s at 2 GPU per replica
- placement: 0 request(s) before the first stage, 0 after the last; stages more than 3 sd off their set rate: none: ok
- knee: μ_SLO = 1.165 req/s (A crosses 0.95 between stages 3 and 4)

The estimate is `1 / mean over M of 1 / μ_SLO(m)` from runs 1a to 1c, 1.155 req/s.

## What the run shows

The run shows a problem when stage 1 fails or stage 4 passes (the crossing is outside the stages). μ_SLO = 1.165 req/s (A crosses 0.95 between stages 3 and 4).

**Outcome: as expected; μ_SLO = 1.165 req/s.**

## Amendments

None.
