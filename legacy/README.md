# legacy/ — Workload-Variant-Autoscaler (deprecated)

Everything in this directory belongs to the **Workload-Variant-Autoscaler (WVA)**
controller, which is deprecated. The directory exists only to **stage its
removal**: nothing here is maintained, and it will be deleted in a future
release.

If you need WVA, use the [`release-0.9`](https://github.com/llm-d/llm-d-autoscaling/tree/release-0.9)
branch ([`v0.9.0`](https://github.com/llm-d/llm-d-autoscaling/releases/tag/v0.9.0)),
not this copy.

Autoscaling on `main` is driven by KEDA — see the [repository README](../README.md).

## What is here

| Path | Contents |
|------|----------|
| `cmd/`, `internal/`, `test/` | the controller's Go module (`go.mod` lives here too) |
| `config/` | kustomize manifests, RBAC, samples (HPA and KEDA-on-WVA) |
| `deploy/` | install scripts, Kind emulator, CI infrastructure scripts, dashboards |
| `docs/` | the former design docs, developer guide, proposals, and plans |
| `hack/` | benchmark and analysis scripts for the controller |
| `Dockerfile`, `Makefile`, `PROJECT` | controller build and release tooling |
| `.github/` | the controller's CI workflows and release-tracking template |
| `CONTRIBUTING.md` | the former WVA contributor guide |

## Caveats

- **CI is not wired up.** GitHub Actions only reads `.github/workflows/` at the
  repository root, so the workflows under `legacy/.github/workflows/` are inert.
- **Tooling expects this directory as its root.** The Makefile, Dockerfile, and
  scripts use paths relative to their own tree, so run them from `legacy/`
  (`cd legacy && make build`). They are unverified after the move.
- **Do not add features here.** Fixes should go to `release-0.9`; new autoscaling
  work belongs in the KEDA blueprints and the `benchmark/` test bed.
