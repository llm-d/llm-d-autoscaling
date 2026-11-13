# Step 2: Verification on a cluster with a simulator

Part of the [evaluation protocol](README.md).

Step 1 predicts, from the text of the configuration, which properties each
trigger fails. Step 2 checks those predictions on a real cluster, with a
simulator in place of GPUs. It runs the real KEDA, Prometheus, router and
gateway, and fakes only the model server. Its job is to prove, for every
property step 1 marked *fail* (or *needs measurement*), that the failure
shows up in a run, or to show that step 1 was wrong. It does not measure
efficiency; that is a later step.

A configuration enters step 2 with a step 1 verdict from
[`verdicts/`](verdicts/). A *rejected* configuration still runs: the point is to
turn a prediction into an observation, and to catch an analysis error before
anyone relies on it.

## 2.1 Choosing the simulator

Two simulators are wired into the test bed as backend overlays.

| | `llm-d-inference-sim` | `vllm-model-sim` |
|---|---|---|
| Overlay | `cluster-configs/k8s/inference-sim.yaml` | `cluster-configs/ocp/model-sim-*.yaml` |
| What runs | a fake HTTP server that emits `vllm:` metrics | the real vLLM stack on CPU (real scheduler, batching, KV-cache accounting, preemption, prefix cache, API server); only the forward pass is replaced by a sleep |
| Latency model | `time-to-first-token` or per-token prefill, `inter-token-latency`, and a `time-factor-under-load` multiplier that grows *linearly* with in-flight requests up to the concurrency limit | `linear` or `physics` (roofline, tuned by `beta`) in the model's `config.json`; batch effects come from vLLM itself |
| Knee | none by construction: slowdown is linear, with no saturation point | emerges from the roofline and the real scheduler, once `beta` is tuned |
| Queue and KV metrics | simulated counters | genuine vLLM behavior, so `num_requests_waiting` and `kv_cache_usage_perc` mean what they mean on a GPU |
| Tuned models | any model name, no calibration | Qwen3-32B on H100 SXM5 (standalone and P/D). Anything else needs a profile |
| Cost | very small | 4 CPUs and about 20 GiB per pod (the KV cache is real RAM) |

**Rule.** Use `vllm-model-sim` whenever the property under test depends on a
knee, that is, on capacity being finite and latency or queueing rising sharply
near it. Use `llm-d-inference-sim` only when the property does not depend on
the engine's saturation behavior.

| Property (step 1) | Needs a knee? | Simulator |
|---|---|---|
| P1 proportional, P2 coupled to actuation | yes: the queue and running gauges must respond to load the way a saturating engine does | `vllm-model-sim` |
| P3 target means fixed busyness | yes: per-replica capacity must change with the mix (prompt and output length) | `vllm-model-sim`, one run per mix |
| P4 metric type | no: arithmetic on the pod count | either; `inference-sim` is enough |
| P5 bounds (capped or integrating signal) | yes: the cap or the overshoot is a property of saturation | `vllm-model-sim` |
| P6 defined at zero replicas | no | either |
| P7 fails visibly | no: break the source (scale Prometheus or the EPP down, change a label) | either |
| P8 lag and noise | partly: lag is measurable on either; noise at steady load needs realistic batching | `vllm-model-sim` for noise |
| P9 measures what it claims | depends on the trap; label and counter-semantics traps (T1, T6, T8) need no knee, KV and queue semantics do | trap by trap |

The `llm-d-inference-sim` has no knee we can rely on. Its linear
`time-factor-under-load` can show that a pool scales out in the right
direction, but it cannot tell a signal that saturates from one that does not.
Do not use it to pass P1, P3 or P5.

## 2.2 When the simulator needs extending

`vllm-model-sim` has few tuned models. Before a run, check that the overlay's
`config.json` models the model and hardware the scenario names. If it does
not, step 2 says so in the verdict, in a section named **Simulator gaps**,
and picks one of:

1. **Extend the simulator.** Add a model profile following the layout of
   `models/<model>/deployments/...` in `vllm-model-sim`: a `config.json` with
   the architecture and a `latency` block, a `hardware` block for the physics
   model, and `beta` tuned against a short real benchmark (the repository's
   `eval.sh tune`). Add a matching overlay `cluster-configs/ocp/model-sim-<model>.yaml`
   (the Qwen3-32B one is the template) and record the tuning error (MAPE of
   TTFT, ITL, throughput) in the verdict.
2. **Run untuned and say so.** Use a nearby profile, and restrict the claims.
   An untuned `beta` still gives a real scheduler, real KV accounting and a
   knee in the right place within a factor; it supports "the signal does or
   does not respond to the queue", not "the threshold is right".
3. **Defer the property** to a GPU run, and record why.

Never present a result from an untuned profile as a capacity number. Capacity
(`μ_SLO`) comes from the knee in a tuned profile or from a GPU run.

Known limits to carry into every verdict: the simulated output text is random;
`deterministic_length` makes every request run to `max_tokens`, so output length
is an input, not an outcome; sleep granularity is about 1 ms, so ITL below a few
milliseconds is not faithful; NIXL transfers are modeled by bandwidth; CPU
contention on a loaded node can add latency that the model does not contain.

## 2.3 Procedure

For one scenario, with its step 1 verdict in hand:

1. **List the claims.** One row per *fail* or *needs measurement* in the
   verdict, each with the observation that would confirm it. Examples:

   | Step 1 says | A run confirms it when |
   |---|---|
   | P9: `role="prefill"` matches no series (kv-early) | `count by (role)(vllm:num_requests_waiting{…})` is empty, and the HPA's external metric reads 0 at load |
   | P4: `max()` under `AverageValue` caps the KV guard at 2 pods | with every pod's KV above threshold, `desired` stays at 2 for 4 pods |
   | P1: queue is a dead band, then explosion | sweep offered load across `[0.2, 1.0]·μ_SLO`; the signal is flat, then jumps; compute `ε` |
   | P7: an empty result reads 0 | stop the source; the external metric reads 0 and `fallback` does not fire |

2. **Pick the simulator** per §2.1, and check for gaps per §2.2.
3. **Stand up** with the `run-benchmark` skill, keeping the scenario unchanged.
   Resolve floating tags and record the image digests (a simulator image on
   `latest` has no version). Record the KEDA release.
4. **Calibrate first** (only for `vllm-model-sim`). Drive one replica with
   `scaledObjects` disabled or `minReplicaCount == maxReplicaCount`, step the
   load, and read the knee: the load where TTFT or the queue turns up. This is
   the simulator's `μ_SLO` for the traffic mix. Without it, the load axis of
   every later claim is undefined.
5. **Run each claim as its own experiment.** Fixed workload shapes, one change
   at a time: a load sweep for P1/P2, a mix sweep for P3, a step up and a step
   down for P5/P8, a broken source for P7. Keep the load profile and seed with
   the result so it can be rerun.
6. **Read the signals directly**, not only the replica count: the Prometheus
   value of each trigger, the HPA's `kubectl describe` (current, target,
   desired per metric), and the pod count over time. A replica count alone
   cannot tell which trigger decided.
7. **Compare to step 1** claim by claim and write the verdict (§2.4).

Keep each run short. A claim about a static property (P4, P7, P9) needs a
minute of steady load, not a full trace.

## 2.4 Verdict

One file per scenario, next to the step 1 verdict, as
`verdicts/<guide>/<scenario>-step2.md`. It states:

- the scenario, backend overlay, simulator and its image digest, the profile
  and whether it is tuned, KEDA release, date;
- the calibrated knee, with the traffic mix it applies to;
- a table, one row per claim: step 1 prediction, observation (with the
  metric values or HPA output that show it), and an outcome:
  - **confirmed**: the run shows the predicted failure;
  - **refuted**: the run contradicts step 1; step 1 and its verdict are
    corrected, and, if the cause is a metric trap, [traps.md](traps.md) is
    updated;
  - **not reproducible here**: the simulator cannot show it (a gap named in
    **Simulator gaps**); the claim stays *needs measurement* for a GPU run;
- **Simulator gaps**: what had to be extended, assumed, or left out (§2.2);
- the overall result: step 1's verdict **holds** (all failed claims
  confirmed), **holds in part**, or is **withdrawn**.

A configuration whose step 1 verdict holds stays rejected. One whose step 1
verdict is withdrawn goes back through step 1. Only a configuration with no
confirmed failure in its driver proceeds to the later steps.

## 2.5 What step 2 does not settle

- Per-replica capacity on real hardware: a simulator's knee comes from the
  roofline and `beta`, not from a measurement of the target GPU.
- Cold start of a real pod (image pull, weight load, CUDA graph capture): the
  simulated pod starts with dummy weights.
- Router behavior that depends on real prefix-cache hit patterns.

These stay *needs measurement* until a GPU run.
