package queueingmodel

import (
	"fmt"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/queueingmodel/tuner"
	"gonum.org/v1/gonum/mat"
)

// flattenCovariance converts a 2D covariance matrix to a flat slice.
func flattenCovariance(cov [][]float64) []float64 {
	if len(cov) == 0 {
		return nil
	}
	n := len(cov)
	flat := make([]float64, 0, n*n)
	for i := range n {
		flat = append(flat, cov[i]...)
	}
	return flat
}

// matrixToSlice2D converts a gonum mat.Dense to a 2D slice.
func matrixToSlice2D(m *mat.Dense) [][]float64 {
	if m == nil {
		return nil
	}
	rows, cols := m.Dims()
	result := make([][]float64, rows)
	for i := range rows {
		result[i] = make([]float64, cols)
		for j := 0; j < cols; j++ {
			result[i][j] = m.At(i, j)
		}
	}
	return result
}

// MakeModelKey creates a unique key for a model
func MakeModelKey(namespace, modelID string) string {
	return fmt.Sprintf("%s/%s", namespace, modelID)
}

// makeVariantKey creates a unique key for a variant
func makeVariantKey(namespace, variantName string) string {
	return fmt.Sprintf("%s/%s", namespace, variantName)
}

func StateVectorToParams(v []float64) (alpha, beta, gamma float64) {
	if len(v) < 3 {
		return 0, 0, 0
	}
	alpha = v[tuner.StateIndexAlpha]
	beta = v[tuner.StateIndexBeta]
	gamma = v[tuner.StateIndexGamma]
	return alpha, beta, gamma
}

func ParamsToStateVector(alpha, beta, gamma float64) (v []float64) {
	v = make([]float64, 3)
	v[tuner.StateIndexAlpha] = alpha
	v[tuner.StateIndexBeta] = beta
	v[tuner.StateIndexGamma] = gamma
	return v
}
