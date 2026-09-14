# Pod → Managed Scaler Locator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `PodLocator` under `internal/collector/locator/` that resolves a pod (by name) to its managed HPA / ScaledObject via owner-walking, with an LRU memoization of the pod → top-level scale-target step, and wire it into the metrics collector so the `llm-d.ai/variant` label becomes optional for Deployment / LWS layouts.

**Architecture:** Two reader paths — `apiReader` for pod-side owner-chain walks (no controller-runtime cache for Pod / ReplicaSet / Deployment / LWS), and the cached client for two new field indexes (`HPAByScaleTargetKey`, `ScaledObjectByScaleTargetKey`) gated on `llm-d.ai/managed: "true"`. A 4096-entry size-only LRU caches `pod → top-level scale-target` (immutable per Kubernetes ownerReference rules). Scaler lookup is always fresh through the field index. The collector's existing `buildInstanceKey` falls back to `Locate` only when `llm_d_ai_variant` is empty.

**Tech Stack:** Go, controller-runtime, KEDA `kedav1alpha1`, LeaderWorkerSet `lwsv1`, `hashicorp/golang-lru/v2`, ginkgo/gomega + envtest for integration tests.

**Source spec:** `docs/superpowers/specs/2026-06-11-pod-to-managed-scaler-locator-design.md`

---

## File Structure

**New files:**
- `internal/collector/locator/locator.go` — `PodLocator` interface, `ManagedScaler` type, `Locate` / `LocateByVariant` implementations
- `internal/collector/locator/walk.go` — `chainNode` type, `walkOwnersUp`, `scaleTargetKindSupported`
- `internal/collector/locator/cache.go` — LRU wrapper with `podKey` keys
- `internal/collector/locator/locator_test.go` — table-driven tests with envtest fake client
- `internal/controller/indexers/hpa.go` — `HPAByScaleTargetKey`, `HPAByScaleTargetIndexFunc`, `FindHPAForScaleTarget`
- `internal/controller/indexers/scaledobject.go` — `ScaledObjectByScaleTargetKey`, `ScaledObjectByScaleTargetIndexFunc`, `FindSOForScaleTarget`
- `internal/controller/indexers/variantautoscaling.go` — receives the existing VA-specific helpers, kept identical

**Modified files:**
- `internal/controller/indexers/indexers.go` — slimmed to `SetupIndexes` + shared `scaleTargetIndexKey` helper; registers all three indexes
- `internal/collector/replica_metrics.go` — `NewReplicaMetricsCollector` accepts a `locator.PodLocator`; `buildInstanceKey` falls back to `Locate` when `llm_d_ai_variant` is empty
- `internal/engines/saturation/engine.go` — `NewEngine` constructs a `PodLocator` and threads it into the collector
- `cmd/main.go` — no direct edit; `saturation.NewEngine` does the construction internally
- `config/rbac/role.yaml` and `charts/workload-variant-autoscaler/templates/rbac/manager-clusterrole.yaml` — add Pod / ReplicaSet read grants (Deployment / LWS already present via existing reconcilers)
- `docs/design/controller-behavior.md` — flip the "Prerequisites" section to mark `llm-d.ai/variant` required only for shadow pods
- `go.mod`, `go.sum` — add `github.com/hashicorp/golang-lru/v2`

Each file owns one concern. The locator package is fully self-contained under `internal/collector/locator/`. The existing `internal/controller/indexers/indexers.go` is split per-resource; the existing `FindVAFor*` API survives unchanged in `variantautoscaling.go`.

---

## Task 1: Add the `golang-lru/v2` dependency

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/hashicorp/golang-lru/v2@v2.0.7
go mod tidy
```

- [ ] **Step 2: Verify build still works**

```bash
go build ./...
```
Expected: build succeeds with no errors.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "deps: add github.com/hashicorp/golang-lru/v2 for locator memoization"
```

---

## Task 2: Split `indexers.go` — move VA helpers to `variantautoscaling.go` (no behaviour change)

**Files:**
- Create: `internal/controller/indexers/variantautoscaling.go`
- Modify: `internal/controller/indexers/indexers.go`

This is a pure refactor: existing tests must continue to pass with no changes.

- [ ] **Step 1: Create the new file with the existing VA-specific contents**

Create `internal/controller/indexers/variantautoscaling.go`:

```go
/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0
*/

package indexers

import (
	"context"
	"fmt"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// VAScaleTargetKey is the index field name for looking up VariantAutoscalings by their scale target.
	VAScaleTargetKey = ".spec.scaleTargetRef.nsAPIVersionKindName"
)

// VAScaleTargetIndexFunc is the index function for VariantAutoscaling by scale target.
func VAScaleTargetIndexFunc(o client.Object) []string {
	va := o.(*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)
	if va.Spec.ScaleTargetRef.Kind == "" || va.Spec.ScaleTargetRef.Name == "" {
		return nil
	}
	return []string{scaleTargetIndexKey(va.Namespace, va.Spec.ScaleTargetRef)}
}

// FindVAForScaleTarget returns the VariantAutoscaling that targets the given scale resource.
func FindVAForScaleTarget(ctx context.Context, c client.Client, ref autoscalingv2.CrossVersionObjectReference, namespace string) (*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, error) {
	var vaList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
	if err := c.List(ctx, &vaList,
		client.InNamespace(namespace),
		client.MatchingFields{VAScaleTargetKey: scaleTargetIndexKey(namespace, ref)},
	); err != nil {
		return nil, fmt.Errorf("failed to list VariantAutoscalings for %s %s/%s: %w", ref.Kind, namespace, ref.Name, err)
	}
	if len(vaList.Items) == 0 {
		return nil, nil
	}
	if len(vaList.Items) > 1 {
		return nil, fmt.Errorf("multiple VariantAutoscalings found for %s %s/%s", ref.Kind, namespace, ref.Name)
	}
	return &vaList.Items[0], nil
}

// FindVAForDeployment returns the VariantAutoscaling that targets a Deployment with the given name.
func FindVAForDeployment(ctx context.Context, c client.Client, deploymentName, namespace string) (*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, error) {
	return FindVAForScaleTarget(ctx, c, autoscalingv2.CrossVersionObjectReference{
		APIVersion: constants.DeploymentAPIVersion,
		Kind:       constants.DeploymentKind,
		Name:       deploymentName,
	}, namespace)
}

// FindVAForLeaderWorkerSet returns the VariantAutoscaling that targets a LeaderWorkerSet with the given name.
func FindVAForLeaderWorkerSet(ctx context.Context, c client.Client, leaderWorkerSetName, namespace string) (*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, error) {
	return FindVAForScaleTarget(ctx, c, autoscalingv2.CrossVersionObjectReference{
		APIVersion: constants.LeaderWorkerSetAPIVersion,
		Kind:       constants.LeaderWorkerSetKind,
		Name:       leaderWorkerSetName,
	}, namespace)
}
```

- [ ] **Step 2: Trim `indexers.go` to shared scaffolding**

Replace `internal/controller/indexers/indexers.go` with:

```go
/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0
*/

package indexers

import (
	"context"
	"fmt"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// scaleTargetIndexKey returns the composite index key for a scale target reference.
// Format: Namespace/APIVersion/Kind/Name (e.g., "default/apps/v1/Deployment/my-app").
// Shared by all per-resource index files (variantautoscaling.go, hpa.go, scaledobject.go).
func scaleTargetIndexKey(namespace string, ref autoscalingv2.CrossVersionObjectReference) string {
	if ref.APIVersion == "" {
		switch ref.Kind {
		case constants.DeploymentKind:
			ref.APIVersion = constants.DeploymentAPIVersion
		case constants.LeaderWorkerSetKind:
			ref.APIVersion = constants.LeaderWorkerSetAPIVersion
		default:
			logger := ctrl.LoggerFrom(context.TODO())
			logger.V(logging.DEBUG).Info("APIVersion not specified for scale target; defaulting to apps/v1", "kind", ref.Kind, "name", ref.Name)
			ref.APIVersion = constants.DeploymentAPIVersion
		}
	}
	return fmt.Sprintf("%s/%s/%s/%s", namespace, ref.APIVersion, ref.Kind, ref.Name)
}

// SetupIndexes registers custom field indexes with the manager's cache.
// Currently only the VariantAutoscaling index is registered here; HPA and
// ScaledObject indexes are added in Tasks 7 and 8.
func SetupIndexes(ctx context.Context, mgr manager.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}, VAScaleTargetKey, VAScaleTargetIndexFunc); err != nil {
		return fmt.Errorf("failed to set up index by scale target for VariantAutoscaling: %w", err)
	}
	return nil
}
```

- [ ] **Step 3: Run existing indexer tests**

```bash
go test ./internal/controller/indexers/... -v -count=1
```
Expected: PASS (no behaviour change).

- [ ] **Step 4: Run full controller test suite to confirm nothing depends on file layout**

```bash
go test ./internal/controller/... -count=1
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/indexers/
git commit -m "refactor(indexers): split VA index into its own file"
```

---

## Task 3: Add the HPA scale-target index

**Files:**
- Create: `internal/controller/indexers/hpa.go`
- Modify: `internal/controller/indexers/indexers.go` (register the new index)
- Modify: `internal/controller/indexers/indexers_test.go` (extend existing test setup, see Step 4)

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/indexers/indexers_test.go`:

```go
var _ = Describe("HPA index", func() {
	It("returns a managed HPA for its Deployment scaleTargetRef", func() {
		ctx := context.Background()
		ns := "ns-hpa-1"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "managed-hpa",
				Namespace:   ns,
				Annotations: map[string]string{"llm-d.ai/managed": "true"},
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					APIVersion: "apps/v1", Kind: "Deployment", Name: "target-deploy",
				},
				MaxReplicas: 10,
			},
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		got, err := indexers.FindHPAForScaleTarget(ctx, k8sClient, autoscalingv2.CrossVersionObjectReference{
			APIVersion: "apps/v1", Kind: "Deployment", Name: "target-deploy",
		}, ns)
		Expect(err).ToNot(HaveOccurred())
		Expect(got).ToNot(BeNil())
		Expect(got.Name).To(Equal("managed-hpa"))
	})

	It("ignores HPAs without llm-d.ai/managed=true", func() {
		ctx := context.Background()
		ns := "ns-hpa-2"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "unmanaged", Namespace: ns},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					APIVersion: "apps/v1", Kind: "Deployment", Name: "target-deploy-2",
				},
				MaxReplicas: 5,
			},
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		got, err := indexers.FindHPAForScaleTarget(ctx, k8sClient, autoscalingv2.CrossVersionObjectReference{
			APIVersion: "apps/v1", Kind: "Deployment", Name: "target-deploy-2",
		}, ns)
		Expect(err).ToNot(HaveOccurred())
		Expect(got).To(BeNil())
	})
})
```

- [ ] **Step 2: Run the test to verify it fails to compile**

```bash
go test ./internal/controller/indexers/... -v -count=1
```
Expected: FAIL — `undefined: indexers.FindHPAForScaleTarget`.

- [ ] **Step 3: Create `hpa.go` with the index and helper**

```go
/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0
*/

package indexers

import (
	"context"
	"fmt"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HPAByScaleTargetKey indexes managed HorizontalPodAutoscalers by their
// spec.scaleTargetRef. Index value: scaleTargetIndexKey(namespace, ref).
const HPAByScaleTargetKey = ".spec.scaleTargetRef.managedHPA"

// scaleTargetKindSupported reports whether a scaleTargetRef.Kind is one of the
// kinds WVA's locator handles. Today: Deployment and LeaderWorkerSet.
func scaleTargetKindSupported(kind string) bool {
	return kind == constants.DeploymentKind || kind == constants.LeaderWorkerSetKind
}

// HPAByScaleTargetIndexFunc indexes managed HPAs (llm-d.ai/managed=true) by
// their scaleTargetRef. Returns no entries for unmanaged HPAs or unsupported
// scale-target kinds.
func HPAByScaleTargetIndexFunc(o client.Object) []string {
	hpa := o.(*autoscalingv2.HorizontalPodAutoscaler)
	if !annotations.IsManaged(hpa) {
		return nil
	}
	ref := hpa.Spec.ScaleTargetRef
	if ref.Name == "" || !scaleTargetKindSupported(ref.Kind) {
		return nil
	}
	return []string{scaleTargetIndexKey(hpa.Namespace, ref)}
}

// FindHPAForScaleTarget returns the managed HPA targeting the given scale
// resource, or nil if none. Errors when more than one managed HPA targets
// the same resource.
func FindHPAForScaleTarget(ctx context.Context, c client.Client, ref autoscalingv2.CrossVersionObjectReference, namespace string) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	var list autoscalingv2.HorizontalPodAutoscalerList
	if err := c.List(ctx, &list,
		client.InNamespace(namespace),
		client.MatchingFields{HPAByScaleTargetKey: scaleTargetIndexKey(namespace, ref)},
	); err != nil {
		return nil, fmt.Errorf("list managed HPAs for %s %s/%s: %w", ref.Kind, namespace, ref.Name, err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	if len(list.Items) > 1 {
		return nil, fmt.Errorf("multiple managed HPAs target %s %s/%s", ref.Kind, namespace, ref.Name)
	}
	return &list.Items[0], nil
}
```

- [ ] **Step 4: Register the index in `SetupIndexes`**

Edit `internal/controller/indexers/indexers.go`. Replace the `SetupIndexes` body:

```go
func SetupIndexes(ctx context.Context, mgr manager.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}, VAScaleTargetKey, VAScaleTargetIndexFunc); err != nil {
		return fmt.Errorf("failed to set up index by scale target for VariantAutoscaling: %w", err)
	}
	if err := mgr.GetFieldIndexer().IndexField(ctx, &autoscalingv2.HorizontalPodAutoscaler{}, HPAByScaleTargetKey, HPAByScaleTargetIndexFunc); err != nil {
		return fmt.Errorf("failed to set up index by scale target for HPA: %w", err)
	}
	return nil
}
```

Add the import for `autoscalingv2 "k8s.io/api/autoscaling/v2"` if not already present.

- [ ] **Step 5: Run the test to verify it passes**

```bash
go test ./internal/controller/indexers/... -v -count=1 -run "HPA index"
```
Expected: PASS for both cases.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/indexers/
git commit -m "feat(indexers): add HPAByScaleTargetKey index for managed HPAs"
```

---

## Task 4: Add the ScaledObject scale-target index

**Files:**
- Create: `internal/controller/indexers/scaledobject.go`
- Modify: `internal/controller/indexers/indexers.go` (register the new index, conditional on KEDA scheme registered)
- Modify: `internal/controller/indexers/indexers_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/indexers/indexers_test.go`:

```go
var _ = Describe("ScaledObject index", func() {
	It("returns a managed ScaledObject for its Deployment scaleTargetRef", func() {
		ctx := context.Background()
		ns := "ns-so-1"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

		so := &kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "managed-so",
				Namespace:   ns,
				Annotations: map[string]string{"llm-d.ai/managed": "true"},
			},
			Spec: kedav1alpha1.ScaledObjectSpec{
				ScaleTargetRef: &kedav1alpha1.ScaleTarget{
					APIVersion: "apps/v1", Kind: "Deployment", Name: "so-deploy",
				},
			},
		}
		Expect(k8sClient.Create(ctx, so)).To(Succeed())

		got, err := indexers.FindSOForScaleTarget(ctx, k8sClient, autoscalingv2.CrossVersionObjectReference{
			APIVersion: "apps/v1", Kind: "Deployment", Name: "so-deploy",
		}, ns)
		Expect(err).ToNot(HaveOccurred())
		Expect(got).ToNot(BeNil())
		Expect(got.Name).To(Equal("managed-so"))
	})
})
```

Add the import for `kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"` to the test file if not already present.

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/controller/indexers/... -v -count=1 -run "ScaledObject index"
```
Expected: FAIL — `undefined: indexers.FindSOForScaleTarget`.

- [ ] **Step 3: Create `scaledobject.go`**

```go
/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0
*/

package indexers

import (
	"context"
	"fmt"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ScaledObjectByScaleTargetKey indexes managed KEDA ScaledObjects by their
// spec.scaleTargetRef. Index value: scaleTargetIndexKey(namespace, ref).
const ScaledObjectByScaleTargetKey = ".spec.scaleTargetRef.managedSO"

// ScaledObjectByScaleTargetIndexFunc indexes managed ScaledObjects
// (llm-d.ai/managed=true) by their scaleTargetRef.
func ScaledObjectByScaleTargetIndexFunc(o client.Object) []string {
	so := o.(*kedav1alpha1.ScaledObject)
	if !annotations.IsManaged(so) {
		return nil
	}
	if so.Spec.ScaleTargetRef == nil || so.Spec.ScaleTargetRef.Name == "" {
		return nil
	}
	kind := so.Spec.ScaleTargetRef.Kind
	if kind == "" {
		kind = constants.DeploymentKind
	}
	if !scaleTargetKindSupported(kind) {
		return nil
	}
	apiVersion := so.Spec.ScaleTargetRef.APIVersion
	ref := autoscalingv2.CrossVersionObjectReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       so.Spec.ScaleTargetRef.Name,
	}
	return []string{scaleTargetIndexKey(so.Namespace, ref)}
}

// FindSOForScaleTarget returns the managed ScaledObject targeting the given
// scale resource, or nil if none. Errors when more than one managed
// ScaledObject targets the same resource.
func FindSOForScaleTarget(ctx context.Context, c client.Client, ref autoscalingv2.CrossVersionObjectReference, namespace string) (*kedav1alpha1.ScaledObject, error) {
	var list kedav1alpha1.ScaledObjectList
	if err := c.List(ctx, &list,
		client.InNamespace(namespace),
		client.MatchingFields{ScaledObjectByScaleTargetKey: scaleTargetIndexKey(namespace, ref)},
	); err != nil {
		return nil, fmt.Errorf("list managed ScaledObjects for %s %s/%s: %w", ref.Kind, namespace, ref.Name, err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	if len(list.Items) > 1 {
		return nil, fmt.Errorf("multiple managed ScaledObjects target %s %s/%s", ref.Kind, namespace, ref.Name)
	}
	return &list.Items[0], nil
}
```

- [ ] **Step 4: Register the index in `SetupIndexes`**

Edit `internal/controller/indexers/indexers.go`. Append to the body (after the HPA registration):

```go
	if err := mgr.GetFieldIndexer().IndexField(ctx, &kedav1alpha1.ScaledObject{}, ScaledObjectByScaleTargetKey, ScaledObjectByScaleTargetIndexFunc); err != nil {
		return fmt.Errorf("failed to set up index by scale target for ScaledObject: %w", err)
	}
```

Add the import `kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"`.

KEDA's scheme is unconditionally registered in `cmd/main.go:89`, so the index function runs even when no ScaledObject CRD is installed; in that case the informer simply won't see any objects.

- [ ] **Step 5: Run the test to verify it passes**

```bash
go test ./internal/controller/indexers/... -v -count=1 -run "ScaledObject index"
```
Expected: PASS.

- [ ] **Step 6: Run the full indexers test suite**

```bash
go test ./internal/controller/indexers/... -count=1
```
Expected: PASS for VA, HPA, and ScaledObject tests.

- [ ] **Step 7: Commit**

```bash
git add internal/controller/indexers/
git commit -m "feat(indexers): add ScaledObjectByScaleTargetKey index for managed ScaledObjects"
```

---

## Task 5: Define `chainNode`, `walkOwnersUp`, and supported-kind check

**Files:**
- Create: `internal/collector/locator/walk.go`
- Create: `internal/collector/locator/walk_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/collector/locator/walk_test.go`:

```go
/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0
*/

package locator

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFakeReader(objs ...runtime.Object) *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...)
}

func TestWalkOwnersUp_PodReplicaSetDeployment(t *testing.T) {
	ns := "default"
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rs",
			Namespace: ns,
			UID:       "uid-rs",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d",
				Controller: ptr.To(true),
			}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p",
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs",
				Controller: ptr.To(true),
			}},
		},
	}
	c := newFakeReader(deploy, rs, pod).Build()

	chain, err := walkOwnersUp(context.Background(), c, pod, ns, defaultMaxDepth)
	if err != nil {
		t.Fatalf("walkOwnersUp: %v", err)
	}
	want := []chainNode{
		{Namespace: ns, APIVersion: "v1", Kind: "Pod", Name: "p"},
		{Namespace: ns, APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs"},
		{Namespace: ns, APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
	}
	if len(chain) != len(want) {
		t.Fatalf("chain length = %d, want %d (chain=%v)", len(chain), len(want), chain)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Errorf("chain[%d] = %v, want %v", i, chain[i], want[i])
		}
	}
}

func TestWalkOwnersUp_StopsAtMaxDepth(t *testing.T) {
	ns := "default"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p",
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs",
				Controller: ptr.To(true),
			}},
		},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rs", Namespace: ns, UID: "uid-rs",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d",
				Controller: ptr.To(true),
			}},
		},
	}
	c := newFakeReader(pod, rs).Build()

	chain, err := walkOwnersUp(context.Background(), c, pod, ns, 1)
	if err != nil {
		t.Fatalf("walkOwnersUp: %v", err)
	}
	// maxDepth=1 means start + 1 owner; further hops are not taken.
	if len(chain) != 2 {
		t.Errorf("len=%d, want 2", len(chain))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails to compile**

```bash
go test ./internal/collector/locator/... -v -count=1
```
Expected: FAIL — `undefined: walkOwnersUp`, `undefined: chainNode`, `undefined: defaultMaxDepth`.

- [ ] **Step 3: Implement `walk.go`**

Create `internal/collector/locator/walk.go`:

```go
/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0
*/

package locator

import (
	"context"
	"fmt"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// defaultMaxDepth caps walkOwnersUp. Pod → ReplicaSet → Deployment → CR → wrapper-CR
// is 5 hops in practice; 8 leaves slack while still bounding runaway chains.
const defaultMaxDepth = 8

// chainNode identifies one element of a Pod's ownerReferences chain.
type chainNode struct {
	Namespace, APIVersion, Kind, Name string
}

// scaleTargetKindSupported reports whether kind is a top-level scale-target
// kind handled by WVA's locator. Today: Deployment and LeaderWorkerSet.
func scaleTargetKindSupported(kind string) bool {
	return kind == constants.DeploymentKind || kind == constants.LeaderWorkerSetKind
}

// walkOwnersUp returns the chain [self, owner, owner-of-owner, ...] starting
// from start and stopping at:
//   - the first node with no controller ownerReference,
//   - maxDepth reached,
//   - a previously-seen node (cycle), or
//   - an unknown kind (walk stops; chain so far is returned without error).
//
// Known kinds are fetched via the supplied reader (apiReader in production,
// fake client in tests). The walk stays in the start object's namespace —
// cross-namespace ownerReferences are illegal in core/v1.
func walkOwnersUp(ctx context.Context, r client.Reader, start client.Object, namespace string, maxDepth int) ([]chainNode, error) {
	chain := make([]chainNode, 0, maxDepth+1)
	seen := make(map[chainNode]bool, maxDepth+1)

	current := nodeOf(start, namespace)
	chain = append(chain, current)
	seen[current] = true

	owner := metav1.GetControllerOf(start)
	for i := 0; i < maxDepth && owner != nil; i++ {
		next := chainNode{
			Namespace:  namespace,
			APIVersion: owner.APIVersion,
			Kind:       owner.Kind,
			Name:       owner.Name,
		}
		if seen[next] {
			return nil, fmt.Errorf("ownerReference cycle at %s/%s/%s", next.APIVersion, next.Kind, next.Name)
		}

		obj, ok := newTypedFor(owner)
		if !ok {
			// Unknown kind — stop walking but return what we have so far.
			break
		}
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: owner.Name}, obj); err != nil {
			if apierrors.IsNotFound(err) {
				// Owner no longer exists; chain so far is the best we can do.
				break
			}
			return nil, fmt.Errorf("get %s %s/%s: %w", owner.Kind, namespace, owner.Name, err)
		}
		chain = append(chain, next)
		seen[next] = true
		owner = metav1.GetControllerOf(obj)
	}
	return chain, nil
}

// nodeOf converts a typed object plus its namespace into a chainNode.
func nodeOf(obj client.Object, namespace string) chainNode {
	gvk := obj.GetObjectKind().GroupVersionKind()
	apiVersion := gvk.GroupVersion().String()
	kind := gvk.Kind
	if kind == "" {
		// fake clients sometimes leave TypeMeta empty; infer from concrete type.
		switch obj.(type) {
		case *corev1.Pod:
			apiVersion, kind = "v1", "Pod"
		case *appsv1.ReplicaSet:
			apiVersion, kind = "apps/v1", "ReplicaSet"
		case *appsv1.Deployment:
			apiVersion, kind = "apps/v1", "Deployment"
		case *lwsv1.LeaderWorkerSet:
			apiVersion, kind = "leaderworkerset.x-k8s.io/v1", "LeaderWorkerSet"
		}
	}
	return chainNode{
		Namespace:  namespace,
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       obj.GetName(),
	}
}

// newTypedFor returns a freshly allocated typed object matching the
// ownerReference's apiVersion/kind. Returns false for unknown kinds, which
// signals walkOwnersUp to stop.
func newTypedFor(owner *metav1.OwnerReference) (client.Object, bool) {
	switch owner.Kind {
	case "ReplicaSet":
		if owner.APIVersion == "apps/v1" {
			return &appsv1.ReplicaSet{}, true
		}
	case "Deployment":
		if owner.APIVersion == "apps/v1" {
			return &appsv1.Deployment{}, true
		}
	case "LeaderWorkerSet":
		if owner.APIVersion == "leaderworkerset.x-k8s.io/v1" {
			return &lwsv1.LeaderWorkerSet{}, true
		}
	}
	return nil, false
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/collector/locator/... -v -count=1
```
Expected: PASS for both `TestWalkOwnersUp_PodReplicaSetDeployment` and `TestWalkOwnersUp_StopsAtMaxDepth`.

- [ ] **Step 5: Commit**

```bash
git add internal/collector/locator/walk.go internal/collector/locator/walk_test.go
git commit -m "feat(locator): add ownerReference chain walk"
```

---

## Task 6: Add the `podKey → chainNode` LRU cache

**Files:**
- Create: `internal/collector/locator/cache.go`
- Create: `internal/collector/locator/cache_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/collector/locator/cache_test.go`:

```go
/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0
*/

package locator

import "testing"

func TestResolutionCache_HitMissEvict(t *testing.T) {
	c, err := newResolutionCache(2)
	if err != nil {
		t.Fatalf("newResolutionCache: %v", err)
	}

	a := podKey{Namespace: "ns", Name: "a"}
	b := podKey{Namespace: "ns", Name: "b"}
	x := podKey{Namespace: "ns", Name: "x"}

	c.add(a, chainNode{Namespace: "ns", Kind: "Deployment", Name: "da"})
	c.add(b, chainNode{Namespace: "ns", Kind: "Deployment", Name: "db"})

	if got, hit := c.get(a); !hit || got.Name != "da" {
		t.Fatalf("a: hit=%v got=%v", hit, got)
	}
	// Adding x evicts the least-recently-used entry; a was just used so b should evict.
	c.add(x, chainNode{Namespace: "ns", Kind: "Deployment", Name: "dx"})
	if _, hit := c.get(b); hit {
		t.Errorf("b should have been evicted")
	}
	if _, hit := c.get(a); !hit {
		t.Errorf("a should still be present")
	}
}

func TestResolutionCache_NegativeEntry(t *testing.T) {
	c, err := newResolutionCache(8)
	if err != nil {
		t.Fatalf("newResolutionCache: %v", err)
	}
	k := podKey{Namespace: "ns", Name: "p"}
	c.add(k, chainNode{}) // negative entry (zero value)

	got, hit := c.get(k)
	if !hit {
		t.Fatalf("expected hit")
	}
	if got != (chainNode{}) {
		t.Errorf("got=%v, want zero chainNode", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/collector/locator/... -v -count=1 -run "Resolution"
```
Expected: FAIL — `undefined: newResolutionCache`, `undefined: podKey`.

- [ ] **Step 3: Implement `cache.go`**

Create `internal/collector/locator/cache.go`:

```go
/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0
*/

package locator

import (
	"fmt"

	lru "github.com/hashicorp/golang-lru/v2"
)

// defaultCacheSize is the size of the pod → top-level scale-target LRU.
// One entry is roughly 100 B of strings + a chainNode value; 4096 entries
// fit in well under a MB and cover typical clusters where the chain-node
// universe is a small multiple of variant count.
const defaultCacheSize = 4096

// podKey identifies a pod for cache purposes. Pods are uniquely named
// within a namespace, which is sufficient to key the immutable
// pod → top-level scale-target relation.
type podKey struct {
	Namespace, Name string
}

// resolutionCache memoizes pod → top-level scale-target resolution. The
// scale-target → managed scaler step is NOT cached; it always runs through
// the field index so annotation toggles and scaleTargetRef edits take
// effect on the next Locate call.
//
// Eviction is size-only LRU. No TTL, no watch-driven invalidation: the
// pod → top-level resource relation is immutable per Kubernetes ownerReference
// rules (controllers cannot be changed after creation), so cached entries
// are correct for the lifetime of the cached pod.
type resolutionCache struct {
	c *lru.Cache[podKey, chainNode]
}

func newResolutionCache(size int) (*resolutionCache, error) {
	if size <= 0 {
		return nil, fmt.Errorf("cache size must be > 0, got %d", size)
	}
	c, err := lru.New[podKey, chainNode](size)
	if err != nil {
		return nil, err
	}
	return &resolutionCache{c: c}, nil
}

// get returns the cached top-level scale-target for the pod. The hit boolean
// is true even for negative entries (target == zero chainNode means the pod
// has no scaler-eligible ancestor).
func (r *resolutionCache) get(k podKey) (chainNode, bool) {
	return r.c.Get(k)
}

func (r *resolutionCache) add(k podKey, target chainNode) {
	r.c.Add(k, target)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/collector/locator/... -v -count=1 -run "Resolution"
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/collector/locator/cache.go internal/collector/locator/cache_test.go
git commit -m "feat(locator): add pod-key LRU resolution cache"
```

---

## Task 7: Implement `PodLocator` interface, `Locate`, and `LocateByVariant`

**Files:**
- Create: `internal/collector/locator/locator.go`
- Create: `internal/collector/locator/locator_test.go`

- [ ] **Step 1: Write failing tests for the happy path**

Create `internal/collector/locator/locator_test.go`:

```go
/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0
*/

package locator_test

import (
	"context"
	"testing"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller/indexers"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		kedav1alpha1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("scheme add: %v", err)
		}
	}
	return s
}

func newClients(t *testing.T, objs ...runtime.Object) (cached, apiReader client.Client) {
	t.Helper()
	scheme := newScheme(t)
	build := func() client.Client {
		return fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(objs...).
			WithIndex(&autoscalingv2.HorizontalPodAutoscaler{}, indexers.HPAByScaleTargetKey, indexers.HPAByScaleTargetIndexFunc).
			WithIndex(&kedav1alpha1.ScaledObject{}, indexers.ScaledObjectByScaleTargetKey, indexers.ScaledObjectByScaleTargetIndexFunc).
			Build()
	}
	return build(), build()
}

func TestLocate_DeploymentChainHitsManagedHPA(t *testing.T) {
	ns := "default"
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rs", Namespace: ns, UID: "uid-rs",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d", Controller: ptr.To(true)}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs", Controller: ptr.To(true)}},
		},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: "h", Namespace: ns,
			Annotations: map[string]string{"llm-d.ai/managed": "true"},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
			MaxReplicas:    5,
		},
	}

	cached, apiReader := newClients(t, deploy, rs, pod, hpa)
	loc, err := locator.New(cached, apiReader)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := loc.Locate(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got == nil || got.HPA == nil {
		t.Fatalf("got=%v, want HPA=h", got)
	}
	if got.HPA.Name != "h" {
		t.Errorf("HPA.Name=%q, want h", got.HPA.Name)
	}
}

func TestLocate_UnmanagedReturnsNil(t *testing.T) {
	ns := "default"
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: ns, UID: "uid-rs",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d", Controller: ptr.To(true)}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs", Controller: ptr.To(true)}}},
	}
	cached, apiReader := newClients(t, deploy, rs, pod)
	loc, _ := locator.New(cached, apiReader)
	got, err := loc.Locate(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != nil {
		t.Errorf("got=%v, want nil", got)
	}
}

func TestLocate_PodNotFound(t *testing.T) {
	cached, apiReader := newClients(t)
	loc, _ := locator.New(cached, apiReader)
	got, err := loc.Locate(context.Background(), "default", "missing")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != nil {
		t.Errorf("got=%v, want nil", got)
	}
}

func TestLocateByVariant_HPA(t *testing.T) {
	ns := "default"
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns,
			Annotations: map[string]string{"llm-d.ai/managed": "true"}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
			MaxReplicas:    1,
		},
	}
	cached, apiReader := newClients(t, hpa)
	loc, _ := locator.New(cached, apiReader)
	got, err := loc.LocateByVariant(context.Background(), ns, "v")
	if err != nil {
		t.Fatalf("LocateByVariant: %v", err)
	}
	if got == nil || got.HPA == nil || got.HPA.Name != "v" {
		t.Fatalf("got=%v, want HPA=v", got)
	}
}

func TestLocateByVariant_UnmanagedHPA(t *testing.T) {
	ns := "default"
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
			MaxReplicas:    1,
		},
	}
	cached, apiReader := newClients(t, hpa)
	loc, _ := locator.New(cached, apiReader)
	got, err := loc.LocateByVariant(context.Background(), ns, "v")
	if err != nil {
		t.Fatalf("LocateByVariant: %v", err)
	}
	if got != nil {
		t.Errorf("got=%v, want nil for unmanaged HPA", got)
	}
}

func TestLocateByVariant_AmbiguousHPAndSO(t *testing.T) {
	ns := "default"
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns,
			Annotations: map[string]string{"llm-d.ai/managed": "true"}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
			MaxReplicas:    1,
		},
	}
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns,
			Annotations: map[string]string{"llm-d.ai/managed": "true"}},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
		},
	}
	cached, apiReader := newClients(t, hpa, so)
	loc, _ := locator.New(cached, apiReader)
	if _, err := loc.LocateByVariant(context.Background(), ns, "v"); err == nil {
		t.Errorf("expected ambiguity error, got nil")
	}
}

func TestLocate_CacheHitOnSecondCall(t *testing.T) {
	ns := "default"
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: ns, UID: "uid-rs",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d", Controller: ptr.To(true)}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs", Controller: ptr.To(true)}}}}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: ns,
			Annotations: map[string]string{"llm-d.ai/managed": "true"}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
			MaxReplicas:    1,
		},
	}

	cached, apiReader := newClients(t, deploy, rs, pod, hpa)
	loc, _ := locator.New(cached, apiReader)

	// Warm the cache.
	if _, err := loc.Locate(context.Background(), ns, "p"); err != nil {
		t.Fatalf("first Locate: %v", err)
	}

	// Delete the pod from apiReader; if the cache works, the second Locate
	// must still resolve to the same HPA because the pod → Deployment step
	// is cached.
	if err := apiReader.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	got, err := loc.Locate(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("second Locate: %v", err)
	}
	if got == nil || got.HPA == nil || got.HPA.Name != "h" {
		t.Errorf("cache miss on second call: got=%v", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/collector/locator/... -v -count=1
```
Expected: FAIL — `undefined: locator.New`, `undefined: locator.PodLocator`.

- [ ] **Step 3: Implement `locator.go`**

Create `internal/collector/locator/locator.go`:

```go
/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0
*/

// Package locator resolves a pod to the managed scaler (HPA or KEDA
// ScaledObject) that controls its replica count, via ownerReferences walking
// for Deployment / LWS layouts and via the variant name for shadow-pod
// layouts. See docs/superpowers/specs/2026-06-11-pod-to-managed-scaler-locator-design.md.
package locator

import (
	"context"
	"fmt"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller/indexers"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ManagedScaler is one of the managed scaler kinds WVA recognizes.
// Exactly one of HPA / ScaledObject is non-nil on a successful Locate.
type ManagedScaler struct {
	HPA          *autoscalingv2.HorizontalPodAutoscaler
	ScaledObject *kedav1alpha1.ScaledObject
}

// PodLocator resolves pods to managed scalers. Implementations are safe
// for concurrent use.
type PodLocator interface {
	// Locate finds the managed scaler whose scale-target chain contains the
	// given pod. Returns (nil, nil) when the pod is unmanaged or when its
	// ownerReferences chain does not reach a Deployment / LeaderWorkerSet
	// (e.g. shadow pod — use LocateByVariant). Errors only on infrastructure
	// failures or invariant violations (cycle, depth exceeded, both an HPA
	// and a ScaledObject managing the same scale target).
	Locate(ctx context.Context, namespace, podName string) (*ManagedScaler, error)

	// LocateByVariant resolves the managed scaler by variant name (the
	// value of the llm_d_ai_variant metric label, equal to the scaler's
	// metadata.name). Use this for shadow-pod layouts where the pod's
	// ownerReferences chain does not reach the scaler's scaleTargetRef.
	LocateByVariant(ctx context.Context, namespace, variantName string) (*ManagedScaler, error)
}

// New constructs a PodLocator.
//
//   - cached  — controller-runtime cached client (mgr.GetClient()), used
//     only for the field-indexed list of managed HPAs / ScaledObjects.
//   - apiReader — uncached reader (mgr.GetAPIReader()), used for every
//     Pod / ReplicaSet / Deployment / LWS read in the owner-chain walk
//     and for LocateByVariant.
func New(cached, apiReader client.Reader) (PodLocator, error) {
	cache, err := newResolutionCache(defaultCacheSize)
	if err != nil {
		return nil, err
	}
	return &podLocator{
		cached:    cached,
		apiReader: apiReader,
		maxDepth:  defaultMaxDepth,
		cache:     cache,
	}, nil
}

type podLocator struct {
	cached    client.Reader
	apiReader client.Reader
	maxDepth  int
	cache     *resolutionCache
}

func (l *podLocator) Locate(ctx context.Context, namespace, podName string) (*ManagedScaler, error) {
	// Step 1: pod → top-level scale target. Immutable per Kubernetes'
	// ownerReference rules, so the result is cacheable indefinitely.
	target, hit := l.cache.get(podKey{Namespace: namespace, Name: podName})
	if !hit {
		pod := &corev1.Pod{}
		if err := l.apiReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("get pod %s/%s: %w", namespace, podName, err)
		}
		var err error
		target, err = l.resolveScaleTarget(ctx, pod, namespace)
		if err != nil {
			return nil, err
		}
		l.cache.add(podKey{Namespace: namespace, Name: podName}, target)
	}
	if target == (chainNode{}) {
		return nil, nil
	}

	// Step 2: scale target → managed scaler. NOT cached; field-index reads
	// are cheap and reflect the current annotation / scaleTargetRef state.
	return l.resolveScaler(ctx, target)
}

func (l *podLocator) LocateByVariant(ctx context.Context, namespace, variantName string) (*ManagedScaler, error) {
	if variantName == "" {
		return nil, nil
	}
	hpa, err := l.getManagedHPA(ctx, namespace, variantName)
	if err != nil {
		return nil, err
	}
	so, err := l.getManagedScaledObject(ctx, namespace, variantName)
	if err != nil {
		return nil, err
	}
	switch {
	case hpa != nil && so != nil:
		return nil, fmt.Errorf("ambiguous variant %s/%s: matched HPA and ScaledObject of the same name",
			namespace, variantName)
	case hpa != nil:
		return &ManagedScaler{HPA: hpa}, nil
	case so != nil:
		return &ManagedScaler{ScaledObject: so}, nil
	}
	return nil, nil
}

// resolveScaleTarget walks the pod's ownerReferences and returns the first
// ancestor that is a Deployment or LWS. Returns the zero chainNode if no
// such ancestor exists.
func (l *podLocator) resolveScaleTarget(ctx context.Context, pod *corev1.Pod, namespace string) (chainNode, error) {
	chain, err := walkOwnersUp(ctx, l.apiReader, pod, namespace, l.maxDepth)
	if err != nil {
		return chainNode{}, err
	}
	for _, n := range chain {
		if scaleTargetKindSupported(n.Kind) {
			return n, nil
		}
	}
	return chainNode{}, nil
}

// resolveScaler runs the field-indexed lookups for a top-level scale target.
func (l *podLocator) resolveScaler(ctx context.Context, target chainNode) (*ManagedScaler, error) {
	ref := autoscalingv2.CrossVersionObjectReference{
		APIVersion: target.APIVersion,
		Kind:       target.Kind,
		Name:       target.Name,
	}
	hpa, err := indexers.FindHPAForScaleTarget(ctx, asClient(l.cached), ref, target.Namespace)
	if err != nil {
		return nil, err
	}
	so, err := indexers.FindSOForScaleTarget(ctx, asClient(l.cached), ref, target.Namespace)
	if err != nil {
		return nil, err
	}
	switch {
	case hpa != nil && so != nil:
		return nil, fmt.Errorf("ambiguous scale target %s/%s/%s: matched HPA %q and ScaledObject %q",
			target.Namespace, target.Kind, target.Name, hpa.Name, so.Name)
	case hpa != nil:
		return &ManagedScaler{HPA: hpa}, nil
	case so != nil:
		return &ManagedScaler{ScaledObject: so}, nil
	}
	return nil, nil
}

// getManagedHPA fetches an HPA by name and returns it only if it carries
// llm-d.ai/managed=true.
func (l *podLocator) getManagedHPA(ctx context.Context, namespace, name string) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := l.cached.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, hpa); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get HPA %s/%s: %w", namespace, name, err)
	}
	if !annotations.IsManaged(hpa) {
		return nil, nil
	}
	return hpa, nil
}

func (l *podLocator) getManagedScaledObject(ctx context.Context, namespace, name string) (*kedav1alpha1.ScaledObject, error) {
	so := &kedav1alpha1.ScaledObject{}
	if err := l.cached.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, so); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get ScaledObject %s/%s: %w", namespace, name, err)
	}
	if !annotations.IsManaged(so) {
		return nil, nil
	}
	return so, nil
}

// asClient adapts a client.Reader into the client.Client expected by the
// indexers package. The locator only ever performs reads; the index
// helpers don't write.
func asClient(r client.Reader) client.Client {
	if c, ok := r.(client.Client); ok {
		return c
	}
	// In production the cached reader is a client.Client. In tests we use the
	// fake client which also implements client.Client.
	panic("locator: cached reader does not implement client.Client")
}
```

- [ ] **Step 4: Run the locator tests to verify they pass**

```bash
go test ./internal/collector/locator/... -v -count=1
```
Expected: PASS for all six tests (`TestLocate_DeploymentChainHitsManagedHPA`, `TestLocate_UnmanagedReturnsNil`, `TestLocate_PodNotFound`, `TestLocateByVariant_HPA`, `TestLocateByVariant_UnmanagedHPA`, `TestLocateByVariant_AmbiguousHPAndSO`, `TestLocate_CacheHitOnSecondCall`).

- [ ] **Step 5: Commit**

```bash
git add internal/collector/locator/locator.go internal/collector/locator/locator_test.go
git commit -m "feat(locator): add Locate and LocateByVariant"
```

---

## Task 8: Add LWS chain test (extends owner-walk coverage)

**Files:**
- Modify: `internal/collector/locator/locator_test.go`

- [ ] **Step 1: Append the LWS test**

```go
func TestLocate_LWSChain(t *testing.T) {
	ns := "default"
	lws := &lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "lws", Namespace: ns, UID: "uid-lws"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "leaderworkerset.x-k8s.io/v1", Kind: "LeaderWorkerSet",
				Name: "lws", UID: "uid-lws", Controller: ptr.To(true),
			}}},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: ns,
			Annotations: map[string]string{"llm-d.ai/managed": "true"}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "leaderworkerset.x-k8s.io/v1", Kind: "LeaderWorkerSet", Name: "lws",
			},
			MaxReplicas: 5,
		},
	}
	cached, apiReader := newClients(t, lws, pod, hpa)
	loc, _ := locator.New(cached, apiReader)
	got, err := loc.Locate(context.Background(), ns, "p")
	if err != nil || got == nil || got.HPA == nil || got.HPA.Name != "h" {
		t.Fatalf("got=%v err=%v", got, err)
	}
}
```

Add the imports `lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"` and extend `newScheme` to include `lwsv1.AddToScheme`.

- [ ] **Step 2: Run the test**

```bash
go test ./internal/collector/locator/... -v -count=1 -run TestLocate_LWSChain
```
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/collector/locator/locator_test.go
git commit -m "test(locator): cover LWS chain"
```

---

## Task 9: Wire the locator into the metrics collector

**Files:**
- Modify: `internal/collector/replica_metrics.go`

- [ ] **Step 1: Read the current `NewReplicaMetricsCollector` signature**

```bash
grep -n "func NewReplicaMetricsCollector" internal/collector/replica_metrics.go
```

- [ ] **Step 2: Add `locator.PodLocator` to the constructor and struct**

Edit `internal/collector/replica_metrics.go`. Add the import:

```go
"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
```

Add a field to `ReplicaMetricsCollector`:

```go
type ReplicaMetricsCollector struct {
	source                source.MetricsSource
	k8sClient             client.Client
	recorder              record.EventRecorder
	locator               locator.PodLocator
	metricsAvailableState map[string]bool
	mu                    sync.Mutex
}
```

Update the constructor:

```go
// NewReplicaMetricsCollector creates a new replica metrics collector.
func NewReplicaMetricsCollector(metricsSource source.MetricsSource, k8sClient client.Client, recorder record.EventRecorder, podLocator locator.PodLocator) *ReplicaMetricsCollector {
	return &ReplicaMetricsCollector{
		source:                metricsSource,
		k8sClient:             k8sClient,
		recorder:              recorder,
		locator:               podLocator,
		metricsAvailableState: make(map[string]bool),
	}
}
```

- [ ] **Step 3: Add the locator fallback in `buildInstanceKey`**

Find `buildInstanceKey` (around line 312). The closure currently captures `c.k8sClient` indirectly — it now needs `c.locator` too. Refactor it from a closure inside `collectReplicaMetrics` into a method, so it can access `c.locator` and `ctx`:

Replace the closure (around line 312-347) with a call to a new method, and add the method below the existing struct methods:

```go
// buildInstanceKey returns (instanceKey, podName, vaName) for a series's labels.
// vaName comes from the llm_d_ai_variant label when present (the legacy /
// shadow-pod fast path). When absent and a pod label is present, falls back
// to the locator's owner-walk for Deployment / LWS layouts. Returns
// vaName="" when neither path resolves; the caller treats that as "skip".
func (c *ReplicaMetricsCollector) buildInstanceKey(ctx context.Context, namespace string, labels map[string]string) (instanceKey, podName, vaName string) {
	podName = labels["pod"]
	if podName == "" {
		podName = labels["pod_name"]
	}

	vaName = labels[constants.VariantLabelPrometheusKey]
	if vaName == "" && podName != "" && c.locator != nil {
		ms, err := c.locator.Locate(ctx, namespace, podName)
		if err != nil {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("locator.Locate failed; treating pod as unmanaged",
				"pod", podName, "namespace", namespace, "error", err)
		} else if ms != nil {
			switch {
			case ms.HPA != nil:
				vaName = ms.HPA.Name
			case ms.ScaledObject != nil:
				vaName = ms.ScaledObject.Name
			}
		}
	}

	instance := labels["instance"]
	port := ""
	if instance != "" && podName != "" {
		if idx := strings.LastIndex(instance, ":"); idx != -1 {
			port = instance[idx+1:]
		}
	}

	switch {
	case podName != "" && port != "":
		instanceKey = podName + ":" + port
	case instance != "":
		instanceKey = instance
	case podName != "":
		instanceKey = podName
	default:
		return "", "", ""
	}
	return instanceKey, podName, vaName
}
```

Replace every call site `instanceKey, podName, vaName := buildInstanceKey(value.Labels)` with `instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)`. There are roughly a dozen of these throughout `collectReplicaMetrics`; use a single grep to find them all:

```bash
grep -n "buildInstanceKey(value.Labels)" internal/collector/replica_metrics.go
```

Update each occurrence.

- [ ] **Step 4: Build to verify the wiring compiles**

```bash
go build ./internal/collector/...
```
Expected: build succeeds.

- [ ] **Step 5: Run existing collector tests**

```bash
go test ./internal/collector/... -count=1
```
Expected: existing tests fail because `NewReplicaMetricsCollector` now needs a fourth argument. This is the next step's job to fix.

- [ ] **Step 6: Update collector unit tests to pass `nil` locator (no fallback in those tests)**

In `internal/collector/replica_metrics_test.go` and any other test calling `NewReplicaMetricsCollector`, add `nil` as the fourth argument. The fallback only triggers when both the label is empty AND the locator is non-nil, so tests that supply the label keep working with `nil`.

```bash
grep -rn "NewReplicaMetricsCollector(" --include="*_test.go" internal/
```

Edit each call site to add `, nil` as the fourth argument.

- [ ] **Step 7: Run collector tests again**

```bash
go test ./internal/collector/... -count=1
```
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/collector/replica_metrics.go internal/collector/replica_metrics_test.go
git commit -m "feat(collector): use PodLocator as fallback when llm_d_ai_variant is absent"
```

---

## Task 10: Construct the locator in `saturation.NewEngine` and pass it to the collector

**Files:**
- Modify: `internal/engines/saturation/engine.go`

- [ ] **Step 1: Add the constructor wiring**

Edit `internal/engines/saturation/engine.go`. Find `NewEngine` (around line 180) and the `collector.NewReplicaMetricsCollector` call (line 221). Add the locator construction just before the engine struct literal:

Add the import:

```go
"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
```

Add a parameter and construction. The engine already receives a `client.Client` (the cached client) and uses `mgr.GetAPIReader()` is available via the saturation engine's existing `client` only as the cached one — `apiReader` must be threaded in. Update `NewEngine`'s signature to accept the apiReader:

```go
func NewEngine(
	client client.Client,
	apiReader client.Reader,
	scheme *runtime.Scheme,
	recorder record.EventRecorder,
	sourceRegistry source.Registry,
	cfg *config.Config,
) *Engine {
```

(If `NewEngine` does not currently take a `scheme` etc., match the existing arg list and just insert `apiReader` as the second positional argument.)

Inside `NewEngine`, after `promSource := ...` and before the `engine := Engine{...}` literal, construct the locator:

```go
podLocator, err := locator.New(client, apiReader)
if err != nil {
	// In practice this only fails when defaultCacheSize <= 0, which is a
	// programming error; surface as a runtime panic rather than threading
	// an error all the way up.
	panic(fmt.Sprintf("locator.New: %v", err))
}
```

Replace the `ReplicaMetricsCollector: collector.NewReplicaMetricsCollector(promSource, client, recorder),` line with:

```go
ReplicaMetricsCollector: collector.NewReplicaMetricsCollector(promSource, client, recorder, podLocator),
```

Add `"fmt"` to imports if it isn't already there.

- [ ] **Step 2: Update `cmd/main.go` to pass the apiReader**

Find the `saturation.NewEngine(...)` call (line 450). Update:

```go
engine := saturation.NewEngine(
	mgr.GetClient(),
	mgr.GetAPIReader(),
	mgr.GetScheme(),
	mgr.GetEventRecorderFor("workload-variant-autoscaler-saturation-engine"),
	sourceRegistry,
	cfg,
)
```

(If the existing signature already lacks `mgr.GetScheme()`, just insert `mgr.GetAPIReader()` after `mgr.GetClient()`.)

- [ ] **Step 3: Build everything**

```bash
go build ./...
```
Expected: build succeeds.

- [ ] **Step 4: Run the full unit-test suite**

```bash
go test ./... -count=1
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engines/saturation/engine.go cmd/main.go
git commit -m "feat(saturation): construct PodLocator and inject into ReplicaMetricsCollector"
```

---

## Task 11: Add RBAC grants for Pod and ReplicaSet read

**Files:**
- Modify: `config/rbac/role.yaml` (or whichever path your `kubebuilder rbac` markers compile into — confirm with `make manifests` after)
- Modify: `internal/collector/locator/locator.go` (add kubebuilder RBAC markers)

Existing grants for `apps/deployments` and `leaderworkerset.x-k8s.io/leaderworkersets` are already present from other reconcilers. We only need Pod and ReplicaSet.

- [ ] **Step 1: Add kubebuilder RBAC markers above the package declaration in locator.go**

Edit `internal/collector/locator/locator.go`. Above the `package locator` line, after the license comment, add:

```go
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
```

(Deployment and LeaderWorkerSet grants already exist elsewhere; do not duplicate.)

- [ ] **Step 2: Regenerate RBAC manifests**

```bash
make manifests
```
Expected: `config/rbac/role.yaml` updated with the new rules; chart templates may also be regenerated depending on the project's tooling.

- [ ] **Step 3: Confirm the diff is only the new rules**

```bash
git diff config/rbac/
```
Expected: addition of `pods` and `replicasets` `get;list;watch` rules; no other changes.

- [ ] **Step 4: Commit**

```bash
git add internal/collector/locator/locator.go config/rbac/
git commit -m "rbac(locator): add Pod and ReplicaSet read grants"
```

---

## Task 12: Update `docs/design/controller-behavior.md` to reflect the new contract

**Files:**
- Modify: `docs/design/controller-behavior.md`

- [ ] **Step 1: Read the existing "Prerequisites" section**

```bash
grep -n "Prerequisites\|llm-d.ai/variant\|llm_d_ai_variant" docs/design/controller-behavior.md | head -20
```

- [ ] **Step 2: Edit the section**

Replace the existing intro of the `### llm-d.ai/variant Label on the Scale Target` subsection with text that makes the label conditional on shadow-pod layouts:

```markdown
### `llm-d.ai/variant` Label on the Scale Target

**Required only for shadow-pod layouts.** For ordinary `Deployment` and `LeaderWorkerSet` scale targets, WVA's `PodLocator` derives the pod → variant association by walking `ownerReferences`; no operator action is required at the metrics layer.

For *shadow-pod* layouts — where the vLLM pod is not in the HPA-scaled target's `ownerReferences` chain — the label is the only viable linkage. Two things must be true in that case:

1. The `llm-d.ai/variant` label is present on the **pod template** of the vLLM-bearing workload, with a value equal to the name of the corresponding `VariantAutoscaling` resource (which equals the managed HPA / ScaledObject name).
2. The `ServiceMonitor` or `PodMonitor` that Prometheus uses to scrape those pods includes a target relabeling rule that propagates the pod label into the scraped metrics as `llm_d_ai_variant`.

For non-shadow-pod layouts the rule is harmless if present — the label-driven fast path simply short-circuits the locator. Operators may keep it for backwards compatibility or remove it.
```

Keep the rest of the section (the YAML examples, troubleshooting steps) intact. Add a one-line note at the top of each YAML example: `# Required for shadow-pod layouts; optional otherwise.`

- [ ] **Step 3: Commit**

```bash
git add docs/design/controller-behavior.md
git commit -m "docs(controller-behavior): mark llm-d.ai/variant label required only for shadow pods"
```

---

## Task 13: End-to-end validation

**Files:** none

- [ ] **Step 1: Run the full build**

```bash
go build ./...
```
Expected: build succeeds.

- [ ] **Step 2: Run the full unit-test suite**

```bash
go test ./... -count=1
```
Expected: PASS.

- [ ] **Step 3: Run `make lint`**

```bash
make lint
```
Expected: PASS.

- [ ] **Step 4: Run `make manifests` once more to confirm RBAC is up to date**

```bash
make manifests
git diff
```
Expected: no diff (manifests already committed in Task 11).

- [ ] **Step 5: Smoke check the locator import graph is contained**

```bash
go list -deps ./internal/collector/locator/... | grep -v "^github.com/llm-d/llm-d-workload-variant-autoscaler/" | sort -u | head -30
```
Expected: only stdlib, k8s.io, sigs.k8s.io, hashicorp/golang-lru, and KEDA / LWS API packages. No surprising transitive deps.

- [ ] **Step 6: Final commit / PR-ready state**

The branch is now ready for PR. There is nothing to commit at this step unless the prior steps surfaced lint or manifest diffs.

---

## Self-Review Checklist (run before handing off)

- [ ] Spec coverage: Each spec section maps to at least one task. Lookup algorithm → Task 7. Owner-chain walk → Task 5. Resolution cache → Task 6. Field indexes → Tasks 3 + 4. Public API → Task 7. RBAC → Task 11. Compatibility / impact on collector → Task 9. Engine wiring → Task 10. Docs flip → Task 12.
- [ ] Placeholder scan: no "TBD", "implement later", or "similar to Task N" without code.
- [ ] Type consistency: `chainNode`, `podKey`, `ManagedScaler`, `PodLocator`, `HPAByScaleTargetKey`, `ScaledObjectByScaleTargetKey`, `defaultCacheSize`, `defaultMaxDepth`, `scaleTargetKindSupported`, `walkOwnersUp`, `newResolutionCache`, `FindHPAForScaleTarget`, `FindSOForScaleTarget` — names match across all tasks where they appear.
- [ ] Every Go code block compiles in context (imports listed, types referenced are defined in earlier tasks).
- [ ] Each task ends in a commit with a conventional-commits message.
