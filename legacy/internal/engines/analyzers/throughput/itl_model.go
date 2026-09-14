package throughput

import "math"

// itlSlopeEpsilon is the smallest slope A that counts as meaningfully positive.
// A perfectly flat fit is mathematically A == 0 but OLS rounding leaves noise of
// order 1e-17; real slopes in this domain are order 1e-2. This threshold sits far
// above the noise floor and far below any genuine slope.
const itlSlopeEpsilon = 1e-12

// ITLModel is the linear inter-token latency model: ITL(k) = A·k + B.
// A is the slope (marginal latency cost per unit of KV utilization) and B is the
// hardware baseline latency observed at near-zero KV load.
type ITLModel struct {
	A float64
	B float64
}

// IsZero returns true when the model has not been fitted (both coefficients are zero).
func (m ITLModel) IsZero() bool {
	return m.A == 0 && m.B == 0
}

// ITLAt returns the predicted ITL (seconds/token) at KV utilization k.
func (m ITLModel) ITLAt(k float64) float64 {
	return m.A*k + m.B
}

// validITLModel reports whether an (A, B) pair is a usable ITL model: finite,
// meaningfully-positive slope, finite intercept, and positive ITL at saturation.
// Shared by the Tier-1 OLS fit (FitITLModel) and the Tier-2 constrained fit
// (resolveITLModel) so the two validation paths cannot drift apart.
func validITLModel(a, b float64) bool {
	// Defensive guard: NaN/+Inf a both slip past the a <= 0 check below, and a non-zero
	// sumK can leave b finite so the b guard would not catch them. Symmetric with the b guard.
	if math.IsNaN(a) || math.IsInf(a, 0) {
		return false
	}
	// Reject flat/inverted fits. A perfectly flat line (constant ITL) is
	// mathematically a == 0, but OLS rounding leaves tiny noise whose sign is
	// platform-dependent (e.g. +2.6e-17 on arm64, ≤0 on amd64). Compare against a
	// small epsilon so any slope that isn't meaningfully positive counts as flat.
	// Real slopes in this domain are order 1e-2; noise is order 1e-17.
	if a <= itlSlopeEpsilon {
		return false
	}
	// Defensive guard: NaN/Inf b is mathematically possible with degenerate input.
	if math.IsNaN(b) || math.IsInf(b, 0) {
		return false
	}
	// Guard: ensure ITL at saturation is positive. A noisy fit can yield negative
	// b (valid a>0), making ITLAt(DefaultKSat) near-zero and inflating supply.
	if a*DefaultKSat+b <= 0 {
		return false
	}
	return true
}

// FitITLModel fits the linear model ITL(k) = A·k + B to the observations using
// ordinary least squares (OLS). Returns (model, true) on success.
//
// Returns (zero, false) when:
//   - fewer than 2 observations are provided
//   - k-spread across observations is zero (degenerate — no discriminating signal)
//   - the fitted (A, B) fails validITLModel (inverted/flat/non-finite/non-positive-at-saturation)
func FitITLModel(obs []ITLObservation) (ITLModel, bool) {
	n := float64(len(obs))
	if n < 2 {
		return ITLModel{}, false
	}

	var sumK, sumITL, sumK2, sumKITL float64
	for _, o := range obs {
		sumK += o.K
		sumITL += o.ITLSec
		sumK2 += o.K * o.K
		sumKITL += o.K * o.ITLSec
	}

	denom := n*sumK2 - sumK*sumK
	if math.Abs(denom) < 1e-12 {
		return ITLModel{}, false
	}

	A := (n*sumKITL - sumK*sumITL) / denom
	B := (sumITL - A*sumK) / n

	if !validITLModel(A, B) {
		return ITLModel{}, false
	}

	return ITLModel{A: A, B: B}, true
}
