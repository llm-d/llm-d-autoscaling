package crd

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

const (
	testCRDName      = "scaledobjects.keda.sh"
	testGroupVersion = "keda.sh/v1alpha1"
	testKind         = "ScaledObject"
)

func absentTarget() Target {
	return Target{
		CRDName:       testCRDName,
		GroupVersion:  testGroupVersion,
		Kind:          testKind,
		PresentAtBoot: false,
	}
}

func TestWatcher_NeedLeaderElectionIsFalse(t *testing.T) {
	w := &Watcher{}
	if w.NeedLeaderElection() {
		t.Error("NeedLeaderElection() = true, want false")
	}
}

func TestWatcher_AllTargetsPresentAtBootStaysIdle(t *testing.T) {
	present := absentTarget()
	present.PresentAtBoot = true

	w := &Watcher{
		targets:  []Target{present},
		interval: time.Millisecond,
		probe: func(string, string, logr.Logger) (bool, error) {
			t.Error("probe must not run when every target was present at boot")
			return true, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start() = %v, want nil on context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

func TestWatcher_StartReturnsRestartRequiredOnDetection(t *testing.T) {
	w := &Watcher{
		targets:  []Target{absentTarget()},
		interval: time.Millisecond,
		probe: func(string, string, logr.Logger) (bool, error) {
			return true, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := w.Start(ctx)
	if !errors.Is(err, ErrRestartRequired) {
		t.Fatalf("Start() = %v, want ErrRestartRequired", err)
	}
	if !errors.Is(err, ErrRestartRequired) || err.Error() != ErrRestartRequired.Error()+": "+testCRDName {
		t.Errorf("Start() = %q, want the CRD name appended", err)
	}
}

func TestWatcher_StartKeepsProbingWhileAbsent(t *testing.T) {
	var calls atomic.Int32
	w := &Watcher{
		targets:  []Target{absentTarget()},
		interval: time.Millisecond,
		probe: func(string, string, logr.Logger) (bool, error) {
			calls.Add(1)
			return false, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	if err := w.Start(ctx); err != nil {
		t.Errorf("Start() = %v, want nil while the CRD stays absent", err)
	}
	if calls.Load() < 2 {
		t.Errorf("probe ran %d times, want repeated probing", calls.Load())
	}
}

func TestWatcher_StartKeepsProbingAfterProbeError(t *testing.T) {
	var calls atomic.Int32
	w := &Watcher{
		targets:  []Target{absentTarget()},
		interval: time.Millisecond,
		probe: func(string, string, logr.Logger) (bool, error) {
			if calls.Add(1) < 3 {
				return false, errors.New("discovery unavailable")
			}
			return true, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := w.Start(ctx); !errors.Is(err, ErrRestartRequired) {
		t.Fatalf("Start() = %v, want ErrRestartRequired after transient probe errors", err)
	}
}

func TestWatcher_StartProbesEveryPendingTarget(t *testing.T) {
	lws := Target{
		CRDName:      "leaderworkersets.leaderworkerset.x-k8s.io",
		GroupVersion: "leaderworkerset.x-k8s.io/v1",
		Kind:         "LeaderWorkerSet",
	}
	keda := absentTarget()

	seen := make(chan string, 8)
	w := &Watcher{
		targets:  []Target{lws, keda},
		interval: time.Millisecond,
		probe: func(groupVersion, _ string, _ logr.Logger) (bool, error) {
			select {
			case seen <- groupVersion:
			default:
			}
			return false, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	close(seen)

	probed := map[string]bool{}
	for gv := range seen {
		probed[gv] = true
	}
	for _, want := range []string{lws.GroupVersion, keda.GroupVersion} {
		if !probed[want] {
			t.Errorf("group version %q was never probed", want)
		}
	}
}
