# blis evaluation: coord-disaggregation/coordinator-kv

Step 3 of the [evaluation protocol](../../README.md)
([`3-blis-evaluation.md`](../../3-blis-evaluation.md)), produced with the
`blis-evaluation` skill. The step 1 verdict is
[`coordinator-kv.md`](coordinator-kv.md); the step 2 verification is
[`coordinator-kv-step2.md`](coordinator-kv-step2.md).

Every number here is a simulator number, for the plant blis priced. None is a
GPU measurement, and none is given to step 5.

| | |
|---|---|
| Plant | blis-signal-lab `25f2dfb` (branch `signals-hpa`), built with Go 1.27.2. Run 4b only: with the TTFT projection of inference-sim `dfa8987` (#2237) applied ([`lab-patch.diff`](coordinator-kv-step3/overload-over/lab-patch.diff)), which raises the reported TTFT of most disaggregated requests by about 20 ms (18.4 ms on average on the prefill 8192:256 knee, which does not move) and nothing the HPAs read (its decision logs are byte-identical without it), so run 4b's TTFT and A are on that basis |
| Catalog | blis-substrate `v0.14.0` (the lab's `go.mod`) has no `Qwen/Qwen3-VL-32B-Instruct`; the runs use a local copy of the catalog with the entry proposed in inference-sim/blis-substrate#415 (open). When #415 merges and the lab moves to it, `blis catalog resolve` needs no local copy |
| BLIS scenario | [`coord-pd-qwen3-vl-32b-h100-bf16-tp2.yaml`](coordinator-kv-step3/coord-pd-qwen3-vl-32b-h100-bf16-tp2.yaml), written for the evaluation: a `prefill` and a `decode` pool of 4 H100 replicas each, bf16, TP2 per role, the guide overlay's GPU type and parallelism. Router [`router/pd-prefix-8192.yaml`](coordinator-kv-step3/router/pd-prefix-8192.yaml): the prefix decider with `nonCachedTokens` 8192, the guide's PD-only value |
| Coordinator | not modelled: blis has no Coordinator, so the EPP's P/D flow with the prefix decider stands in for conditional-decode → prefill → decode (upstream ask inference-sim/inference-sim#1959). Step 2 checked the Coordinator's 412 path on a cluster |
| Scenario | `benchmark/config/scenarios/staging/coord-disaggregation/coordinator-kv.yaml`, rendered with `llmdbenchmark standup --dry-run` (llm-d-benchmark v1.0.0) against llm-d `main` with llm-d/llm-d#3111 and #3114 merged locally |
| ScaledObjects | [`scaledobjects.json`](coordinator-kv-step3/scaledobjects.json): `coord-pd-prefill` → `pd-disagg-coord-prefill`, `coord-pd-decode` → `pd-disagg-coord-decode`; each `minReplicaCount` 1, `maxReplicaCount` 4. Policy [`policy.json`](coordinator-kv-step3/policy.json), translated by `so_to_policy.py` with [`terms.json`](coordinator-kv-step3/terms.json) |
| Model | `Qwen/Qwen3-VL-32B-Instruct`, 2 H100 per replica (TP2), both roles |
| SLO | TTFT ≤ 2 s, TPOT ≤ 0.05 s; X = 0.95 |
| Mix set M | 8192:256, 2048:2048, 256:4096; blend B draws each request's mix from M with equal weight |
| Cold start | 120 s (`loading_delay`): the protocol's default; this overlay has no GPU measurement |
| Metrics pipeline | scrape 30 s: the model-server PodMonitors (#3114) set no interval, and the stack's default and llm-d-benchmark's rendered PodMonitor are 30 s. The EPP's ServiceMonitor scrapes every 10 s, which blis cannot express (one interval for every series). `metrics_delay_s` 0: the triggers query raw series. HPA sync 15 s, `pollingInterval` 15 s |
| Seed | blis's default (no `--seed`); each load's seed is in [`index.json`](coordinator-kv-step3/index.json) |
| Thresholds | the scenario's, unchanged: prefill 7,782 uncached tok/s, decode KV 0.45, alarm 5. Run 1e gives `0.7 × 1.341 × 8,192 = 7,690` for prefill, 1.2 % below 7,782; the scenario was not changed |
| Loads | written by `gen_load.py --target blis`: the knees from the estimates below, runs 2 to 10 from the knees, in [`index.json`](coordinator-kv-step3/index.json). Client timeout 311 s (the run set's rule for 4,096-token outputs) |
| Lengths | T = 450 s (≥ 1 × 180 + 120 + 15 + 120 = 435 s; prefill's 1 pod per 180 s is the slower scale-up); phases 900 s; R = 15 + 0 + 120 = 135 s; Q = 300 + 300 + 120 = 720 s; G = min(270, 150) = 150 s; H = (4 − 1) × 180 + 135 + 120 = 795 s; D = 1,020 s |
| Reruns | each run's `command.txt` and `blis-run.json` name the lab `<lab>` and this repository's checkout `<repo>`; substitute both (or rerun from the raw `sim/` directory) before `blis_run.py --rerun` |
| Memory | `blis_run.py`'s guard on every run, one at a time; peak RSS 0.35 to 4.75 GiB against budgets of 3.65 to 9.03 GiB (`memory` in each `blis-run.json`); no run killed |

## Translation of the triggers

| Trigger | Query (namespace label omitted) | blis terms | Fidelity |
|---|---|---|---|
| `prefill-uncached-prompt-tokens` | `sum(rate(llm_d_epp_prefix_prompt_tokens_sum{endpoint_role="prefill",plugin_name="decode-prefix-producer"}[2m])) - sum(rate(llm_d_epp_prefix_predicted_cached_tokens_sum{...}[2m]))` | `prefill_prompt_tokens`, `rate`, 120 s | approximated: counted at dispatch to a prefill pod, and cached tokens are not subtracted. The run set's loads share no prefix except agentic's 4,096-token system prompt, so the difference is 0 outside run 5 |
| `decode-kv-usage-2m` | `sum(max by (pod, engine) (avg_over_time(vllm:kv_cache_usage_perc{pod=~"pd-disagg-coord-.+-decode-.+"}[2m])))` | `kv_usage`, `sum`, `role: decode`, `avg_over_time`, 120 s | exact |
| `decode-waiting-alarm` | `sum(max by (pod, engine) (vllm:num_requests_waiting{pod=~"pd-disagg-coord-.+-decode-.+"}))` | `waiting`, `sum`, `role: decode` | exact |

The translation gate (§3.5 step 5) passed on run 1a: every trigger left 0
while load was sent.

## Simulator gaps that apply

From 3-blis-evaluation.md §3.2:

- **No Coordinator** (above): the decider is the EPP's, with the guide's threshold.
- **Requests past the client timeout keep running** (inference-sim#2291). They
  hold engine capacity a cluster would free. This slows recovery in run 10
  (see its report).
- **A draining pod finishes its work.** No request can fail at a scale-down;
  that half of the criteria of runs 8 to 10 cannot show.
- **One scrape interval** for the EPP and vLLM series (30 s here, 10 s for the
  EPP on the cluster).
- **Scrapes and syncs on the same instants**: the dead time after a load
  change is the shortest the phases allow.
- **The prefill counter** is approximated (translation table).

## μ_SLO

Knee estimates (inputs to run 1, not measurements): decode 8192:256 3.0
req/s, 2048:2048 1.0 req/s, 256:4096 0.8 req/s, blend 1.155 req/s (from the
three); prefill 8192:256 1.4 req/s, blend 4.0 req/s. Each knee pins its pool at
1 and the other pool at 4.

| Pool | Mix | μ_SLO (req/s) | Source |
|---|---|---|---|
| decode | 8192:256 | 3.078 | [run 1a](coordinator-kv-step3/knees/decode-8192x256/report.md): A 1.000 at 3.00 req/s, 0.517 at 3.75 req/s |
| decode | 2048:2048 | 1.000 | [run 1b](coordinator-kv-step3/knees/decode-2048x2048/report.md): A 0.950 at 1.00 req/s, 0.000 at 1.25 req/s |
| decode | 256:4096 | 0.678 | [run 1c](coordinator-kv-step3/knees/decode-256x4096/report.md): A 1.000 at 0.64 req/s, 0.792 at 0.80 req/s; an upper bound (300 s stages, 205 s requests at the TPOT limit) |
| decode | blend | 1.165 | [run 1d](coordinator-kv-step3/knees/decode-blend/report.md): A 0.966 at 1.16 req/s, 0.083 at 1.45 req/s; C = min over pools of 4 × μ_SLO(B) = 4 × 1.165 = **4.66 req/s**, set by decode |
| prefill | 8192:256 | 1.341 | [run 1e](coordinator-kv-step3/knees/prefill-8192x256/report.md): A 0.994 at 1.12 req/s, 0.938 at 1.40 req/s; 10,985 uncached tok/s per pod |
| prefill | blend | 4.369 (protocol rule); 3.972 for prefill's need | [run 1f](coordinator-kv-step3/knees/prefill-blend/report.md): A over all requests crosses at 4.369 req/s, but two thirds of the blend never reach prefill. The 8192-token requests alone cross at 1.324 req/s, 1.3 % from run 1e, so prefill's blend capacity is 3 × 1.324 = 3.972 req/s. The reports use 3.972 for prefill's need |

## Runs

GPU-seconds are requested replica-seconds × 2 GPUs, both pools.

| # | Run | Status | Outcome | A | Errors | GPU-s (decode + prefill) | Report |
|---|---|---|---|---|---|---|---|
| 1a–1f | knees | done | as expected; μ_SLO above | | timeouts in the last stage of 1b (59) and 1c (72) | 2,400 per knee pool | [1a](coordinator-kv-step3/knees/decode-8192x256/report.md), [1b](coordinator-kv-step3/knees/decode-2048x2048/report.md), [1c](coordinator-kv-step3/knees/decode-256x4096/report.md), [1d](coordinator-kv-step3/knees/decode-blend/report.md), [1e](coordinator-kv-step3/knees/prefill-8192x256/report.md), [1f](coordinator-kv-step3/knees/prefill-blend/report.md) |
| 2 | SO-active staircase | done | as expected: decode 1 → 4 with the stages, prefill 1 → 2; A ≥ 0.951 per stage | 0.994 | 0 | 18,900 (12,780 + 6,120) | [report](coordinator-kv-step3/stair/report.md) |
| 3 | mix-shift | done | problem shown: phase 2 ended at 3 decode against a headroom need of 2 (the alarm's step to the cap); 180 s at A 0.000 to 0.420 after the shift; A 1.000 once settled | 0.943 | 0 | 20,640 (12,840 + 7,800) | [report](coordinator-kv-step3/mixshift/report.md) |
| 4a | overload under the cap | done | as expected: decode at the cap 210 s after the step; A 0.987 after | 0.891 | 0 | 16,380 (11,160 + 5,220) | [report](coordinator-kv-step3/overload-under/report.md) |
| 4b | overload over the cap | done (second attempt, lab patch) | as expected: decode at the cap 180 s after the step and held it, both triggers above the cap's count; prefill held 3, its own need with headroom | 0.203 | 4,844 timeout | 19,080 (12,360 + 6,720) | [report](coordinator-kv-step3/overload-over/report.md) |
| 5 | agentic | done | as expected: both pools at 1, A ≥ 0.998, 0 errors, no context overflow | 0.999 | 0 | 10,800 (5,400 + 5,400) | [report](coordinator-kv-step3/agentic/report.md) |
| 8 | bursty | done | as expected: A ≥ 0.967 in every quiet phase; each spike's pods Ready after it ended; decode held 2 to 4 in quiet phases that need 1 | 0.986 | 0 | 28,740 (19,950 + 8,790) | [report](coordinator-kv-step3/bursty/report.md) |
| 9 | short spikes | done | as expected: A 1.000 throughout; decode held 3 through gaps that need 1 | 1.000 | 0 | 15,732 (11,388 + 4,344) | [report](coordinator-kv-step3/short-spikes/report.md) |
| 10 | scale-down | done | problem shown: A 0.333 in the opening hold from 1 pod; the 0.6 × C step ended at 4 decode against 3; the next steps reached their need | 0.695 | 340 timeout | 35,100 (23,250 + 11,850) | [report](coordinator-kv-step3/scale-down/report.md) |
| 6 | static-replica baseline | done | cost reference: prefill 2 and decode 4 pinned, A 1.000 | 1.000 | 0 | 32,400 (21,600 + 10,800) | [report](coordinator-kv-step3/mixshift-static/report.md) |
| 7 | scale from zero | not run | `minReplicaCount` is 1 (run-set.md §2: only with `minReplicaCount: 0`) | | | | |

## Discarded attempts

| Run | Reason | Rerun |
|---|---|---|
| 4b, overload over the cap (1.2 × μ_SLO(B), then 1.5 × C = 6.99 req/s) | blis exited 1: `the emitted Result does not validate against blis-schemas`, with `request_rows[2627].e2e_us: is 976051970, less than ttft_us 1040973507` and `tpot_us: must be a finite number, not negative`, on 2 of 7,551 rows. Memory was not the cause (peak 5.22 of 6.25 GiB, not killed). A simulator defect, outside the configuration | rerun on `25f2dfb` with the TTFT projection of inference-sim `dfa8987` (#2237), which upstream fixed for this case; the two failing rows are decode sub-requests preempted after their first token, whose TTFT was rebuilt from a scheduling delay re-stamped at re-admission |

## Run 3 against run 6

The same schedule (`mixshift`, seed 103), autoscaled from 1 pod per pool (run
3) and pinned at run 3's peaks, prefill 2 and decode 4 (run 6). GPU-seconds
are requested replica-seconds × 2 GPUs, both pools.

| Phase | Mix | A, run 3 | A after settling, run 3 | A, run 6 | GPU-s, run 3 | GPU-s, run 6 | Run 3 / run 6 |
|---|---|---|---|---|---|---|---|
| 1 | 8192:256 | 0.994 | 0.994 | 1.000 | 5,340 | 10,800 | 0.49 |
| 2 | 2048:2048 | 0.835 | 1.000 | 1.000 | 8,400 | 10,800 | 0.78 |
| 3 | 256:4096 | 1.000 | 1.000 | 1.000 | 6,900 | 10,800 | 0.64 |
| all | | 0.943 | | 1.000 | 20,640 | 32,400 | 0.64 |

Run 3 used 36 % fewer GPU-seconds (20,640 against 32,400). Its attainment cost
was the 270 s from the shift to 2048:2048 until 4 decode pods were Ready
(phase 2, A 0.835 against 1.000). Once settled, the two runs differ by at most
0.006 in A (n = 1 each).

## Conclusions

2026-11-24 16:25 UTC, final: covering every run; run 7 not run
(`minReplicaCount` is 1).

**Verdict.** In blis, on Qwen3-VL-32B with TP2 per role, `coordinator-kv`
sizes both pools to the load. Each pool settles at `ceil(signal / threshold)`,
which is the knee need plus the configuration's headroom. Once the pools
settled and the backlog of a step drained, A was 0.987 or more in every
phase; run 10's opening hold, which starts from 1 pod at 0.9 × C, did not
drain within its 795 s. Run 3 used 36 % fewer
GPU-seconds than the static fleet at its peaks, and lost attainment only
after its mix shift. Two properties cost attainment or GPU-seconds:

1. **A load step from a small pool costs minutes of attainment.** Decode
   proposes within 60 to 90 s of a step, and its 4-pods-per-60 s policy
   requests the cap in one or two decisions, but the pods are Ready one cold
   start (120 s) later: 210 s after the step in run 4a, 270 s in run 3, 180 s
   in run 10. A per 60 s fell to 0.037 at its lowest in run 4a and to 0.000 in
   run 3. In run 10,
   one decode pod at 3.6 × its blend capacity for 180 s built a backlog that
   held A at 0.360 for the rest of the 795 s hold; blis's serving of requests
   past their timeout slows that recovery by an amount the run does not measure.
2. **The KV target holds a pod above the knee need at low and moderate
   load.** The 0.45 target is reached at 0.72 to 0.83 of each mix's knee, so at
   0.2 × C decode holds 2 where 1 serves the SLO (runs 8 and 9), and at 0.6 × C
   it holds 4 where 3 does (run 10). With gaps shorter than the 300 s
   scale-down window, short spikes keep decode at 3 (run 9); it does not reach
   the cap of 4.

**Against step 1** ([`coordinator-kv.md`](coordinator-kv.md)):

- *Each pool settles at `ceil(signal / threshold)` and holds at constant
  load*: shown in runs 2, 3, 4a and 10.
- *A mix shift toward long outputs is seen by decode about one decode duration
  late; the alarm then takes decode to the cap within one cold start*: in run
  3, decode asked 2 at 90 s after the shift to 2048:2048 and the cap at 150 s;
  the alarm won 8 of 60 syncs. The shift to 256:4096 came with 3 decode pods
  and cost nothing (A 1.000).
- *Under overload, both pools reach `maxReplicaCount`*: decode reached it in
  runs 4a (0.8 × C) and 4b (1.5 × C) and held it while the load held. Prefill
  did not: C is decode's capacity, so at 1.5 × C prefill's need is 2 and it held
  3 (run 4b), and at 0.8 × C 1 to 2 (run 4a).
- *The alarm wins only under overload or a step*: it won 8 of 60, 10 of 60 and
  49 of 53 syncs in the steps of runs 3, 4a and 10, and no sync in runs 2, 5,
  8 and 9 (P1 to P3 by kind, as step 1 required of it).
- *KV occupancy at the knee is mix-robust (decode P3)*: interpolated at each
  knee rate, KV per pod reads 0.695 (8192:256), 0.777 (2048:2048), 0.616
  (256:4096) and 0.754 (blend), V = 1.26, against step 1's V = 1.25 (0.677,
  0.844, 0.795, 0.782). The 0.45 target sits at 0.73, 0.76 and 0.83 of each
  mix's knee rate, where step 1 put it at 0.75, 0.72 and 0.66 (256:4096
  extrapolated). The largest difference is 256:4096, whose knee KV reads 0.616
  here against 0.795.
- *Prefill coupling* (prefill starts cold when long prompts return after a
  shift that sends nothing to prefill) is not exercised: run 3's order sends
  8192:256 first.

**Against step 2** ([`coordinator-kv-step2.md`](coordinator-kv-step2.md)):
the pipeline, actuation and fallback that step 2 checked are outside blis's
model; step 3 adds the control-loop dynamics on the run set's loads.

Detail by run:

- Run 2: decode Ready 1, 1–3, 3, 3–4 and 4 over the stages; prefill 1, 1, 1,
  1–2 and 2 against a need of 1, 1, 1, 1 and 2. Stage 2 lost 60 s (A 0.587 in
  [570, 630) s) before its second decode pod was Ready.
- Run 3: phase 1 decode 1, prefill 1–2 (need 2 at 1.357 req/s of 8192:256);
  phase 2 decode to the cap of 4, released to 3 at 1,545 s; phase 3 decode
  2–3 (headroom need 3).
- Run 4a: at 0.8 × C decode reached the cap at 1,110 s; A per 60 s 0.320,
  0.037, 0.833, 0.878, then 0.990 or more.
- Run 5: KV read at most 0.18 and the prefill trigger at most 295 tok/s; no
  pool moved. The largest prompt was 26,551 tokens; 0 errors.
- Run 8: each spike's added decode pods were Ready at 930, 1,860 and 2,670 s,
  after the spikes ended (855, 1,710, 2,565 s); decode rose to 4 in each quiet
  phase. The last quiet phase lost its first 60 s after spike 3 (A 0.61).
- Run 9: KV proposals in the 67 s spikes reached 2 to 3; the 2 m average does
  not reach a spike's level in 67 s.
- Run 10: opening hold, decode 1 → 4 requested at 60 s, Ready at 180 s;
  prefill 1 → 2 Ready at 150 s; 340 timeouts among requests sent from 65 to
  180 s; A 1.000 from 1,080 s. Steps: 4 for the whole 0.6 × C step (1,020
  replica-s above the need of 3); the need reached 675 s into the 0.3 × C step
  and 435 s into the 0.1 × C step.
- Run 6: 2 prefill and 4 decode pinned held A 1.000 on run 3's schedule for
  32,400 GPU-seconds, against run 3's 20,640.

**For the protocol** (not about the configuration):

- A pool that serves a subset of the requests (P/D with a decider) has its
  knee overstated when A counts every request: prefill's blend knee is 4.369
  req/s by the rule and 3.972 req/s from the requests it serves.
- blis-signal-lab `25f2dfb` predates inference-sim's fix of the P/D TTFT
  projection for preempted decode sub-requests (`dfa8987`, #2237): under
  overload with preemptions it refuses its own Result (run 4b, first attempt).

## Log

- 2026-11-24: lab `25f2dfb` built; scenario rendered with `--dry-run`;
  ScaledObjects read (placeholders resolved, thresholds 7,782, 0.45, 5);
  model resolved from the local catalog; plant, policy and translation
  written.
- 2026-11-24: runs 1a to 1f done, each crossing inside its stages; the
  translation gate passed on run 1a. The knees are final.
- 2026-11-24: runs 2, 3, 4a, 5, 8, 9 and 10 done; run 4b failed in blis
  (Result validation); run 6 done at prefill 2, decode 4.
- 2026-11-24: repeatability gate: run 1a rerun with `blis_run.py --rerun`
  gave the same sha256 for both decision logs (peak RSS 1.32 GiB).
- 2026-11-24: reports and this summary written. Prefill's need uses 3.972
  req/s for the blend (run 1f's prefill-served crossing), not the rule's 4.369.
- 2026-11-24: run 4b's failure traced to two preempted decode sub-requests
  and to inference-sim's later fix (`dfa8987`); run 4b rerun on `25f2dfb` with
  that fix applied. Its decision logs and pod series match the failed attempt's
  byte for byte.
- 2026-11-24, amendment: the fix was described as changing only the preempted
  requests' TTFT. It raises the TTFT of most disaggregated requests: on the
  prefill 8192:256 knee rerun with it
  ([`lab-patch-rerun/`](coordinator-kv-step3/knees/prefill-8192x256/lab-patch-rerun/ttft-shift.txt)),
  1,286 of 1,534 requests by 18.4 ms on
  average (at most 24.3 ms), E2E unchanged, A unchanged in stages 1 to 3, so
  the knee does not move. Run 4b's TTFT and A are on that basis; runs 1 to 10
  are not. Found porting the fix to blis-signal-lab
  (vishakha-ramani/blis-signal-lab#7). The plant row and run 4b's report are
  amended; no other number changes.
