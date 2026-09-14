package coordinator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultInterval is the default Coordinator loop interval.
const DefaultInterval = 15 * time.Second

// Options configures the Coordinator.
type Options struct {
	// Interval between successive Coordinator ticks. Defaults to
	// DefaultInterval when zero.
	Interval time.Duration

	// KEDAEnabled controls whether the Coordinator lists KEDA
	// ScaledObjects each tick. When false, the selected set contains
	// only HPAs.
	KEDAEnabled bool

	// CycleErrorCounter is invoked once per kind of cycle-level failure
	// (e.g. discovery). It is the loop's only metrics emission point;
	// plugins emit their own metrics. May be nil for tests.
	CycleErrorCounter CycleErrorCounter
}

// CycleErrorCounter records loop-level cycle errors. The loop calls Inc
// with a "kind" label (e.g. "discovery").
type CycleErrorCounter func(kind string)

// Coordinator is a leader-elected periodic loop that selects scale
// targets under its control and dispatches them to a registered set of
// plugins. Wire one into the controller manager via mgr.Add(c) so the
// loop only runs on the leader.
type Coordinator struct {
	client  client.Client
	plugins []Plugin
	opts    Options
}

// New constructs a Coordinator. Plugins are dispatched in declared order
// each tick. Plugin names must be unique across the registered set.
func New(c client.Client, plugins []Plugin, opts Options) (*Coordinator, error) {
	if c == nil {
		return nil, errors.New("coordinator: client must not be nil")
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	seen := make(map[string]struct{}, len(plugins))
	for _, p := range plugins {
		if p == nil {
			return nil, errors.New("coordinator: nil plugin in registration list")
		}
		name := p.Name()
		if name == "" {
			return nil, errors.New("coordinator: plugin Name() must not be empty")
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("coordinator: duplicate plugin name %q", name)
		}
		seen[name] = struct{}{}
	}
	return &Coordinator{
		client:  c,
		plugins: plugins,
		opts:    opts,
	}, nil
}

// Start runs the Coordinator loop until ctx is cancelled. It satisfies
// controller-runtime's manager.Runnable interface.
func (c *Coordinator) Start(ctx context.Context) error {
	logger := ctrl.LoggerFrom(ctx).WithName("coordinator")
	logger.Info("Coordinator loop starting",
		"interval", c.opts.Interval,
		"plugins", c.pluginNames(),
		"kedaEnabled", c.opts.KEDAEnabled,
	)

	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()

	// Run a tick immediately on start, then at every interval, so the
	// first dispatch does not wait one full interval.
	c.tick(ctx, logger)

	for {
		select {
		case <-ctx.Done():
			logger.Info("Coordinator loop stopping", "reason", ctx.Err())
			return nil
		case <-ticker.C:
			c.tick(ctx, logger)
		}
	}
}

// tick executes one Coordinator cycle: list scale targets, filter, and
// dispatch. The loop never writes to the cluster directly; plugin errors
// do not abort the loop.
func (c *Coordinator) tick(ctx context.Context, logger logr.Logger) {
	selected, err := c.discover(ctx)
	if err != nil {
		logger.Error(err, "Coordinator discovery failed; skipping cycle")
		c.recordCycleError("discovery")
		return
	}

	if len(selected) == 0 || len(c.plugins) == 0 {
		logger.V(1).Info("Coordinator tick is a no-op",
			"selected", len(selected),
			"plugins", len(c.plugins),
		)
		return
	}

	logger.V(1).Info("Coordinator dispatching to plugins",
		"selected", len(selected),
		"plugins", len(c.plugins),
	)

	for _, p := range c.plugins {
		if err := p.Tick(ctx, selected); err != nil {
			// Plugins are expected to handle their own errors and
			// only return one for failures the operator should see.
			// The loop continues to the next plugin regardless.
			logger.Error(err, "Coordinator plugin returned an error", "plugin", p.Name())
		}
	}
}

// discover lists all HPAs and (if KEDA is enabled) ScaledObjects, applies
// the per-kind selection rule, and returns the union as a single mixed
// slice of client.Object.
//
// Discovery errors return immediately so the loop counts a cycle error
// rather than dispatching a partial set.
func (c *Coordinator) discover(ctx context.Context) ([]client.Object, error) {
	var selected []client.Object

	hpaList := &autoscalingv2.HorizontalPodAutoscalerList{}
	if err := c.client.List(ctx, hpaList); err != nil {
		return nil, fmt.Errorf("listing HorizontalPodAutoscalers: %w", err)
	}
	for i := range hpaList.Items {
		hpa := &hpaList.Items[i]
		if IsHPAUnderControl(hpa) {
			selected = append(selected, hpa)
		}
	}

	if c.opts.KEDAEnabled {
		soList := &kedav1alpha1.ScaledObjectList{}
		if err := c.client.List(ctx, soList); err != nil {
			// NoKindMatchError can occur if the CRD was uninstalled
			// after startup; tolerate it instead of failing the cycle.
			if apimeta.IsNoMatchError(err) {
				return selected, nil
			}
			return nil, fmt.Errorf("listing ScaledObjects: %w", err)
		}
		for i := range soList.Items {
			so := &soList.Items[i]
			if IsScaledObjectUnderControl(so) {
				selected = append(selected, so)
			}
		}
	}

	return selected, nil
}

// pluginNames returns the registered plugin names in declared order, for
// logging.
func (c *Coordinator) pluginNames() []string {
	out := make([]string, 0, len(c.plugins))
	for _, p := range c.plugins {
		out = append(out, p.Name())
	}
	return out
}

// recordCycleError forwards to the configured counter when set.
func (c *Coordinator) recordCycleError(kind string) {
	if c.opts.CycleErrorCounter != nil {
		c.opts.CycleErrorCounter(kind)
	}
}
