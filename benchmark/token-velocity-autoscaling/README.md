# Token-aware autoscaling on llm-d

Scripts that deploy an llm-d inference stack, measure the one hardware-specific constant the
design needs, arm token-aware KEDA autoscaling, prove the trigger metrics actually reach KEDA,
and benchmark the result — plus staged workloads that exercise the autoscaler end to end.

**Two deployment topologies are covered, sharing the same token-velocity design:**

| Path | Topology | Scripts |
|---|---|---|
| **P/D disaggregation** | prefill and decode on separate pods ([upstream guide](https://github.com/llm-d/llm-d/tree/main/guides/pd-disaggregation)) | [`pd-setup/`](./pd-setup/) |
| **Optimized baseline** | co-located `kv_both`, single `InferencePool`, one Deployment | [`optimized-baseline/`](./optimized-baseline/) |

Shared across both: [`calibrate-peak-prefill.sh`](./calibrate-peak-prefill.sh) (measures the
constant), [`benchmark/run-benchmark.sh`](./benchmark/run-benchmark.sh),
[`workloads/`](./workloads/), and [`cleanup-namespace.sh`](./cleanup-namespace.sh).

The design is described in [TOKEN-AWARE-AUTOSCALING-SUMMARY.md](TOKEN-AWARE-AUTOSCALING-SUMMARY.md).
Read that first — it explains *why* prefill divides tokens by a token rate and decode does
not (§1–§7 for P/D, §8 for the co-located variant), which is the part that makes the thresholds
mean something.

Everything here was run end to end on OpenShift 4.x / k8s 1.32, H100-80GB, Qwen3-32B (P/D at
prefill 1×TP2 + decode 1×TP2; optimized baseline at 1×TP2 co-located).

## Prerequisites

Asserted by the scripts, never installed by them — each fails with the exact command to run
if something is missing.

| Requirement | Why | Check |
|---|---|---|
| `kubectl`, `helm`, `kustomize`, `python3` (+ `pyyaml`), `git`, `envsubst` | client tooling | `pd-setup/deploy-pd-guide.sh` stage 0 |
| GAIE CRDs, `InferencePool` **v1** | the router chart renders an InferencePool | `kubectl get crd inferencepools.inference.networking.k8s.io` |
| Kubernetes **≥ 1.29** | the decode routing sidecar is a native sidecar (initContainer + `restartPolicy: Always`) | `kubectl version` |
| GPUs free **on one node per pod** | tensor parallelism is intra-node: a TP=N pod needs N free GPUs on a single node | `pd-setup/deploy-pd-guide.sh` bin-packs and reports |
| KEDA / Custom Metrics Autoscaler | runs the ScaledObjects | `kubectl get crd scaledobjects.keda.sh` |
| Prometheus user-workload monitoring + Thanos Querier | where the trigger queries run | `kubectl get pods -n openshift-user-workload-monitoring` |
| Permission to create a `ClusterRoleBinding` | KEDA→Thanos needs a `cluster-monitoring-view` binding | `kubectl auth can-i create clusterrolebinding` |
| `llmdbenchmark` CLI + the `llm-d-benchmark` checkout | only for `benchmark/run-benchmark.sh`; also supplies the `config/` and `workload/` trees | `llmdbenchmark --version` |
| `oc` (OpenShift CLI) | `pd-setup/launch-scaledobjects.sh` uses it throughout; the Thanos/`service-ca` auth path is OpenShift-specific | `oc whoami` |

Install the benchmark CLI with:

```bash
curl -sSL https://raw.githubusercontent.com/llm-d/llm-d-benchmark/main/install.sh | bash
cd llm-d-benchmark && source .venv/bin/activate
```

`LLMD_DIR` controls where the llm-d checkout lives. By default the scripts use `./llm-d`,
falling back to `../llm-d` if that already exists, and cloning if neither does. Point it
anywhere: `LLMD_DIR=/path/to/llm-d ./pd-setup/deploy-pd-guide.sh`.

## Run order (P/D disaggregation)

For the co-located path, see [Optimized baseline quick-start](#optimized-baseline-quick-start-pd-test-namespace) below.

```bash
# 1. deploy the upstream guide (clean namespace -> router -> model servers -> verify)
./pd-setup/deploy-pd-guide.sh               # ~6 min; --render-only needs no cluster

# 2. enable monitoring — REQUIRED before step 4, see "Monitoring" below

# 3. measure peakPrefillThroughput on YOUR hardware, and apply it to the EPP
./calibrate-peak-prefill.sh                  # measure only
./calibrate-peak-prefill.sh --apply          # measure, patch the EPP, verify it took

# 4. arm the two ScaledObjects
./pd-setup/launch-scaledobjects.sh --vp <measured> --max 4

# 5. prove the metrics reach KEDA
./pd-setup/test-metric-flow.sh              # read-only health check
./pd-setup/test-metric-flow.sh --probe 180  # drive load, prove the values MOVE (may scale)

# 6. benchmark, or run the staged autoscaling experiment
./benchmark/run-benchmark.sh                                              # guide's latency profile
./benchmark/run-benchmark.sh --workload-file workloads/pd-autoscaling-ramp.yaml   # 57-min staged ramp
./benchmark/run-benchmark.sh --workload-file workloads/pd-autoscaling-ramp.yaml --pause-autoscaling
                                                                # same load, fixed fleet (baseline)
```

Teardown: `./pd-setup/deploy-pd-guide.sh --teardown` (releases the GPUs) and
`./pd-setup/launch-scaledobjects.sh --delete`.

## The scripts

### `pd-setup/deploy-pd-guide.sh`
Deploys the guide by its own path — `helm install llm-d-router-standalone` plus a kustomize
overlay — not through llm-d-benchmark. Eight stage-gated stages, each asserting a
postcondition rather than trusting an exit code. Verifies disaggregation actually happened
by finding the decode pod's IP in the prefill pod's access log, because a P/D stack that
quietly serves everything from decode passes a plain `curl` check and is still broken.

Modes: `--render-only` (no cluster), `--dry-run`, `--verify-only`, `--teardown`,
`--namespace`, `--ref`. Topology via `MODEL PREFILL_REPLICAS PREFILL_TP DECODE_REPLICAS DECODE_TP`.

### `calibrate-peak-prefill.sh`
Wraps upstream `guides/recipes/router/calibration/calibrate.sh` unmodified, adding the
guards whose absence makes its output silently wrong: `CHUNK_SIZE` verified against the
effective `--max-num-batched-tokens` read from each prefill pod's own startup log; an idle
gate before and after (queue wait inside TTFT understates throughput); repeats with the
run-to-run spread reported. `--apply` rewrites `peakPrefillThroughput` (which lives inside a
YAML string no `--set` can reach), upgrades, restarts the EPP, and re-reads the live ConfigMap
to confirm. Shared by both paths — set `NAMESPACE` and `GUIDE_NAME` to target the co-located
stack (`GUIDE_NAME=optimized-baseline`).

### `pd-setup/launch-scaledobjects.sh`
Creates the KEDA auth chain (metrics-reader SA + `cluster-monitoring-view` binding + token
Secret + `TriggerAuthentication`) and the two ScaledObjects from the summary's §4.
Deployment and InferencePool names are **discovered**, not hardcoded. `--decode-signal`
selects the summary's occupancy trigger (default) or the refused-admission variant; the
default threshold follows the signal because they are different units.

### `pd-setup/test-metric-flow.sh`
Reads the queries **out of the live ScaledObjects** rather than keeping its own copy, and
queries Thanos with the **metrics-reader ServiceAccount's own token** — the exact credential
KEDA uses. Both choices are deliberate: a test with its own copy of the PromQL passes while
KEDA runs something else, and a test using your `oc whoami -t` succeeds where the SA may
not. `--probe N` drives load and writes a timeline CSV proving the values move.

### `benchmark/run-benchmark.sh`
Benchmarks the already-deployed stack with `llm-d-benchmark`, **run-only** — it never calls
`standup`/`teardown`, so it cannot redeploy or disturb the stack. Follows the guide's
documented path (`--endpoint-url` + `--gateway-class epponly`; without the latter the CLI
re-renders against the scenario's default topology and measures something else). Results land
in `./benchmark-results/<timestamp>/` with a `latest` symlink — never `~/data`.

Adds around the CLI: preflight (all pods Ready, endpoint answers `/v1/models`, model read from
the live Deployment); a replica + trigger-value timeline in `autoscaling-timeline.csv`;
`--pause-autoscaling` for a fixed-topology baseline, restored on exit even on Ctrl-C; EPP
counter snapshots asserting `llm_d_epp_disagg_decision_total` actually rose; and it raises
`--wait-timeout` to cover a long profile's own duration, since a harness killed mid-ramp
returns partial results that look complete.

### `workloads/`
`workloads/pd-autoscaling-ramp.yaml` is a 7-stage ramp (rate 0.25 → 1.5 → 0.25, ISL/OSL 2048,
3420 s) built to cross both thresholds and then recover, so one run shows scale-up, the cap,
and scale-down. Traffic changes *within* a run via `load.stages` — the standard upstream
pattern. Pass it with `--workload-file`.

`workloads/pd-autoscaling-ramp-prefill-heavy.yaml` is the prefill-dominated sibling: same
7-stage shape but **ISL 8192 / OSL 256 (32:1)** and rates rescaled to the larger per-request
prefill cost (0.15 → 1.4 → 0.15). It exercises the *prefill* trigger specifically — per-replica
saturation is only `V_P / ISL ≈ 0.33 req/s`, so prefill drives to the cap early while decode
compute stays idle.

`workloads/pd-autoscaling-ramp-decode-heavy.yaml` is the mirror image: **ISL 256 / OSL 8192
(1:32)** and rates 0.10 → 0.80 → 0.10. Each request does trivial prefill and a long generation,
so it exercises the *decode* trigger (KV-cache occupancy) while prefill stays idle. Sizing is
anchored to the measured decode KV cache (330,752 tokens/replica), so the ramp walks concurrency
up past the single-replica KV ceiling and back. Because generations are long, set a
`request_timeout` above `OSL × steady-ITL` — see the bite below.

**Reading the analysis charts through `peakPrefillThroughput` (V_P).** The
`throughput_vs_qps` / `latency_vs_qps` charts `run-benchmark.sh` emits are the same thing V_P
predicts up front. Saturation for one prefill replica is at `QPS_sat = V_P / ISL` (e.g. at
V_P = 2696 with ISL 2048, ≈ **1.32 req/s**): stages at or below that rate hold TTFT low, and the
first stage above it is where the latency chart's TTFT knee appears and the throughput chart
flattens toward the per-replica ceiling (~V_P input tok/s). The blowup is all *time-to-first-token*
(ITL barely moves), i.e. prefill queue — which is precisely what the trigger's
`inflight_tokens / V_P` measures. So the practical read is: keep offered load per replica below
`V_P / ISL`, and pre-provision headroom so a rate step does not outrun scale-up + weight-load lag
(a one-off large stage-average TTFT is transition cost, not steady state).

## Monitoring is required, and is not part of the deploy

The guide treats monitoring as optional, but the ScaledObjects cannot work without it. The
EPP metrics endpoint answers **401** until it is enabled.

```bash
helm upgrade pd-disaggregation oci://ghcr.io/llm-d/charts/llm-d-router-standalone --version v0 \
  -f ${LLMD_DIR}/guides/recipes/router/base.values.yaml \
  -f ${LLMD_DIR}/guides/pd-disaggregation/router/pd-disaggregation.values.yaml \
  -f ${LLMD_DIR}/guides/recipes/router/features/monitoring.values.yaml \
  -n <namespace> --wait
kubectl apply -n <namespace> -k ${LLMD_DIR}/guides/recipes/modelserver/components/monitoring-pd
```

**If you have already run `calibrate-peak-prefill.sh --apply`**, add its override as a final
`-f` or this upgrade silently reverts `peakPrefillThroughput` to the guide's shipped value:

```bash
  -f .pd-guide-workspace/calibration/router-calibrated.values.yaml \
```

That file is generated (and git-ignored), so it exists only after a calibration run — on a
fresh clone there is nothing to add yet.

## Things that will bite you

**`peakPrefillThroughput` depends on which path you measure, and upstream's two published
values are not comparable.** Measured here: **15965 tok/s** against the prefill pod directly
(matching upstream's configuration-matrix value of 15928 to 0.2%), but **2619 tok/s** through
the full P/D path — because 84% of TTFT was the NIXL KV transfer, moving 2.00 GiB per
8192-token request (256 KiB/token for Qwen3-32B) at ~6.6 Gbps over TCP. Confirmed
bandwidth-bound by re-measuring at chunk 2048: predicted 0.65s, measured 0.666s. The
`pd-disaggregation` guide ships **33821**, measured on H200/gpt-oss-120b with a fast fabric.
Measure your own, and know which number you are holding. If RDMA works on your fabric, the
P/D figure moves a long way up.

**The denominator sets the trigger's aggressiveness, not just its units.** At V_P=2665 an
observed 26.9s backlog asks for `ceil(26.9/1.5)` = 18 replicas; the same load at 15928 reads
4.5s and asks for 3.

**`llm_d_epp_inflight_tokens` is registered lazily on the first dispatched request.** A
freshly restarted EPP has no such series, and the query's `or vector(0)` renders that as a
confident zero — indistinguishable from "no backlog". Send traffic before trusting the
trigger. `pd-setup/test-metric-flow.sh` reports *absent* separately from *zero* for this reason.

**The prefill trigger flaps under steady load.** `llm_d_epp_inflight_tokens` is an
*instantaneous* gauge and the query does no time-averaging, so at `pollingInterval: 15` most
samples read exactly 0 — a prefill leg occupies only ~2 s of a request's life. Under a rising
ramp this can retire a replica *while load is still climbing* ("All metrics below target"), then
ask for it back seconds later. Consider `avg_over_time(...[1m])` around the numerator.

**`maxReplicaCount: 10`** (the summary's value) is 10 × TP GPUs per role. At TP=2 that is 40
GPUs across both roles. Use `--max` to match your fleet.

**Long-OSL workloads hit the harness's 300 s request timeout and get counted as failures.**
inference-perf defaults to `request_timeout: null` (a 300 s library default). A single OSL-8192
generation already needs `OSL × steady-ITL ≈ 8192 × 28 ms ≈ 229 s`, so once decode is contended the
total slips past 300 s and the client aborts a request that was still generating — 18% of the
decode-heavy run "failed" this way while the server was fine. Set `request_timeout` above
`OSL × steady-ITL` (e.g. 600 s) for long generations, or decode saturation is measured as failure
instead of latency. (This is the decode analogue of prefill's queue: decode backpressure is
memory/stream, so it surfaces as timeouts, not a backlog that eventually drains.)

**On OpenShift, Thanos `:9091` answers unauthenticated queries with 401 — and KEDA
suppresses that error and serves `fallback` replicas**, so a broken trigger looks healthy.
This is why the auth chain exists and why the verifier tests it with the SA's own token.

**The upstream guide provisions no model PVC**, so by default every pod downloads its own
copy of the weights (~65 GB for Qwen3-32B) to node ephemeral storage through the `emptyDir`
at `/.cache`. `deploy-pd-guide.sh --model-cache` fixes this the way `llm-d-benchmark` does: a
shared ReadWriteMany PVC, populated once by a Job, mounted read-only into every prefill/decode
pod (`vllm serve` is pointed at the local path and paired with `--served-model-name` so
client requests are unaffected). It is opt-in and scoped to `$NAMESPACE` — a full clean
run (the default) still deletes the namespace and the PVC with it; pass `--skip-clean`
to reuse a populated cache across reruns.

## Optimized baseline quick-start (pd-test namespace)

The **co-located** topology: prefill and decode run together on one model-server Deployment
(`kv_role: kv_both`) behind a single `InferencePool` — *not* P/D disaggregation. One KEDA
`ScaledObject` carries both token-aware triggers and scales the one Deployment to the max of the
two (see [summary §8](TOKEN-AWARE-AUTOSCALING-SUMMARY.md)). The
[`optimized-baseline/`](./optimized-baseline/) directory holds scripts to deploy, calibrate,
monitor, and optionally autoscale:

```bash
# 1. Deploy the stack
export HF_TOKEN="your_token"                               # optional for ungated models
./optimized-baseline/deploy-optimized-baseline.sh

# 2. Test metrics and health
./optimized-baseline/test-metrics.sh
./optimized-baseline/test-metrics.sh --probe 60            # with load

# 3. Calibrate peakPrefillThroughput (REQUIRED for autoscaling)
NAMESPACE=pd-test GUIDE_NAME=optimized-baseline ./calibrate-peak-prefill.sh --apply

# 4. (Optional) Enable token-aware autoscaling with KEDA (single ScaledObject, both triggers)
./optimized-baseline/launch-scaledobject.sh                # discovers V_P from the EPP ConfigMap
./optimized-baseline/launch-scaledobject.sh --max 8 --vp 2665  # customize

# 5. (Cleanup) Remove all resources from pd-test namespace (preserves the model PVC)
./cleanup-namespace.sh
```

**Shared scripts** (work for both P/D disaggregation and optimized-baseline):
- `./calibrate-peak-prefill.sh` — measure peakPrefillThroughput (set `NAMESPACE` + `GUIDE_NAME` env vars)
- `./cleanup-namespace.sh` — remove all resources from a namespace (set `NAMESPACE` env var)

For options and detailed configuration, see script help: `./optimized-baseline/deploy-optimized-baseline.sh --help`.

## Upstream gaps found while testing this (unreported as of 2026-08-18, llm-d @ main)

1. **`kubectl apply -k modelserver/gpu/vllm/base` cannot work.** It renders
   `image: REPLACE_MODEL_SERVER_IMAGE`, which the API server rejects, yet the README prints
   it with `INFRA_PROVIDER=base` as the default. `base/kustomization.yaml` omits the image
   component deliberately and every sibling overlay (`coreweave`, `aws`, `gke`) adds one;
   there is no generic or OCP overlay. `deploy-pd-guide.sh` generates the missing overlay
   and re-proves the gap on every run, so it will tell you when upstream fixes it.
2. **`calibrate.sh` is tracked as mode `100644`** while its sibling
   `calibrate-min-cached-token-delta.sh` is `100755`, so the README's documented
   `./calibrate.sh` fails with "permission denied" on a fresh clone.
   `calibrate-peak-prefill.sh` invokes it via `bash`.
