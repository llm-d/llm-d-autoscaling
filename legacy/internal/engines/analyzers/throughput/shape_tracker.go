package throughput

// ShapeTracker maintains the current workload shape bucket for one variant and
// detects when the workload has shifted enough to require a new ITL model fit.
//
// The shape is characterised by the variant-average (IL, OL) across all replicas.
// A shape change is declared when either IL or OL deviates from the stored shape
// by more than the configured tolerance fraction.
type ShapeTracker struct {
	current   WorkloadShape
	hasShape  bool
	tolerance float64
}

// newShapeTracker creates a ShapeTracker with the given fractional tolerance.
// For example, tolerance=0.20 means a ≥20% change in IL or OL triggers a reset.
func newShapeTracker(tolerance float64) *ShapeTracker {
	return &ShapeTracker{tolerance: tolerance}
}

// Observe updates the tracker with the variant-averaged (il, ol, hitRate) for
// this reconcile cycle and reports whether the shape bucket changed.
//
// On the first call (no prior shape), the shape is set and changed=false is
// returned — there is nothing to refit against yet.
//
// On subsequent calls, changed=true is returned when the new shape falls outside
// the tolerance band of the stored shape. The stored shape is updated to the new
// value regardless.
func (t *ShapeTracker) Observe(il, ol, hitRate float64) (shape WorkloadShape, changed bool) {
	next := newWorkloadShape(il, ol, hitRate)

	if !t.hasShape {
		t.current = next
		t.hasShape = true
		return next, false
	}

	changed = !next.Within(t.current, t.tolerance)
	t.current = next
	return next, changed
}

// Current returns the most recently stored shape and whether any shape has been
// observed yet. Returns (zero WorkloadShape, false) before the first Observe call.
func (t *ShapeTracker) Current() (WorkloadShape, bool) {
	return t.current, t.hasShape
}

// Reset clears the stored shape, as if the tracker had just been created.
// The next Observe call will set a fresh shape without triggering a change event.
func (t *ShapeTracker) Reset() {
	t.current = WorkloadShape{}
	t.hasShape = false
}
