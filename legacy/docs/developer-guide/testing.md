# Testing Guide

Comprehensive guide for testing the Workload-Variant-Autoscaler (WVA).

## Overview

WVA has a multi-layered testing strategy:

1. **Unit Tests** - Fast, isolated tests for individual packages and functions
2. **Integration Tests** - Tests for component interactions within the controller
3. **E2E Tests** - Environment-agnostic end-to-end tests (Kind emulated or OpenShift), with smoke and full tiers

## Unit Tests

### Running Unit Tests

```bash
# Run all unit tests
make test

# Run with coverage report
go test -cover ./...

# Run specific package
go test ./internal/queueing/analyzer/...

# Run with verbose output
go test -v ./internal/controller/...

# Generate HTML coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out -o coverage.html
```

### Unit Test Structure

Unit tests are co-located with the code they test:

```
internal/
├── controller/
│   ├── variantautoscaling_controller.go
│   └── variantautoscaling_controller_test.go
├── saturation/
│   ├── analyzer.go
│   └── analyzer_test.go
├── collector/
│   ├── collector.go
│   └── collector_test.go
└── queueing/
    └── analyzer/
        ├── queueanalyzer.go
        └── queueanalyzer_test.go
```

### Writing Unit Tests

Example unit test structure:

```go
package solver_test

import (
    "testing"
    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"
)

func TestSolver(t *testing.T) {
    RegisterFailHandler(Fail)
    RunSpecs(t, "Solver Suite")
}

var _ = Describe("Solver", func() {
    Context("when optimizing single variant", func() {
        It("should calculate optimal replicas", func() {
            // Test implementation
            Expect(result).To(Equal(expected))
        })
    })
})
```

### Unit Test Best Practices

- **Use table-driven tests** for testing multiple scenarios
- **Mock external dependencies** (Kubernetes API, Prometheus, etc.)
- **Test edge cases** (zero values, negative numbers, nil pointers, etc.)
- **Keep tests fast** - unit tests should run in milliseconds
- **Use descriptive test names** - clearly state what is being tested
- **Follow AAA pattern** - Arrange, Act, Assert

## Integration Tests

Integration tests validate component interactions within the controller using envtest.

### Running Integration Tests

```bash
# Run integration tests (included in make test)
make test

# Run only controller integration tests
go test ./internal/controller/... -v
```

### envtest Setup

Integration tests use controller-runtime's envtest, which provides a real Kubernetes API server for testing:

```go
var _ = BeforeSuite(func() {
    testEnv = &envtest.Environment{
        CRDDirectoryPaths: []string{
            filepath.Join("..", "..", "config", "crd", "bases"),
        },
    }

    cfg, err := testEnv.Start()
    Expect(err).NotTo(HaveOccurred())

    k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
    Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
    Expect(testEnv.Stop()).To(Succeed())
})
```

## End-to-End Tests

WVA provides a **single consolidated E2E suite** that runs on multiple environments (Kind with emulated GPUs, or OpenShift/kubernetes with real infrastructure). Tests are environment-agnostic and parameterized via environment variables; they create VA, HPA, and model services dynamically as part of the test workflow.

- **Location**: `test/e2e/`
- **Environments**: Kind (emulated), OpenShift, or generic Kubernetes
- **Tiers**: Smoke (~5–10 min) for PRs; full suite (~15–25 min) for comprehensive validation

### Scope

E2E is intended to be a **deterministic correctness signal**: resource wiring, reconciliation, and stable invariants (e.g., CRs reconcile, status conditions are set, scalers are created and point at the right targets/metrics). Traffic generation and performance/benchmarking scenarios should live outside `test/e2e/`.

### E2E shared fixtures

Code lives under `test/e2e/fixtures`. The `fixtures` package holds reusable helpers to create, ensure (idempotent setup), and delete Kubernetes objects used by the e2e suite (VariantAutoscaling, HPA, KEDA ScaledObject, model services, Services, ServiceMonitors, InferenceObjective, etc.). Package-level documentation and naming conventions (`Create*` / `Ensure*` / `Delete*`, `baseName` vs full resource names) live in the package doc:

```bash
go doc ./test/e2e/fixtures
```

After changing fixture APIs or generated object shape, compile e2e without running specs:

```bash
go test ./test/e2e/... -run TestDoesNotExist
```

### Infra-Only Setup (Required Before Running Tests)

Tests expect **WVA + monitoring + scaler + llm-d EPP/gateway** to be deployed; they create VariantAutoscaling resources, HPAs, and model workloads themselves. Use **`make deploy-e2e-infra`** (runs `deploy/install.sh` then `deploy/install-epp.sh`) or invoke those scripts with the same environment variables the Makefile sets.

This deploys:
- WVA controller (via Kustomize)
- llm-d EPP (GAIE standalone chart) via `deploy/install-epp.sh`
- Prometheus stack and KEDA
- **No** VariantAutoscaling or HPA (tests create these)

When `ENABLE_SCALE_TO_ZERO=true` (set by `make deploy-e2e-infra` when `SCALE_TO_ZERO_ENABLED=true`), **`install-epp.sh`** enables the **flowControl feature gate** on the EPP so it exposes `inference_extension_flow_control_queue_size`. The **InferenceObjective** `e2e-default` is created by the scale-from-zero tests (`test/e2e/fixtures`), not by the install scripts.

**Install script tuning (optional, same variables as `deploy/install.sh`):**

- **`SKIP_HELM_REPO_UPDATE`**: When set to **`true`**, `helm repo update` is skipped during installs (faster, less network churn). Default runs `helm repo update` to refresh repo indexes.

Alternatively, use the Makefile to deploy infra and run tests in one go:

```bash
# Kind: create cluster, deploy infra, run smoke tests
make test-e2e-smoke-with-setup

# Kind: deploy infra only (if cluster already exists), then run full suite
make deploy-e2e-infra
make test-e2e-full
```

See the [E2E Test Suite README](../../test/e2e/README.md) for full configuration options and examples.

### Direct KEDA+EPP guide contract

`test-e2e-keda-epp-guide-with-setup` is a narrow, controller-free contract for
the canonical KEDA+EPP guide in the `llm-d/llm-d` repository. It qualifies an
optimized-baseline-compatible KEDA+EPP guide topology, not the full
optimized-baseline behavior. The deploy target
composes the existing `deploy-e2e-infra` lifecycle with `DEPLOY_WVA=false` and
`SCALER_BACKEND=keda`; it does not deploy the WVA controller, copy the guide's
autoscaling resources into this repository, or provide a generic guide runner.

Run the complete fresh-cluster lifecycle with one command:

```bash
# Optional: materialize the selected revision from a nearby read-only checkout.
export LLMD_SOURCE=../llm-d
make test-e2e-keda-epp-guide-with-setup LLMD_SOURCE="${LLMD_SOURCE:-}"
```

The simulator is CPU-only: its Deployment has no GPU resource request even
though its canonical guide labels and name describe the NVIDIA GPU variant.
`CLUSTER_GPUS=0` therefore exercises the contract without a GPU. Never point
this flow at an existing or shared cluster. The exact current Kind context,
absence of the guide namespaces, and absence of KEDA are checked before
deployment; the whole fresh cluster remains the final cleanup guarantee.
The deploy target installs this guide-owned simulator after the existing
Prometheus, KEDA, and EPP infrastructure, waits for its single replica to be
Ready, and only then applies the canonical `TriggerAuthentication` and
`ScaledObject`. The test observes that pre-created Deployment and deletes it
during its isolated cleanup.

Current official llm-d `main` is the normal source. At the start of each guide
setup, the deploy target resolves `main` from the official llm-d repository
exactly once to a full immutable commit SHA, logs that SHA, and uses only that
revision for canonical rendering, deployment, and the remainder of the run.
This intentionally exposes llm-d/WVA drift when a later run selects a newer
`main` revision.

Set `LLMD_SOURCE_SHA=<full SHA>` only as an explicit pin for reproducibility,
debugging, compatibility investigation, or temporary stabilization. The pin
takes precedence over `main` resolution and is not the normal operating mode.
`LLMD_SOURCE` is optional and never selects a revision: when set, it only
materializes the already selected commit from that read-only checkout without
reading or changing its checked-out branch, `HEAD`, local `main`, or tracking
refs. If the selected object is absent, setup fails rather than falling back to
another revision. When `LLMD_SOURCE` is empty, the selected immutable commit is
downloaded into temporary storage.

With either source location, the target layers exactly the current canonical
router base, optimized-baseline topology, monitoring, and KEDA+EPP queue
values, followed by one WVA-owned deterministic Kind override. The historical
WVA v0.8.1 base and optimized-baseline values remain defaults for existing
callers but do not participate in this guide path. It applies the canonical
`TriggerAuthentication` and `ScaledObject` through a temporary Kustomize
overlay. The temporary overlay changes the environment-specific namespace and
Prometheus endpoint. The canonical trigger queries, thresholds, replica bounds,
scale target, HPA behavior, and authentication wiring remain unchanged.

The deployed and tested inputs are:

| Input | Required value |
|-------|----------------|
| Environment | `kind-emulator` |
| WVA controller | disabled with `DEPLOY_WVA=false` |
| llm-d namespace | `llm-d-optimized-baseline` |
| Prometheus namespace | `workload-variant-autoscaler-monitoring` |
| KEDA namespace | `keda-system` |
| EPP Service | `optimized-baseline-epp` |
| Model | `Qwen/Qwen3-32B` |

For this disposable Kind flow, KEDA `2.20.0` is installed with WVA-owned Kind
values that mount the Prometheus public CA into the KEDA operator trust
store. The Prometheus serving key remains only in the monitoring namespace;
the KEDA operator namespace and `keda-prometheus-auth` Secret receive only the
public CA. This is a Kind-only TLS compatibility boundary, not production
authentication guidance and not an OpenShift configuration.

The `keda-epp-guide` job in `.github/workflows/ci-pr-checks.yaml` runs this same
fresh Kind lifecycle for WVA pull requests. The target selects only Ginkgo label
`keda-epp-guide`; WVA smoke, scale-from-zero, and controller-dependent specs are
excluded. The guide spec uses semantically named bounds: 120 seconds for
simulator readiness, 60 seconds for request startup and probe completion, 90
seconds for metric observation, 180 seconds for stabilization, and 300 seconds
for the scale transition. Individual Kubernetes API and bounded log calls use a
10-second timeout; normal and quick polling use five and two seconds.

The complete request-bearing observation budget has a conservative 20-minute
cap. Flow Control `defaultRequestTTL`, the simulator response time, and curl
`--max-time` remain aligned at 1500 seconds (25 minutes), so the stimulus lives
strictly longer than that cap. Ginkgo is bounded at 65 minutes and Go at 70
minutes, leaving room for suite preflight and diagnostics. The CI job remains
bounded at 120 minutes, containing setup, the Go boundary, final diagnostics,
and fresh-cluster cleanup.

The repository-default Kind image currently uses Kubernetes v1.32. KEDA 2.20 does
not list Kubernetes v1.32 in its tested compatibility matrix,
even though KEDA's deployment prerequisites allow Kubernetes 1.30 and newer.
Treat that pairing as a CI compatibility risk, not as evidence
of official KEDA support; this lane intentionally does not
carry an unvalidated Kubernetes v1.33 override.

This contract proves the request → EPP Flow Control → queue/running metrics →
Prometheus → secure KEDA external metrics → KEDA-owned HPA → Deployment chain
and one bounded `1 -> 2` transition. It deliberately excludes nightly or
stable-promotion qualification, sustained or performance load, scale-down,
post-scale inference, real models, GPUs, KV-event routing, and OpenShift/Thanos
behavior.

### Quick Start

```bash
# Smoke tests (recommended for every PR)
make test-e2e-smoke

# Full suite (on-demand)
make test-e2e-full

# OpenShift: point at cluster and run
export KUBECONFIG=/path/to/openshift/kubeconfig
export ENVIRONMENT=openshift
make test-e2e-smoke
# or make test-e2e-full

# Run a specific test by name
FOCUS="Basic VA lifecycle" make test-e2e-smoke
```

### What the Suite Validates

- **Smoke (label `smoke`)**: Infrastructure readiness, basic VA lifecycle, target condition validation
- **Full (label `full`)**: Smoke plus additional deterministic correctness checks (scale-from-zero, limiter, pod scraping, etc.)

### Configuration

Key environment variables (see [E2E Test Suite README](../../test/e2e/README.md) for the full list):

| Variable | Default | Description |
|----------|---------|-------------|
| `ENVIRONMENT` | `kind-emulator` | `kind-emulator`, `openshift`, or `kubernetes` |
| `USE_SIMULATOR` | `true` | Emulated GPUs (true) or real vLLM (false) |
| `SCALE_TO_ZERO_ENABLED` | `false` | Enable scale-to-zero tests (Kind supports both enabled and disabled) |
| `SCALER_BACKEND` | `keda` | `keda` (ScaledObject) or `none` (skip install, use pre-installed backend) |
| `POD_READY_TIMEOUT` / `SCALE_UP_TIMEOUT` | `300` / `600` | Model ready vs longest scale/job waits (seconds) |
| `E2E_EVENTUALLY_STANDARD`, etc. | see README | Optional `Eventually` timeouts and poll intervals (`E2E_EVENTUALLY_*`, `E2E_EVENTUALLY_POLL*`) |

Deploy-time knobs: `SKIP_HELM_REPO_UPDATE`, optional `KV_SPARE_TRIGGER` / `QUEUE_SPARE_TRIGGER` (Makefile patches the `wva-saturation-scaling-config` ConfigMap when set) — see **Install script tuning** above.

For running multiple test runs in parallel, use [multi-controller isolation](../user-guide/multi-controller-isolation.md) (`CONTROLLER_INSTANCE`).

## Test Comparison Matrix

| Aspect | Unit Tests | Integration Tests | E2E Consolidated (Kind emulated) | E2E Consolidated (OpenShift) |
|--------|-----------|-------------------|----------------------------------|------------------------------|
| **Speed** | Fast (<1min) | Fast (1-3min) | Smoke 5-10min / Full 15-25min | Smoke 5-10min / Full 15-25min |
| **Isolation** | Complete | Partial | Complete (Kind) | Shared cluster |
| **GPU Required** | No | No | No (emulated) | Yes (real) |
| **Infrastructure** | None | envtest | Kind + infra-only deploy | OpenShift + infra-only deploy |
| **Realism** | Low | Medium | High (emulated) | Production-like |
| **CI-Friendly** | Yes | Yes | Yes | Requires cluster |
| **Local Dev** | Yes | Yes | Yes | Cluster access needed |

## Continuous Integration

### GitHub Actions Workflows

WVA uses GitHub Actions for automated testing:

#### PR Checks Workflow

**File**: `.github/workflows/ci-pr-checks.yaml`

Runs on every pull request:
- Linting (golangci-lint)
- Unit tests
- Build verification
- Code coverage reporting

#### E2E Tests Workflow

E2E workflows run the **consolidated suite** (`test/e2e/`):
- **Smoke** (`make test-e2e-smoke`): Fast validation on Kind (or OpenShift when `ENVIRONMENT=openshift`)
- **Full** (`make test-e2e-full`): Full suite; typically run with infra deployed via `deploy-e2e-infra` or equivalent

Infrastructure is deployed in **infra-only** mode (WVA + llm-d only); tests create VA, HPA, and model services dynamically.

#### OpenShift E2E Tests Workflow

**File**: `.github/workflows/ci-e2e-openshift.yaml`

Runs OpenShift E2E tests on dedicated cluster:
- Triggered manually or on specific labels
- Deploys PR-specific namespaces
- On failure: automatically scales down GPU workloads while preserving debugging resources (VA, HPA, logs)
- Smart resource management frees GPUs for other PRs without manual intervention

#### Triggering E2E via PR Comments

You can trigger E2E runs by commenting on a PR:

| Comment | Workflow | Who can use | Effect |
|--------|----------|-------------|--------|
| **`/ok-to-test`** | `ci-pr-checks.yaml` + `ci-e2e-openshift.yaml` | Users with write access | Runs the **full** Kind E2E suite **and** the OpenShift E2E (GPU) run on this PR. On fork PRs, this is required before OpenShift E2E can run. |
| **`/retest`** | `ci-e2e-openshift.yaml` | Users with write access | **OpenShift E2E only:** Re-run the OpenShift E2E workflow (e.g. after a failure, flake, or new commits). Same workflow as `/ok-to-test`, different trigger intent. |

**When to use:**

- **`/ok-to-test`**: Comment this when you want the full E2E suite to run on your PR. It triggers both the full Kind E2E (instead of smoke only) and the OpenShift E2E. By default, PRs only run smoke E2E on Kind.
- **`/retest`**: Use to re-run only the OpenShift E2E workflow (e.g. after a failure or new commits).
- **Fork PRs**: If you opened a PR from a fork, OpenShift E2E will not run until a maintainer or admin comments **`/ok-to-test`**. Branch protection should require the **e2e-openshift** status check so merge stays blocked until that run passes (the gate check is intentionally green on fork PRs to avoid a false failure that cannot be updated from upstream).

### Running CI Tests Locally

#### Simulate PR Checks

```bash
# Run linter
make lint

# Run unit tests
make test

# Build binary
make build

# Build Docker image
make docker-build
```

#### Simulate E2E CI

```bash
# Deploy infra (infra-only), then run smoke or full suite
make deploy-e2e-infra
make test-e2e-smoke
# or: make test-e2e-full

# One-shot: create cluster, deploy infra, run smoke tests
make test-e2e-smoke-with-setup
```

## Testing Best Practices

### General Guidelines

1. **Write tests first** (TDD approach) - helps design better APIs
2. **Test behavior, not implementation** - tests should survive refactoring
3. **Keep tests independent** - tests should not depend on each other
4. **Use meaningful assertions** - prefer specific matchers over generic equality
5. **Clean up resources** - always clean up in AfterEach/AfterAll blocks
6. **Document complex tests** - add comments explaining non-obvious test logic

### Ginkgo/Gomega Patterns

#### Use Descriptive Test Names

```go
// ✅ Good
It("should recommend scale-up when KV cache exceeds 70% threshold", func() {
    // ...
})

// ❌ Bad
It("should work", func() {
    // ...
})
```

#### Use Eventually for Async Operations

```go
// ✅ Good - waits for condition to become true
Eventually(func(g Gomega) {
    va := &v1alpha1.VariantAutoscaling{}
    err := k8sClient.Get(ctx, key, va)
    g.Expect(err).NotTo(HaveOccurred())
    g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 2))
}, timeout, interval).Should(Succeed())

// ❌ Bad - may fail due to timing
va := &v1alpha1.VariantAutoscaling{}
k8sClient.Get(ctx, key, va)
Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 2))
```

#### Use Consistently for Stable State

```go
// Verify replicas remain stable for 30 seconds
Consistently(func(g Gomega) {
    deploy := &appsv1.Deployment{}
    err := k8sClient.Get(ctx, key, deploy)
    g.Expect(err).NotTo(HaveOccurred())
    g.Expect(*deploy.Spec.Replicas).To(Equal(int32(2)))
}, 30*time.Second, 5*time.Second).Should(Succeed())
```

#### Use Ordered for Sequential Tests

```go
var _ = Describe("Scale-up workflow", Ordered, func() {
    // These tests run in order and share state
    It("should create resources", func() { /* ... */ })
    It("should detect saturation", func() { /* ... */ })
    It("should scale up", func() { /* ... */ })
})
```

### Test Organization

#### Use Contexts for Grouping

```go
var _ = Describe("Optimizer", func() {
    Context("with single variant", func() {
        It("should optimize for cost", func() { /* ... */ })
        It("should meet SLO requirements", func() { /* ... */ })
    })

    Context("with multiple variants", func() {
        It("should prefer cheaper variant", func() { /* ... */ })
        It("should distribute load evenly", func() { /* ... */ })
    })
})
```

#### Use BeforeEach/AfterEach for Setup/Teardown

```go
var _ = Describe("Controller", func() {
    var (
        namespace string
        cleanup   func()
    )

    BeforeEach(func() {
        namespace = "test-" + randomString()
        // Setup test resources
    })

    AfterEach(func() {
        // Clean up test resources
        if cleanup != nil {
            cleanup()
        }
    })

    It("should reconcile resources", func() {
        // Test implementation
    })
})
```

## Debugging Tests

### Debugging Unit Tests

```bash
# Run with verbose output
go test -v ./internal/engines/pipeline/...

# Enable Ginkgo trace
go test -v ./internal/queueing/analyzer/... -ginkgo.trace

# Run with debugger (delve)
dlv test ./internal/controller/... -- -ginkgo.v
```

### Debugging E2E Tests

#### View Test Logs

```bash
# Consolidated E2E suite (smoke or full)
go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter="smoke"
go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter="full && !flaky" -timeout 35m
```

#### Access Test Cluster

```bash
# For Kind E2E tests (default cluster name: kind-wva-gpu-cluster or from CLUSTER_NAME)
export KUBECONFIG=~/.kube/config   # or path from kind get kubeconfig
kubectl get pods -A
kubectl logs -n workload-variant-autoscaler-system deployment/controller-manager

# For OpenShift E2E tests
oc get pods -A
oc logs -n workload-variant-autoscaler-system deployment/controller-manager
```

#### Keep Cluster Alive After Failure

```bash
# Run tests; on failure, cluster is kept by default (DELETE_CLUSTER=false)
make test-e2e-smoke-with-setup
# Inspect: kubectl get all -A
# To delete cluster after: DELETE_CLUSTER=true make test-e2e-smoke-with-setup
# Or manually: kind delete cluster --name <CLUSTER_NAME>
```

### Common Test Failures

#### Test Times Out

**Symptoms**: Test hangs or exceeds timeout

**Possible causes**:
- Controller stuck in reconciliation loop
- HPA not reading metrics
- Prometheus not scraping metrics
- Resource quotas preventing pod creation

**Debugging steps**:
```bash
kubectl get events -A --sort-by='.lastTimestamp'
kubectl describe va -n <namespace>
kubectl logs -n workload-variant-autoscaler-system deployment/controller-manager
```

#### Metrics Not Available

**Symptoms**: External metrics API returns empty or error

**Possible causes**:
- KEDA operator not running
- Metrics not being scraped
- Incorrect metric labels or selectors

**Debugging steps**:
```bash
# Check external metrics API
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/<namespace>/wva_desired_replicas" | jq

# Check Prometheus
kubectl port-forward -n workload-variant-autoscaler-monitoring svc/prometheus-operated 9090:9090
# Query: wva_desired_replicas{variant_name="..."}
```

#### Deployment Not Scaling

**Symptoms**: HPA shows desired replicas but deployment doesn't scale

**Possible causes**:
- Resource constraints (CPU/memory/GPU)
- Node capacity exceeded
- PDB preventing scale-up
- Deployment controller issues

**Debugging steps**:
```bash
kubectl describe hpa -n <namespace>
kubectl describe deploy -n <namespace>
kubectl get events -n <namespace> --sort-by='.lastTimestamp'
kubectl top nodes
```

## Performance / Benchmarking

Performance and benchmarking scenarios (traffic generation, throughput/latency measurement, scale-up latency, etc.) are intentionally **out of scope** for `test/e2e/` so that e2e remains deterministic. Use the project’s dedicated benchmarking tooling/workflows instead.

## Test Coverage Goals

Current coverage targets:
- **Unit tests**: 70%+ code coverage
- **Integration tests**: All controller operations
- **E2E tests**: Critical user workflows

### Checking Coverage

```bash
# Generate coverage report
go test -coverprofile=coverage.out ./...

# View summary
go tool cover -func=coverage.out

# Generate HTML report
go tool cover -html=coverage.out -o coverage.html

# View in browser
open coverage.html  # macOS
xdg-open coverage.html  # Linux
```

## Contributing Tests

When contributing, please ensure:

1. ✅ **All new code has unit tests** - aim for 70%+ coverage
2. ✅ **Critical paths have integration tests** - especially controller logic
3. ✅ **New features have E2E tests** - validate end-to-end behavior
4. ✅ **Tests are documented** - explain what is being tested and why
5. ✅ **Tests follow naming conventions** - use descriptive names
6. ✅ **Tests clean up resources** - no resource leaks in tests
7. ✅ **Tests pass locally before pushing** - run `make test` and `make test-e2e-smoke` (or `make test-e2e-full`)

## Related Documentation

- [Development Guide](development.md) - Development environment setup
- [E2E Test Suite README](../../test/e2e/README.md) - Consolidated E2E tests (Kind, OpenShift, infra-only setup)
- [Contributing Guide](../../CONTRIBUTING.md) - Contribution guidelines
