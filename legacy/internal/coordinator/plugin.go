// Package coordinator hosts the Coordinator component: a leader-elected,
// periodic loop that computes the set of scale targets (HPAs and KEDA
// ScaledObjects) under Coordinator control and dispatches them to a
// registered set of plugins.
//
// The loop and the selection rules live here; what each tick does is the
// responsibility of plugins, which are self-contained packages under
// internal/coordinator/plugins/<name>/.
package coordinator

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Plugin is the contract every Coordinator capability satisfies. The
// Coordinator does not inspect anything inside a plugin beyond this
// interface and the plugin's name.
type Plugin interface {
	// Name uniquely identifies the plugin (e.g. "gpu-rebalance"). It is
	// used in metrics labels, event reasons, and the per-plugin damping
	// cache key.
	Name() string

	// Tick is called once per Coordinator interval, on the leader, with
	// the set of scale targets the loop has decided are under
	// Coordinator control this tick. Each element is one of:
	//   - *autoscalingv2.HorizontalPodAutoscaler
	//   - *kedav1alpha1.ScaledObject
	//
	// Plugins type-switch on the concrete kind and ignore kinds they do
	// not handle.
	//
	// Plugins MUST be deterministic given the same input. Plugins MUST
	// return nil for "no work this cycle" and only return a non-nil
	// error for failures the operator should see in the Coordinator's
	// cycle-error counter.
	Tick(ctx context.Context, selected []client.Object) error
}
