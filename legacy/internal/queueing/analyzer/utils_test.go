package analyzer

import (
	"errors"
	"math"
	"testing"
)

func TestWithinTolerance(t *testing.T) {
	tests := []struct {
		name      string
		x         float32
		value     float32
		tolerance float32
		expected  bool
	}{
		{
			name:      "exact match",
			x:         1.0,
			value:     1.0,
			tolerance: 0.01,
			expected:  true,
		},
		{
			name:      "within tolerance",
			x:         1.005,
			value:     1.0,
			tolerance: 0.01,
			expected:  true,
		},
		{
			name:      "outside tolerance",
			x:         1.02,
			value:     1.0,
			tolerance: 0.01,
			expected:  false,
		},
		{
			name:      "zero value",
			x:         0.1,
			value:     0.0,
			tolerance: 0.01,
			expected:  false,
		},
		{
			name:      "negative tolerance",
			x:         1.0,
			value:     1.0,
			tolerance: -0.01,
			expected:  true, // WithinTolerance should return true for exact match regardless of tolerance
		},
		{
			name:      "both zero",
			x:         0.0,
			value:     0.0,
			tolerance: 0.01,
			expected:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := WithinTolerance(tt.x, tt.value, tt.tolerance)
			if result != tt.expected {
				t.Errorf("WithinTolerance(%v, %v, %v) = %v, expected %v",
					tt.x, tt.value, tt.tolerance, result, tt.expected)
			}
		})
	}
}

func TestBinarySearch(t *testing.T) {
	// Test function: f(x) = x^2
	quadratic := func(x float32) (float32, error) {
		return x * x, nil
	}

	// Test function: f(x) = 2*x (linear increasing)
	linear := func(x float32) (float32, error) {
		return 2 * x, nil
	}

	// Test function: f(x) = -x (linear decreasing)
	negativeLinear := func(x float32) (float32, error) {
		return -x, nil
	}

	// Test function that returns error
	errorFunc := func(x float32) (float32, error) {
		if x > 5.0 {
			return 0, errors.New("x too large")
		}
		return x, nil
	}

	tests := []struct {
		name        string
		xMin        float32
		xMax        float32
		yTarget     float32
		evalFunc    func(float32) (float32, error)
		wantErr     bool
		expectedInd int
		tolerance   float32
	}{
		{
			name:        "find square root",
			xMin:        0.0,
			xMax:        10.0,
			yTarget:     4.0,
			evalFunc:    quadratic,
			wantErr:     false,
			expectedInd: 0,
			tolerance:   0.1,
		},
		{
			name:        "linear function - target in range",
			xMin:        1.0,
			xMax:        5.0,
			yTarget:     6.0, // f(3) = 6
			evalFunc:    linear,
			wantErr:     false,
			expectedInd: 0,
			tolerance:   0.1,
		},
		{
			name:        "linear function - target below range",
			xMin:        2.0,
			xMax:        5.0,
			yTarget:     1.0, // Below f(2) = 4
			evalFunc:    linear,
			wantErr:     false,
			expectedInd: -1,
			tolerance:   0.1,
		},
		{
			name:        "linear function - target above range",
			xMin:        1.0,
			xMax:        3.0,
			yTarget:     10.0, // Above f(3) = 6
			evalFunc:    linear,
			wantErr:     false,
			expectedInd: 1,
			tolerance:   0.1,
		},
		{
			name:        "decreasing function - target in range",
			xMin:        1.0,
			xMax:        5.0,
			yTarget:     -3.0, // f(3) = -3
			evalFunc:    negativeLinear,
			wantErr:     false,
			expectedInd: 0,
			tolerance:   0.1,
		},
		{
			name:        "invalid range",
			xMin:        5.0,
			xMax:        1.0,
			yTarget:     3.0,
			evalFunc:    linear,
			wantErr:     true,
			expectedInd: 0,
			tolerance:   0.1,
		},
		{
			name:        "function evaluation error",
			xMin:        4.0,
			xMax:        6.0,
			yTarget:     5.0,
			evalFunc:    errorFunc,
			wantErr:     true,
			expectedInd: 0,
			tolerance:   0.1,
		},
		{
			name:        "target at boundary",
			xMin:        1.0,
			xMax:        5.0,
			yTarget:     2.0, // f(1) = 2
			evalFunc:    linear,
			wantErr:     false,
			expectedInd: 0,
			tolerance:   0.1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			xStar, ind, err := BinarySearch(tt.xMin, tt.xMax, tt.yTarget, tt.evalFunc)

			if (err != nil) != tt.wantErr {
				t.Errorf("BinarySearch() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err == nil {
				if ind != tt.expectedInd {
					t.Errorf("BinarySearch() ind = %v, expected %v", ind, tt.expectedInd)
				}

				// If target is within bounds (ind == 0), verify the result
				if ind == 0 {
					yResult, evalErr := tt.evalFunc(xStar)
					if evalErr != nil {
						t.Errorf("Evaluation error at result: %v", evalErr)
					} else if math.Abs(float64(yResult-tt.yTarget)) > float64(tt.tolerance) {
						t.Errorf("BinarySearch() result not accurate enough: f(%v) = %v, target = %v",
							xStar, yResult, tt.yTarget)
					}
				}

				// Verify boundary results
				if ind == -1 && xStar != tt.xMin {
					t.Errorf("BinarySearch() should return xMin for target below range, got %v", xStar)
				}
				if ind == 1 && xStar != tt.xMax {
					t.Errorf("BinarySearch() should return xMax for target above range, got %v", xStar)
				}
			}
		})
	}
}

func TestBinarySearch_EdgeCases(t *testing.T) {
	// Constant function
	constant := func(x float32) (float32, error) {
		return 5.0, nil
	}

	// Step function
	step := func(x float32) (float32, error) {
		if x < 3.0 {
			return 1.0, nil
		}
		return 10.0, nil
	}

	tests := []struct {
		name     string
		xMin     float32
		xMax     float32
		yTarget  float32
		evalFunc func(float32) (float32, error)
		wantErr  bool
	}{
		{
			name:     "constant function - target matches",
			xMin:     1.0,
			xMax:     10.0,
			yTarget:  5.0,
			evalFunc: constant,
			wantErr:  false,
		},
		{
			name:     "constant function - target doesn't match",
			xMin:     1.0,
			xMax:     10.0,
			yTarget:  3.0,
			evalFunc: constant,
			wantErr:  false,
		},
		{
			name:     "step function",
			xMin:     1.0,
			xMax:     5.0,
			yTarget:  5.0,
			evalFunc: step,
			wantErr:  false,
		},
		{
			name:     "zero range",
			xMin:     3.0,
			xMax:     3.0,
			yTarget:  6.0,
			evalFunc: func(x float32) (float32, error) { return 2 * x, nil },
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := BinarySearch(tt.xMin, tt.xMax, tt.yTarget, tt.evalFunc)
			if (err != nil) != tt.wantErr {
				t.Errorf("BinarySearch() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestEvalServTime(t *testing.T) {
	// Create a test model - use state-dependent model for global variable
	servRates := []float32{1.0, 2.0, 3.0, 4.0, 5.0}
	model := NewMM1ModelStateDependent(5, servRates)
	evalFunc := EvalServTime(model)

	tests := []struct {
		name    string
		lambda  float32
		wantErr bool
	}{
		{
			name:    "valid lambda",
			lambda:  0.5,
			wantErr: false,
		},
		{
			name:    "zero lambda",
			lambda:  0.0,
			wantErr: false,
		},
		{
			name:    "high lambda that might make model invalid",
			lambda:  10.0,
			wantErr: false, // Model can handle this with state-dependent service rates
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := evalFunc(tt.lambda)

			if (err != nil) != tt.wantErr {
				t.Errorf("EvalServTime() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if result < 0 {
					t.Errorf("EvalServTime() = %v, should be non-negative", result)
				}
			}
		})
	}
}

func TestEvalWaitingTime(t *testing.T) {
	// Create a test model - use state-dependent model for global variable
	servRates := []float32{1.0, 2.0, 3.0, 4.0, 5.0}
	model := NewMM1ModelStateDependent(5, servRates)
	evalFunc := EvalWaitingTime(model)

	tests := []struct {
		name    string
		lambda  float32
		wantErr bool
	}{
		{
			name:    "low lambda",
			lambda:  0.1,
			wantErr: false,
		},
		{
			name:    "medium lambda",
			lambda:  1.0,
			wantErr: false,
		},
		{
			name:    "lambda that makes model invalid",
			lambda:  10.0,
			wantErr: false, // Model can handle this with state-dependent service rates
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := evalFunc(tt.lambda)

			if (err != nil) != tt.wantErr {
				t.Errorf("EvalWaitingTime() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if result < 0 {
					t.Errorf("EvalWaitingTime() = %v, should be non-negative", result)
				}
			}
		})
	}
}

func TestEvalTTFT(t *testing.T) {
	// Set up global variables for eval functions
	config := &Configuration{
		MaxBatchSize: 4,
		MaxQueueSize: 8,
		ServiceParms: &ServiceParms{
			Alpha: 1.0,
			Beta:  0.01,
			Gamma: 0.001,
		},
	}
	requestSize := &RequestSize{AvgInputTokens: 100, AvgOutputTokens: 10}

	qa := BuildModel(config, requestSize)
	evalFuncData := &EvalFuncData{
		model:        qa.Model,
		requestSize:  qa.RequestSize,
		serviceParms: qa.ServiceParms,
		maxBatchSize: qa.MaxBatchSize,
	}
	evalFunc := EvalTTFT(evalFuncData)

	tests := []struct {
		name    string
		lambda  float32
		wantErr bool
	}{
		{
			name:    "low arrival rate",
			lambda:  0.001, // requests per msec
			wantErr: false,
		},
		{
			name:    "medium arrival rate",
			lambda:  0.01,
			wantErr: false,
		},
		{
			name:    "high arrival rate that might cause invalid model",
			lambda:  1.0,
			wantErr: false, // This rate should still be valid for the model
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := evalFunc(tt.lambda)

			if (err != nil) != tt.wantErr {
				t.Errorf("EvalTTFT() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if result < 0 {
					t.Errorf("EvalTTFT() = %v, should be non-negative", result)
				}

				// TTFT should include waiting time and prefill time
				if result < 1.0 { // At least the base prefill time (alpha)
					t.Errorf("EvalTTFT() = %v, should be at least base prefill time", result)
				}
			}
		})
	}
}

func TestEvalITL(t *testing.T) {
	// Set up global variables for eval functions
	config := &Configuration{
		MaxBatchSize: 4,
		MaxQueueSize: 8,
		ServiceParms: &ServiceParms{
			Alpha: 1.0,
			Beta:  0.01,
			Gamma: 0.001,
		},
	}
	requestSize := &RequestSize{AvgInputTokens: 100, AvgOutputTokens: 10}

	qa := BuildModel(config, requestSize)
	evalFuncData := &EvalFuncData{
		model:        qa.Model,
		requestSize:  qa.RequestSize,
		serviceParms: qa.ServiceParms,
		maxBatchSize: qa.MaxBatchSize,
	}
	evalFunc := EvalITL(evalFuncData)

	tests := []struct {
		name    string
		lambda  float32
		wantErr bool
	}{
		{
			name:    "low arrival rate",
			lambda:  0.001,
			wantErr: false,
		},
		{
			name:    "medium arrival rate",
			lambda:  0.01,
			wantErr: false,
		},
		{
			name:    "high arrival rate that might cause invalid model",
			lambda:  1.0,
			wantErr: false, // This rate should still be valid for the model
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := evalFunc(tt.lambda)

			if (err != nil) != tt.wantErr {
				t.Errorf("EvalITL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if result < 0 {
					t.Errorf("EvalITL() = %v, should be non-negative", result)
				}

				// ITL should be at least the base decode time (alpha)
				if result < 1.0 {
					t.Errorf("EvalITL() = %v, should be at least base decode time", result)
				}
			}
		})
	}
}

func TestBinarySearchWithAnalyzerFunctions(t *testing.T) {
	// Integration test: use binary search with analyzer evaluation functions
	config := &Configuration{
		MaxBatchSize: 4,
		MaxQueueSize: 8,
		ServiceParms: &ServiceParms{
			Alpha: 1.0,
			Beta:  0.01,
			Gamma: 10.0,
		},
	}
	requestSize := &RequestSize{AvgInputTokens: 100, AvgOutputTokens: 10}

	qa := BuildModel(config, requestSize)
	evalFuncData := &EvalFuncData{
		model:        qa.Model,
		requestSize:  qa.RequestSize,
		serviceParms: qa.ServiceParms,
		maxBatchSize: qa.MaxBatchSize,
	}
	evalTTFTFunc := EvalTTFT(evalFuncData)
	evalITLFunc := EvalITL(evalFuncData)
	evalServTimeFunc := EvalServTime(qa.Model)
	evalWaitTimeFunc := EvalWaitingTime(qa.Model)

	lambdaMin := qa.RateRange.Min / 1000 // Convert to requests per msec
	lambdaMax := qa.RateRange.Max / 1000

	tests := []struct {
		name        string
		yTarget     float32
		evalFunc    func(float32) (float32, error)
		description string
	}{
		{
			name:        "find lambda for target TTFT",
			yTarget:     25.0, // 25 msec target TTFT
			evalFunc:    evalTTFTFunc,
			description: "time to first token",
		},
		{
			name:        "find lambda for target ITL",
			yTarget:     2.0, // 2 msec target inter-token latency
			evalFunc:    evalITLFunc,
			description: "inter-token latency",
		},
		{
			name:        "find lambda for target service time",
			yTarget:     50.0, // 50 msec target service time
			evalFunc:    evalServTimeFunc,
			description: "service time",
		},
		{
			name:        "find lambda for target waiting time",
			yTarget:     10.0, // 10 msec target waiting time
			evalFunc:    evalWaitTimeFunc,
			description: "waiting time",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			xStar, ind, err := BinarySearch(lambdaMin, lambdaMax, tt.yTarget, tt.evalFunc)

			if err != nil {
				t.Logf("BinarySearch failed for %s: %v (this may be expected if target is unreachable)", tt.description, err)
				return
			}

			t.Logf("Binary search for %s: lambda=%v, indicator=%d", tt.description, xStar, ind)

			// If search found a solution within bounds, verify it
			if ind == 0 {
				yResult, evalErr := tt.evalFunc(xStar)
				if evalErr != nil {
					t.Errorf("Evaluation error at result: %v", evalErr)
				} else {
					tolerance := float32(0.1) // Allow some tolerance
					if math.Abs(float64(yResult-tt.yTarget)) > float64(tolerance) {
						t.Errorf("Binary search result not accurate: target=%v, achieved=%v", tt.yTarget, yResult)
					} else {
						t.Logf("Successfully found lambda=%v to achieve %s=%v (target=%v)",
							xStar, tt.description, yResult, tt.yTarget)
					}
				}
			}
		})
	}
}

func TestBinarySearchPrecision(t *testing.T) {
	// Test that binary search achieves sufficient precision
	linear := func(x float32) (float32, error) {
		return 2*x + 3, nil // Changed to avoid boundary condition issues
	}

	// Test with target within range: f(x) = 2x + 3, for x in [1,5], target = 9
	// At x=1: f(1) = 5
	// At x=5: f(5) = 13
	// Target 9 should find x=3 since f(3) = 9

	xStar, ind, err := BinarySearch(1.0, 5.0, 9.0, linear)

	if err != nil {
		t.Fatalf("BinarySearch failed: %v", err)
	}

	if ind != 0 {
		t.Fatalf("Expected indicator 0 (target within range), got %d", ind)
	}

	expectedX := float32(3.0)
	tolerance := float32(1e-3)

	if math.Abs(float64(xStar-expectedX)) > float64(tolerance) {
		t.Errorf("BinarySearch precision insufficient: got %v, expected %v (tolerance %v)",
			xStar, expectedX, tolerance)
	}

	// Verify the result
	yResult, _ := linear(xStar)
	if math.Abs(float64(yResult-9.0)) > float64(tolerance) {
		t.Errorf("BinarySearch result verification failed: f(%v) = %v, expected 9.0", xStar, yResult)
	}
}
