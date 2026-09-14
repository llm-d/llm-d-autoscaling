package tuner

import (
	"math"

	"gonum.org/v1/gonum/mat"
)

// FloatEqual checks if two float64 numbers are approximately equal within a given epsilon.
func FloatEqual(a, b, epsilon float64) bool {
	// Handle the case where they are exactly equal.
	if a == b {
		return true
	}

	// Calculate the absolute difference.
	diff := math.Abs(a - b)

	// Compare the absolute difference with a combination of absolute and relative tolerance.
	// This helps handle cases with very small or very large numbers.
	if a == 0.0 || b == 0.0 || diff < math.SmallestNonzeroFloat64 {
		return diff < (epsilon * math.SmallestNonzeroFloat64)
	}
	return diff/(math.Abs(a)+math.Abs(b)) < epsilon
}

// IsSymmetric checks if a given mat.Matrix is symmetric.
func IsSymmetric(m mat.Matrix, epsilon float64) bool {
	r, c := m.Dims()

	// 1. Check if it's a square matrix
	if r != c {
		return false
	}

	// 2. Check if elements are equal to their transposes
	// We only need to check the upper or lower triangle (excluding the diagonal)
	// because if a_ij = a_ji, then a_ji = a_ij is also true.
	for i := 0; i < r; i++ {
		for j := i + 1; j < c; j++ { // Start from j = i + 1 to avoid checking diagonal and duplicates
			if !FloatEqual(m.At(i, j), m.At(j, i), epsilon) {
				return false
			}
		}
	}

	return true
}

// GetFactoredSlice multiplies each element in a slice by multiplier and returns the new slice.
func GetFactoredSlice(x []float64, multiplier float64) []float64 {
	y := make([]float64, len(x))
	for i, val := range x {
		y[i] = val * multiplier
	}
	return y
}

// createTunerConfigFromData builds a TunerConfigData for a specific variant.
// FilterData is shared for all variants (from filterConfig or defaults). TunerModelData is always
// built per-variant from the environment because different variants run on
// different accelerators with different latency characteristics.
func CreateTunerConfigFromData(filterDataFromConfig *FilterData, env *Environment) *TunerConfigData {
	// FilterData: use user-provided config or defaults, if missing
	var filterData FilterData
	if filterDataFromConfig != nil {
		filterData = *filterDataFromConfig
	} else {
		filterData = FilterData{
			GammaFactor: DefaultGammaFactor,
			ErrorLevel:  DefaultErrorLevel,
			TPercentile: DefaultTPercentile,
		}
	}

	// TunerModelData: always built per-variant from environment
	// State vector: [alpha, beta, gamma]
	// Using reasonable initial estimates (will be refined by filter)
	initState := []float64{DefaultAlpha, DefaultBeta, DefaultGamma}

	// Percent change per parameter (using DefaultPercentChange)
	percentChange := []float64{
		DefaultPercentChange,
		DefaultPercentChange,
		DefaultPercentChange,
	}

	// State bounds using factors
	minState := GetFactoredSlice(initState, DefaultMinStateFactor)
	maxState := GetFactoredSlice(initState, DefaultMaxStateFactor)

	// Expected observations [TTFT, ITL] in milliseconds
	// Use actual observations from environment if valid, otherwise use typical values
	var expectedObservations []float64
	if env != nil && env.Valid() {
		expectedObservations = []float64{float64(env.AvgTTFT), float64(env.AvgITL)}
	} else {
		expectedObservations = []float64{DefaultExpectedTTFT, DefaultExpectedITL}
	}

	return &TunerConfigData{
		FilterData: filterData,
		ModelData: TunerModelData{
			InitState:            initState,
			InitCovarianceMatrix: nil, // Will use defaults in tuner
			PercentChange:        percentChange,
			BoundedState:         true,
			MinState:             minState,
			MaxState:             maxState,
			ExpectedObservations: expectedObservations,
		},
	}
}
