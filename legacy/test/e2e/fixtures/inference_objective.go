package fixtures

import (
	"context"
	"fmt"
	"os"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	infextv1alpha2 "sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
)

const defaultInferenceObjectiveName = "e2e-default"

// EnsureInferenceObjective creates the default InferenceObjective for GIE flow-control
// queuing when the CRD exists. poolName must match the InferencePool metadata.name (typically the
// EPP Service name with an "-epp" suffix removed, e.g. optimized-baseline-epp → optimized-baseline).
//
// Returns applied=true if the object exists or was created. If the InferenceObjective API is not
// available on the cluster, returns (false, nil).
func EnsureInferenceObjective(ctx context.Context, crClient client.Client, namespace, poolName string) (applied bool, err error) {
	return EnsureInferenceObjectiveNamed(ctx, crClient, namespace, defaultInferenceObjectiveName, poolName)
}

// EnsureInferenceObjectiveNamed creates or updates a named InferenceObjective when the CRD exists.
func EnsureInferenceObjectiveNamed(ctx context.Context, crClient client.Client, namespace, objectiveName, poolName string) (applied bool, err error) {
	poolGroup, gErr := resolveInferencePoolGroup(ctx, crClient, namespace, poolName)
	if gErr != nil {
		return false, gErr
	}
	desired := buildInferenceObjective(namespace, objectiveName, poolName, poolGroup)

	if cErr := crClient.Create(ctx, desired); cErr != nil {
		if apierrors.IsAlreadyExists(cErr) {
			key := client.ObjectKey{Namespace: namespace, Name: objectiveName}
			current := &infextv1alpha2.InferenceObjective{}
			getErr := crClient.Get(ctx, key, current)
			if getErr != nil {
				return false, fmt.Errorf("get existing InferenceObjective %s: %w", objectiveName, getErr)
			}
			if inferenceObjectiveSpecEqual(current.Spec, desired.Spec) {
				return true, nil
			}
			current.Spec = desired.Spec
			if upErr := crClient.Update(ctx, current); upErr != nil {
				return false, fmt.Errorf("update InferenceObjective %s: %w", objectiveName, upErr)
			}
			return true, nil
		}
		if inferenceObjectiveAPIMissing(cErr) {
			return false, nil
		}
		return false, fmt.Errorf("create InferenceObjective %s: %w", objectiveName, cErr)
	}
	return true, nil
}

// DeleteInferenceObjective removes the default InferenceObjective if present.
func DeleteInferenceObjective(ctx context.Context, crClient client.Client, namespace string) error {
	return DeleteInferenceObjectiveNamed(ctx, crClient, namespace, defaultInferenceObjectiveName)
}

// DeleteInferenceObjectiveNamed removes a named InferenceObjective if present.
func DeleteInferenceObjectiveNamed(ctx context.Context, crClient client.Client, namespace, objectiveName string) error {
	err := crClient.Delete(ctx, &infextv1alpha2.InferenceObjective{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objectiveName,
			Namespace: namespace,
		},
	})
	if apierrors.IsNotFound(err) || inferenceObjectiveAPIMissing(err) {
		return nil
	}
	return err
}

func buildInferenceObjective(namespace, objectiveName, poolName, poolGroup string) *infextv1alpha2.InferenceObjective {
	return &infextv1alpha2.InferenceObjective{
		TypeMeta: metav1.TypeMeta{
			APIVersion: infextv1alpha2.SchemeGroupVersion.String(),
			Kind:       "InferenceObjective",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      objectiveName,
			Namespace: namespace,
		},
		Spec: infextv1alpha2.InferenceObjectiveSpec{
			Priority: ptr.To(0),
			PoolRef: infextv1alpha2.PoolObjectReference{
				Group: infextv1alpha2.Group(poolGroup),
				Kind:  infextv1alpha2.Kind("InferencePool"),
				Name:  infextv1alpha2.ObjectName(poolName),
			},
		},
	}
}

// inferenceObjectiveSpecEqual compares spec semantically (priority unset vs 0 matches).
func inferenceObjectiveSpecEqual(a, b infextv1alpha2.InferenceObjectiveSpec) bool {
	return apiequality.Semantic.DeepEqual(
		normalizeInferenceObjectiveSpec(a),
		normalizeInferenceObjectiveSpec(b),
	)
}

func normalizeInferenceObjectiveSpec(spec infextv1alpha2.InferenceObjectiveSpec) infextv1alpha2.InferenceObjectiveSpec {
	if spec.Priority == nil {
		spec.Priority = ptr.To(0)
	}
	return spec
}

func inferenceObjectiveAPIMissing(err error) bool {
	if err == nil {
		return false
	}
	if meta.IsNoMatchError(err) {
		return true
	}
	se, ok := err.(*apierrors.StatusError)
	if !ok {
		return false
	}
	if se.ErrStatus.Reason != metav1.StatusReasonNotFound {
		return false
	}
	if strings.Contains(strings.ToLower(se.ErrStatus.Message), "the server could not find the requested resource") {
		return true
	}
	details := se.ErrStatus.Details
	if details == nil {
		return false
	}
	return details.Group == infextv1alpha2.GroupName && details.Kind == "inferenceobjectives"
}

func resolveInferencePoolGroup(ctx context.Context, crClient client.Client, namespace, poolName string) (string, error) {
	if envPoolGroup := os.Getenv("POOL_GROUP"); envPoolGroup != "" {
		return envPoolGroup, nil
	}

	inferencePoolCandidates := []schema.GroupVersionResource{
		{Group: "inference.networking.k8s.io", Version: "v1", Resource: "inferencepools"},
		{Group: "inference.networking.x-k8s.io", Version: "v1alpha2", Resource: "inferencepools"},
	}

	for _, gvr := range inferencePoolCandidates {
		pool := &unstructured.Unstructured{}
		pool.SetAPIVersion(gvr.Group + "/" + gvr.Version)
		pool.SetKind("InferencePool")
		err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: poolName}, pool)
		if err == nil {
			return gvr.Group, nil
		}
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			continue
		}
		return "", fmt.Errorf("detect InferencePool API group for %q: %w", poolName, err)
	}

	return "", fmt.Errorf("detect InferencePool API group for %q: no supported InferencePool resource found", poolName)
}
