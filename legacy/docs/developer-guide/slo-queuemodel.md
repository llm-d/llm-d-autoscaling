# QueueingModelAnalyzer

**Location:** `internal/engines/analyzers/queueingmodel/analyzer.go`

The QueueingModelAnalyzer is a capacity analyzer that uses queueing theory to determine
how many requests each model variant can handle while meeting latency SLOs. It learns
hardware characteristics online via an Extended Kalman Filter and uses a closed-form
queueing model to predict per-replica capacity — no manual calibration needed.

## Table of Contents

1. [Activating the Analyzer](#activating)
2. [ConfigMap Reference](#configmap-reference)
3. [SLO Targeting](#slo-targeting)
4. [How It Works](#how-it-works)
5. [Cold Start Behavior](#cold-start)
6. [Data Flow](#data-flow)
7. [Key Files](#key-files)
8. [Defaults Reference](#defaults-reference)
9. [Theoretical Background](#theory)

---

<a name="activating"></a>
## 1. Activating the Analyzer

The analyzer is activated by **applying the ConfigMap**
`deploy/configmap-queueing-model.yaml` to the cluster. When this ConfigMap
(`wva-queueing-model-config`) exists and contains a `default` key, the WVA controller uses
the queueing model path.

```bash
kubectl apply -f deploy/configmap-queueing-model.yaml
```

> The ConfigMap is re-read on every reconcile cycle, so changes take effect within one
> reconcile interval — no controller restart required.

---

<a name="configmap-reference"></a>
## 2. ConfigMap Reference

The ConfigMap has two types of entries:

- **`default`** — global settings applied to all models.
- **Per-model entries** — any other key name is a per-model override; the model is
  identified by `model_id` + `namespace` inside the value.

### Full annotated example

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-queueing-model-config
  namespace: workload-variant-autoscaler-system
  labels:
    app.kubernetes.io/name: workload-variant-autoscaler
    app.kubernetes.io/managed-by: kustomize
data:
  # ── Global defaults (required) ─────────────────────────────────────────────
  default: |
    # sloMultiplier (k) controls the target server utilisation.
    # At utilisation rho, mean iteration time = alpha/(1-rho).
    # Setting the SLO at T_iter = k×alpha yields target utilisation rho = 1-1/k.
    #
    # Common values:
    #   k=2.0  -> rho=0.50  (conservative; lower tail latency)
    #   k=3.0  -> rho=0.67  (default; good balance for most deployments)
    #   k=5.0  -> rho=0.80  (aggressive; maximise throughput, higher tail latency)
    #
    # Constraint: must be > 1.0. Default: 3.0
    sloMultiplier: 3.0

    # tuningEnabled must always be true. The queueing model requires learned
    # (alpha, beta, gamma) parameters to compute capacity; without the Kalman
    # filter running, no parameters are ever produced and the analyzer cannot
    # make scaling decisions.
    tuningEnabled: true

  # ── Per-model overrides (optional) ─────────────────────────────────────────
  # Key name is arbitrary. model_id + namespace identify the target model.
  # Per-model entries override sloMultiplier and can provide explicit SLO targets.
  #
  # Rules:
  #   - targetTTFT and targetITL must both be set, or both omitted (0 = infer).
  #   - If both are set, the explicit SLOs are used directly; the inferred SLO
  #     path is skipped (tuning still runs to keep parameters fresh).
  #   - If omitted, SLOs are inferred from learned parameters and sloMultiplier
  #     (or from observations if tuning hasn't converged yet).

  # Example: explicit SLOs for a production Llama model
  llama-prod: |
    model_id: "meta-llama/Meta-Llama-3.1-8B-Instruct"
    namespace: "llm-d-prod"
    targetTTFT: 500.0    # ms — time-to-first-token budget
    targetITL: 50.0      # ms — per-token decode budget
    sloMultiplier: 3.0

  # Example: inferred SLOs with a more aggressive utilisation target
  mistral-staging: |
    model_id: "mistralai/Mistral-7B-Instruct-v0.2"
    namespace: "llm-d-staging"
    sloMultiplier: 4.0   # rho=0.75; SLO derived from k and learned parameters
```

### Configuration field reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `sloMultiplier` | float | `3.0` | Utilisation target multiplier k (must be > 1.0). Target rho = 1 - 1/k. |
| `tuningEnabled` | bool | `true` | Must always be `true`. The queueing model requires learned parameters to function. |
| `model_id` | string | — | Model identifier (per-model entries only). |
| `namespace` | string | — | Kubernetes namespace (per-model entries only). |
| `targetTTFT` | float | `0` | Explicit TTFT SLO in milliseconds. `0` = infer automatically. |
| `targetITL` | float | `0` | Explicit ITL SLO in milliseconds. `0` = infer automatically. |

---

<a name="slo-targeting"></a>
## 3. SLO Targeting

The analyzer determines SLO targets for each model through a three-level priority chain:

### 3.1 Explicit SLOs (highest priority)

If `targetTTFT > 0` and `targetITL > 0` are set in the per-model ConfigMap entry, those
values are used directly as the TTFT and ITL budgets. This is the recommended mode for
production deployments where latency requirements are well-defined.

> **Both must be set together, or neither.** Setting only one is a validation error.

### 3.2 Model-Inferred SLOs (preferred automatic mode)

When explicit targets are absent and at least one variant has learned parameters
`(alpha, beta, gamma)`, the SLO is derived from the queueing model using `sloMultiplier` k.
At the target utilisation rho = 1 − 1/k, the steady-state iteration time equals `k × alpha`.
The SLO is then the latency that corresponds to that iteration time, with token-processing
work added at its true cost (see [Section 9](#theory) for the full derivation):

```
TargetTTFT = k×alpha + (beta + gamma) × avg_input_len
TargetITL  = k×alpha + beta + gamma × (avg_input_len + (avg_output_len + 1) / 2)
```

When multiple variants serve the same model, the SLO is taken as the **maximum** across
variants — a single SLO per model, since all variants serve the same traffic.

This mode requires the tuner to have converged, which typically takes 3–10 reconcile cycles
after a variant first receives traffic.

### 3.3 Observation-Based Fallback (cold start)

If no variant has learned parameters yet (the first few cycles after deployment, or after a
variant is freshly added), the SLO is estimated from observed latency metrics with a
headroom multiplier:

```
TargetTTFT = min(avg_observed_TTFT × 1.5,  10000 ms)
TargetITL  = min(avg_observed_ITL  × 1.5,    500 ms)
```

This is intentionally conservative: the system scales out rather than under-provisioning
while the model is still learning. Once the tuner has converged, SLO resolution
automatically transitions to path 3.2.

> **Summary of SLO resolution order:**
> 1. Explicit `targetTTFT` + `targetITL` from per-model ConfigMap entry.
> 2. Derived from learned parameters using `sloMultiplier` (requires tuner convergence).
> 3. Observed latency × 1.5 headroom (cold start only).

---

<a name="how-it-works"></a>
## 4. How It Works

### 4.1 Workload Aggregation

All components operate on a single workload summary per variant, aggregated from per-pod
metrics. Only pods with active traffic (`arrival_rate > 0`) contribute. Token lengths,
TTFT, and ITL are averaged **weighted by each pod's arrival rate**:

```
avg_input_len  = Σ(arrival_rate_i × input_len_i)  / Σ(arrival_rate_i)
avg_output_len = Σ(arrival_rate_i × output_len_i) / Σ(arrival_rate_i)
avg_TTFT       = Σ(arrival_rate_i × TTFT_i)       / Σ(arrival_rate_i)
avg_ITL        = Σ(arrival_rate_i × ITL_i)        / Σ(arrival_rate_i)
```

The total arrival rate for capacity sizing is the sum across all busy pods:
`total_arrival_rate = Σ arrival_rate_i`.

### 4.2 The Tuner (Online Parameter Estimator)

**Location:** `internal/engines/analyzers/queueingmodel/tuner/tuner.go`

The Tuner wraps an **Extended Kalman Filter (EKF)** that treats the three hardware
parameters `(alpha, beta, gamma)` as the hidden state and `(AvgTTFT, AvgITL)` as
observations. On each reconcile cycle it:

1. **Restores** the previous state estimate and error covariance from the `ParameterStore`
   (or bootstraps from a cold-start guess — see [Section 5](#cold-start)).
2. **Predicts** the next state using an identity transition (parameters are assumed slowly
   varying).
3. **Updates** by comparing the TTFT and ITL predicted by the queueing model at the current
   parameter estimates against the newly observed values.
4. **Validates** the update using the Normalized Innovation Squared (NIS). Updates with
   NIS ≥ 7.378 (95th percentile of χ²₂) are rejected as outliers and the previous state is
   restored — the filter never accepts a bad update.
5. **Stores** the accepted `(alpha, beta, gamma)` and covariance back in the `ParameterStore`
   for the next cycle.

### 4.3 Capacity Sizing

Once `(alpha, beta, gamma)` are available, `QueueAnalyzer.Size(targetPerf)` binary-searches
for the maximum arrival rate `lambda*` at which both predicted TTFT and ITL remain within
the SLO targets. The required replica count is then:

```
required_replicas = ceil(total_arrival_rate / lambda*)
```

### 4.4 Per-Variant Failure Behavior

If analysis of an individual variant fails at any step — no metrics, no active traffic, no
learned parameters yet, or a queueing model error — the variant is **not dropped**. Instead
it is included in the result with a zero-capacity placeholder:

```
PerReplicaCapacity = 0,  TotalCapacity = 0,  TotalDemand = 0,  Utilization = 0
```

The variant's current and pending replica counts are still reported accurately, so the
optimizer sees the full variant roster and can apply safe-hold behavior.

An error is returned at the model level only if **every** variant fails, in which case no
scaling decision is emitted for that model during that cycle.

---

<a name="cold-start"></a>
## 5. Cold Start Behavior

When a variant receives traffic for the first time, the `ParameterStore` has no prior
`(alpha, beta, gamma)`. The analyzer bootstraps an initial estimate analytically from
observed TTFT, ITL, and token lengths (see [Section 9.3](#theory-cold-start) for the
derivation). If the bootstrap produces valid positive values, the EKF starts from that
estimate with tightened bounds to converge quickly. If the bootstrap fails (e.g. due to
unusual metric ratios), the EKF falls back to hardcoded defaults
(`alpha=5.0 ms, beta=0.05 ms, gamma=0.00005 ms`) and converges from there.

During this warm-up period — typically 3–10 reconcile cycles — scaling decisions rely on
the observation-based SLO fallback (Section 3.3), which applies a 1.5× headroom to
observed latencies to avoid under-provisioning.

---

<a name="data-flow"></a>
## 6. Data Flow

```
Prometheus / vLLM metrics per pod
  (arrival_rate, avg_TTFT, avg_ITL, avg_input_tokens, avg_output_tokens)
        │
        ▼
  ┌─────────────────────────────────────────────────────────┐
  │  Collector (replica_metrics.go)                         │
  │  Groups metrics by model → variant → pod                │
  └───────────────────────┬─────────────────────────────────┘
                          │ []ReplicaMetrics
                          ▼
  ┌─────────────────────────────────────────────────────────┐
  │  Workload Aggregation (arrival-rate weighted per variant)│
  └───────────────────────┬─────────────────────────────────┘
                          │
          ┌───────────────┴───────────────┐
          ▼                               ▼
  ┌───────────────┐               ┌────────────────────────┐
  │  Tuner (EKF)  │               │  SLO Resolution        │
  │               │               │  1. Explicit config    │
  │  Predict      │               │  2. k×alpha model      │
  │  Update obs   │               │  3. 1.5× observed      │
  │  NIS validate │               └──────────┬─────────────┘
  │  Store params │                          │ SLOTarget
  └───────┬───────┘                          │
          │ (alpha, beta, gamma)             │
          └──────────────┬───────────────────┘
                         │
                         ▼
  ┌─────────────────────────────────────────────────────────┐
  │  computeAllVariantCapacities                            │
  │  For each variant:                                      │
  │    qa = QueueAnalyzer(alpha, beta, gamma, maxBatchSize) │
  │    lambda* = qa.Size(SLOTarget)   ← binary search      │
  │    required = ceil(totalArrival / lambda*)              │
  └───────────────────────┬─────────────────────────────────┘
                          │ []VariantCapacity
                          ▼
  ┌─────────────────────────────────────────────────────────┐
  │  Optimizer → VariantDecisions → Enforcer → Apply        │
  └─────────────────────────────────────────────────────────┘
```

---

<a name="key-files"></a>
## 7. Key Files

| Component             | Path                                                             |
|-----------------------|------------------------------------------------------------------|
| QueueingModelAnalyzer | `internal/engines/analyzers/queueingmodel/analyzer.go`           |
| Config types          | `internal/engines/analyzers/queueingmodel/config.go`             |
| Parameter Store       | `internal/engines/analyzers/queueingmodel/parameters.go`         |
| Defaults              | `internal/engines/analyzers/queueingmodel/defaults.go`           |
| Tuner (EKF)           | `internal/engines/analyzers/queueingmodel/tuner/tuner.go`        |
| Tuner defaults        | `internal/engines/analyzers/queueingmodel/tuner/defaults.go`     |
| Tuner configurator    | `internal/engines/analyzers/queueingmodel/tuner/configurator.go` |
| Tuner environment     | `internal/engines/analyzers/queueingmodel/tuner/environment.go`  |
| QueueAnalyzer         | `internal/queueing/analyzer/queueanalyzer.go`                    |
| Engine integration    | `internal/engines/saturation/engine_queueing_model.go`           |
| ConfigMap interface   | `internal/domain/queueing_model_scaling.go`                      |
| ConfigMap YAML        | `deploy/configmap-queueing-model.yaml`                           |

---

<a name="defaults-reference"></a>
## 8. Defaults Reference

| Constant | Value | Meaning |
|----------|-------|---------|
| `DefaultSLOMultiplier` | `3.0` | k=3 → target utilisation rho=0.67 |
| `DefaultMaxBatchSize` | `256` | Max concurrent requests per replica when not parseable from deployment spec |
| `DefaultMaxQueueSize` | `100` | Queue depth limit in the queueing model |
| `DefaultFallbackHeadroom` | `1.5` | Multiplier on observed latency for cold-start SLO |
| `DefaultMaxFallbackTTFT` | `10000 ms` | Cap on fallback TTFT SLO |
| `DefaultMaxFallbackITL` | `500 ms` | Cap on fallback ITL SLO |
| `DefaultMaxNIS` | `7.378` | χ²₂ at 95th percentile — EKF update rejection threshold |
| `DefaultAlpha` | `5.0 ms` | EKF initial guess when bootstrap fails |
| `DefaultBeta` | `0.05 ms/token` | EKF initial guess when bootstrap fails |
| `DefaultGamma` | `0.00005 ms/token` | EKF initial guess when bootstrap fails |
| `BaseFactor` | `0.9` | Fraction of avg ITL used as initial alpha estimate during bootstrap |
| `TransientDelaySeconds` | `120 s` | Grace period before tuning a freshly scaled-up replica |

---

<a name="theory"></a>
## 9. Theoretical Background

This section provides the analytical foundations of the queueing model for readers who want
to understand how TTFT, ITL, and capacity are derived.

### 9.1 Hardware Parameters

Three hardware-specific parameters connect abstract work to wall-clock time:

| Parameter | Meaning | Governed by |
|-----------|---------|-------------|
| **alpha** | Baseline iteration overhead (ms) — kernel launch latencies, synchronization barriers, model weight loading — constant regardless of batch composition | — |
| **beta** | Compute time per token (ms/token) — matrix multiplications and nonlinear ops in the forward pass | GPU FLOP throughput |
| **gamma** | KV-cache memory access time per token (ms/token) — attention KV read/write cost | GPU memory bandwidth |

### 9.2 Service Time Model

A request with `i_l` input tokens and `o_l` output tokens participates in `o_l + 1`
iterations (one prefill + `o_l` decodes). The work it contributes per iteration varies
by phase:

- **Prefill** (age=0): compute over `i_l` tokens (`β × i_l`) plus KV-cache read of `i_l`
  tokens (`γ × i_l`), giving prefill work `w_prefill = (β + γ) × i_l`.
- **Decode step k** (k=1..o_l): compute on one output token (`β`) plus KV-cache read of
  all `i_l + k` cached tokens (`γ × (i_l + k)`), giving `W_k = β + γ(i_l + k)`.

Averaged uniformly over all `o_l + 1` iterations, the marginal work per request per
iteration is:

```
δ = β × (i_l + o_l) / (o_l + 1)  +  γ × (i_l + o_l / 2)
```

With `n` concurrent requests, the iteration time is:

```
T_iter(n) = α + n × δ
```

### 9.3 Steady-State Analysis

By Little's Law, the average number of concurrent requests is `n = λ × (o_l + 1) × T_iter`.
Substituting into the iteration time equation and solving yields the closed-form result:

```
T_iter = α / (1 - ρ)

where  ρ = λ × (o_l + 1) × δ
         = λ × ( β(i_l + o_l)  +  γ(o_l + 1)(i_l + o_l/2) )
```

The system is stable when ρ < 1. As ρ → 1, `T_iter` diverges — this is the fundamental
capacity limit of a single replica.

### 9.4 TTFT and ITL

Because of Poisson arrivals, an incoming request must wait for the current iteration to
finish (residual wait ≈ `T_iter`) before being scheduled. TTFT is that wait plus the
prefill work:

```
TTFT = T_iter + (β + γ) × i_l
```

Each decode step takes one full iteration plus the step's own decode work. Averaging over
all `o_l` decode steps gives:

```
ITL = T_iter + β + γ × (i_l + (o_l + 1) / 2)
```

Setting `T_iter = k × alpha` (i.e. targeting utilisation ρ = 1 − 1/k) yields the SLO
inference formulas in Section 3.2.

<a name="theory-cold-start"></a>
### 9.5 Initial Parameter Estimation (Cold Start)

When no prior parameters exist, the analyzer bootstraps `(alpha, beta, gamma)` analytically
by inverting the TTFT and ITL equations under the assumption `T_iter ≈ alpha` (valid at
light load, where ρ ≈ 0):

**Step 1:** Estimate alpha from observed ITL, since at light load ITL ≈ α plus minimal
decode work:
```
alpha ≈ BaseFactor × avg_ITL     (BaseFactor = 0.9)
```

**Step 2:** Solve for `(beta + gamma)` from the TTFT equation:
```
(beta + gamma) = (avg_TTFT - alpha) / avg_input_len
```

**Step 3:** Separate beta and gamma from the ITL equation:
```
gamma = ((avg_ITL - alpha) - (beta+gamma)) / (avg_input_len + (avg_output_len+1)/2 - 1)
beta  = (beta+gamma) - gamma
```

If any derived value is ≤ 0, the bootstrap fails and the EKF starts from hardcoded
defaults (`alpha=5.0 ms, beta=0.05 ms, gamma=0.00005 ms`), converging over subsequent
cycles.
