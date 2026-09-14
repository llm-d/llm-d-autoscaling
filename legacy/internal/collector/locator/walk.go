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
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
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
			apiVersion, kind = constants.PodAPIVersion, constants.PodKind
		case *appsv1.ReplicaSet:
			apiVersion, kind = constants.DeploymentAPIVersion, constants.ReplicaSetKind
		case *appsv1.Deployment:
			apiVersion, kind = constants.DeploymentAPIVersion, constants.DeploymentKind
		case *appsv1.StatefulSet:
			apiVersion, kind = constants.DeploymentAPIVersion, constants.StatefulSetKind
		case *lwsv1.LeaderWorkerSet:
			apiVersion, kind = constants.LeaderWorkerSetAPIVersion, constants.LeaderWorkerSetKind
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
	case constants.PodKind:
		if owner.APIVersion == constants.PodAPIVersion {
			return &corev1.Pod{}, true
		}
	case constants.ReplicaSetKind:
		if owner.APIVersion == constants.DeploymentAPIVersion {
			return &appsv1.ReplicaSet{}, true
		}
	case constants.DeploymentKind:
		if owner.APIVersion == constants.DeploymentAPIVersion {
			return &appsv1.Deployment{}, true
		}
	case constants.StatefulSetKind:
		if owner.APIVersion == constants.DeploymentAPIVersion {
			return &appsv1.StatefulSet{}, true
		}
	case constants.LeaderWorkerSetKind:
		if owner.APIVersion == constants.LeaderWorkerSetAPIVersion {
			return &lwsv1.LeaderWorkerSet{}, true
		}
	}
	return nil, false
}
