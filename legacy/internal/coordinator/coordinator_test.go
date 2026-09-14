package coordinator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
)

const hpaManagedName = "hpa-managed"

// stubPlugin records every Tick invocation it receives and returns a
// pre-configured error.
type stubPlugin struct {
	name string
	mu   sync.Mutex
	got  [][]client.Object
	err  error
}

func (s *stubPlugin) Name() string { return s.name }

func (s *stubPlugin) Tick(_ context.Context, selected []client.Object) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, selected)
	return s.err
}

func (s *stubPlugin) calls() [][]client.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]client.Object, len(s.got))
	copy(out, s.got)
	return out
}

// newScheme returns a runtime.Scheme with the kinds the Coordinator
// lists registered.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := kedav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("keda AddToScheme: %v", err)
	}
	return s
}

func TestNew_Validations(t *testing.T) {
	if _, err := New(nil, nil, Options{}); err == nil {
		t.Fatal("expected error for nil client")
	}

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	if _, err := New(c, []Plugin{nil}, Options{}); err == nil {
		t.Fatal("expected error for nil plugin")
	}

	if _, err := New(c, []Plugin{&stubPlugin{name: ""}}, Options{}); err == nil {
		t.Fatal("expected error for empty plugin name")
	}

	if _, err := New(c, []Plugin{&stubPlugin{name: "p"}, &stubPlugin{name: "p"}}, Options{}); err == nil {
		t.Fatal("expected error for duplicate plugin name")
	}

	c2, err := New(c, []Plugin{&stubPlugin{name: "p"}}, Options{})
	if err != nil {
		t.Fatalf("unexpected New error: %v", err)
	}
	if c2.opts.Interval != DefaultInterval {
		t.Errorf("expected default interval %v, got %v", DefaultInterval, c2.opts.Interval)
	}
}

// TestDiscover_FiltersAndMixesScaleTargets builds a fake cluster with a
// mix of HPAs and ScaledObjects and verifies discover() returns exactly
// the items that satisfy the selection rules.
func TestDiscover_FiltersAndMixesScaleTargets(t *testing.T) {
	scheme := newScheme(t)

	managedHPA := hpaWith(managedAnn(), nil)
	managedHPA.Name = hpaManagedName

	unmanagedHPA := hpaWith(nil, nil)
	unmanagedHPA.Name = "hpa-unmanaged"

	managedHPAWithWVA := hpaWith(managedAnn(), []autoscalingv2.MetricSpec{wvaExternalMetric()})
	managedHPAWithWVA.Name = "hpa-managed-wva"

	managedKEDAOwnedHPA := hpaWith(managedAnn(), nil, kedaOwnerRef())
	managedKEDAOwnedHPA.Name = "hpa-keda-owned"

	managedSO := soWith(managedAnn(), nil)
	managedSO.Name = "so-managed"

	managedSOWithWVA := soWith(managedAnn(), []kedav1alpha1.ScaleTriggers{promTriggerWVA()})
	managedSOWithWVA.Name = "so-managed-wva"

	unmanagedSO := soWith(nil, nil)
	unmanagedSO.Name = "so-unmanaged"

	objs := []client.Object{
		managedHPA,
		unmanagedHPA,
		managedHPAWithWVA,
		managedKEDAOwnedHPA,
		managedSO,
		managedSOWithWVA,
		unmanagedSO,
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	coord, err := New(c, nil, Options{KEDAEnabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := coord.discover(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	gotNames := map[string]string{}
	for _, o := range got {
		kind := "?"
		switch o.(type) {
		case *autoscalingv2.HorizontalPodAutoscaler:
			kind = "HPA"
		case *kedav1alpha1.ScaledObject:
			kind = "SO"
		}
		gotNames[o.GetName()] = kind
	}

	want := map[string]string{
		hpaManagedName: "HPA",
		"so-managed":   "SO",
	}
	if fmt.Sprintf("%v", gotNames) != fmt.Sprintf("%v", want) {
		t.Fatalf("discover names = %v, want %v", gotNames, want)
	}
}

// TestDiscover_KEDADisabled_OmitsScaledObjects asserts that when the
// KEDA CRD is absent (KEDAEnabled=false), the loop never lists SOs.
func TestDiscover_KEDADisabled_OmitsScaledObjects(t *testing.T) {
	scheme := newScheme(t)

	managedHPA := hpaWith(managedAnn(), nil)
	managedHPA.Name = hpaManagedName

	managedSO := soWith(managedAnn(), nil)
	managedSO.Name = "so-managed"

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(managedHPA, managedSO).
		Build()

	coord, err := New(c, nil, Options{KEDAEnabled: false})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := coord.discover(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 selected target (HPA only), got %d", len(got))
	}
	if got[0].GetName() != hpaManagedName {
		t.Fatalf("expected hpa-managed, got %s", got[0].GetName())
	}
}

// TestStart_DispatchesToAllPluginsAndContinuesOnError starts the loop
// briefly, verifies every registered plugin receives the same selected
// slice, and that an error from one plugin does not prevent the next
// plugin from being called.
func TestStart_DispatchesToAllPluginsAndContinuesOnError(t *testing.T) {
	scheme := newScheme(t)

	managedHPA := hpaWith(managedAnn(), nil)
	managedHPA.Name = hpaManagedName

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(managedHPA).
		Build()

	failing := &stubPlugin{name: "failing", err: errors.New("boom")}
	healthy := &stubPlugin{name: "healthy"}

	cycleErrs := map[string]int{}
	var cycleErrsMu sync.Mutex

	coord, err := New(c, []Plugin{failing, healthy}, Options{
		Interval:    20 * time.Millisecond,
		KEDAEnabled: false,
		CycleErrorCounter: func(kind string) {
			cycleErrsMu.Lock()
			defer cycleErrsMu.Unlock()
			cycleErrs[kind]++
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- coord.Start(ctx) }()

	<-ctx.Done()
	if err := <-done; err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	failingCalls := failing.calls()
	healthyCalls := healthy.calls()
	if len(failingCalls) == 0 {
		t.Fatal("failing plugin received no Tick calls")
	}
	if len(healthyCalls) == 0 {
		t.Fatal("healthy plugin received no Tick calls (loop aborted on plugin error?)")
	}
	if len(failingCalls) != len(healthyCalls) {
		t.Fatalf("mismatched call counts: failing=%d healthy=%d", len(failingCalls), len(healthyCalls))
	}
	// Each tick must dispatch the same selected slice to every plugin.
	for i := range failingCalls {
		if len(failingCalls[i]) != 1 || len(healthyCalls[i]) != 1 {
			t.Fatalf("tick %d dispatched %d/%d items, want 1/1", i, len(failingCalls[i]), len(healthyCalls[i]))
		}
		if failingCalls[i][0].GetName() != hpaManagedName || healthyCalls[i][0].GetName() != hpaManagedName {
			t.Fatalf("tick %d dispatched wrong target: failing=%s healthy=%s",
				i, failingCalls[i][0].GetName(), healthyCalls[i][0].GetName())
		}
	}

	cycleErrsMu.Lock()
	if cycleErrs["discovery"] != 0 {
		t.Errorf("unexpected discovery cycle errors: %d", cycleErrs["discovery"])
	}
	cycleErrsMu.Unlock()
}

// TestStart_NoPluginsIsNoop verifies that with zero plugins registered,
// Start does not error and quietly waits for cancellation.
func TestStart_NoPluginsIsNoop(t *testing.T) {
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	coord, err := New(c, nil, Options{Interval: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := coord.Start(ctx); err != nil {
		t.Fatalf("Start with no plugins should not error, got %v", err)
	}
}

// TestStart_NoSelectionIsNoop ensures that with no scale targets in the
// cluster, plugins are not invoked.
func TestStart_NoSelectionIsNoop(t *testing.T) {
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	p := &stubPlugin{name: "p"}
	coord, err := New(c, []Plugin{p}, Options{Interval: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := coord.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(p.calls()) != 0 {
		t.Fatalf("plugin called %d times despite empty selection set", len(p.calls()))
	}
}

// quiet a lint about unused import of annotations when only used in helper
var _ = annotations.Managed
