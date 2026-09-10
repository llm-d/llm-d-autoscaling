package gpurebalance

import (
	"fmt"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Damping guards for the rebalance patch loop.
//
// The plugin recomputes every ceiling from an instantaneous EPP queue-depth
// reading each tick. Queue depth fluctuates a few percent between scrapes, and
// the allocation is a floor() of a weight, so a one-unit change in queue can
// move a ceiling across a fractional boundary and back again. Patching on every
// tick turns that noise into real pod churn: a lowered ceiling is enforced by
// the HPA immediately, evicting pods mid-request and bypassing the
// scaleDown.stabilizationWindowSeconds the operator configured.
//
// The two directions are not symmetric, so they are not damped the same way:
//
//   - Raising a ceiling is cheap and reversible. It only widens the range the
//     HPA may use; the HPA still decides whether to actually add pods, and its
//     own scale-up policy still applies. Delaying it makes real demand spikes
//     late, which is its own SLO problem.
//   - Lowering a ceiling is destructive. It can force an immediate scale-down.
//     A lower target therefore has to prove it is not noise before it is acted
//     on: it must persist across several consecutive ticks.
//
// This is deliberately a stabilization window expressed in ticks rather than a
// blanket cooldown, so it mirrors the semantics the operator already expects
// from the HPA instead of inventing a second, unrelated delay.
const (
	// downscaleStabilizationTicks is how many consecutive ticks a lower ceiling
	// must be computed before it is applied.
	//
	// At the default COORDINATOR_INTERVAL of 15s this is one minute of agreement,
	// which is long enough to ride out the few-percent scrape-to-scrape jitter
	// described in the issue, and short enough to stay well inside the HPA's own
	// 300s default scale-down stabilization window — so the Coordinator relaxing
	// a ceiling never becomes the slowest link in a genuine scale-down.
	downscaleStabilizationTicks = 4
)

// stabilizer tracks, per managed scaler, how many consecutive ticks have
// computed a ceiling below the one currently set.
//
// Coordinator ticks are dispatched sequentially from a single loop, so this is
// not contended in practice; the mutex is here so the plugin stays safe if the
// loop ever fans out, and costs nothing in the current path.
type stabilizer struct {
	mu sync.Mutex
	// downTicks maps a scaler key to the number of consecutive ticks its
	// computed target has been strictly below its current ceiling.
	downTicks map[string]int
}

func newStabilizer() *stabilizer {
	return &stabilizer{downTicks: make(map[string]int)}
}

// scalerKey identifies a managed scaler across ticks.
//
// Namespace and name alone are not enough: an HPA and a KEDA ScaledObject can
// share a name in one namespace, and they are distinct scalers to this plugin.
// GroupVersionKind is not always populated on typed objects read through the
// controller-runtime client, so the plugin's own display kind is used instead.
func scalerKey(ns, displayKind string, obj client.Object) string {
	return fmt.Sprintf("%s/%s/%s", ns, displayKind, obj.GetName())
}

// shouldPatch reports whether a newly computed ceiling should be written now,
// and records what it observed for the next tick.
//
//   - target == current  -> no patch, and any pending downgrade streak resets
//     (the ceiling agrees with reality again, so earlier low readings were noise)
//   - target > current   -> patch immediately, and reset the streak
//   - target < current   -> only patch once this has held for
//     downscaleStabilizationTicks consecutive ticks; reset the streak when it does
//
// The streak resets on apply as well as on recovery. Without that, a scaler
// drifting steadily downward would clear the threshold once and then patch on
// every subsequent tick, which is the churn this exists to prevent: each
// downgrade has to earn its own stabilization window.
func (s *stabilizer) shouldPatch(key string, current, target int32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if target >= current {
		// Nothing to hold back: either the ceiling is being raised, which cannot
		// evict anything, or it already agrees with the computed target. Either
		// way any pending downgrade was noise.
		delete(s.downTicks, key)
		return target > current
	}

	s.downTicks[key]++
	if s.downTicks[key] < downscaleStabilizationTicks {
		return false
	}
	delete(s.downTicks, key)
	return true
}

// retain drops state for scalers that were not seen this tick, so the map does
// not grow without bound as ScaledObjects and HPAs come and go. Called once per
// tick with every key observed across all namespaces.
func (s *stabilizer) retain(seen map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for k := range s.downTicks {
		if _, ok := seen[k]; !ok {
			delete(s.downTicks, k)
		}
	}
}
