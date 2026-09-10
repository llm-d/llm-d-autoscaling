package tuner

import (
	"math"
	"testing"

	"gonum.org/v1/gonum/mat"
)

// ---------------------------------------------------------------------------
// FloatEqual
// ---------------------------------------------------------------------------

func TestFloatEqual(t *testing.T) {
	tests := []struct {
		name    string
		a, b    float64
		epsilon float64
		want    bool
	}{
		{"exactly equal", 1.0, 1.0, 1e-9, true},
		{"zero both", 0.0, 0.0, 1e-9, true},
		{"within epsilon", 1.0, 1.0 + 1e-10, 1e-6, true},
		{"outside epsilon", 1.0, 2.0, 1e-6, false},
		{"one zero other nonzero", 0.0, 1e-200, 1e-6, false},
		{"large equal values", 1e15, 1e15, 1e-9, true},
		{"large close values", 1e15, 1e15 + 1e6, 1e-9, true},
		{"large different values", 1e15, 2e15, 1e-6, false},
		{"negative equal", -5.0, -5.0, 1e-9, true},
		{"negative different", -5.0, -4.0, 1e-9, false},
		{"small nonzero diff below epsilon", 1.0, 1.0 + 1e-12, 1e-6, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FloatEqual(tt.a, tt.b, tt.epsilon)
			if got != tt.want {
				t.Errorf("FloatEqual(%v, %v, %v) = %v, want %v", tt.a, tt.b, tt.epsilon, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// IsSymmetric
// ---------------------------------------------------------------------------

func TestIsSymmetric(t *testing.T) {
	eps := DefaultEpsilon

	tests := []struct {
		name string
		m    mat.Matrix
		want bool
	}{
		{
			name: "1x1 matrix is symmetric",
			m:    mat.NewDense(1, 1, []float64{7}),
			want: true,
		},
		{
			name: "non-square matrix is not symmetric",
			m:    mat.NewDense(2, 3, []float64{1, 2, 3, 4, 5, 6}),
			want: false,
		},
		{
			name: "symmetric 2x2",
			m:    mat.NewDense(2, 2, []float64{1, 2, 2, 4}),
			want: true,
		},
		{
			name: "non-symmetric 2x2",
			m:    mat.NewDense(2, 2, []float64{1, 2, 3, 4}),
			want: false,
		},
		{
			name: "symmetric 3x3 identity",
			m: mat.NewDense(3, 3, []float64{
				1, 0, 0,
				0, 1, 0,
				0, 0, 1,
			}),
			want: true,
		},
		{
			name: "symmetric 3x3 non-identity",
			m: mat.NewDense(3, 3, []float64{
				1, 2, 3,
				2, 5, 6,
				3, 6, 9,
			}),
			want: true,
		},
		{
			name: "non-symmetric 3x3",
			m: mat.NewDense(3, 3, []float64{
				1, 2, 3,
				4, 5, 6,
				7, 8, 9,
			}),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsSymmetric(tt.m, eps)
			if got != tt.want {
				t.Errorf("IsSymmetric() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GetFactoredSlice
// ---------------------------------------------------------------------------

func TestGetFactoredSlice(t *testing.T) {
	tests := []struct {
		name       string
		x          []float64
		multiplier float64
		want       []float64
	}{
		{
			name:       "empty slice",
			x:          []float64{},
			multiplier: 2.0,
			want:       []float64{},
		},
		{
			name:       "unit multiplier",
			x:          []float64{1, 2, 3},
			multiplier: 1.0,
			want:       []float64{1, 2, 3},
		},
		{
			name:       "double",
			x:          []float64{1, 2, 3},
			multiplier: 2.0,
			want:       []float64{2, 4, 6},
		},
		{
			name:       "zero multiplier",
			x:          []float64{1, 2, 3},
			multiplier: 0.0,
			want:       []float64{0, 0, 0},
		},
		{
			name:       "negative multiplier",
			x:          []float64{1, -2, 3},
			multiplier: -1.0,
			want:       []float64{-1, 2, -3},
		},
		{
			name:       "fractional multiplier",
			x:          []float64{10, 20},
			multiplier: 0.5,
			want:       []float64{5, 10},
		},
		{
			name:       "does not modify original",
			x:          []float64{1, 2, 3},
			multiplier: 3.0,
			want:       []float64{3, 6, 9},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Capture original before calling to check mutation
			original := make([]float64, len(tt.x))
			copy(original, tt.x)

			got := GetFactoredSlice(tt.x, tt.multiplier)

			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %v, want %v", i, got[i], tt.want[i])
				}
			}

			// Ensure original slice is not mutated
			for i := range tt.x {
				if tt.x[i] != original[i] {
					t.Errorf("original slice mutated at [%d]: got %v, want %v", i, tt.x[i], original[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Environment.Valid
// ---------------------------------------------------------------------------

func validEnv() *Environment {
	return &Environment{
		Lambda:        10.0,
		AvgInputToks:  512,
		AvgOutputToks: 128,
		MaxBatchSize:  8,
		AvgTTFT:       50.0,
		AvgITL:        5.0,
	}
}

func TestEnvironmentValid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Environment)
		want   bool
	}{
		{
			name:   "fully valid environment",
			mutate: nil,
			want:   true,
		},
		{
			name:   "zero lambda",
			mutate: func(e *Environment) { e.Lambda = 0 },
			want:   false,
		},
		{
			name:   "negative lambda",
			mutate: func(e *Environment) { e.Lambda = -1 },
			want:   false,
		},
		{
			name:   "inf lambda",
			mutate: func(e *Environment) { e.Lambda = float32(math.Inf(1)) },
			want:   false,
		},
		{
			name:   "NaN lambda",
			mutate: func(e *Environment) { e.Lambda = float32(math.NaN()) },
			want:   false,
		},
		{
			name:   "zero AvgInputToks",
			mutate: func(e *Environment) { e.AvgInputToks = 0 },
			want:   false,
		},
		{
			name:   "zero AvgOutputToks",
			mutate: func(e *Environment) { e.AvgOutputToks = 0 },
			want:   false,
		},
		{
			name:   "zero MaxBatchSize",
			mutate: func(e *Environment) { e.MaxBatchSize = 0 },
			want:   false,
		},
		{
			name:   "zero AvgTTFT",
			mutate: func(e *Environment) { e.AvgTTFT = 0 },
			want:   false,
		},
		{
			name:   "negative AvgTTFT",
			mutate: func(e *Environment) { e.AvgTTFT = -1 },
			want:   false,
		},
		{
			name:   "zero AvgITL",
			mutate: func(e *Environment) { e.AvgITL = 0 },
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validEnv()
			if tt.mutate != nil {
				tt.mutate(e)
			}
			got := e.Valid()
			if got != tt.want {
				t.Errorf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Environment.GetObservations
// ---------------------------------------------------------------------------

func TestEnvironmentGetObservations(t *testing.T) {
	e := &Environment{
		AvgTTFT: 42.5,
		AvgITL:  7.3,
	}
	obs := e.GetObservations()
	if obs == nil {
		t.Fatal("GetObservations() returned nil")
	}
	if obs.Len() != 2 {
		t.Fatalf("len = %d, want 2", obs.Len())
	}
	if obs.AtVec(0) != float64(e.AvgTTFT) {
		t.Errorf("obs[0] = %v, want %v", obs.AtVec(0), float64(e.AvgTTFT))
	}
	if obs.AtVec(1) != float64(e.AvgITL) {
		t.Errorf("obs[1] = %v, want %v", obs.AtVec(1), float64(e.AvgITL))
	}
}

// ---------------------------------------------------------------------------
// CreateTunerConfigFromData
// ---------------------------------------------------------------------------

func TestCreateTunerConfigFromData(t *testing.T) {
	t.Run("nil filterData uses defaults", func(t *testing.T) {
		env := validEnv()
		cfg := CreateTunerConfigFromData(nil, env)
		if cfg == nil {
			t.Fatal("got nil config")
		}
		if cfg.FilterData.GammaFactor != DefaultGammaFactor {
			t.Errorf("GammaFactor = %v, want %v", cfg.FilterData.GammaFactor, DefaultGammaFactor)
		}
		if cfg.FilterData.ErrorLevel != DefaultErrorLevel {
			t.Errorf("ErrorLevel = %v, want %v", cfg.FilterData.ErrorLevel, DefaultErrorLevel)
		}
		if cfg.FilterData.TPercentile != DefaultTPercentile {
			t.Errorf("TPercentile = %v, want %v", cfg.FilterData.TPercentile, DefaultTPercentile)
		}
	})

	t.Run("provided filterData is used", func(t *testing.T) {
		fd := &FilterData{GammaFactor: 2.0, ErrorLevel: 0.1, TPercentile: 2.5}
		env := validEnv()
		cfg := CreateTunerConfigFromData(fd, env)
		if cfg.FilterData.GammaFactor != fd.GammaFactor {
			t.Errorf("GammaFactor = %v, want %v", cfg.FilterData.GammaFactor, fd.GammaFactor)
		}
		if cfg.FilterData.ErrorLevel != fd.ErrorLevel {
			t.Errorf("ErrorLevel = %v, want %v", cfg.FilterData.ErrorLevel, fd.ErrorLevel)
		}
		if cfg.FilterData.TPercentile != fd.TPercentile {
			t.Errorf("TPercentile = %v, want %v", cfg.FilterData.TPercentile, fd.TPercentile)
		}
	})

	t.Run("valid env sets expected observations from env", func(t *testing.T) {
		env := validEnv()
		cfg := CreateTunerConfigFromData(nil, env)
		obs := cfg.ModelData.ExpectedObservations
		if len(obs) != 2 {
			t.Fatalf("ExpectedObservations len = %d, want 2", len(obs))
		}
		if obs[0] != float64(env.AvgTTFT) {
			t.Errorf("obs[0] = %v, want %v", obs[0], float64(env.AvgTTFT))
		}
		if obs[1] != float64(env.AvgITL) {
			t.Errorf("obs[1] = %v, want %v", obs[1], float64(env.AvgITL))
		}
	})

	t.Run("nil env uses default observations", func(t *testing.T) {
		cfg := CreateTunerConfigFromData(nil, nil)
		obs := cfg.ModelData.ExpectedObservations
		if len(obs) != 2 {
			t.Fatalf("ExpectedObservations len = %d, want 2", len(obs))
		}
		if obs[0] != DefaultExpectedTTFT {
			t.Errorf("obs[0] = %v, want %v", obs[0], DefaultExpectedTTFT)
		}
		if obs[1] != DefaultExpectedITL {
			t.Errorf("obs[1] = %v, want %v", obs[1], DefaultExpectedITL)
		}
	})

	t.Run("invalid env uses default observations", func(t *testing.T) {
		env := &Environment{} // all zero — invalid
		cfg := CreateTunerConfigFromData(nil, env)
		obs := cfg.ModelData.ExpectedObservations
		if len(obs) != 2 {
			t.Fatalf("ExpectedObservations len = %d, want 2", len(obs))
		}
		if obs[0] != DefaultExpectedTTFT {
			t.Errorf("obs[0] = %v, want %v", obs[0], DefaultExpectedTTFT)
		}
		if obs[1] != DefaultExpectedITL {
			t.Errorf("obs[1] = %v, want %v", obs[1], DefaultExpectedITL)
		}
	})

	t.Run("state bounds derived from defaults", func(t *testing.T) {
		cfg := CreateTunerConfigFromData(nil, nil)
		md := cfg.ModelData
		if len(md.InitState) != 3 {
			t.Fatalf("InitState len = %d, want 3", len(md.InitState))
		}
		if len(md.MinState) != 3 || len(md.MaxState) != 3 {
			t.Fatalf("min/max state length mismatch")
		}
		for i := range md.InitState {
			wantMin := md.InitState[i] * DefaultMinStateFactor
			wantMax := md.InitState[i] * DefaultMaxStateFactor
			if md.MinState[i] != wantMin {
				t.Errorf("MinState[%d] = %v, want %v", i, md.MinState[i], wantMin)
			}
			if md.MaxState[i] != wantMax {
				t.Errorf("MaxState[%d] = %v, want %v", i, md.MaxState[i], wantMax)
			}
		}
	})

	t.Run("bounded state is true", func(t *testing.T) {
		cfg := CreateTunerConfigFromData(nil, nil)
		if !cfg.ModelData.BoundedState {
			t.Errorf("BoundedState = false, want true")
		}
	})
}

// ---------------------------------------------------------------------------
// NewConfigurator (exercises checkConfigData)
// ---------------------------------------------------------------------------

func validConfigData() *TunerConfigData {
	return &TunerConfigData{
		FilterData: FilterData{
			GammaFactor: 1.0,
			ErrorLevel:  0.05,
			TPercentile: 1.96,
		},
		ModelData: TunerModelData{
			InitState:            []float64{DefaultAlpha, DefaultBeta, DefaultGamma},
			PercentChange:        []float64{0.05, 0.05, 0.05},
			BoundedState:         true,
			MinState:             []float64{DefaultAlpha * DefaultMinStateFactor, DefaultBeta * DefaultMinStateFactor, DefaultGamma * DefaultMinStateFactor},
			MaxState:             []float64{DefaultAlpha * DefaultMaxStateFactor, DefaultBeta * DefaultMaxStateFactor, DefaultGamma * DefaultMaxStateFactor},
			ExpectedObservations: []float64{DefaultExpectedTTFT, DefaultExpectedITL},
		},
	}
}

func TestNewConfigurator(t *testing.T) {
	t.Run("valid config succeeds", func(t *testing.T) {
		c, err := NewConfigurator(validConfigData())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c == nil {
			t.Fatal("got nil configurator")
		}
		if c.NumStates() != 3 {
			t.Errorf("NumStates = %d, want 3", c.NumStates())
		}
		if c.NumObservations() != 2 {
			t.Errorf("NumObservations = %d, want 2", c.NumObservations())
		}
	})

	t.Run("nil config returns error", func(t *testing.T) {
		_, err := NewConfigurator(nil)
		if err == nil {
			t.Fatal("expected error for nil config, got nil")
		}
	})

	t.Run("zero GammaFactor returns error", func(t *testing.T) {
		cd := validConfigData()
		cd.FilterData.GammaFactor = 0
		_, err := NewConfigurator(cd)
		if err == nil {
			t.Fatal("expected error for zero GammaFactor, got nil")
		}
	})

	t.Run("negative ErrorLevel returns error", func(t *testing.T) {
		cd := validConfigData()
		cd.FilterData.ErrorLevel = -0.1
		_, err := NewConfigurator(cd)
		if err == nil {
			t.Fatal("expected error for negative ErrorLevel, got nil")
		}
	})

	t.Run("zero TPercentile returns error", func(t *testing.T) {
		cd := validConfigData()
		cd.FilterData.TPercentile = 0
		_, err := NewConfigurator(cd)
		if err == nil {
			t.Fatal("expected error for zero TPercentile, got nil")
		}
	})

	t.Run("empty InitState returns error", func(t *testing.T) {
		cd := validConfigData()
		cd.ModelData.InitState = []float64{}
		_, err := NewConfigurator(cd)
		if err == nil {
			t.Fatal("expected error for empty InitState, got nil")
		}
	})

	t.Run("NaN in InitState returns error", func(t *testing.T) {
		cd := validConfigData()
		cd.ModelData.InitState = []float64{math.NaN(), 1.0, 1.0}
		_, err := NewConfigurator(cd)
		if err == nil {
			t.Fatal("expected error for NaN in InitState, got nil")
		}
	})

	t.Run("Inf in InitState returns error", func(t *testing.T) {
		cd := validConfigData()
		cd.ModelData.InitState = []float64{math.Inf(1), 1.0, 1.0}
		_, err := NewConfigurator(cd)
		if err == nil {
			t.Fatal("expected error for Inf in InitState, got nil")
		}
	})

	t.Run("PercentChange length mismatch returns error", func(t *testing.T) {
		cd := validConfigData()
		cd.ModelData.PercentChange = []float64{0.05} // wrong length
		_, err := NewConfigurator(cd)
		if err == nil {
			t.Fatal("expected error for PercentChange length mismatch, got nil")
		}
	})

	t.Run("zero PercentChange value returns error", func(t *testing.T) {
		cd := validConfigData()
		cd.ModelData.PercentChange = []float64{0.0, 0.05, 0.05}
		_, err := NewConfigurator(cd)
		if err == nil {
			t.Fatal("expected error for zero PercentChange, got nil")
		}
	})

	t.Run("non-square InitCovarianceMatrix returns error", func(t *testing.T) {
		cd := validConfigData()
		// n=3 states but providing 4 elements (not 3x3=9)
		cd.ModelData.InitCovarianceMatrix = []float64{1, 0, 0, 1}
		_, err := NewConfigurator(cd)
		if err == nil {
			t.Fatal("expected error for wrong-sized InitCovarianceMatrix, got nil")
		}
	})

	t.Run("non-symmetric InitCovarianceMatrix returns error", func(t *testing.T) {
		cd := validConfigData()
		// 3x3 non-symmetric
		cd.ModelData.InitCovarianceMatrix = []float64{
			1, 2, 3,
			4, 5, 6,
			7, 8, 9,
		}
		_, err := NewConfigurator(cd)
		if err == nil {
			t.Fatal("expected error for non-symmetric InitCovarianceMatrix, got nil")
		}
	})

	t.Run("bounded state with wrong MinState length returns error", func(t *testing.T) {
		cd := validConfigData()
		cd.ModelData.MinState = []float64{0.01} // wrong length
		_, err := NewConfigurator(cd)
		if err == nil {
			t.Fatal("expected error for wrong MinState length, got nil")
		}
	})

	t.Run("MinState >= MaxState returns error", func(t *testing.T) {
		cd := validConfigData()
		// Make min >= max for one element
		cd.ModelData.MinState = []float64{
			DefaultAlpha * DefaultMaxStateFactor, // equal to MaxState[0]
			DefaultBeta * DefaultMinStateFactor,
			DefaultGamma * DefaultMinStateFactor,
		}
		_, err := NewConfigurator(cd)
		if err == nil {
			t.Fatal("expected error when MinState >= MaxState, got nil")
		}
	})

	t.Run("empty ExpectedObservations returns error", func(t *testing.T) {
		cd := validConfigData()
		cd.ModelData.ExpectedObservations = []float64{}
		_, err := NewConfigurator(cd)
		if err == nil {
			t.Fatal("expected error for empty ExpectedObservations, got nil")
		}
	})

	t.Run("NaN in ExpectedObservations returns error", func(t *testing.T) {
		cd := validConfigData()
		cd.ModelData.ExpectedObservations = []float64{math.NaN(), 5.0}
		_, err := NewConfigurator(cd)
		if err == nil {
			t.Fatal("expected error for NaN in ExpectedObservations, got nil")
		}
	})

	t.Run("valid symmetric InitCovarianceMatrix succeeds", func(t *testing.T) {
		cd := validConfigData()
		// 3x3 symmetric PD matrix
		cd.ModelData.InitCovarianceMatrix = []float64{
			4, 2, 0,
			2, 3, 0,
			0, 0, 1,
		}
		c, err := NewConfigurator(cd)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c == nil {
			t.Fatal("got nil configurator")
		}
	})
}

// ---------------------------------------------------------------------------
// Configurator.GetStateCov
// ---------------------------------------------------------------------------

func TestConfiguratorGetStateCov(t *testing.T) {
	cd := validConfigData()
	c, err := NewConfigurator(cd)
	if err != nil {
		t.Fatalf("NewConfigurator failed: %v", err)
	}

	t.Run("valid state vector returns diagonal covariance", func(t *testing.T) {
		x := mat.NewVecDense(3, []float64{DefaultAlpha, DefaultBeta, DefaultGamma})
		cov, err := c.GetStateCov(x)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cov == nil {
			t.Fatal("got nil covariance")
		}
		r, col := cov.Dims()
		if r != 3 || col != 3 {
			t.Errorf("covariance dims = (%d, %d), want (3, 3)", r, col)
		}
		// Diagonal entries should be (percentChange * stateVal)^2
		for i := 0; i < 3; i++ {
			v := cd.ModelData.PercentChange[i] * cd.ModelData.InitState[i]
			expectedDiag := v * v
			got := cov.At(i, i)
			if math.Abs(got-expectedDiag) > DefaultEpsilon {
				t.Errorf("cov[%d][%d] = %v, want %v", i, i, got, expectedDiag)
			}
		}
	})

	t.Run("wrong length state vector returns error", func(t *testing.T) {
		x := mat.NewVecDense(2, []float64{1.0, 2.0}) // wrong length
		_, err := c.GetStateCov(x)
		if err == nil {
			t.Fatal("expected error for wrong-length state vector, got nil")
		}
	})
}

// ---------------------------------------------------------------------------
// NewTuner
// ---------------------------------------------------------------------------

func TestNewTuner(t *testing.T) {
	t.Run("valid inputs succeed", func(t *testing.T) {
		env := validEnv()
		cd := CreateTunerConfigFromData(nil, env)
		tuner, err := NewTuner(cd, env)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tuner == nil {
			t.Fatal("got nil tuner")
		}
	})

	t.Run("nil environment returns error", func(t *testing.T) {
		cd := validConfigData()
		_, err := NewTuner(cd, nil)
		if err == nil {
			t.Fatal("expected error for nil environment, got nil")
		}
	})

	t.Run("invalid environment returns error", func(t *testing.T) {
		cd := validConfigData()
		env := &Environment{} // all zero fields — invalid
		_, err := NewTuner(cd, env)
		if err == nil {
			t.Fatal("expected error for invalid environment, got nil")
		}
	})

	t.Run("nil config returns error", func(t *testing.T) {
		env := validEnv()
		_, err := NewTuner(nil, env)
		if err == nil {
			t.Fatal("expected error for nil config, got nil")
		}
	})
}

// ---------------------------------------------------------------------------
// Tuner.UpdateEnvironment
// ---------------------------------------------------------------------------

func TestTunerUpdateEnvironment(t *testing.T) {
	env := validEnv()
	cd := CreateTunerConfigFromData(nil, env)
	tuner, err := NewTuner(cd, env)
	if err != nil {
		t.Fatalf("NewTuner failed: %v", err)
	}

	t.Run("nil environment returns error", func(t *testing.T) {
		err := tuner.UpdateEnvironment(nil)
		if err == nil {
			t.Fatal("expected error for nil environment, got nil")
		}
	})

	t.Run("invalid environment returns error", func(t *testing.T) {
		err := tuner.UpdateEnvironment(&Environment{})
		if err == nil {
			t.Fatal("expected error for invalid environment, got nil")
		}
	})

	t.Run("valid environment succeeds", func(t *testing.T) {
		newEnv := validEnv()
		newEnv.Lambda = 20.0
		err := tuner.UpdateEnvironment(newEnv)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tuner.GetEnvironment().Lambda != 20.0 {
			t.Errorf("Lambda = %v, want 20.0", tuner.GetEnvironment().Lambda)
		}
	})
}
