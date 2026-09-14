// Package locator resolves a pod to the managed scaler (HPA or KEDA
// ScaledObject) that controls its replica count, via ownerReferences walking
// for Deployment / LWS layouts and via the variant name for shadow-pod
// layouts. See docs/superpowers/specs/2026-06-11-pod-to-managed-scaler-locator-design.md.

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch

package locator

import (
	"context"
	"fmt"
	"sync/atomic"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller/indexers"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ManagedScaler is one of the managed scaler kinds WVA recognizes.
// Exactly one of HPA / ScaledObject is non-nil on a successful Locate.
type ManagedScaler struct {
	HPA          *autoscalingv2.HorizontalPodAutoscaler
	ScaledObject *kedav1alpha1.ScaledObject
}

// kedaEnabled records whether the KEDA ScaledObject CRD is installed. It is set
// once at startup via SetKEDAEnabled before any locator is constructed. Defaults
// to true so that ScaledObject lookups remain enabled unless explicitly disabled.
var kedaEnabled atomic.Bool

func init() { kedaEnabled.Store(true) }

// SetKEDAEnabled configures whether locators attempt KEDA ScaledObject lookups.
// Call once at startup (cmd/main.go) with the result of crd.CheckKEDACRD. When
// false, the ScaledObject field index is not registered, so locators must skip
// every ScaledObject access to avoid unregistered-index and unsyncable-informer
// errors.
func SetKEDAEnabled(enabled bool) { kedaEnabled.Store(enabled) }

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

	// ResolveScaleTarget returns the top-level Deployment / LWS scale target
	// in the pod's ownerReferences chain, independent of whether a managed
	// scaler controls it. ok is false when the pod has no scaler-eligible
	// ancestor (shadow pod, unknown chain) or does not exist. Reuses the
	// pod→target cache, so a call following Locate for the same pod issues
	// no extra API reads. Use this to attribute metrics for scale targets
	// fronted by an unmanaged scaler (e.g. a KServe-created HPA without
	// llm-d.ai/managed=true), where Locate returns (nil, nil).
	//
	// TODO(va-removal): this method exists only for the CRD-based dual-mode
	// fallback in the collector (buildInstanceKey). Remove it when the
	// VariantAutoscaling CRD is removed.
	ResolveScaleTarget(ctx context.Context, namespace, podName string) (ref autoscalingv2.CrossVersionObjectReference, ok bool, err error)

	// GetPodLabels returns the labels for the specified pod. This reuses the
	// same pod fetch that Locate performs. Returns nil if the pod does not exist or on error.
	GetPodLabels(ctx context.Context, namespace, podName string) map[string]string
}

// New constructs a PodLocator.
//
//   - cached    — controller-runtime cached client (mgr.GetClient()), used
//     for the field-indexed scaler lookups and for LocateByVariant's
//     direct Get by name (HPAs and ScaledObjects are already watched).
//   - apiReader — uncached reader (mgr.GetAPIReader()), used for every
//     Pod / ReplicaSet / Deployment / LWS read in the owner-chain walk.
func New(cached, apiReader client.Reader) (PodLocator, error) {
	cache, err := newResolutionCache(defaultCacheSize)
	if err != nil {
		return nil, err
	}
	return &podLocator{
		cached:      cached,
		apiReader:   apiReader,
		maxDepth:    defaultMaxDepth,
		cache:       cache,
		kedaEnabled: kedaEnabled.Load(),
	}, nil
}

type podLocator struct {
	cached    client.Reader
	apiReader client.Reader
	maxDepth  int
	cache     *resolutionCache
	// kedaEnabled snapshots the package-level flag at construction. When false,
	// the ScaledObject field index is not registered, so SO lookups are skipped.
	kedaEnabled bool
}

func (l *podLocator) Locate(ctx context.Context, namespace, podName string) (*ManagedScaler, error) {
	// Step 1: pod → top-level scale target. Immutable per Kubernetes'
	// ownerReference rules, so the result is cacheable indefinitely.
	target, err := l.resolveTarget(ctx, namespace, podName)
	if err != nil {
		return nil, err
	}
	if target == (chainNode{}) {
		return nil, nil
	}

	// Step 2: scale target → managed scaler. NOT cached; field-index reads
	// are cheap and reflect the current annotation / scaleTargetRef state.
	return l.resolveScaler(ctx, target)
}

// resolveTarget runs Step 1 of resolution: pod → top-level scale-target
// chainNode, memoized in the pod→target cache. Returns the zero chainNode
// (with nil error) when the pod has no scaler-eligible ancestor or does not
// exist. Shared by Locate and ResolveScaleTarget.
func (l *podLocator) resolveTarget(ctx context.Context, namespace, podName string) (chainNode, error) {
	if target, hit := l.cache.getTarget(podKey{Namespace: namespace, Name: podName}); hit {
		return target, nil
	}
	pod := &corev1.Pod{}
	if err := l.apiReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return chainNode{}, nil
		}
		return chainNode{}, fmt.Errorf("get pod %s/%s: %w", namespace, podName, err)
	}
	target, err := l.resolveScaleTarget(ctx, pod, namespace)
	if err != nil {
		return chainNode{}, err
	}
	l.cache.add(podKey{Namespace: namespace, Name: podName}, target, pod.Labels)
	return target, nil
}

func (l *podLocator) LocateByVariant(ctx context.Context, namespace, variantName string) (*ManagedScaler, error) {
	if variantName == "" {
		return nil, nil
	}
	hpa, err := l.getManagedHPA(ctx, namespace, variantName)
	if err != nil {
		return nil, err
	}
	// Skip ScaledObject lookup when KEDA is absent: its informer cannot sync
	// without the CRD, so a cached Get would error.
	var so *kedav1alpha1.ScaledObject
	if l.kedaEnabled {
		if so, err = l.getManagedScaledObject(ctx, namespace, variantName); err != nil {
			return nil, err
		}
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

func (l *podLocator) GetPodLabels(ctx context.Context, namespace, podName string) map[string]string {
	if podName == "" {
		return nil
	}

	// Check cache first
	key := podKey{Namespace: namespace, Name: podName}
	if labels, hit := l.cache.getLabels(key); hit {
		return labels
	}

	// Not in cache, fetch the pod
	pod := &corev1.Pod{}
	if err := l.apiReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err != nil {
		if !apierrors.IsNotFound(err) {
			ctrl.LoggerFrom(ctx).Error(err, "GetPodLabels: failed to get pod", "namespace", namespace, "pod", podName)
		}
		return nil
	}

	// Resolve the target to populate the cache (so subsequent Locate calls don't refetch).
	// Skip caching on error to avoid a permanent negative entry from a transient failure.
	target, err := l.resolveScaleTarget(ctx, pod, namespace)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "GetPodLabels: resolveScaleTarget failed, skipping cache", "namespace", namespace, "pod", podName)
		return pod.Labels
	}
	l.cache.add(key, target, pod.Labels)

	return pod.Labels
}

// ResolveScaleTarget returns the top-level scale target for the given pod.
func (l *podLocator) ResolveScaleTarget(ctx context.Context, namespace, podName string) (autoscalingv2.CrossVersionObjectReference, bool, error) {
	target, err := l.resolveTarget(ctx, namespace, podName)
	if err != nil {
		return autoscalingv2.CrossVersionObjectReference{}, false, err
	}
	if target == (chainNode{}) {
		return autoscalingv2.CrossVersionObjectReference{}, false, nil
	}
	return autoscalingv2.CrossVersionObjectReference{
		APIVersion: target.APIVersion,
		Kind:       target.Kind,
		Name:       target.Name,
	}, true, nil
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
	// Skip ScaledObject lookup when KEDA is absent: the ScaledObject field index
	// is not registered, so a MatchingFields List would error.
	var so *kedav1alpha1.ScaledObject
	if l.kedaEnabled {
		if so, err = indexers.FindSOForScaleTarget(ctx, asClient(l.cached), ref, target.Namespace); err != nil {
			return nil, err
		}
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

// getManagedScaledObject fetches a ScaledObject by name and returns it only
// if it carries llm-d.ai/managed=true.
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
