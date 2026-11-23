# Step 2: Verification with a simulator: coord-disaggregation/coordinator-kv

Step 2 of the [evaluation protocol](../../2-simulator-verification.md). It
checks, on a real cluster with a CPU simulator in place of the GPUs, the parts
of the step 1 verdict [`coordinator-kv.md`](coordinator-kv.md) that the blis
runs cannot reach: the metrics pipeline (scrape, selectors, duplicate series),
KEDA's actuation and fallback through the `DisaggregatedSetRoleScaler`, the
Coordinator's conditional-decode path, and the needs-measurement items that
depend on the real EPP. The control-loop dynamics (P1/P2 convexity, P3 by mix,
P8 lag) were measured in blis for step 1 and are not repeated here. No capacity
number is claimed: the simulator profile is Qwen3-32B on H100, not this
guide's model.

| | |
|---|---|
| Scenario | `benchmark/config/scenarios/staging/coord-disaggregation/coordinator-kv.yaml`, with three run-only changes: `kustomize.overlayPath` = [`coordinator-kv-step2/sim-overlay/`](coordinator-kv-step2/sim-overlay/), `keda.prometheus` = the OpenShift Thanos querier (9091, `bearer-secret`, `unsafeSsl: true`), `maxReplicas: 2` per role |
| llm-d | `main` (`a0f3cb6c`) with llm-d/llm-d#3111 and #3114 merged locally, plus `nonCachedTokens: 1024` in the PD-only router values (the simulator runs `--max-model-len 8192`, so at 8,192 nothing would reach prefill) |
| llm-d-benchmark | `main` (`61e9e47`), `standup -t kustomize` |
| Simulator | `vllm-model-sim`: `docker.io/vllm/vllm-openai-cpu@sha256:495c84fe30f3572b83a78100a1e8ba423dbb77fc532ecefebe114e6b77327bd9` (tag `v0.31.0`), plugin `ghcr.io/lionelvillard/vllm-sim-deps@sha256:b8cae7559367ec73c4ccaea60df188fadb9efb792deed995da65213aa87bc2b7` (tag `v0.3.0`) with `nixl_sim.py` from `llm-d-extensions/vllm-model-sim@71f74c4` mounted over it (the KV-leak fix); `SimulatedNixlConnector`, producer on prefill, consumer on decode |
| Router | the guide's single EPP, `ghcr.io/llm-d/llm-d-router-endpoint-picker:main` (floating; the digest was not recorded before teardown); Coordinator `v1.0.0`, `coordinator/pd-only`; Gateway `istio` |
| Cluster | OpenShift `4.19.19`, Kubernetes `v1.32.9`, KEDA `2.19` (`custom-metrics-autoscaler`), LWS `v1.0.0` |
| Namespace | `step2-coordkv-20261123` (torn down after the run, with its cluster-scoped bindings) |
| Load | [`loadgen.py`](coordinator-kv-step2/loadgen.py), Poisson through the Gateway, shared 2,048-word prefixes: steady 1 req/s, half misses (3,072-word suffix, disaggregated) and half hits (512-word suffix, served by decode), 128 output tokens, 600 s; then 0.4 req/s of hits with 1,024 output tokens, 900 s |
| Date | 2026-11-23 |
| Result | every checked condition **holds**; needs-measurement item 5 is **settled**; P7 detection time measured |

The guide's model servers run vLLM `v0.31.0` (the shared recipe image
component), not the `v0.30.0` its README states, so the simulator's vLLM matches
the guide. Both pools started at 1 replica.

## Checks

| # | Step 1 item | Observation | Outcome |
|---|---|---|---|
| S1 | decode P9 (1)(2): model-server PodMonitors, decode pod selector, duplicate series | `up`: one target per vLLM pod, jobs `<ns>/decode` and `<ns>/prefill` from the #3114 PodMonitors; the series carry `container, endpoint, engine, instance, job, model_name, namespace, pod`; `count by (pod)` with `pod=~"pd-disagg-coord-.+-decode-.+"` selects the one decode pod and not prefill; no duplicate scrape (llm-d-benchmark also applies the generic recipe `decode` PodMonitor, which the guide's, of the same name, replaces) | holds |
| S2 | alarm P9, needs-measurement 4 | steady load, about half disaggregated: decode `num_requests_waiting` 0 now, over 3 min on average and at its maximum; 4 running; the KV driver read 0.28, asking 1 pod ([`data/checks-s2-s4-s5.txt`](coordinator-kv-step2/data/checks-s2-s4-s5.txt)) | holds with simulated transport (see Simulator gaps) |
| S3a | actuation | 0.4 req/s of decode-served, 1,024-token requests: sum KV usage 0 to 0.515 by t = 167 s; 0.515 / 0.45 = 1.14, past the 10 % tolerance, so the HPA asks 2; the decode RoleScaler goes to 2 and a second decode pod is Ready at t = 213 s; the per-pod reading drops to 263m (0.515 / 2); the alarm reads 0 throughout ([`data/s3-monitor.csv`](coordinator-kv-step2/data/s3-monitor.csv)) | holds |
| S3b | P7 fails visibly | decode PodMonitor deleted at t = 203 s with decode at 2. Series kept arriving until t = 446 s (the user-workload Prometheus reloading its configuration), empty from t = 470 s; `Fallback=True` at t = 519 s with both triggers `Failing`; decode held at 2 from 519 to 951 s, longer than the 300 s scale-down window and past the end of the load. Under fallback the HPA shows each metric at its target (450m, 5) | holds; detection about 5 min on this cluster, mostly the monitoring reload |
| S4 | prefill P9 (4), needs-measurement 5 | on prefill calls `approx-prefix-cache-producer` and `decode-prefix-producer` record identical prompt tokens (2,699.6/s) and predicted cached tokens (1,079.9/s). The predicted-cached share, 40 %, is the shared prefix's share of a miss prompt (2,048 of 5,120 words): the trigger counts the suffix prefill computes | settled: pinning `plugin_name` is required (unpinned sums double) and either producer gives the same value |
| S5 | the Coordinator's 412 path | over 3 min, `llm_d_coordinator_conditional_decode_probes_total{result="deferred"}` 92.4 against 91.4 prefill dispatches per producer (within 1 %), `served` 79.2; the Coordinator logs conditional-decode, `cache miss (412)`, prefill, decode. 345 requests, all HTTP 200 ([`data/load-steady.csv`](coordinator-kv-step2/data/load-steady.csv)) | holds: the prefill trigger counts each disaggregated request once, and hits never reach prefill |

## Simulator gaps

- `SimulatedNixlConnector` transfers at 100 Gbps with a 2 ms handshake, so
  decode requests waiting for their KV barely appear in `num_requests_waiting`.
  The alarm baseline with a real NIXL transport is still a step 5 measurement.
- The simulator profile is Qwen3-32B on H100 under a Qwen3-VL-32B alias; its
  timing says nothing about this guide's capacity, so no knee is read here.

## Observations outside the properties

The standup needed manual steps that a guide-driven run should not:

- a `llm-d-hf-token` Secret (now required for guide standups);
- the README's `${VAR}`s are substituted in the command text but not exported
  to `| envsubst`, so the router HTTPRoute got an empty name and the
  Coordinator's `gateway.address` would have been empty; exported by hand;
- no Gateway is created (the README's Gateway step is prose); applied
  `guides/recipes/gateway/istio`;
- the `keda-prometheus-auth` ServiceAccount the bearer-token step needs is not
  created on the kustomize path; created with a namespace-scoped
  `cluster-monitoring-view` binding;
- the smoketest probes the EPP Service, not the Gateway, and times out although
  the model serves.

## Teardown

`helm uninstall` of the router release (its ClusterRole and ClusterRoleBinding
go with it), the namespace, and the `keda-prometheus-auth` ClusterRoleBinding.
