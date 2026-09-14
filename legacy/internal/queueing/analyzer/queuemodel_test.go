package analyzer

import (
	"math"
	"strings"
	"testing"
)

func TestQueueModel_Basic(t *testing.T) {
	// Create an MM1KModel to test the base QueueModel functionality
	model := NewMM1KModel(10)

	tests := []struct {
		name      string
		lambda    float32
		mu        float32
		wantValid bool
	}{
		{
			name:      "valid parameters",
			lambda:    1.0,
			mu:        2.0,
			wantValid: true,
		},
		{
			name:      "zero arrival rate",
			lambda:    0.0,
			mu:        2.0,
			wantValid: true,
		},
		{
			name:      "negative arrival rate",
			lambda:    -1.0,
			mu:        2.0,
			wantValid: false,
		},
		{
			name:      "zero service rate",
			lambda:    1.0,
			mu:        0.0,
			wantValid: false,
		},
		{
			name:      "negative service rate",
			lambda:    1.0,
			mu:        -1.0,
			wantValid: false,
		},
		{
			name:      "utilization at limit",
			lambda:    9.9,
			mu:        1.0,
			wantValid: true,
		},
		{
			name:      "utilization over limit",
			lambda:    11.0,
			mu:        1.0,
			wantValid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model.Solve(tt.lambda, tt.mu)

			if model.IsValid() != tt.wantValid {
				t.Errorf("IsValid() = %v, want %v", model.IsValid(), tt.wantValid)
			}

			// Check that getters return expected values
			if model.GetLambda() != tt.lambda {
				t.Errorf("GetLambda() = %v, want %v", model.GetLambda(), tt.lambda)
			}
			if model.GetMu() != tt.mu {
				t.Errorf("GetMu() = %v, want %v", model.GetMu(), tt.mu)
			}

			// For valid models, check that statistics are computed
			if tt.wantValid {
				if tt.lambda > 0 && model.GetRho() <= 0 {
					t.Error("GetRho() should be positive for valid model with positive arrival rate")
				}
				if model.GetAvgRespTime() <= 0 {
					t.Error("GetAvgRespTime() should be positive for valid model")
				}
				if model.GetAvgNumInSystem() < 0 {
					t.Error("GetAvgNumInSystem() should be non-negative")
				}
				if model.GetAvgQueueLength() < 0 {
					t.Error("GetAvgQueueLength() should be non-negative")
				}
				if model.GetAvgWaitTime() < 0 {
					t.Error("GetAvgWaitTime() should be non-negative")
				}
				if model.GetAvgServTime() <= 0 {
					t.Error("GetAvgServTime() should be positive")
				}
			}
		})
	}
}

func TestQueueModel_String(t *testing.T) {
	model := NewMM1KModel(5)
	model.Solve(1.0, 2.0)

	result := model.String()

	// Check that string contains key information
	if !strings.Contains(result, "lambda=1") {
		t.Error("String() should contain lambda value")
	}
	if !strings.Contains(result, "mu=2") {
		t.Error("String() should contain mu value")
	}
	if !strings.Contains(result, "isValid=true") {
		t.Error("String() should contain validity status")
	}
}

func TestMM1KModel_Creation(t *testing.T) {
	tests := []struct {
		name string
		K    int
	}{
		{"small capacity", 5},
		{"medium capacity", 50},
		{"large capacity", 500},
		{"single capacity", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewMM1KModel(tt.K)

			if model.K != tt.K {
				t.Errorf("NewMM1KModel(%d).K = %d, want %d", tt.K, model.K, tt.K)
			}

			if len(model.p) != tt.K+1 {
				t.Errorf("NewMM1KModel(%d) probabilities length = %d, want %d", tt.K, len(model.p), tt.K+1)
			}

			if model.GetRhoMax() != float32(tt.K) {
				t.Errorf("GetRhoMax() = %v, want %v", model.GetRhoMax(), float32(tt.K))
			}
		})
	}
}

func TestMM1KModel_ProbabilityCalculation(t *testing.T) {
	model := NewMM1KModel(3)

	tests := []struct {
		name         string
		lambda       float32
		mu           float32
		checkProbSum bool
	}{
		{
			name:         "low utilization",
			lambda:       0.5,
			mu:           2.0,
			checkProbSum: true,
		},
		{
			name:         "medium utilization",
			lambda:       1.5,
			mu:           2.0,
			checkProbSum: true,
		},
		{
			name:         "high utilization",
			lambda:       1.9,
			mu:           2.0,
			checkProbSum: true,
		},
		{
			name:         "equal arrival and service rates",
			lambda:       2.0,
			mu:           2.0,
			checkProbSum: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model.Solve(tt.lambda, tt.mu)

			if !model.IsValid() {
				t.Skip("Skipping probability test for invalid model")
			}

			probabilities := model.GetProbabilities()

			// Check that probabilities are non-negative
			for i, p := range probabilities {
				if p < 0 {
					t.Errorf("Probability p[%d] = %v should be non-negative", i, p)
				}
			}

			// Check that probabilities sum to approximately 1
			if tt.checkProbSum {
				sum := 0.0
				for _, p := range probabilities {
					sum += p
				}
				if math.Abs(sum-1.0) > 1e-6 {
					t.Errorf("Probabilities sum = %v, want ~1.0", sum)
				}
			}

			// Check that throughput is reasonable
			throughput := model.GetThroughput()
			if throughput < 0 || throughput > tt.lambda {
				t.Errorf("Throughput = %v should be in [0, %v]", throughput, tt.lambda)
			}
		})
	}
}

func TestMM1KModel_EdgeCases(t *testing.T) {
	tests := []struct {
		name   string
		K      int
		lambda float32
		mu     float32
		desc   string
	}{
		{
			name:   "single server single slot",
			K:      1,
			lambda: 0.5,
			mu:     1.0,
			desc:   "minimal system",
		},
		{
			name:   "very low arrival rate",
			K:      10,
			lambda: 0.001,
			mu:     1.0,
			desc:   "near zero arrivals",
		},
		{
			name:   "very high service rate",
			K:      10,
			lambda: 1.0,
			mu:     1000.0,
			desc:   "near instantaneous service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewMM1KModel(tt.K)
			model.Solve(tt.lambda, tt.mu)

			if !model.IsValid() {
				t.Errorf("%s: model should be valid", tt.desc)
			}

			// Basic sanity checks
			if model.GetAvgNumInSystem() < 0 {
				t.Errorf("%s: average number in system should be non-negative", tt.desc)
			}

			if model.GetThroughput() < 0 {
				t.Errorf("%s: throughput should be non-negative", tt.desc)
			}
		})
	}
}

func TestMM1ModelStateDependent_Creation(t *testing.T) {
	tests := []struct {
		name     string
		K        int
		servRate []float32
	}{
		{
			name:     "constant service rate",
			K:        5,
			servRate: []float32{2.0, 2.0, 2.0, 2.0, 2.0},
		},
		{
			name:     "increasing service rate",
			K:        4,
			servRate: []float32{1.0, 2.0, 3.0, 4.0},
		},
		{
			name:     "decreasing service rate",
			K:        3,
			servRate: []float32{4.0, 3.0, 2.0},
		},
		{
			name:     "single state",
			K:        2,
			servRate: []float32{1.5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewMM1ModelStateDependent(tt.K, tt.servRate)

			if model.K != tt.K {
				t.Errorf("NewMM1ModelStateDependent K = %d, want %d", model.K, tt.K)
			}

			if len(model.servRate) != len(tt.servRate) {
				t.Errorf("Service rate array length = %d, want %d", len(model.servRate), len(tt.servRate))
			}

			for i, rate := range tt.servRate {
				if model.servRate[i] != rate {
					t.Errorf("Service rate[%d] = %v, want %v", i, model.servRate[i], rate)
				}
			}
		})
	}
}

func TestMM1ModelStateDependent_Solve(t *testing.T) {
	servRate := []float32{1.0, 2.0, 3.0}
	model := NewMM1ModelStateDependent(5, servRate)

	tests := []struct {
		name      string
		lambda    float32
		mu        float32 // Not used in state-dependent model
		wantValid bool
	}{
		{
			name:      "low arrival rate",
			lambda:    0.5,
			mu:        1.0,
			wantValid: true,
		},
		{
			name:      "medium arrival rate",
			lambda:    1.5,
			mu:        1.0,
			wantValid: true,
		},
		{
			name:      "high arrival rate",
			lambda:    2.8,
			mu:        1.0,
			wantValid: true,
		},
		{
			name:      "zero arrival rate",
			lambda:    0.0,
			mu:        1.0,
			wantValid: true,
		},
		{
			name:      "negative arrival rate",
			lambda:    -1.0,
			mu:        1.0,
			wantValid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model.Solve(tt.lambda, tt.mu)

			if model.IsValid() != tt.wantValid {
				t.Errorf("IsValid() = %v, want %v", model.IsValid(), tt.wantValid)
			}

			if tt.wantValid {
				// Check state-dependent specific metrics
				avgNumInServers := model.GetAvgNumInServers()
				if avgNumInServers < 0 {
					t.Error("GetAvgNumInServers() should be non-negative")
				}

				// Check that utilization is properly computed
				rho := model.GetRho()
				if rho < 0 || rho > 1 {
					t.Errorf("Utilization rho = %v should be in [0, 1]", rho)
				}

				// Check consistency between metrics
				if model.GetAvgRespTime() > 0 && model.GetThroughput() > 0 {
					expectedAvgNumInSystem := model.GetThroughput() * model.GetAvgRespTime()
					actualAvgNumInSystem := model.GetAvgNumInSystem()
					diff := math.Abs(float64(expectedAvgNumInSystem - actualAvgNumInSystem))
					if diff > 1e-4 {
						t.Errorf("Little's Law check failed: expected %v, got %v", expectedAvgNumInSystem, actualAvgNumInSystem)
					}
				}
			}
		})
	}
}

func TestMM1ModelStateDependent_UtilizationCalculation(t *testing.T) {
	servRate := []float32{2.0, 4.0, 6.0}
	model := NewMM1ModelStateDependent(4, servRate)

	// Test that utilization is calculated differently than standard MM1K
	model.Solve(1.0, 1.0) // mu is not used in state-dependent model

	if !model.IsValid() {
		t.Skip("Model invalid, skipping utilization test")
	}

	rho := model.ComputeRho()

	// For state-dependent model, rho = 1 - p[0]
	probabilities := model.GetProbabilities()
	expectedRho := 1.0 - float32(probabilities[0])

	if math.Abs(float64(rho-expectedRho)) > 1e-6 {
		t.Errorf("ComputeRho() = %v, expected %v", rho, expectedRho)
	}
}

func TestMM1ModelStateDependent_ServiceRateExtension(t *testing.T) {
	// Test when system has more states than defined service rates
	servRate := []float32{1.0, 2.0}                 // Only 2 rates defined
	model := NewMM1ModelStateDependent(5, servRate) // But K=5

	model.Solve(0.5, 1.0)

	if !model.IsValid() {
		t.Skip("Model invalid, skipping service rate extension test")
	}
	if model.GetAvgNumInSystem() < 0 {
		t.Error("Model with extended service rates should produce valid results")
	}

	if model.GetThroughput() < 0 {
		t.Error("Throughput should be non-negative with extended service rates")
	}
}

func TestMM1ModelStateDependent_String(t *testing.T) {
	servRate := []float32{1.0, 2.0, 3.0}
	model := NewMM1ModelStateDependent(3, servRate)
	model.Solve(1.0, 1.0)

	result := model.String()

	// Check that string contains key identifiers
	if !strings.Contains(result, "MM1ModelStateDependent") {
		t.Error("String() should identify the model type")
	}

	// Should contain base model information
	if !strings.Contains(result, "MM1KModel") {
		t.Error("String() should contain base model information")
	}
}

func TestMM1Models_Comparison(t *testing.T) {
	// Compare MM1K with constant service rate vs MM1ModelStateDependent with same rate
	K := 5
	constantRate := float32(3.0)
	lambda := float32(1.5)
	mu := constantRate

	// Create MM1K model
	mm1k := NewMM1KModel(K)
	mm1k.Solve(lambda, mu)

	// Create state-dependent model with constant rates
	servRates := make([]float32, K)
	for i := range servRates {
		servRates[i] = constantRate
	}
	stateDependent := NewMM1ModelStateDependent(K, servRates)
	stateDependent.Solve(lambda, 1.0) // mu not used in state-dependent

	if !mm1k.IsValid() || !stateDependent.IsValid() {
		t.Skip("One or both models invalid, skipping comparison")
	}

	// Results should be similar (allowing for numerical differences)
	tolerance := float32(1e-3)

	if math.Abs(float64(mm1k.GetAvgNumInSystem()-stateDependent.GetAvgNumInSystem())) > float64(tolerance) {
		t.Errorf("Average number in system differs: MM1K=%v, StateDependent=%v",
			mm1k.GetAvgNumInSystem(), stateDependent.GetAvgNumInSystem())
	}

	if math.Abs(float64(mm1k.GetThroughput()-stateDependent.GetThroughput())) > float64(tolerance) {
		t.Errorf("Throughput differs: MM1K=%v, StateDependent=%v",
			mm1k.GetThroughput(), stateDependent.GetThroughput())
	}
}

func TestQueueModel_LittlesLaw(t *testing.T) {
	// Test Little's Law: L = λW (Average number in system = arrival rate × average response time)
	tests := []struct {
		name   string
		lambda float32
		mu     float32
	}{
		{"low load", 0.5, 2.0},
		{"medium load", 1.5, 3.0},
		{"high load", 2.8, 4.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewMM1KModel(10)
			model.Solve(tt.lambda, tt.mu)

			if !model.IsValid() {
				t.Skip("Invalid model, skipping Little's Law test")
			}

			// Check Little's Law: L = λW
			effectiveLambda := model.GetThroughput() // Use effective arrival rate (throughput)
			avgNumInSystem := model.GetAvgNumInSystem()
			avgRespTime := model.GetAvgRespTime()

			expectedL := effectiveLambda * avgRespTime
			tolerance := float64(1e-4)

			if math.Abs(float64(avgNumInSystem-expectedL)) > tolerance {
				t.Errorf("Little's Law violation: L=%v, λW=%v (λ=%v, W=%v)",
					avgNumInSystem, expectedL, effectiveLambda, avgRespTime)
			}
		})
	}
}
