# Running WVA Scaling Benchmarks

Step-by-step guide for deploying and running WVA scaling benchmarks on an OpenShift cluster. This covers both **single-model** and **multi-model** benchmarks, from cluster access to running the tests and interpreting results.

## Prerequisites

### Required Tools

Verify the following tools are installed on your machine:

```bash
oc version --client
oc version --client  # includes kubectl functionality
helm version --short
yq --version
jq --version
go version
```

If any are missing, install via Homebrew: `brew install openshift-cli helm yq jq go`

### Required Access

- OpenShift cluster credentials (API URL + token)
- HuggingFace token with access to the models you want to deploy

---

## Step 1: Log In to the OpenShift Cluster

Get your login token from the OpenShift web console:

1. Open the OpenShift console in your browser
2. Click your username (top right) → **Copy login command**
3. Click **Display Token**
4. Copy the `oc login` command and run it:

```bash
oc login --token=sha256~XXXXXXXXXXXXXXXXXXXX --server=https://api.your-cluster.example.com:6443
```

Verify access and confirm which cluster you're connected to:

```bash
oc whoami
oc whoami --show-console
oc whoami --show-server
```

Check available GPUs on the cluster:

```bash
oc get nodes -o jsonpath='{range .items[?(@.status.allocatable.nvidia\.com/gpu)]}{.metadata.name}{"\t"}{.metadata.labels.nvidia\.com/gpu\.product}{"\n"}{end}'
```

---

## Step 2: Set Up Your Namespace

First, check which namespaces you already have access to:

```bash
oc projects
```

If you have an existing namespace you can use, use that as `<your-namespace>` in the commands below.

If you have cluster-admin access, create a fresh namespace:

```bash
oc new-project <your-namespace>
```

> **Note**: If you get a `Forbidden` error, you don't have permission to create namespaces. Contact the cluster admin to get admin access or have a namespace created for you.

Label the namespace for OpenShift user-workload monitoring (so Prometheus can scrape metrics):

```bash
oc label namespace <your-namespace> openshift.io/user-monitoring=true --overwrite
```

---

## Step 3: Export Your HuggingFace Token

The only environment variable you need to export is the HuggingFace token (required for model downloads):

```bash
export HF_TOKEN="hf_xxxxxxxxxxxxxxxxxxxxx"
```

All other configuration is passed directly to the deploy/test commands in later steps.

---

## Step 4: Clone the Repository

Clone the WVA repository and enter the directory:

```bash
git clone https://github.com/llm-d/llm-d-workload-variant-autoscaler.git
cd llm-d-workload-variant-autoscaler
```

Make sure you're on the correct branch:

```bash
git checkout main
# Or check out a specific PR branch:
# gh pr checkout <pr-number>
```

Install the benchmark CLI — this clones `llm-d-benchmark` into your workspace and sets up the `llmdbenchmark` CLI (once per workspace):

```bash
make benchmark-install
```

> **Note:** If your system Python is older than 3.11, the install will fail. Use the `--uv` flag to let `uv` download the correct Python version automatically:
> ```bash
> make benchmark-install BENCHMARK_UV=true
> ```

After this, your workspace will look like:

```
llm-d-workload-variant-autoscaler/
├── llm-d-benchmark/          ← cloned by make benchmark-install
├── test/benchmark/scenarios/
│   ├── prefill_heavy.yaml
│   ├── decode_heavy.yaml
│   └── symmetrical.yaml
└── Makefile
```

---

## Step 5: Run the Single-Model Benchmark

The single-model benchmark tests WVA scaling behavior with one model under different workload patterns. Scenario configurations are defined in `test/benchmark/scenarios/`.

| Scenario | Prompt Tokens | Output Tokens | Rate | What it tests |
|----------|--------------|---------------|------|---------------|
| `prefill_heavy` | 4000 | 1000 | 20 RPS | Prefill (prompt processing) — long input, short output |
| `decode_heavy` | 1000 | 4000 | 20 RPS | Decode (token generation) — short input, long output |
| `symmetrical` | 1000 | 1000 | 20 RPS | Balanced load — equal input and output |

**1. Stand up the benchmark environment:**

```bash
make benchmark-standup BENCHMARK_NAMESPACE=<your-namespace>
```

To deploy a specific model (defaults to `unsloth/Meta-Llama-3.1-8B`):

```bash
make benchmark-standup BENCHMARK_NAMESPACE=<your-namespace> MODEL_ID=Qwen/Qwen3-0.6B
```

Wait until you see `✅ All smoketest steps complete.`

**2. Run a scenario:**

```bash
make benchmark-run BENCHMARK_NAMESPACE=<your-namespace> BENCHMARK_WORKLOAD=prefill_heavy.yaml
```

If you stood up with a non-default model, pass the same `MODEL_ID`:

```bash
make benchmark-run BENCHMARK_NAMESPACE=<your-namespace> BENCHMARK_WORKLOAD=prefill_heavy.yaml MODEL_ID=Qwen/Qwen3-0.6B
```

Repeat with `decode_heavy.yaml` or `symmetrical.yaml` for the other scenarios.

Wait until you see:
```
✅ Run complete (mode=full, harness=guidellm).
```

Results are saved automatically in a timestamped directory at the repo root:
```
<username>-YYYYMMDD-HHMMSS/
└── results/
    └── guidellm-<id>/
        ├── benchmark_report_v0.2,_results.json_0.yaml   ← full metrics
        ├── results.json
        └── run_metadata.yaml
```

**3. Tear down when done:**

```bash
make benchmark-teardown BENCHMARK_NAMESPACE=<your-namespace>
```

Wait until you see:
```
✅ Teardown complete (normal).
```

> **Tip:** To run standup + all 3 scenarios + teardown in one command:
> ```bash
> make benchmark-full BENCHMARK_NAMESPACE=<your-namespace>
> ```

### Monitor During the Benchmark

In a separate terminal, monitor the scaling behavior:

```bash
oc get pods -n <your-namespace>
```

### Cleanup

```bash
oc delete project <your-namespace>
```

---
