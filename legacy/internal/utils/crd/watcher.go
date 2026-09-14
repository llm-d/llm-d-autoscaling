package crd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

var ErrRestartRequired = errors.New("CRD installed after startup, controller restart required")

const defaultProbeInterval = time.Minute

type Target struct {
	CRDName       string
	GroupVersion  string
	Kind          string
	PresentAtBoot bool
}

type probeFunc func(groupVersion, kind string, logger logr.Logger) (bool, error)

type Watcher struct {
	targets  []Target
	probe    probeFunc
	interval time.Duration
}

var _ manager.LeaderElectionRunnable = (*Watcher)(nil)

func NewWatcher(restConfig *rest.Config, targets []Target) *Watcher {
	return &Watcher{
		targets: targets,
		probe: func(groupVersion, kind string, logger logr.Logger) (bool, error) {
			return DetectCRDInstalled(restConfig, groupVersion, kind, logger)
		},
		interval: defaultProbeInterval,
	}
}

func (w *Watcher) NeedLeaderElection() bool { return false }

func (w *Watcher) Start(ctx context.Context) error {
	logger := ctrl.LoggerFrom(ctx).WithName("crd-watcher")

	pending := make([]Target, 0, len(w.targets))
	for _, t := range w.targets {
		if !t.PresentAtBoot {
			pending = append(pending, t)
		}
	}
	if len(pending) == 0 {
		logger.V(logging.DEBUG).Info("all optional CRDs present at startup, CRD probe idle")
		<-ctx.Done()
		return nil
	}

	for _, t := range pending {
		logger.V(logging.DEBUG).Info("probing for post-startup CRD installation",
			"crd", t.CRDName, "groupVersion", t.GroupVersion, "kind", t.Kind, "interval", w.interval)
	}

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			for _, t := range pending {
				installed, err := w.probe(t.GroupVersion, t.Kind, logger)
				if err != nil {
					logger.V(logging.DEBUG).Info("discovery probe failed, will retry",
						"crd", t.CRDName, "error", err)
					continue
				}
				if !installed {
					continue
				}
				logger.Info("CRD available after controller startup, restarting to enable support",
					"crd", t.CRDName, "groupVersion", t.GroupVersion, "kind", t.Kind)
				return fmt.Errorf("%w: %s", ErrRestartRequired, t.CRDName)
			}
		}
	}
}
