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
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

const ns = "default"

func newFakeReader(objs ...runtime.Object) *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = lwsv1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...)
}

func TestWalkOwnersUp_PodReplicaSetDeployment(t *testing.T) {
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

// TestWalkOwnersUp_LWSLeaderPod covers Pod → StatefulSet(leader) → LeaderWorkerSet.
func TestWalkOwnersUp_LWSLeaderPod(t *testing.T) {
	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "lws", Namespace: ns, UID: "uid-lws"},
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lws",
			Namespace: ns,
			UID:       "uid-sts",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: constants.LeaderWorkerSetAPIVersion, Kind: constants.LeaderWorkerSetKind,
				Name: "lws", UID: "uid-lws", Controller: ptr.To(true),
			}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lws-0",
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: constants.DeploymentAPIVersion, Kind: constants.StatefulSetKind,
				Name: "lws", UID: "uid-sts", Controller: ptr.To(true),
			}},
		},
	}
	c := newFakeReader(lws, sts, pod).Build()

	chain, err := walkOwnersUp(context.Background(), c, pod, ns, defaultMaxDepth)
	if err != nil {
		t.Fatalf("walkOwnersUp: %v", err)
	}
	want := []chainNode{
		{Namespace: ns, APIVersion: constants.PodAPIVersion, Kind: constants.PodKind, Name: "lws-0"},
		{Namespace: ns, APIVersion: constants.DeploymentAPIVersion, Kind: constants.StatefulSetKind, Name: "lws"},
		{Namespace: ns, APIVersion: constants.LeaderWorkerSetAPIVersion, Kind: constants.LeaderWorkerSetKind, Name: "lws"},
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

// TestWalkOwnersUp_LWSWorkerPod covers the worker chain:
// Pod → StatefulSet(worker) → Pod(leader) → StatefulSet(leader) → LeaderWorkerSet.
func TestWalkOwnersUp_LWSWorkerPod(t *testing.T) {
	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "lws", Namespace: ns, UID: "uid-lws"},
	}
	leaderSTS := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lws",
			Namespace: ns,
			UID:       "uid-sts-leader",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: constants.LeaderWorkerSetAPIVersion, Kind: constants.LeaderWorkerSetKind,
				Name: "lws", UID: "uid-lws", Controller: ptr.To(true),
			}},
		},
	}
	leaderPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lws-0",
			Namespace: ns,
			UID:       "uid-pod-leader",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: constants.DeploymentAPIVersion, Kind: constants.StatefulSetKind,
				Name: "lws", UID: "uid-sts-leader", Controller: ptr.To(true),
			}},
		},
	}
	workerSTS := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lws-0",
			Namespace: ns,
			UID:       "uid-sts-worker",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: constants.PodAPIVersion, Kind: constants.PodKind,
				Name: "lws-0", UID: "uid-pod-leader", Controller: ptr.To(true),
			}},
		},
	}
	workerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lws-0-1",
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: constants.DeploymentAPIVersion, Kind: constants.StatefulSetKind,
				Name: "lws-0", UID: "uid-sts-worker", Controller: ptr.To(true),
			}},
		},
	}
	c := newFakeReader(lws, leaderSTS, leaderPod, workerSTS, workerPod).Build()

	chain, err := walkOwnersUp(context.Background(), c, workerPod, ns, defaultMaxDepth)
	if err != nil {
		t.Fatalf("walkOwnersUp: %v", err)
	}
	want := []chainNode{
		{Namespace: ns, APIVersion: constants.PodAPIVersion, Kind: constants.PodKind, Name: "lws-0-1"},
		{Namespace: ns, APIVersion: constants.DeploymentAPIVersion, Kind: constants.StatefulSetKind, Name: "lws-0"},
		{Namespace: ns, APIVersion: constants.PodAPIVersion, Kind: constants.PodKind, Name: "lws-0"},
		{Namespace: ns, APIVersion: constants.DeploymentAPIVersion, Kind: constants.StatefulSetKind, Name: "lws"},
		{Namespace: ns, APIVersion: constants.LeaderWorkerSetAPIVersion, Kind: constants.LeaderWorkerSetKind, Name: "lws"},
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
