package gpurebalance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// stubPromAPI implements promv1.API with only Query stubbed.
// Pool→queue values are matched by searching the query string for the pool name.
type stubPromAPI struct {
	promv1.API
	queues map[string]float64
	errs   map[string]error
}

func (s *stubPromAPI) Query(_ context.Context, query string, _ time.Time, _ ...promv1.Option) (model.Value, promv1.Warnings, error) {
	for pool, err := range s.errs {
		if strings.Contains(query, pool) {
			return nil, nil, err
		}
	}
	for pool, q := range s.queues {
		if strings.Contains(query, pool) {
			return model.Vector{{Value: model.SampleValue(q)}}, nil, nil
		}
	}
	return model.Vector{}, nil, nil
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := kedav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme KEDA: %v", err)
	}
	return s
}

func makeHPA(name, ns, pool string, maxReplicas int32) *autoscalingv2.HorizontalPodAutoscaler {
	ann := map[string]string{}
	if pool != "" {
		ann[AnnotationInferencePool] = pool
	}
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: ann,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			MaxReplicas: maxReplicas,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       name + "-deploy",
			},
		},
	}
}

func makeScaledObject(name, pool string, maxReplicas int32) *kedav1alpha1.ScaledObject {
	ann := map[string]string{}
	if pool != "" {
		ann[AnnotationInferencePool] = pool
	}
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "ns",
			Annotations: ann,
		},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       name + "-deploy",
			},
			MaxReplicaCount: ptr.To(maxReplicas),
			Triggers: []kedav1alpha1.ScaleTriggers{{
				Type: "prometheus",
				Metadata: map[string]string{
					"query":     `sum(inference_extension_flow_control_queue_size{inference_pool="` + pool + `"})`,
					"threshold": "1",
				},
			}},
		},
	}
}

func makeGPUQuota(name, ns string, gpus int64) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceName(gpuQuotaResource): *resource.NewQuantity(gpus, resource.DecimalSI),
			},
		},
	}
}

func newPlugin(t *testing.T, queues map[string]float64, errs map[string]error, objs ...client.Object) (*Plugin, client.Client) {
	t.Helper()
	scheme := newTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return New(c, &stubPromAPI{queues: queues, errs: errs}), c
}

func getMaxReplicas(t *testing.T, c client.Client, hpa *autoscalingv2.HorizontalPodAutoscaler) int32 {
	t.Helper()
	name := hpa.Name
	ns := hpa.Namespace
	got := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, got); err != nil {
		t.Fatalf("Get HPA %s/%s: %v", ns, name, err)
	}
	return got.Spec.MaxReplicas
}

func getMaxReplicaCount(t *testing.T, c client.Client, so *kedav1alpha1.ScaledObject) int32 {
	t.Helper()
	name := so.Name
	ns := so.Namespace
	got := &kedav1alpha1.ScaledObject{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, got); err != nil {
		t.Fatalf("Get ScaledObject %s/%s: %v", ns, name, err)
	}
	return got.GetHPAMaxReplicas()
}

func TestScalerEntryFromObject_EffectiveMinimum(t *testing.T) {
	hpaNil := makeHPA("hpa-nil", "ns", "hpa-nil", 10)
	hpaZero := makeHPA("hpa-zero", "ns", "hpa-zero", 10)
	hpaZero.Spec.MinReplicas = ptr.To[int32](0)
	hpaThree := makeHPA("hpa-three", "ns", "hpa-three", 10)
	hpaThree.Spec.MinReplicas = ptr.To[int32](3)

	soNil := makeScaledObject("so-nil", "so-nil", 10)
	soZero := makeScaledObject("so-zero", "so-zero", 10)
	soZero.Spec.MinReplicaCount = ptr.To[int32](0)
	soThree := makeScaledObject("so-three", "so-three", 10)
	soThree.Spec.MinReplicaCount = ptr.To[int32](3)

	tests := []struct {
		name string
		obj  client.Object
		want int32
	}{
		{name: "HPA nil defaults to one", obj: hpaNil, want: 1},
		{name: "HPA zero uses minimum-one policy", obj: hpaZero, want: 1},
		{name: "HPA configured minimum", obj: hpaThree, want: 3},
		{name: "ScaledObject nil defaults to one", obj: soNil, want: 1},
		{name: "ScaledObject zero defaults to one", obj: soZero, want: 1},
		{name: "ScaledObject configured minimum", obj: soThree, want: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, ok := scalerEntryFromObject(tt.obj)
			if !ok {
				t.Fatal("scalerEntryFromObject returned ok=false")
			}
			if entry.effectiveMin != tt.want {
				t.Errorf("effectiveMin = %d, want %d", entry.effectiveMin, tt.want)
			}
		})
	}
}

// TestTick_EmptySelectionIsNoop verifies that an empty selected slice returns
// nil without touching the cluster.
func TestTick_EmptySelectionIsNoop(t *testing.T) {
	p, _ := newPlugin(t, nil, nil)
	if err := p.Tick(context.Background(), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestTick_SkipsUnsupportedObjects verifies that objects that are neither HPAs
// nor KEDA ScaledObjects are silently skipped.
func TestTick_SkipsUnsupportedObjects(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns"}}
	p, _ := newPlugin(t, nil, nil)
	if err := p.Tick(context.Background(), []client.Object{pod}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestTick_SkipsUnannotatedHPA verifies that an HPA without the
// llm-d.ai/epp-inference-pool annotation is skipped and never patched.
func TestTick_SkipsUnannotatedHPA(t *testing.T) {
	hpa := makeHPA("model-a", "ns", "" /* no pool */, 10)
	p, c := newPlugin(t, map[string]float64{"model-a": 100}, nil, hpa, makeGPUQuota("q", "ns", 10))

	if err := p.Tick(context.Background(), []client.Object{hpa}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, hpa); got != 10 {
		t.Errorf("maxReplicas changed to %d, want 10 (unannotated HPA should be skipped)", got)
	}
}

// TestTick_SkipsUnannotatedScaledObject verifies that a ScaledObject without
// the llm-d.ai/epp-inference-pool annotation is skipped and never patched.
func TestTick_SkipsUnannotatedScaledObject(t *testing.T) {
	so := makeScaledObject("model-a", "" /* no pool */, 10)
	p, c := newPlugin(t, map[string]float64{"model-a": 100}, nil, so, makeGPUQuota("q", "ns", 10))

	if err := p.Tick(context.Background(), []client.Object{so}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicaCount(t, c, so); got != 10 {
		t.Errorf("maxReplicaCount changed to %d, want 10 (unannotated ScaledObject should be skipped)", got)
	}
}

// TestTick_NoQuota_Skips verifies that when no ResourceQuota exists in a
// namespace, the plugin skips rebalancing and leaves HPAs unchanged.
func TestTick_NoQuota_Skips(t *testing.T) {
	hpa := makeHPA("model-a", "ns", "model-a", 10)
	// No ResourceQuota added to the cluster.
	p, c := newPlugin(t, map[string]float64{"model-a": 100}, nil, hpa)

	if err := p.Tick(context.Background(), []client.Object{hpa}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, hpa); got != 10 {
		t.Errorf("maxReplicas changed to %d, want 10 (no quota → skip)", got)
	}
}

// TestRebalance_SinglePool verifies that a single annotated HPA receives the
// entire GPU quota.
func TestRebalance_SinglePool(t *testing.T) {
	hpa := makeHPA("model-a", "ns", "model-a", 1)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 100},
		nil,
		hpa, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpa}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, hpa); got != 10 {
		t.Errorf("maxReplicas = %d, want 10", got)
	}
}

// TestRebalance_EqualQueues verifies that two pools with the same queue depth
// each receive half the quota.
func TestRebalance_EqualQueues(t *testing.T) {
	hpaA := makeHPA("model-a", "ns", "model-a", 1)
	hpaB := makeHPA("model-b", "ns", "model-b", 1)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 100, "model-b": 100},
		nil,
		hpaA, hpaB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, hpaA); got != 5 {
		t.Errorf("model-a maxReplicas = %d, want 5", got)
	}
	if got := getMaxReplicas(t, c, hpaB); got != 5 {
		t.Errorf("model-b maxReplicas = %d, want 5", got)
	}
}

// TestRebalance_ProportionalQueues verifies that when queue depths differ, the
// allocation is proportional: 70% queue → 70% replicas (7/10), 30% → 3/10.
func TestRebalance_ProportionalQueues(t *testing.T) {
	hpaA := makeHPA("model-a", "ns", "model-a", 1)
	hpaB := makeHPA("model-b", "ns", "model-b", 1)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 70, "model-b": 30},
		nil,
		hpaA, hpaB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, hpaA); got != 7 {
		t.Errorf("model-a maxReplicas = %d, want 7", got)
	}
	if got := getMaxReplicas(t, c, hpaB); got != 3 {
		t.Errorf("model-b maxReplicas = %d, want 3", got)
	}
}

// TestRebalance_AllQueuesZero verifies that when all queues are zero the
// quota is split equally.
func TestRebalance_AllQueuesZero(t *testing.T) {
	hpaA := makeHPA("model-a", "ns", "model-a", 1)
	hpaB := makeHPA("model-b", "ns", "model-b", 1)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 0, "model-b": 0},
		nil,
		hpaA, hpaB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, hpaA); got != 5 {
		t.Errorf("model-a maxReplicas = %d, want 5 (equal split when queues are zero)", got)
	}
	if got := getMaxReplicas(t, c, hpaB); got != 5 {
		t.Errorf("model-b maxReplicas = %d, want 5 (equal split when queues are zero)", got)
	}
}

// TestRebalance_QueryError_TreatedAsZero verifies that a Prometheus query
// failure for a pool is treated as queue depth 0 — the pool still receives
// the minimum 1 replica and the other pool gets the remaining quota.
func TestRebalance_QueryError_TreatedAsZero(t *testing.T) {
	hpaA := makeHPA("model-a", "ns", "model-a", 10)
	hpaB := makeHPA("model-b", "ns", "model-b", 10)
	p, c := newPlugin(t,
		map[string]float64{"model-b": 100},
		map[string]error{"model-a": errors.New("prometheus unreachable")},
		hpaA, hpaB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Reserve one replica for each pool, then give the remaining eight to model-b.
	if got := getMaxReplicas(t, c, hpaA); got != 1 {
		t.Errorf("model-a maxReplicas = %d, want 1 (query error → treated as 0, clamped to min)", got)
	}
	if got := getMaxReplicas(t, c, hpaB); got != 9 {
		t.Errorf("model-b maxReplicas = %d, want 9 (all remaining quota)", got)
	}
}

// TestRebalance_RemainderToHighestWeight verifies that fractional replicas
// (remainder after floor) are assigned to the pool with the highest weight.
func TestRebalance_RemainderToHighestWeight(t *testing.T) {
	// q_a=7, q_b=4, total=11, quota=10
	// weight_a=7/11≈0.636 → floor(6.36)=6
	// weight_b=4/11≈0.364 → floor(3.63)=3
	// allocated=9, remainder=1 → goes to model-a (higher weight)
	// final: model-a=7, model-b=3
	hpaA := makeHPA("model-a", "ns", "model-a", 1)
	hpaB := makeHPA("model-b", "ns", "model-b", 1)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 7, "model-b": 4},
		nil,
		hpaA, hpaB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, hpaA); got != 7 {
		t.Errorf("model-a maxReplicas = %d, want 7 (gets remainder)", got)
	}
	if got := getMaxReplicas(t, c, hpaB); got != 3 {
		t.Errorf("model-b maxReplicas = %d, want 3", got)
	}
}

// TestRebalance_NoChangeWhenAlreadyCorrect verifies that when the current
// maxReplicas already matches the computed target, the patch is skipped (no
// error, value unchanged).
func TestRebalance_NoChangeWhenAlreadyCorrect(t *testing.T) {
	// Equal queues, quota=10 → each should get 5; set maxReplicas=5 already.
	hpaA := makeHPA("model-a", "ns", "model-a", 5)
	hpaB := makeHPA("model-b", "ns", "model-b", 5)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 50, "model-b": 50},
		nil,
		hpaA, hpaB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, hpaA); got != 5 {
		t.Errorf("model-a maxReplicas = %d, want 5", got)
	}
	if got := getMaxReplicas(t, c, hpaB); got != 5 {
		t.Errorf("model-b maxReplicas = %d, want 5", got)
	}
}

// TestRebalance_ScaledObjects verifies that KEDA ScaledObjects receive
// proportional GPU quota by patching spec.maxReplicaCount.
func TestRebalance_ScaledObjects(t *testing.T) {
	soA := makeScaledObject("model-a", "model-a", 1)
	soB := makeScaledObject("model-b", "model-b", 1)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 70, "model-b": 30},
		nil,
		soA, soB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{soA, soB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicaCount(t, c, soA); got != 7 {
		t.Errorf("model-a maxReplicaCount = %d, want 7", got)
	}
	if got := getMaxReplicaCount(t, c, soB); got != 3 {
		t.Errorf("model-b maxReplicaCount = %d, want 3", got)
	}
}

// TestRebalance_MixedHPAAndScaledObject verifies that direct HPAs and KEDA
// ScaledObjects in the same namespace share the ResourceQuota calculation.
func TestRebalance_MixedHPAAndScaledObject(t *testing.T) {
	hpaA := makeHPA("model-a", "ns", "model-a", 1)
	soB := makeScaledObject("model-b", "model-b", 1)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 20, "model-b": 80},
		nil,
		hpaA, soB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, soB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, hpaA); got != 2 {
		t.Errorf("model-a maxReplicas = %d, want 2", got)
	}
	if got := getMaxReplicaCount(t, c, soB); got != 8 {
		t.Errorf("model-b maxReplicaCount = %d, want 8", got)
	}
}

func TestRebalance_ReservesHPAMinimum(t *testing.T) {
	hpaA := makeHPA("model-a", "ns", "model-a", 10)
	hpaA.Spec.MinReplicas = ptr.To[int32](5)
	hpaB := makeHPA("model-b", "ns", "model-b", 10)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 50, "model-b": 50},
		nil,
		hpaA, hpaB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, hpaA); got != 7 {
		t.Errorf("model-a maxReplicas = %d, want 7", got)
	}
	if got := getMaxReplicas(t, c, hpaB); got != 3 {
		t.Errorf("model-b maxReplicas = %d, want 3", got)
	}
}

func TestRebalance_ReservesScaledObjectMinimum(t *testing.T) {
	soA := makeScaledObject("model-a", "model-a", 10)
	soA.Spec.MinReplicaCount = ptr.To[int32](5)
	soB := makeScaledObject("model-b", "model-b", 10)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 50, "model-b": 50},
		nil,
		soA, soB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{soA, soB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicaCount(t, c, soA); got != 7 {
		t.Errorf("model-a maxReplicaCount = %d, want 7", got)
	}
	if got := getMaxReplicaCount(t, c, soB); got != 3 {
		t.Errorf("model-b maxReplicaCount = %d, want 3", got)
	}
}

func TestRebalance_ReservesMixedMinimums(t *testing.T) {
	hpaA := makeHPA("model-a", "ns", "model-a", 10)
	hpaA.Spec.MinReplicas = ptr.To[int32](3)
	soB := makeScaledObject("model-b", "model-b", 10)
	soB.Spec.MinReplicaCount = ptr.To[int32](4)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 20, "model-b": 80},
		nil,
		hpaA, soB, makeGPUQuota("q", "ns", 12),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, soB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, hpaA); got != 4 {
		t.Errorf("model-a maxReplicas = %d, want 4", got)
	}
	if got := getMaxReplicaCount(t, c, soB); got != 8 {
		t.Errorf("model-b maxReplicaCount = %d, want 8", got)
	}
}

// TestRebalance_AllocationNeverExceedsQuota verifies that configured minima
// are reserved before the remaining quota is distributed proportionally.
func TestRebalance_AllocationNeverExceedsQuota(t *testing.T) {
	const quota int64 = 12

	hpaA := makeHPA("model-a", "ns", "model-a", 20)
	hpaA.Spec.MinReplicas = ptr.To[int32](5)
	soB := makeScaledObject("model-b", "model-b", 20)
	soB.Spec.MinReplicaCount = ptr.To[int32](4)
	hpaC := makeHPA("model-c", "ns", "model-c", 20)
	p, c := newPlugin(t,
		map[string]float64{
			"model-a": 10,
			"model-b": 10,
			"model-c": 80,
		},
		nil,
		hpaA, soB, hpaC, makeGPUQuota("q", "ns", quota),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, soB, hpaC}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	gotA := getMaxReplicas(t, c, hpaA)
	gotB := getMaxReplicaCount(t, c, soB)
	gotC := getMaxReplicas(t, c, hpaC)
	if gotA != 5 {
		t.Errorf("model-a maxReplicas = %d, want 5", gotA)
	}
	if gotB != 4 {
		t.Errorf("model-b maxReplicaCount = %d, want 4", gotB)
	}
	if gotC != 3 {
		t.Errorf("model-c maxReplicas = %d, want 3", gotC)
	}

	if total := int64(gotA) + int64(gotB) + int64(gotC); total != quota {
		t.Errorf("sum of target max replicas = %d, want quota %d", total, quota)
	}
}

func TestRebalance_InfeasibleMinimumsLeaveNamespaceUnchanged(t *testing.T) {
	hpaA := makeHPA("model-a", "ns", "model-a", 10)
	hpaA.Spec.MinReplicas = ptr.To[int32](4)
	soB := makeScaledObject("model-b", "model-b", 9)
	soB.Spec.MinReplicaCount = ptr.To[int32](3)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 90, "model-b": 10},
		nil,
		hpaA, soB, makeGPUQuota("q", "ns", 6),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, soB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, hpaA); got != 10 {
		t.Errorf("model-a maxReplicas = %d, want 10 (infeasible namespace must be unchanged)", got)
	}
	if got := getMaxReplicaCount(t, c, soB); got != 9 {
		t.Errorf("model-b maxReplicaCount = %d, want 9 (infeasible namespace must be unchanged)", got)
	}
}

// TestRebalance_MultiNamespace verifies that HPAs in different namespaces are
// rebalanced independently against their own ResourceQuota and queue metrics.
func TestRebalance_MultiNamespace(t *testing.T) {
	// ns-x: quota=10, q_a=30, q_b=70 → a=3, b=7
	// ns-y: quota=6,  q_c=50, q_d=50 → c=3, d=3
	hpaA := makeHPA("model-a", "ns-x", "model-a", 1)
	hpaB := makeHPA("model-b", "ns-x", "model-b", 1)
	hpaC := makeHPA("model-c", "ns-y", "model-c", 1)
	hpaD := makeHPA("model-d", "ns-y", "model-d", 1)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 30, "model-b": 70, "model-c": 50, "model-d": 50},
		nil,
		hpaA, hpaB, hpaC, hpaD,
		makeGPUQuota("q-x", "ns-x", 10),
		makeGPUQuota("q-y", "ns-y", 6),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB, hpaC, hpaD}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		hpa  *autoscalingv2.HorizontalPodAutoscaler
		want int32
	}{
		{hpaA, 3},
		{hpaB, 7},
		{hpaC, 3},
		{hpaD, 3},
	}
	for _, tc := range tests {
		if got := getMaxReplicas(t, c, tc.hpa); got != tc.want {
			t.Errorf("%s/%s maxReplicas = %d, want %d", tc.hpa.Namespace, tc.hpa.Name, got, tc.want)
		}
	}
}

// newPluginWithInterceptor builds a plugin whose fake client routes calls
// through funcs, letting a test inject a transient 409 Conflict on Patch.
func newPluginWithInterceptor(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) (*Plugin, client.Client) {
	t.Helper()
	scheme := newTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithInterceptorFuncs(funcs).
		Build()
	return New(c, &stubPromAPI{}), c
}

// conflictOnceThenDelegate returns a Patch interceptor that fails the first
// call with a 409 Conflict and delegates every subsequent call to the real
// client. patchCalls is incremented on each invocation.
func conflictOnceThenDelegate(patchCalls *int, gr schema.GroupResource) func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
	return func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		*patchCalls++
		if *patchCalls == 1 {
			return apierrors.NewConflict(gr, obj.GetName(), errors.New("stale resourceVersion"))
		}
		return c.Patch(ctx, obj, patch, opts...)
	}
}

// TestSetMaxReplicas_RetriesOnConflictHPA verifies that a transient 409 on the
// first HPA patch is retried against a freshly read object and the target is
// applied on the retry.
func TestSetMaxReplicas_RetriesOnConflictHPA(t *testing.T) {
	hpa := makeHPA("hpa-a", "ns", "pool-a", 5)
	var patchCalls int
	funcs := interceptor.Funcs{
		Patch: conflictOnceThenDelegate(&patchCalls,
			schema.GroupResource{Group: "autoscaling", Resource: "horizontalpodautoscalers"}),
	}
	p, c := newPluginWithInterceptor(t, funcs, hpa)

	if err := p.setMaxReplicas(context.Background(), hpa, 8); err != nil {
		t.Fatalf("setMaxReplicas: %v", err)
	}
	if patchCalls != 2 {
		t.Errorf("patch calls = %d, want 2 (one conflict, one success)", patchCalls)
	}
	if got := getMaxReplicas(t, c, hpa); got != 8 {
		t.Errorf("maxReplicas = %d, want 8", got)
	}
}

// TestSetMaxReplicas_RetriesOnConflictScaledObject verifies the same retry
// behavior for KEDA ScaledObjects.
func TestSetMaxReplicas_RetriesOnConflictScaledObject(t *testing.T) {
	so := makeScaledObject("so-a", "pool-a", 5)
	var patchCalls int
	funcs := interceptor.Funcs{
		Patch: conflictOnceThenDelegate(&patchCalls,
			schema.GroupResource{Group: "keda.sh", Resource: "scaledobjects"}),
	}
	p, c := newPluginWithInterceptor(t, funcs, so)

	if err := p.setMaxReplicas(context.Background(), so, 8); err != nil {
		t.Fatalf("setMaxReplicas: %v", err)
	}
	if patchCalls != 2 {
		t.Errorf("patch calls = %d, want 2 (one conflict, one success)", patchCalls)
	}
	if got := getMaxReplicaCount(t, c, so); got != 8 {
		t.Errorf("maxReplicaCount = %d, want 8", got)
	}
}

// TestSetMaxReplicas_GivesUpAfterMaxRetries verifies that a persistent conflict
// exhausts the bounded retry, returns a Conflict error rather than hanging, and
// leaves the object unchanged.
func TestSetMaxReplicas_GivesUpAfterMaxRetries(t *testing.T) {
	hpa := makeHPA("hpa-a", "ns", "pool-a", 5)
	var patchCalls int
	funcs := interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
			patchCalls++
			return apierrors.NewConflict(
				schema.GroupResource{Group: "autoscaling", Resource: "horizontalpodautoscalers"},
				obj.GetName(), errors.New("stale resourceVersion"))
		},
	}
	p, c := newPluginWithInterceptor(t, funcs, hpa)

	err := p.setMaxReplicas(context.Background(), hpa, 8)
	if !apierrors.IsConflict(err) {
		t.Fatalf("err = %v, want a Conflict error", err)
	}
	if patchCalls != 5 {
		t.Errorf("patch calls = %d, want 5 (retry.DefaultRetry steps)", patchCalls)
	}
	if got := getMaxReplicas(t, c, hpa); got != 5 {
		t.Errorf("maxReplicas = %d, want 5 (unchanged)", got)
	}
}
