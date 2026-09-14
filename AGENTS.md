# Claude Code Assistant Guidelines

## Repository Scope

This repository provides **KEDA manifest blueprints** (`benchmark/config/scenarios/`)
and the **evaluation test bed** (`benchmark/`) for llm-d autoscaling. KEDA is the
autoscaling engine; there is no controller to build here.

The deprecated Workload-Variant-Autoscaler lives under `legacy/`, frozen and
staged for removal:

- Do not add features, tests, or docs there, and do not reference it from new
  work. Fixes belong on the `release-0.9` branch.
- The Go, kustomize, and `config/` conventions below apply to that tree only.
  Its Makefile, Dockerfile, and scripts run from `legacy/` as their root.

## Go Code Style

Applies to the frozen `legacy/` Go module only.

- Follow the standard Go code style and conventions. Use `gofmt` for formatting and adhere to idiomatic Go practices.
- Follow best practices from the [Effective Go](https://go.dev/doc/effective_go) guide:

### Naming Conventions
- Use **MixedCaps** or **mixedCaps** rather than underscores for multi-word names
- Package names should be short, lowercase, single-word names
- Getters don't use "Get" prefix (use `obj.Name()` not `obj.GetName()`)
- Interface names use "-er" suffix for single-method interfaces (e.g., `Reader`, `Writer`)

### Formatting
- Use `gofmt` for consistent formatting (tabs for indentation, spaces for alignment)
- Line length: no strict limit, but keep lines reasonable
- Group related declarations together

### Error Handling
- Return errors as the last return value
- Check errors immediately after the call
- Provide context with `fmt.Errorf` and error wrapping

### Logging
- Use `ctrl.Log` for structured logging
- Keep log fields consistent and meaningful
- Avoid logging sensitive data

### Documentation
- Every exported name should have a doc comment
- Start comments with the name being described
- Use complete sentences

### Concurrency
- Share memory by communicating; don't communicate by sharing memory
- Use channels to orchestrate goroutines
- Always handle goroutine cleanup and cancellation properly

### Project Structure
- Keep packages focused and cohesive
- Avoid circular dependencies
- Place tests in `*_test.go` files

### Headers and Licenses

- Do not include license headers in source files.

## Documentation

Prefer placing documentation in the `docs/` directory.

There are 3 main types of documentation targeting different audiences:

1. **Developer Documentation** - For contributors and maintainers of this project
   - Architecture decisions
   - Development setup and workflow
   - Contributing guidelines
   - usually in the `docs/developer-guide/` subdirectory

2. **Agents plans** - For AI agent plans.
   - in the `docs/plans/<area>/` subdirectory, where `<area>` is the specific area of the project (e.g., `engine`, `installation`, `monitoring`, etc.)

3. **Superpowers specs** - For superpowers specification
   - in the `docs/superpowers/<area>/` subdirectory, where `<area>` is the specific area of the project (e.g., `engine`, `installation`, `monitoring`, etc.)


## Kustomize / Config File Naming

All files under `legacy/config/` follow the `(<app>-)?<kind>.yaml` pattern:

- **`<kind>`** is the Kubernetes kind as a single lowercase word — no hyphens.
  - `ConfigMap` → `configmap`, `ClusterRole` → `clusterrole`, `ClusterRoleBinding` → `clusterrolebinding`, `RoleBinding` → `rolebinding`, `ServiceAccount` → `serviceaccount`, `ServiceMonitor` → `servicemonitor`, `CustomResourceDefinition` → `customresourcedefinition`
- **`<app>`** is an optional kebab-case prefix identifying the component or logical scope of the resource.
- The app prefix comes **first**, the kind suffix comes **last**: e.g., `saturation-scaling-configmap.yaml`, `epp-metrics-serviceaccount.yaml`, `manager-clusterrole.yaml`.
- `kustomization.yaml` and other kustomize-internal files are exempt.
- File names use hyphens (`-`), never underscores.

## Evaluation and Testing

- Evaluate scaling-strategy changes with the `benchmark/` test bed: add the
  variant under `benchmark/config/scenarios/staging/<guide>/` next to
  `baseline.yaml`, run it, and report the comparison. See `benchmark/README.md`.
- `make lint-scripts` syntax-checks the shell scripts outside `legacy/`.
- The controller's `make test` / `make test-e2e-*` targets live in
  `legacy/Makefile` and are unmaintained.
- **Never use images from docker.io in e2e tests.** All container images must use fully-qualified registry paths (e.g., `registry.k8s.io/`, `quay.io/`, or a private registry). Do not rely on Docker Hub as a default registry.

## Deprecation

- the helm chart has been removed. Do not re-introduce it or add helm chart features.
