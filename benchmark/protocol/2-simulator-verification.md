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
Do not use it to show or refute P1, P3 or P5.

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

For one scenario, with its step 1 verdict in hand. The
[`simulator-verification` skill](../../.claude/skills/simulator-verification/SKILL.md)
runs these steps in this order, with the commands.

1. **List the properties.** An experiment can show that a property fails; it
   cannot prove that one passes (P1 is an example: no finite sweep shows the
   elasticity is 1 everywhere). So test every property step 1 marked *fail*,
   and every *needs measurement* that a run can settle. The properties step 1
   marked *pass* or *n/a* are not tested; they stay in the verdict as *not
   verified by this skill*. For each property under test, write down the
   observation that would show the failure:

   | Property | Experiment | Fail is shown when |
   |---|---|---|
   | P1 proportional | sweep offered load over `[0.2, 1.0]·μ_SLO`; read the signal at each level | `ε ≈ 0` (dead band) or `ε ≫ 1` |
   | P2 coupled to actuation | fix the load, vary the pod count; read the per-pod signal | it does not move with `R` |
   | P3 fixed busyness | repeat the sweep for several prompt/output mixes; read the signal when one replica is at capacity | `V ≫ 1` |
   | P4 metric type | with a known total, compare the HPA's `desired` to `ceil(U/T)` (`AverageValue`) or `ceil(N·U/T)` (`Value`) at several pod counts | `desired` is capped or shrinks as pods are added |
   | P5 bounds | step the load past capacity; read the signal and replica count | signal saturates below the SLO, or overshoot is unbounded |
   | P6 defined at zero | only if `minReplicaCount: 0`: scale to zero, send load, read the signal | `s = 0` or empty |
   | P7 fails visibly | break the source (scale Prometheus or the EPP down, or use a wrong label) | the external metric reads 0 and nothing fires |
   | P8 lag and noise | step the load; at steady load read the sample-to-sample swing | either one is not |
   | P9 measures what it claims | read the metric against an independent count of the quantity it claims | it does not, or the series is absent |

2. **Pick the simulator** per §2.1, and check for gaps per §2.2.
3. **Isolate the run.** Use the Kubernetes context the user chose (the
   current one by default), confirm with the user that the cluster accepts
   experiments before the first write, and never switch their context. Create
   a new namespace named for the scenario and the date (for example
   `step2-kv-early-20261113`) and use it for everything; never reuse one.
   Cluster-scoped objects the overlay creates carry the namespace name, so
   they go with it at teardown.
   Standup needs `helm` and `helmfile` to reach the chart repositories
   (`llm-d-incubation.github.io`, `prometheus-community.github.io`, and
   others) with a trusted TLS chain. Behind a TLS-intercepting proxy, Go
   tools ignore `SSL_CERT_FILE` on macOS and fail with `x509: certificate
   signed by unknown authority`; run from a host whose trust store holds the
   proxy's CA, or the run stops at standup.
4. **Stand up the scenario unchanged** (`run-benchmark` skill). Resolve
   floating tags and record the image digests (a simulator image on `latest`
   has no version), and the KEDA release. Do not apply load until every
   ScaledObject is ready and the HPA shows real values: a run against
   triggers that cannot read Prometheus measures nothing.
5. **Calibrate first** (only for `vllm-model-sim`). Drive one replica with
   `scaledObjects` disabled or `minReplicaCount == maxReplicaCount`, step the
   load, and read the knee: the load where TTFT or the queue turns up. This is
   the simulator's `μ_SLO` for the traffic mix. Without it, the load axis of
   every later load experiment is undefined.
6. **Run each property as its own experiment.** Fixed workload shapes, one
   change at a time: a load sweep for P1/P2, a mix sweep for P3, a step up and
   a step down for P5/P8, a broken source for P7. Keep the load profile and
   seed with the result so it can be rerun. A static property (P4, P7, P9)
   needs a minute of steady load, not a full trace.
7. **Read the signals directly**, not only the replica count: the Prometheus
   value of each trigger, the HPA's current, target and desired per metric,
   and the pod count over time. A replica count alone cannot tell which
   trigger decided. Export these series as CSV into the verdict's data
   directory (§2.4) as you go; a plot is drawn only from captured data.
8. **Write the verdict** (§2.4), property by property.
9. **Tear down**, also after a failed or abandoned run: uninstall the
   releases, delete the namespace and the cluster-scoped leftovers named for
   it, and confirm they are gone. Never delete a namespace the run did not
   create.

## 2.4 Verdict

One file per scenario, next to the step 1 verdict, as
`verdicts/<guide>/<scenario>-step2.md`. It states:

- the namespace and cluster version the runs used, never the context or
  cluster name;
- the scenario, backend overlay, simulator and its image digest, the profile
  and whether it is tuned, KEDA release, date;
- the calibrated knee, with the traffic mix it applies to;
- the verdict **by property**: one section per property, P1 to P9, in order,
  each with a table of one row per trigger: step 1's result, the
  observation (the numbers read from the cluster), and an outcome:
  - **not verified by this skill**: step 1 marked it pass or n/a; the row
    shows step 1's result and no observation;
  - **confirmed fail**: the run shows the failure step 1 predicted;
  - **refuted**: the run contradicts step 1; step 1 and its verdict are
    corrected, and, if the cause is a metric trap, [traps.md](traps.md) is
    updated;
  - **not reproducible here**: the simulator cannot show it (a gap named in
    **Simulator gaps**); the property stays *needs measurement* for a GPU run;
- **empirical proof for every fail**: a *confirmed fail* needs the measured
  values that show it (the query result, the HPA's current/target/desired,
  the replica counts, with their timestamps or load levels), the arithmetic
  that turns them into the property's number (for example `ε = ln(s₂/s₁) /
  ln(λ₂/λ₁) = 0.08`), and an SVG plot (§2.5) drawn from the exported data. A
  failure shown only by reasoning from the source or the configuration is
  step 1's evidence, not step 2's, and stays *not confirmed*;
- **Simulator gaps**: what had to be extended, assumed, or left out (§2.2);
- the overall result: step 1's verdict **holds** (every tested property's outcome
  agrees with step 1), **holds in part**, or is **withdrawn**.

A configuration whose step 1 verdict holds stays rejected. One whose step 1
verdict is withdrawn goes back through step 1. Only a configuration with no
confirmed failure in its driver proceeds to the later steps.

## 2.5 Plots

Every *confirmed fail* comes with an SVG plot built from data captured in
its run (§2.4). A failure that is a single reading with no time or load
dimension (a static P4 or P9 check) is shown by the raw reading in the table
plus a plot of that reading against its ground truth over the run, so the
proof is still empirical.

- **Data only from the run.** The plot is generated by a script from the
  exported CSV (Prometheus range queries, HPA readings, pod counts). No
  hand-drawn, modeled or illustrative curves. Keep the CSV and the script
  next to the verdict, in `verdicts/<guide>/<scenario>-step2/` as
  `<property>.csv`, `<property>.svg` and `plot.py` (or equivalent), so a reader
  can regenerate the plot. Embed the SVG in the verdict with a relative link.
- **Relevant to the property.** Plot the quantity the property is about, against
  what it should follow, and mark the threshold or the expected value:

  | Property | Plot |
  |---|---|
  | P1 proportional | signal vs. offered load (log-log), with the line of slope 1 and the fitted `ε` |
  | P2 coupled to actuation | per-pod signal vs. replica count at fixed load |
  | P3 mix sensitivity | signal at one replica's capacity, one bar or point per mix, with `V` |
  | P4 metric type | HPA `desired` vs. pod count, with the step 1 prediction overlaid |
  | P5 bounds | signal and replica count over time through a load step, with `μ_SLO` and the cap |
  | P7 fails visibly | trigger value and replica count over time, with the moment the source is broken |
  | P8 lag and noise | signal against the true load over time, with the lag marked and the noise band against the 10 % tolerance |
  | P9 measures what it claims | the metric against an independent ground truth (for example the load generator's own count) over time |

- **Readable alone.** Axis labels with units, a legend, the threshold or
  target drawn and labelled, and a title that states what the plot shows.
  Colors must stay distinguishable in grayscale, and the SVG needs a
  `<title>` and `<desc>` for accessibility. Render it and look at it before
  committing: titles, legends, annotations and event labels must not overlap
  each other or the data.
- **Caption.** Directly below each plot, two to four sentences: what was
  varied, what was measured, what the plot shows, and whether it confirms
  or refutes the prediction, with the number that decides it (for example
  "`ε = 0.08` between 0.2 and 0.9 of capacity; step 1 predicted a dead band").
  Say plainly when the simulator limits what the plot proves.

## 2.6 What step 2 does not settle

- Per-replica capacity on real hardware: a simulator's knee comes from the
  roofline and `beta`, not from a measurement of the target GPU.
- Cold start of a real pod (image pull, weight load, CUDA graph capture): the
  simulated pod starts with dummy weights.
- Router behavior that depends on real prefix-cache hit patterns.

These stay *needs measurement* until a GPU run.
