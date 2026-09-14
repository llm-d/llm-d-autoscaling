# Developer Guide

Guide for developers contributing to Workload-Variant-Autoscaler.

## Development Environment Setup

### Prerequisites

- Go 1.26.0+
- Docker 17.03+
- kubectl 1.32.0+
- Kind (for local testing)
- Make

### Initial Setup

1. **Clone the repository:**

   ```bash
   git clone https://github.com/llm-d/llm-d-workload-variant-autoscaler.git 
   cd llm-d-workload-variant-autoscaler
   ```

2. **Install dependencies:**

   ```bash
   go mod download
   ```

3. **Install development tools:**

   ```bash
   make setup-envtest
   make controller-gen
   make kustomize
   ```

## Project Structure

```bash
workload-variant-autoscaler/
├── api/v1alpha1/          # CRD definitions
├── cmd/                   # Main application entry points
├── config/                # Kubernetes manifests
│   ├── base/             # Base manifests (crd/, manager/, monitoring/, rbac/)
│   ├── components/       # Reusable kustomize components (namespace-scoped/)
│   ├── overlays/         # Environment overlays (cluster-scoped/, namespace-scoped/)
│   └── samples/          # Example resources (hpa/, keda/, simulator/)
├── deploy/                # Deployment scripts
│   ├── kind-emulator/    # Local Kind cluster with GPU emulation
├── docs/                  # Documentation
├── internal/              # Private application code
│   ├── actuator/         # Metric emission & scaling
│   ├── collector/        # Metrics collection
│   ├── config/           # Internal configuration
│   ├── constants/        # Application constants
│   ├── controller/       # Controller implementation
│   ├── datastore/        # Data storage abstractions
│   ├── discovery/        # Resource discovery
│   ├── engines/          # Scaling engines (saturation, scale-from-zero)
│   ├── indexers/         # Kubernetes indexers
│   ├── interfaces/       # Interface definitions
│   ├── logging/          # Logging utilities
│   ├── metrics/          # Metrics definitions
│   ├── queueing/         # Queue theory models & config (analyzer, config)
│   ├── saturation/       # Saturation detection logic
│   └── utils/            # Utility functions
├── test/                  # Tests
│   ├── e2e/                  # E2E tests (consolidated suite: Kind, OpenShift)
│   └── utils/                 # Test utilities
```

## Development Workflow

### Running Locally

#### Option 1: Outside the cluster

```bash
# Run the controller on your machine (connects to configured cluster)
make run
```

#### Option 2: In a Kind cluster

**One-shot — create cluster and deploy WVA + EPP + monitoring:**

```bash
CREATE_CLUSTER=true make deploy-e2e-infra
```

This creates a Kind cluster with emulated GPUs, then deploys the WVA controller, llm-d EPP (GAIE standalone), Prometheus stack, and KEDA (external metrics). No model service is included.

If you already have a cluster running, omit `CREATE_CLUSTER=true`:

```bash
make deploy-e2e-infra
```

**Step 2 (optional) — Deploy a simulator model service:**

The simulator samples are under `config/samples/simulator/` and support three configurations:

```bash
# Decode only
kubectl apply -k config/samples/simulator/nodeSelector/decode/

# Prefill only
kubectl apply -k config/samples/simulator/nodeSelector/prefill/

# Both prefill and decode (disaggregated serving)
kubectl apply -k config/samples/simulator/nodeSelector/disaggregated/
```

Each configuration creates a Deployment (using `llm-d-inference-sim:v0.9.0`), a Service, a ServiceMonitor, and an HPA in the `llm-d-sim` namespace. The HPAs carry `llm-d.ai/managed: "true"` annotations so WVA discovers them without a VariantAutoscaling CRD and begins emitting `wva_desired_replicas` metrics.

To clean up the simulator, use the same path you applied:

```bash
kubectl delete -k config/samples/simulator/nodeSelector/decode/
# or
kubectl delete -k config/samples/simulator/nodeSelector/disaggregated/
```

To tear down the cluster entirely:

```bash
make destroy-kind-cluster
```

### Making Changes

1. **Create a feature branch:**

   ```bash
   git checkout -b feature/my-feature
   ```

2. **Make your changes**

3. **Generate code if needed:**

   ```bash
   # After modifying CRDs
   make manifests generate
   ```

4. **Run unit tests:**

   ```bash
   make test
   ```

5. **Run linter:**

   ```bash
   make lint
   ```

## Building and Testing

### Build the Binary

```bash
make build
```

The binary will be in `bin/manager`.

### Build Docker Image

```bash
make docker-build IMG=<your-registry>/wva-controller:tag
```

### Push Docker Image

```bash
make docker-push IMG=<your-registry>/wva-controller:tag
```

### Multi-architecture Build

```bash
PLATFORMS=linux/arm64,linux/amd64 make docker-buildx IMG=<your-registry>/wva-controller:tag
```

## Testing

### Unit Tests

```bash
# Run all unit tests
make test

# Run specific package tests
go test ./internal/controller/...

# With coverage
go test -cover ./...
```

### E2E Tests

WVA has a single consolidated E2E suite (`test/e2e/`) that runs on Kind (emulated) or OpenShift/kubernetes. Deploy infrastructure in infra-only mode first, then run tests.

**Location**: `test/e2e/`

```bash
# Smoke tests (Kind, ~5-10 min)
make test-e2e-smoke

# Full suite (Kind)
make test-e2e-full

# OpenShift: set KUBECONFIG and ENVIRONMENT=openshift first
export ENVIRONMENT=openshift
make test-e2e-smoke
# or make test-e2e-full

# Run specific tests
FOCUS="Basic VA lifecycle" make test-e2e-smoke
```

See [Testing Guide](testing.md) and [E2E Test Suite README](../../test/e2e/README.md) for infra-only setup and configuration. For OpenShift, set `ENVIRONMENT=openshift` and use the same targets.

### Manual Testing

1. **Deploy infrastructure to Kind cluster:**

   ```bash
   CREATE_CLUSTER=true make deploy-e2e-infra
   ```

2. **Deploy simulator model service:**

   ```bash
   kubectl apply -k config/samples/simulator/nodeSelector/disaggregated/
   ```

3. **Monitor controller logs:**

   ```bash
   kubectl logs -n workload-variant-autoscaler-system \
     deployment/controller-manager -f
   ```

## Code Generation

### After Modifying CRDs

```bash
# Generate deepcopy, CRD manifests, and RBAC
make manifests generate
```

## Debugging

### VSCode Launch Configuration

Create `.vscode/launch.json`:

```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Debug Controller",
      "type": "go",
      "request": "launch",
      "mode": "auto",
      "program": "${workspaceFolder}/cmd/main.go",
      "env": {
        "KUBECONFIG": "${env:HOME}/.kube/config"
      },
      "args": []
    }
  ]
}
```

### Debugging in Cluster

```bash
# Build debug image
go build -gcflags="all=-N -l" -o bin/manager cmd/main.go

# Deploy and attach debugger (e.g., Delve)
```

### Viewing Controller Logs

```bash
kubectl logs -n workload-variant-autoscaler-system \
  -l control-plane=controller-manager --tail=100 -f
```

## Common Development Tasks

### Adding a New Field to CRD

1. Modify `api/v1alpha1/variantautoscaling_types.go`
2. Run `make manifests generate`
3. Update tests
4. Update user documentation

### Adding a New Metric

1. Define metric in `internal/metrics/metrics.go`
2. Emit metric from appropriate controller location
3. Update Prometheus integration docs
4. Add to Grafana dashboards (if applicable)

### Modifying Optimization Logic

1. Update code in `internal/engines/pipeline/` or `internal/queueing/analyzer/`
2. Add/update unit tests
3. Run `make test`
4. Update design documentation if algorithm changes

## Documentation

### Updating Documentation

After code changes, update relevant docs in:

- `docs/user-guide/` - User-facing changes
- `docs/design/` - Architecture/design changes
- `docs/integrations/` - Integration guide updates

**Note**: Documentation updates are partially automated via the [Update Docs workflow](agentic-workflows.md#update-docs). The workflow analyzes code changes and creates draft PRs with documentation updates.

### Testing Documentation

Verify all commands and examples in documentation work:

```bash
# Test installation steps
# Test configuration examples
# Test all code snippets
```

## GitHub Agentic Workflows

The repository uses AI-powered workflows to automate documentation updates, workflow creation, and debugging. These workflows are powered by the `gh-aw` CLI extension.

Key workflows:
- **Update Docs**: Automatically updates documentation on every push to main
- **Create Agentic Workflow**: Interactive workflow designer
- **Debug Agentic Workflow**: Workflow debugging assistant

See [Agentic Workflows Guide](agentic-workflows.md) for detailed information on working with these automation tools.

## Release Process

See the [Release Process](release-process.md) guide for how to cut a release. It covers:

- Pre-release checklist (changelog, optional version bumps, upstream pins)
- Creating the tag and GitHub Release (which triggers the image build)
- What runs automatically: Docker image build and push to GHCR
- Post-release (required): update the llm-d [workload-autoscaling](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling) guide to the new WVA version
- Enabling other team members to perform releases (permissions, secrets, documentation)

## Getting Help

- Check [CONTRIBUTING.md](../../CONTRIBUTING.md)
- Review existing code and tests
- Ask in GitHub Discussions
- Attend community meetings

## Useful Commands

```bash
# Format code
make fmt

# Vet code
make vet

# Run linter
make lint

# Fix linting issues
make lint-fix

# Clean build artifacts
rm -rf bin/ dist/

# Reset Kind cluster
make destroy-kind-cluster
make create-kind-cluster
```

## Next Steps

- Review [Code Style Guidelines](../../CONTRIBUTING.md#coding-guidelines)
- Check out [Good First Issues](https://github.com/llm-d/llm-d-workload-variant-autoscaler/labels/good%20first%20issue)

---

## Known Setup Issues

### InferencePool CRD not found during `make deploy-e2e-infra`

**Symptom:**
```
Error: no matches for kind "InferencePool" in version "inference.networking.x-k8s.io/v1alpha2"
ensure CRDs are installed first
```

**Cause:** The Gateway API Inference Extension CRDs are applied by `deploy/install-epp.sh` but
the Kubernetes API server may not have finished registering them before the next `kubectl apply`
runs.

**Fix:** Re-run `make deploy-e2e-infra` — it is idempotent and the CRDs will be registered by
the second run. If the error persists, wait a few seconds and retry:

```bash
make deploy-e2e-infra
```
