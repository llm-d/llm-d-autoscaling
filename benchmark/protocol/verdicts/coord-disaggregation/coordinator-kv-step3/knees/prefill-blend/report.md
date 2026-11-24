# Step 3, run 1f: knee blend, prefill pool (coord-disaggregation/coordinator-kv)

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
| Run | 1f, knee blend, prefill pool |
| Load | four 300 s stages at 0.6, 0.8, 1.0 and 1.25 × an estimate of 4.0 req/s ([`knee-index.json`](knee-index.json)) |
| Arm | pinned: prefill 1, decode 4 (the other pool large enough not to bind) |
| Raw results | `benchmark/results/step3-coordinator-kv-20261124/prefill-knee-blend/` (not committed) |

![load vs supply, prefill pool](load-vs-supply-prefill.svg)

blend at four rates on one pinned prefill pod. A crosses 0.95 between two stages; μ_SLO = 4.369 req/s. The trigger panels are the HPA's own reading at each sync, from blis's decision log, not a Prometheus query. Each run is one sample (n = 1).

## Per stage, prefill pool

| stage | s | set req/s | offered req/s | λ_in tok/s | λ_out tok/s | sent | errors | A | TTFT p50/p90/p99 s | TPOT p50/p90/p99 ms | Ready | need | requested replica-s | Ready replica-s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 300 | 2.400 | 2.400 | 8,397 | 5,120 | 720 | 0 | 100.0 % | 0.21/0.71/1.27 | 28.5/30.5/31.9 | 1–1 | – | 300 | 300 |
| 2 | 300 | 3.200 | 3.210 | 11,231 | 6,848 | 963 | 0 | 99.9 % | 0.22/0.90/1.55 | 32.3/35.6/37.6 | 1–1 | – | 300 | 300 |
| 3 | 300 | 4.000 | 4.000 | 13,995 | 8,533 | 1,200 | 0 | 98.2 % | 0.23/1.07/2.44 | 38.3/39.9/40.7 | 1–1 | – | 300 | 300 |
| 4 | 300 | 5.000 | 5.010 | 17,528 | 10,688 | 1,503 | 0 | 89.4 % | 0.25/2.11/8.85 | 41.9/45.5/48.0 | 1–1 | – | 300 | 300 |

- run: 4,386 sent, A 95.9 %, errors 0, requested 1,200 replica-s = 2,400 GPU-s at 2 GPU per replica
- placement: 0 request(s) before the first stage, 0 after the last; stages more than 3 sd off their set rate: none: ok
- knee: μ_SLO = 4.369 req/s (A crosses 0.95 between stages 3 and 4)

A counts every request, and two thirds of the blend never reach prefill. Counting only the 8192-token requests (the ones prefill serves), A per stage is 1.000, 0.997, 0.948 and 0.710 at 0.807, 1.063, 1.337 and 1.667 req/s of them, crossing 0.95 at 1.324 req/s, 1.3 % from run 1e's 1.341. The other requests' A is 1.000 until stage 4 (0.985). The protocol's μ_SLO(B) for prefill, 4.369, therefore overstates prefill's capacity for the blend; the reports use 3 × 1.324 = 3.972 req/s for prefill's need. Decode was pinned at 4, near its own blend capacity (4 × 1.165 = 4.66 req/s): TPOT p50 rose from 28.5 to 41.9 ms over the stages.

## What the run shows

The run shows a problem when stage 1 fails or stage 4 passes (the crossing is outside the stages). μ_SLO = 4.369 req/s (A crosses 0.95 between stages 3 and 4).

**Outcome: as expected; μ_SLO = 4.369 req/s.**

## Amendments

None.
