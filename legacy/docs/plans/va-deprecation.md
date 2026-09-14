# Deprecate & Remove the VariantAutoscaling CRD

## Context

WVA currently requires operators to create a `VariantAutoscaling` (VA) CRD per model variant. The CRD duplicates information already present on the scaling target (Deployment, ScaledObject, HPA) and makes WVA a mandatory integration point. The proposal at `docs/proposals/deprecate-va-crd.md` argues the CRD is unnecessary: WVA can discover work by watching KEDA ScaledObjects / HPAs with the `llm-d.ai/managed: "true"` annotation, and variant metadata (model ID, cost, etc.) can live as annotations on those same objects.

The internal pipeline — Prometheus collection, saturation/queueing analyzers, cost-aware optimization, exposure of `wva_desired_replicas` — does **not** change. Only discovery changes.

This plan implements the proposal end-to-end across the three phases it defines:

- **Phase 1** — Dual-mode: add annotation-based discovery without breaking existing VA CRD users.
- **Phase 2** — Deprecation: mark the VA CRD deprecated, point users at annotations (docs-only migration; no CLI).
- **Phase 3** — Removal: delete CRD manifests, VA reconciler, RBAC, and public API types. Keep `VariantAutoscaling` as an **internal struct** to minimize churn in the collector/analyzer/optimizer. The `HPAReconciler` and `ScaledObjectReconciler` from Phase 1 remain.

Watch surfaces in Phase 1: both **KEDA ScaledObject** and **HPA** (any object bearing `llm-d.ai/managed: "true"` with a `scaleTargetRef`).

---

## Critical files

**API / CRD**
- `api/v1alpha1/variantautoscaling_types.go` — spec/status types
- `api/v1alpha1/conditions.go`, `api/v1alpha1/groupversion_info.go`, `api/v1alpha1/zz_generated.deepcopy.go`
- `config/crd/bases/llmd.ai_variantautoscalings.yaml`
- `charts/workload-variant-autoscaler/crds/llmd.ai_variantautoscalings.yaml`

**Controller**
- `internal/controller/variantautoscaling_controller.go` — existing VA reconciler (untouched in Phase 1)
- `internal/controller/hpa_reconciler.go` — new HPAReconciler (created in 1.5)
- `internal/controller/scaledobject_reconciler.go` — new ScaledObjectReconciler (created in 1.5)
- `internal/controller/indexers/indexers.go`
- `cmd/main.go` (scheme registration L83-84, reconciler wiring L506-516)

**Discovery entry points (the key refactor surface)**
- `internal/utils/variant.go` L77-91 — `ActiveVariantAutoscaling` / `InactiveVariantAutoscaling`
- `internal/utils/variant.go` L93+ — `filterVariantsByScaleTargetAccessor`, `readyVariantAutoscalings`, `GroupVariantAutoscalingByModel`
- `internal/engines/saturation/engine.go` L259 — single call site of `ActiveVariantAutoscaling`
- `internal/datastore/datastore.go` — `NamespaceTrack("VariantAutoscaling", …)` for ConfigMap discovery

**RBAC**
- `config/rbac/role.yaml` L104-128
- `config/rbac/variantautoscaling_{admin,editor,viewer}_role.yaml`
- Matching chart templates under `charts/workload-variant-autoscaler/templates/rbac/`

**Samples / Chart**
- `config/samples/variantautoscaling-*.yaml`, `config/samples/{keda,hpa}/va.yaml`
- `charts/workload-variant-autoscaler/templates/variantautoscaling.yaml`
- `charts/workload-variant-autoscaler/values.yaml` (`va.*`, `llmd.modelID`, `llmd.scaleTargetKind`)

**Tests**
- `test/e2e/fixtures/va_builder.go`, `test/e2e/fixtures/wait.go`, `test/utils/e2eutils.go`
- `test/e2e/{smoke,limiter,saturation_analyzer_path,scale_from_zero}_test.go`
- `internal/controller/variantautoscaling_controller_test.go`

**Docs**
- `docs/user-guide/{crd-reference,hpa-integration,keda-integration,configuration,LeaderWorkerSet-support}.md`
- `docs/design/controller-behavior.md`
- `docs/proposals/deprecate-va-crd.md` (the proposal itself)

---

## Phase 1 — Dual-Mode Discovery

Add annotation-based discovery that runs alongside the existing VA CRD reconciler. Two new dedicated reconcilers (`HPAReconciler`, `ScaledObjectReconciler`) handle namespace tracking; both paths feed the same internal pipeline.

### 1.1 Annotation schema package

Create `internal/annotations/annotations.go` with the constants the proposal calls out:

```go
const (
    Managed         = "llm-d.ai/managed"          // required, "true"
    ModelID         = "llm-d.ai/model-id"         // required
    VariantCost     = "llm-d.ai/variant-cost"     // optional, default "1.0"
    // forward-compat: gpus-per-replica, scale-to-zero-retention, role — not needed
    // until the corresponding features are wired; declare as constants now to lock the names.
)
```

Also a small `Parse(obj metav1.Object) (*SyntheticVA, error)` helper that validates `Managed == "true"` and `ModelID != ""`, parses `VariantCost` (defaulting to `"1.0"`), and returns a struct mirroring the fields needed downstream.

### 1.2 Synthesize `VariantAutoscaling` from annotations

Add `internal/utils/variant_fromannotations.go` with a function that builds an in-memory `*wvav1alpha1.VariantAutoscaling` from a `ScaledObject` or `HPA`:

- `metadata.namespace` / `metadata.name` — from the ScaledObject/HPA
- `spec.scaleTargetRef` — copied from ScaledObject `.spec.scaleTargetRef` or HPA `.spec.scaleTargetRef`
- `spec.modelID`, `spec.variantCost` — from annotations
- `spec.{min,max}Replicas` — from ScaledObject `{minReplicaCount, maxReplicaCount}` or HPA `{minReplicas, maxReplicas}`
- Status conditions — synthesized as `OptimizationReady=true` so `readyVariantAutoscalings` filtering continues to work

Tag the synthesized object (e.g. an annotation on the in-memory copy, or a sibling map) so downstream code can tell "synthetic" from "real VA" — needed by the status-writeback logic in 1.4.

### 1.3 Extend discovery functions

Modify `internal/utils/variant.go`:

- Add `ActiveVariantsFromAnnotations(ctx, client)` that lists `ScaledObjectList` (kedav1alpha1) **and** `HorizontalPodAutoscalerList` (autoscalingv2), filters by `Managed == "true"`, and runs each through the synthesizer from 1.2.
- Extend `ActiveVariantAutoscaling` (and `Inactive…`) to concatenate CRD-sourced + annotation-sourced results, deduplicating by `(namespace, scaleTargetRef)` with CRD winning a tie (explicit beats implicit during dual-mode).
- Keep `GroupVariantAutoscalingByModel` unchanged — it already works on any `VariantAutoscaling` slice.

The single engine entry point at `internal/engines/saturation/engine.go:259` continues to call `ActiveVariantAutoscaling` — no change there.

### 1.4 Status writeback handles both sources

Today the VA reconciler patches `va.status.desiredOptimizedAlloc` after the engine runs. Synthetic (annotation-sourced) variants have no VA object to patch.

- In `applySaturationDecisions` (and equivalent paths), check the synthetic tag from 1.2.
  - Synthetic → skip the status patch; the `wva_desired_replicas` metric emission is the sole output (matches the proposal's "Actuation: None — KEDA/HPA reads the metric").
  - CRD-sourced → unchanged behavior.
- Verify the metric labels (`variant_name`, `exported_namespace`) match for both sources so KEDA triggers don't need to know which mode produced the metric.

### 1.5 New reconcilers for HPA and ScaledObject

Following the project convention of one reconciler per Kubernetes object kind, create two new dedicated reconcilers:

**`internal/controller/hpa_reconciler.go`** — `HPAReconciler`
- Watches `autoscalingv2.HorizontalPodAutoscaler` with `AnnotatedScalerPredicate()`.
- `Reconcile` body: if object is being deleted or no longer managed (`!annotations.IsManaged`), call `datastore.NamespaceUntrack`; otherwise call `datastore.NamespaceTrack`. Return `ctrl.Result{}` with no requeue — namespace tracking is the sole side-effect.
- `+kubebuilder:rbac` markers for `autoscaling/horizontalpodautoscalers` (get;list;watch) live in this file.
- Registered unconditionally in `cmd/main.go` alongside the other reconcilers.

**`internal/controller/scaledobject_reconciler.go`** — `ScaledObjectReconciler`
- Watches `kedav1alpha1.ScaledObject` with `AnnotatedScalerPredicate()`.
- Same `Reconcile` body as `HPAReconciler` (track/untrack).
- `+kubebuilder:rbac` markers for `keda.sh/scaledobjects` (get;list;watch) live in this file.
- Registered in `cmd/main.go` **only when** `kedaEnabled == true` (KEDA CRD detected at startup), mirroring the existing `lwsEnabled` pattern.

Add a KEDA detection step in `cmd/main.go` (mirroring the existing `lwsEnabled` check) if not already present. If KEDA CRDs aren't present, skip `ScaledObjectReconciler` registration but still register `HPAReconciler`.

### 1.6 RBAC for watching

The `+kubebuilder:rbac` markers live in the new reconciler files created in 1.5 (`hpa_reconciler.go` and `scaledobject_reconciler.go`) rather than in `variantautoscaling_controller.go`. Remove the existing markers from `variantautoscaling_controller.go` if they were added there, regenerate `config/rbac/role.yaml` via `make manifests`, and update the Helm chart template (`charts/workload-variant-autoscaler/templates/rbac/role.yaml`) to mirror the change.

### 1.7 Kustomization samples for annotation-based scaling

Add side-by-side kustomize overlays that consume `wva_desired_replicas` without a VA CRD. These are the user-facing artifacts Phase 1 produces — they prove both integration points work and become the starting point for migration docs in Phase 2.

- `config/samples/keda/annotations/` — a new overlay containing:
  - `scaledobject.yaml`: KEDA `ScaledObject` with `llm-d.ai/managed: "true"`, `llm-d.ai/model-id`, `llm-d.ai/variant-cost` annotations; `spec.triggers[0]` a Prometheus trigger querying `wva_desired_replicas{variant_name=…,exported_namespace=…}` (matches the example in `docs/proposals/deprecate-va-crd.md`).
  - `deployment.yaml`: sample target Deployment.
  - `kustomization.yaml`: bases + common labels.
- `config/samples/hpa/annotations/` — equivalent overlay with an `HorizontalPodAutoscaler` carrying the same annotations and an external-metric source reading `wva_desired_replicas` via prometheus-adapter (reuse `prometheus-adapter-values.yaml`).
- Update the existing `config/samples/keda/kustomization.yaml` and `config/samples/hpa/kustomization.yaml` to offer both the legacy (VA-based) and the new (annotation-based) overlays as selectable components.
- Verify both overlays render (`kubectl kustomize config/samples/keda/annotations`) and apply cleanly on a kind cluster running the Phase 1 WVA build.

These overlays are the templates Phase 2 will promote to be the default and Phase 3 will leave as the only sample.

### 1.8 Tests

- **Unit**: `internal/annotations/annotations_test.go` (parsing/defaulting/validation), `internal/utils/variant_fromannotations_test.go` (synthesis from fake ScaledObject/HPA).
- **Integration**: add `internal/controller/hpa_reconciler_test.go` and `internal/controller/scaledobject_reconciler_test.go` covering the track/untrack logic; extend `internal/controller/variantautoscaling_controller_test.go` with a case that creates only an annotated ScaledObject (no VA) and asserts the engine produces `wva_desired_replicas` with correct labels.
- **E2E (new annotation path)**: add `test/e2e/annotation_discovery_test.go` mirroring `smoke_test.go` but driven by annotations and backed by the kustomize overlays from 1.7. Uses the existing `test/e2e/fixtures/hpa_builder.go` and `test/e2e/fixtures/scaled_object_builder.go` (with annotation helpers added) instead of `va_builder.go`. Keep existing VA-based e2e tests green.
- **E2E (extend existing tests)**: update the existing e2e tests (`smoke_test.go`, `limiter_test.go`, `saturation_analyzer_path_test.go`, `scale_from_zero_test.go`) to exercise both VA-based and annotation-based (HPA / KEDA ScaledObject) setup paths for the same scenarios, so dual-mode is validated end-to-end across all test suites and not only in the new annotation-discovery test.

### 1.9 Documentation update

Add annotation-based discovery coverage to the existing user-facing docs without removing any CRD content (that is Phase 2's job).

### 1.10 Well-lit path update

Update WVA well-lit path to use the annotation-based samples instead of the VA CRD samples. This ensures new users see the new path first and start migrating.

---

## Phase 2 — Deprecation

Phase 1 must be fully merged before Phase 2 starts.

### 2.1 Mark CRD deprecated

In `api/v1alpha1/variantautoscaling_types.go`, add the kubebuilder deprecation marker:

```go
// +kubebuilder:deprecatedversion:warning="llmd.ai/v1alpha1 VariantAutoscaling is deprecated; migrate to annotations on ScaledObject/HPA. See docs/user-guide/migrating-from-va-crd.md"
```

Regenerate CRD manifests (`make manifests`) and the chart copy at `charts/workload-variant-autoscaler/crds/…`. kube-apiserver will then emit a warning on every operation against the CRD.

### 2.2 Log deprecation warning in the reconciler

In `VariantAutoscalingReconciler.Reconcile`, log a once-per-VA `logger.Info("VariantAutoscaling CRD is deprecated, migrate to annotations")` (guarded by a cache so it doesn't spam every reconcile). Emit a Kubernetes Event of type `Warning` reason `Deprecated` on the VA the first time it's seen.

### 2.3 Migration documentation (docs only)

Create `docs/user-guide/migrating-from-va-crd.md` covering:

- Before/after side-by-side (from the proposal's table)
- A `kubectl` / jq recipe for patching annotations onto an existing ScaledObject:
  ```
  kubectl annotate scaledobject <name> \
    llm-d.ai/managed=true \
    llm-d.ai/model-id=<id> \
    llm-d.ai/variant-cost=<cost>
  kubectl delete variantautoscaling <name>
  ```
- HPA equivalent
- Validation steps (check `wva_desired_replicas` metric still present, check deprecation warning appears on VA).

Update `docs/user-guide/{hpa-integration,keda-integration}.md` to lead with annotations, keep CRD usage as an "(deprecated)" subsection.

### 2.4 Switch samples to annotations

- Update `config/samples/keda/scaledobject.yaml` to carry the full annotation set; delete `config/samples/keda/va.yaml`.
- Update `config/samples/hpa/hpa.yaml` similarly; delete `config/samples/hpa/va.yaml`.
- Replace `config/samples/variantautoscaling-*.yaml` with annotation-based equivalents. Keep one `variantautoscaling-legacy.yaml` sample showing the deprecated path for reference until Phase 3.

### 2.5 Chart default flip

In `charts/workload-variant-autoscaler/values.yaml`, mark `va.enabled` as deprecated and default it to `false`. The `charts/workload-variant-autoscaler/templates/variantautoscaling.yaml` template should be gated and emit a `helm.sh/hook` notice when rendered. Add chart values for annotation-based samples (or document them in the chart README).

### 2.6 Release notes

Add a `CHANGELOG.md` / release-notes entry announcing the deprecation and linking to the migration doc.

---

## Phase 3 — Removal

Phase 2 must have shipped in at least one release (supplying users a deprecation window) before Phase 3 starts.

### 3.1 Move `VariantAutoscaling` types to an internal package

Keep the type as the in-memory representation for the pipeline, but stop publishing it as a CRD API:

- Move `api/v1alpha1/variantautoscaling_types.go`, `conditions.go`, `zz_generated.deepcopy.go` to `internal/variant/types.go` (drop `groupversion_info.go`; remove kubebuilder markers and `runtime.Object` boilerplate not needed internally).
- Drop the `TypeMeta` / `ObjectMeta` embedding if unused, or keep a trimmed struct that exposes only the fields the pipeline reads (`Namespace`, `Name`, `Spec.ModelID`, `Spec.VariantCost`, `Spec.ScaleTargetRef`, `Spec.Min/MaxReplicas`, `Status.DesiredOptimizedAlloc`).
- Update all callers (`s/wvav1alpha1.VariantAutoscaling/variant.Instance/`).

### 3.2 Delete CRD manifests and scheme wiring

- Delete `config/crd/bases/llmd.ai_variantautoscalings.yaml` and any kustomization references.
- Delete `charts/workload-variant-autoscaler/crds/llmd.ai_variantautoscalings.yaml`.
- Remove `llmdVariantAutoscalingV1alpha1.AddToScheme(scheme)` from `cmd/main.go` (and the import).

### 3.3 Delete the VA reconciler

- Remove `internal/controller/variantautoscaling_controller.go` and its test.
- Remove `internal/controller/indexers/indexers.go` if it only indexed VAs.
- Keep `internal/controller/hpa_reconciler.go` and `internal/controller/scaledobject_reconciler.go` from Phase 1.5 (now the only discovery controllers).
- Remove `controller.NewVariantAutoscalingReconciler(...)` wiring from `cmd/main.go`.

### 3.4 Delete RBAC

- Drop the VA blocks from `config/rbac/role.yaml` (L104-128 and corresponding subresource rules).
- Delete `config/rbac/variantautoscaling_{admin,editor,viewer}_role.yaml` and their chart templates.

### 3.5 Delete samples and chart template

- Delete `config/samples/variantautoscaling-legacy.yaml` (the reference sample kept during Phase 2).
- Delete `charts/workload-variant-autoscaler/templates/variantautoscaling.yaml`.
- Remove `va.*` keys from `values.yaml`; annotation examples stay as the default deployment path.

### 3.6 Delete CRD-specific tests and fixtures

- Remove `test/e2e/fixtures/va_builder.go` (replace usages with a `ScaledObjectBuilder` / `HPABuilder` in the same directory).
- Update every e2e test that still builds VAs to build annotated ScaledObjects/HPAs instead; delete any test case that was purely validating CRD behavior.

### 3.7 Docs cleanup

- Delete `docs/user-guide/crd-reference.md` (and the `hack/crd-doc-gen` configuration entry that generates it).
- Remove the "(deprecated)" CRD subsections added in Phase 2 from the integration docs.
- Update `docs/design/controller-behavior.md` to describe the annotation-based discovery as the only path.
- Update `docs/proposals/deprecate-va-crd.md` status from Draft to Implemented.

---

## Verification

**Per phase**

- Phase 1: `make test` green; new unit + integration tests pass; e2e `annotation_discovery_test.go` produces `wva_desired_replicas` with matching labels to the VA-driven `smoke_test.go`; existing VA-driven e2e remains green.
- Phase 2: `kubectl apply -f <legacy-va.yaml>` prints the `deprecated version` warning; reconciler logs / Events show the deprecation notice; migration doc recipe works end-to-end on a kind cluster.
- Phase 3: `make build` succeeds with no `api/v1alpha1` package; `kubectl get crds | grep variantautoscalings` returns empty after `helm upgrade`; all e2e tests pass using annotated ScaledObjects/HPAs.

**End-to-end smoke (across phases)**

1. Deploy WVA + KEDA + vLLM simulator via `make test-e2e-smoke`.
2. Apply an annotated ScaledObject (no VA).
3. Confirm: `kubectl get --raw /metrics | grep wva_desired_replicas` shows a series for the variant, and the KEDA trigger reads it and scales the Deployment.

**Regression surface to watch**

- Namespace tracking in `datastore` — both paths must register their namespaces for namespace-local `wva-saturation-scaling-config` lookups to keep working.
- Metric label cardinality — `variant_name` / `exported_namespace` values must be identical across modes so existing Prometheus queries and KEDA triggers don't break.
- LeaderWorkerSet scale targets — annotation discovery must treat `scaleTargetRef.kind == LeaderWorkerSet` the same as VA did (uses `lwsEnabled` flag).
