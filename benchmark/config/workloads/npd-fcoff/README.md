# npd-fcoff: the research GPU benchmark on llm-d-benchmark

This directory reproduces the **npd-fcoff** GPU evaluation from
`llm-d-autoscaling-research` (task 0426, group `fc-off-no-pd`; findings in
`findings/epp-work-rate-gpu.md` § npd-fcoff) with standard llm-d-benchmark
tooling instead of the research repo's `run.py` / `loadgen.py` / aiperf harness.

| piece | file |
|---|---|
| scaling law (variant) | [`scenarios/staging/optimized-baseline/npd-fcoff.yaml`](../../scenarios/staging/optimized-baseline/npd-fcoff.yaml) |
| static-fleet control | [`scenarios/staging/optimized-baseline/baseline.yaml`](../../scenarios/staging/optimized-baseline/baseline.yaml) |
| spec entrypoints | [`specification/staging/optimized-baseline/`](../../specification/staging/optimized-baseline/) |
| GPU plant overlay | [`cluster-configs/ocp/vllm-npd.yaml`](../../cluster-configs/ocp/vllm-npd.yaml) |
| kind plant overlay | [`cluster-configs/k8s/model-sim-qwen3-8b-npd.yaml`](../../cluster-configs/k8s/model-sim-qwen3-8b-npd.yaml) |
| inference-perf workloads | this directory (`*.yaml.in`) |
| request traces | [`hack/npd-fcoff/gen_traces.py`](../../../hack/npd-fcoff/gen_traces.py), staged by [`stage-traces.sh`](../../../hack/npd-fcoff/stage-traces.sh) |
| analysis | [`hack/npd-fcoff/analyze.py`](../../../hack/npd-fcoff/analyze.py) |

## The plant and the law

The research plant is one vLLM Deployment (no P/D) behind the llm-d-router
EPP + Envoy with default plugins and **flow control off**, scraped by
Prometheus, scaled by KEDA. That is the upstream `optimized-baseline` topology,
so the scenario is a staging variant of that guide (`modelservice`,
`gateway.className: epponly`, prefill disabled, no routing sidecar).

The ScaledObject carries the research law
(`experiments/epp-input-rate/manifests/laws/npd-fcoff.yaml`) unchanged: the
same PromQL, `metricType: AverageValue`, `activationThreshold: "0"`, the same
HPA behavior (+1 pod / 180 s up; −1 pod / 300 s down after a 300 s
stabilization window), and KEDA's default 30 s polling. Only the law's
`envsubst` variables become config references:

| research (`constants-gpu.env`) | here | GPU value | kind value |
|---|---|---|---|
| `${NS}` | `${namespace.name}` | — | — |
| `${THRESHOLD_WORK}` | `npdFcoff.thresholdWork` | 14619 (0.70 × knee ν_max 20,884) | 9000 (`constants-kind.env`) |
| `${C_W}` | `npdFcoff.cW` | 3.3 | 3.3 |
| `${LP_DEF}` / `${LOUT_DEF}` | `npdFcoff.lpDef` / `loutDef` | 8192 / 128 | 8192 / 128 |
| `${MIN_REPLICAS}` / `${MAX_REPLICAS}` | `npdFcoff.minReplicas` / `maxReplicas` | 1 / 6 | 1 / 3 |

Plant constants (threshold, cap) live in the backend overlay; the law and its
GPU defaults live in the scenario. Override any of them per run with
`--set npdFcoff.<key>=<value>`.

## Workload mapping

The research client was an open-loop Poisson generator with a
piecewise-constant rate (`--schedule`) and a time-varying prompt:output mix
(`--mix`). inference-perf's `poisson` load type cannot change the mix
mid-run, and it drains in-flight requests at every stage boundary, so a
multi-stage profile is not the same arrival process. Its `trace_replay` load
type is: it sends one request per CSV row at the row's offset with the row's
prompt and output length, without pausing between rows. `gen_traces.py` writes
those CSVs with `loadgen.py`'s own sampling code (same RNG and call order). For
the task-0426 runs it reproduces the research client exactly: same request
count, arrival times within 0.6 µs, identical lengths. This was checked against
the committed `raw/synth-mix`, `raw/synth-over2` and `raw/synth-stair-r3` logs.

| research run | workload | trace (GPU / kind) | expresses |
|---|---|---|---|
| `raw/knee` — fixed 1 replica, 1→6 req/s of 8192:128, 300 s stages | `knee.yaml.in` on `baseline` with `npdFcoff.staticReplicas=1` | `gpu-knee` / — | C_P at the A = 0.95 TTFT ≤ 1 s crossing → `thresholdWork = 0.70 × ν_max` |
| `raw/synth-stair-r3` — law active, 0.8→4.0 req/s | `stair.yaml.in` | `gpu-stair` / `kind-stair` | replicas step at ⌈ν/threshold⌉; trigger tracks offered ν |
| `raw/synth-mix` — ν ≈ 42.2k at 4.9 then 23.22 req/s (8192:128 → 128:512) | `mixshift.yaml.in` | `gpu-mixshift` / `kind-mixshift` (research kind `ms2`) | replicas hold steady across a 4.74× req/s swing |
| `raw/baseline-mix` — same, static 4 replicas | `mixshift.yaml.in` on `baseline` | `gpu-mixshift` | GPU-seconds and A vs a fixed fleet |
| `raw/synth-over` / `synth-over2` — 1.2× then 2.5× / 6.6× C_P | `overload.yaml.in` | `gpu-overload`, `gpu-overload-hard` / `kind-overload` | trigger keeps rising past the cap; FC-off failures are client timeouts |
| kind `npd-fcoff-r1` bootstrap — 0 replicas, min-0 SO, 1 req/s | `bootstrap.yaml.in` with `npdFcoff.minReplicas=0` | `gpu-bootstrap` / `kind-bootstrap` | constant fallback keeps the trigger live at zero pods; KEDA 0 → 1 |
| `raw/agentic-r2` — AgentX-shaped sessions, 0.5 → 1.5 req/s | `agentic.yaml.in` (`conversation_replay`) | generated in the harness | vLLM running-ratio means vs the 8192/128 fallback |

Client settings carried over: streaming `/v1/completions`, `ignore_eos`,
random token content (no prefix sharing), 60 s request timeout, 240 s drain.
Attainment A uses the research definition (TTFT ≤ 1 s, failures count as
misses) and is also reported by inference-perf as goodput.

### What llm-d-benchmark cannot express, and what stands in

| research feature | gap | closest equivalent here |
|---|---|---|
| open-loop Poisson with a rate *and mix* schedule | `poisson` stages drain between stages and carry one mix | `trace_replay` of a pre-generated trace (exact for task 0426's schedules) |
| `loadgen.py --prefix-pool N:P` (shared prefixes; the pd-* agentic runs) | `random` data has no prefix pool; `shared_prefix` is closed-form (fixed groups × questions) and not schedulable | `data.type: shared_prefix` on a `poisson` stage for a steady-state check; the npd-fcoff GPU run itself did not use `--prefix-pool` |
| AgentX-shaped agentic sessions via aiperf (`gen_agentic.py`, 1:8 context scale, subagent fan-out) | no AgentX replay; `synthetic_agentic` needs the chat API with tool calls | `conversation_replay`: shared 4k system prompt + growing per-session history, lognormal in/out lengths, tool pauses; no fan-out, and the 0.5 → 1.5 req/s step drains at the boundary |
| per-phase ScaledObject constants (min-0 bootstrap, static baseline, knee pin) | KEDA config is set at standup, not per workload; `update` does not map `npdFcoff.*` to the keda component | one standup per arm: `--set npdFcoff.minReplicas=0`, the `baseline` spec, or `--set npdFcoff.staticReplicas=1`; or `kubectl patch` the live ScaledObject between runs |
| recorder (1 s Deployment/HPA/pod state) and Prometheus export inside `run.py` | the harness does not record cluster state | `analyze.py run` reads the same series from Prometheus afterwards (trigger, D1, D15, L̄_p, L̄_out, HPA desired/current, ready replicas, prefix-cache hit rate) |
| GPU-seconds from DCGM or the recorder | — | ∫ ready replicas dt from kube-state-metrics (`replica_seconds`) |

## Running it

Prerequisites are in [`benchmark/README.md`](../../../README.md): the sibling
`../llm-d-benchmark` clone with its venv, Prometheus (kube-prometheus-stack in
`monitoring`) and KEDA ≥ 2.20.

```bash
SPEC=benchmark/config/specification/staging/optimized-baseline/npd-fcoff.yaml.j2
CC=benchmark/config/cluster-configs/k8s/model-sim-qwen3-8b-npd.yaml   # or ocp/vllm-npd.yaml on GPUs
NS=npdfc
COMMON="--spec $SPEC --workspace benchmark/results"

llmdbenchmark $COMMON standup --cluster-config $CC -p $NS
benchmark/hack/npd-fcoff/stage-traces.sh $NS kind        # or gpu
kubectl -n $NS get scaledobject,hpa                      # READY=True, TARGETS not <unknown>

llmdbenchmark $COMMON run --cluster-config $CC -p $NS -l inference-perf \
  --workload-file-path benchmark/config/workloads/npd-fcoff/mixshift.yaml.in \
  --dataset /requests/datasets/kind-mixshift.csv --wait-timeout 3600

benchmark/hack/npd-fcoff/analyze.py run benchmark/results/<session>/results/<experiment>_1 \
  --trace <trace dir>/kind-mixshift.csv --schedule 0:2.0,600:9.4773 \
  --namespace $NS --threshold 9000 --kube-context <ctx>   # or --prom http://...
```

`gen_traces.py --list` prints each trace's schedule (pass it to `--schedule`
for per-segment rows); `gen_traces.py --out-dir <dir>` regenerates the CSVs
locally for `--trace`. Between runs, return the Deployment to one replica (or
wait out the 300 s scale-down windows) so each run starts from the same state.

The knee, on a fresh plant:

```bash
llmdbenchmark --spec .../optimized-baseline/baseline.yaml.j2 --workspace benchmark/results \
  standup --cluster-config $CC -p $NS --set npdFcoff.staticReplicas=1
# run knee.yaml.in with --dataset /requests/datasets/gpu-knee.csv, then
benchmark/hack/npd-fcoff/analyze.py knee <experiment dir> --trace gpu-knee.csv
# -> threshold_work_0p7; use it as --set npdFcoff.thresholdWork=<value> on the law arm
```

## Staging validation (kind)

Run on a 3-node kind cluster (Apple silicon, arm64) with the
`k8s/model-sim-qwen3-8b-npd.yaml` overlay (threshold 9000, max 3 replicas),
2026-11-05: one standup, then one `llmdbenchmark run` per workload, each
starting from one ready replica (zero for bootstrap). The numbers come from
`analyze.py run`. The session directories are under `benchmark/results/`
(`villardl-20261105-*`).

| workload | segment | offered req/s · mix | offered ν | trigger median (÷ offered) | ⌈ν/9000⌉ | ready replicas | A | errors |
|---|---|---|---|---|---|---|---|---|
| `kind-stair` | 0–300 s | 1.07 · 8192:128 | 9,217 | 9,409 (1.02) | 2 | 1 → 2 | 0.997 | 0 |
| | 300–600 s | 2.18 · 8192:128 | 18,808 | 19,486 (1.04) | 3 | 2 → 3 | 0.998 | 0 |
| | 600–900 s | 2.76 · 8192:128 | 23,800 | 23,339 (0.98) | 3 | 3 | 1.000 | 0 |
| `kind-mixshift` | 0–600 s | 1.94 · 8192:128 | 16,712 | 16,927 (1.01) | 2 | 1 → 2 → 3 | 0.999 | 0 |
| | 600–1200 s | 9.52 · 128:512 | 17,301 | 17,163 (0.99) | 2 | 3 | 1.000 | 0 |
| `kind-overload` | 0–300 s | 2.14 · 8192:128 | 18,464 | 17,080 (0.93) | 3 | 1 → 2 | 0.978 | 0 |
| | 300–900 s | 12.18 · 8192:128 | 104,890 | 105,632 (1.01) | 12 | 2 → 3 (cap) | 0.001 | 14.6 % (client timeouts) |
| `kind-bootstrap` (min 0) | 0–420 s | 0.99 · 8192:128 | 8,513 | see below | 1 | 0 → 1 → 2 | 0.915 | 8.5 % (503 before the first pod) |
| `agentic` | 0–600 s | 0.65 · 8681:527 (mean) | 6,790 | 6,328 (0.93) | 1 | 1 | 1.000 | 0 |
| | 600–1775 s | 1.45 · 9869:504 (mean) | 16,768 | 17,799 (1.06) | 2 | 1 → 2 → 3 | 0.996 | 0 |
| `kind-mixshift`, static 3 (control) | 0–600 s | 1.94 · 8192:128 | 16,712 | 16,915 (1.01) | — | 3 | 1.000 | 0 |
| | 600–1200 s | 9.52 · 128:512 | 17,301 | 17,188 (0.99) | — | 3 | 1.000 | 0 |

- **Signal-level equivalence holds.** The rendered trigger read the client's
  offered work rate within ±4 % in every segment. Per 60 s bin the
  trigger/offered ratio had a median of 1.03 (staircase) and 1.02 (mix-shift),
  with p10–p90 of 0.93–1.25 and 0.97–1.10; the wide bins are the 1 m rate
  windows lagging a step. The vLLM running-ratio means recovered the true mix
  on both sides of the shift (L̄_p 8192 → 128, L̄_out 128 → 512.05).
- **KEDA reacts as the law prescribes.** The HPA stepped at the predicted
  points, +1 pod per 180 s policy period. In the staircase: desired 2 at
  t = 250 s, desired 3 at t = 460 s.
- **Mix-shift holds replicas.** Across a 4.9× req/s swing at constant ν the
  trigger moved 1 %, and replicas stayed at 3. The research kind run's req/s
  counterfactual (threshold 2.0 req/s per replica, `ms2`) would have asked for
  5 in the second phase. The run settled at 3 rather than ⌈ν/threshold⌉ = 2
  because ν/9000 = 1.86 sits close to 2: Poisson noise in phase 1 pushed the
  trigger above 2 × 9000 near t = 300 s (desired 3 at t = 375 s), and the 300 s
  scale-down window never saw a full window below 18k afterwards. The research
  GPU run showed the same pattern (a transient +1 held for ~300 s).
- **Overload: the trigger keeps rising.** At 12 req/s, about 4.5× what
  three simulated replicas serve, the trigger read 1.01× the offered ν
  (median 105.6k, asking for 12 replicas against a cap of 3). It neither read
  0 nor flattened. Spikes reached 278k because L̄_p overshoots to 21.9k while
  the success rate collapses: the served-side ratio bias the research run
  measured, here with the same over-asking direction. Failures were all client
  timeouts at 60 s, and D15 stayed 0, the FC-off failure mode of
  `raw/synth-over2`.
- **Bootstrap from zero.** With `minReplicaCount: 0` and the Deployment at 0,
  the empty pool answered the client's POSTs with EPP `ServiceUnavailable`
  (D15 up to 0.55 /s). The trigger went non-zero 10 s into the run,
  reading λ × (8192 + 3.3 × 128) through the `or vector()` fallbacks, since
  no vLLM series existed. The HPA asked for 1 replica at t = 30 s, which was
  Ready at t = 60 s, and 2 at t = 120 s, when the 1 req/s load plus the
  fallback crossed 9000. The 35 failed requests were the 503s before the first
  pod. The research kind run (`npd-fcoff-r1`) took 12 s to a non-zero trigger
  and 30 s to scale up.
- **Agentic: the running-ratio means carry a real mix.** With growing
  multi-turn contexts (mean prompt 9.7k tokens, 4k shared system prompt) the
  vLLM ratio means read L̄_p 10.8k / 12.0k and L̄_out 333 / 362. Those are far
  from the 8192 / 128 fallback, and the trigger still tracked the client's
  offered ν (median bin ratio 0.99, p10–p90 0.81–1.15). L̄_p runs above the
  client mean because long-context turns are a larger share of what completes
  in a window. That is the served-side bias the research run documented
  (GPU: L̄_p 18.3k, L̄_out 360). Prefix-cache hit rate was 0.39–0.42.
  Bucketed TTFT rose from 34 ms (prompts < 2k) to 667 ms (25k+), the same
  length dependence as the research findings §5.
- **Efficiency vs a static fleet** (the research `raw/baseline-mix` check).
  The same mix-shift trace against 3 pinned replicas used 3,585
  replica-seconds; the autoscaled run used 3,075 (14 % fewer). The
  autoscaled run's only attainment cost was the ramp: phase-1 A 0.999 against
  1.000, TTFT p90 0.43 s against 0.32 s. The research GPU run measured 26 %
  fewer GPU-seconds at a larger ramp cost (A 0.795 against 0.990), because an
  H100 replica cold-starts in ~95 s and a simulated one in ~30 s.
- D15 stayed 0 in every run with a ready endpoint. With flow control off, the
  EPP sheds only when the pool is empty.

Three things to know about the bootstrap arm:

- `llmdbenchmark run` first checks `/v1/models` through the EPP and aborts
  when no pod exists. Run the arm with
  `--endpoint-url http://<stack>-router-epp.<ns>.svc.cluster.local:80 --model Qwen/Qwen3-8B`,
  which makes that check non-fatal.
- The check's bodiless GET takes the EPP's random-endpoint path, which fails
  with `Internal - no pods available in datastore`. That error is not D15, so
  probes never wake the pool, which is the correct behavior. Only real POSTs
  reach the director and count as `ServiceUnavailable`.
- `llmdbenchmark update` cannot re-apply `npdFcoff.*` (it maps overrides to
  components by top-level key). Either stand up with
  `--set npdFcoff.minReplicas=0`, or patch the live ScaledObject's
  `minReplicaCount` and scale the Deployment to 0, as this validation did.

## What differs from the GPU benchmark

The check is signal-level equivalence, not identical absolute numbers.

- **Capacity.** The kind replica is vLLM v0.31.0's real scheduler with a
  simulated forward pass (C1's H100 step-time fit). The research repo measured
  its knee at 3.26 req/s of 8192:128 (26.7k work tok/s), against 2.42 req/s
  (20.9k) on the H100. Thresholds, rates and replica counts from one plant do
  not transfer to the other. The kind threshold (9000) is the research kind
  placeholder, not a knee measurement.
- **Latency.** TTFT and ITL on kind come from the latency model, not from
  hardware. A and the TTFT percentiles validate the harness plumbing, not the
  SLO. The GPU SLO claims (A ≥ 0.994 on the staircase, 26 % fewer
  GPU-seconds than static-4) need the `ocp/vllm-npd.yaml` run.
- **Model server.** The research GPU plant ran upstream `vllm/vllm-openai`
  v0.31.0. The GPU overlay uses `ghcr.io/llm-d/llm-d-cuda` (`tag: auto`).
  Re-measure the knee if the vLLM version differs.
- **EPP.** v0.11.0 on both, with default plugins and flow control off.
  The router chart's ServiceMonitor scrapes the EPP every 5 s; the research
  plant used a namespace Prometheus with 5 s scrapes of vLLM, Envoy and EPP.
- **Client.** The arrival process and lengths are identical; the HTTP client
  is not (aiohttp in `loadgen.py`, inference-perf's worker pool here). Prompts
  are random text tokenized to the requested length, not raw token-id lists.
- **Agentic.** `conversation_replay` is a different session generator from
  the research aiperf/AgentX workload (see the table above). Compare the
  L̄_p / L̄_out behavior, not absolute session counts.
