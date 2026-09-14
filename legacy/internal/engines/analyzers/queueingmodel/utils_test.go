package queueingmodel

import (
	"testing"

	"gonum.org/v1/gonum/mat"
)

func TestFlattenCovariance(t *testing.T) {
	tests := []struct {
		name string
		cov  [][]float64
		want []float64
	}{
		{
			name: "nil input",
			cov:  nil,
			want: nil,
		},
		{
			name: "empty input",
			cov:  [][]float64{},
			want: nil,
		},
		{
			name: "1x1 matrix",
			cov:  [][]float64{{5.0}},
			want: []float64{5.0},
		},
		{
			name: "2x2 matrix",
			cov:  [][]float64{{1, 2}, {3, 4}},
			want: []float64{1, 2, 3, 4},
		},
		{
			name: "3x3 identity",
			cov:  [][]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}},
			want: []float64{1, 0, 0, 0, 1, 0, 0, 0, 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := flattenCovariance(tt.cov)
			if tt.want == nil {
				if got != nil {
					t.Errorf("got %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestMatrixToSlice2D(t *testing.T) {
	tests := []struct {
		name string
		m    *mat.Dense
		want [][]float64
	}{
		{
			name: "nil matrix",
			m:    nil,
			want: nil,
		},
		{
			name: "1x1 matrix",
			m:    mat.NewDense(1, 1, []float64{7}),
			want: [][]float64{{7}},
		},
		{
			name: "2x3 matrix",
			m:    mat.NewDense(2, 3, []float64{1, 2, 3, 4, 5, 6}),
			want: [][]float64{{1, 2, 3}, {4, 5, 6}},
		},
		{
			name: "3x3 identity",
			m: mat.NewDense(3, 3, []float64{
				1, 0, 0,
				0, 1, 0,
				0, 0, 1,
			}),
			want: [][]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matrixToSlice2D(tt.m)
			if tt.want == nil {
				if got != nil {
					t.Errorf("got %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("rows = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if len(got[i]) != len(tt.want[i]) {
					t.Fatalf("row %d cols = %d, want %d", i, len(got[i]), len(tt.want[i]))
				}
				for j := range got[i] {
					if got[i][j] != tt.want[i][j] {
						t.Errorf("[%d][%d] = %v, want %v", i, j, got[i][j], tt.want[i][j])
					}
				}
			}
		})
	}
}

func TestMatrixToSlice2D_RoundTrip(t *testing.T) {
	original := [][]float64{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}}
	flat := flattenCovariance(original)
	m := mat.NewDense(3, 3, flat)
	got := matrixToSlice2D(m)

	for i := range original {
		for j := range original[i] {
			if got[i][j] != original[i][j] {
				t.Errorf("[%d][%d] = %v, want %v", i, j, got[i][j], original[i][j])
			}
		}
	}
}

func TestMakeModelKey(t *testing.T) {
	tests := []struct {
		namespace string
		modelID   string
		want      string
	}{
		{"default", "llama", "default/llama"},
		{"ns1", "gpt-4", "ns1/gpt-4"},
		{"", "", "/"},
		{"ns", "", "ns/"},
	}

	for _, tt := range tests {
		got := MakeModelKey(tt.namespace, tt.modelID)
		if got != tt.want {
			t.Errorf("MakeModelKey(%q, %q) = %q, want %q", tt.namespace, tt.modelID, got, tt.want)
		}
	}
}

func TestMakeVariantKey(t *testing.T) {
	tests := []struct {
		namespace   string
		variantName string
		want        string
	}{
		{"default", "v1", "default/v1"},
		{"ns", "large-variant", "ns/large-variant"},
		{"", "", "/"},
	}

	for _, tt := range tests {
		got := makeVariantKey(tt.namespace, tt.variantName)
		if got != tt.want {
			t.Errorf("makeVariantKey(%q, %q) = %q, want %q", tt.namespace, tt.variantName, got, tt.want)
		}
	}
}

func TestStateVectorToParams(t *testing.T) {
	tests := []struct {
		name                           string
		v                              []float64
		wantAlpha, wantBeta, wantGamma float64
	}{
		{
			name:      "nil vector",
			v:         nil,
			wantAlpha: 0, wantBeta: 0, wantGamma: 0,
		},
		{
			name:      "empty vector",
			v:         []float64{},
			wantAlpha: 0, wantBeta: 0, wantGamma: 0,
		},
		{
			name:      "short vector (len 2)",
			v:         []float64{1, 2},
			wantAlpha: 0, wantBeta: 0, wantGamma: 0,
		},
		{
			name:      "exact 3 elements",
			v:         []float64{1.1, 2.2, 3.3},
			wantAlpha: 1.1, wantBeta: 2.2, wantGamma: 3.3,
		},
		{
			name:      "longer vector ignores extras",
			v:         []float64{1.1, 2.2, 3.3, 4.4, 5.5},
			wantAlpha: 1.1, wantBeta: 2.2, wantGamma: 3.3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alpha, beta, gamma := StateVectorToParams(tt.v)
			if alpha != tt.wantAlpha {
				t.Errorf("alpha = %v, want %v", alpha, tt.wantAlpha)
			}
			if beta != tt.wantBeta {
				t.Errorf("beta = %v, want %v", beta, tt.wantBeta)
			}
			if gamma != tt.wantGamma {
				t.Errorf("gamma = %v, want %v", gamma, tt.wantGamma)
			}
		})
	}
}

func TestParamsToStateVector(t *testing.T) {
	tests := []struct {
		name               string
		alpha, beta, gamma float64
	}{
		{"zeros", 0, 0, 0},
		{"positive values", 1.1, 2.2, 3.3},
		{"negative values", -1.0, -2.0, -3.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := ParamsToStateVector(tt.alpha, tt.beta, tt.gamma)
			if len(v) != 3 {
				t.Fatalf("len = %d, want 3", len(v))
			}
			if v[0] != tt.alpha {
				t.Errorf("v[0] = %v, want %v", v[0], tt.alpha)
			}
			if v[1] != tt.beta {
				t.Errorf("v[1] = %v, want %v", v[1], tt.beta)
			}
			if v[2] != tt.gamma {
				t.Errorf("v[2] = %v, want %v", v[2], tt.gamma)
			}
		})
	}
}

func TestParamsRoundTrip(t *testing.T) {
	alpha, beta, gamma := 1.5, 2.5, 3.5
	v := ParamsToStateVector(alpha, beta, gamma)
	gotAlpha, gotBeta, gotGamma := StateVectorToParams(v)

	if gotAlpha != alpha || gotBeta != beta || gotGamma != gamma {
		t.Errorf("round-trip failed: got (%v, %v, %v), want (%v, %v, %v)",
			gotAlpha, gotBeta, gotGamma, alpha, beta, gamma)
	}
}
