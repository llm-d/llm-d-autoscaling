package analyzer_test

import (
	"math"
	"strings"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/queueing/analyzer"
)

var testConfig = &analyzer.Configuration{
	MaxBatchSize: 8,
	MaxQueueSize: 16,
	ServiceParms: &analyzer.ServiceParms{
		Alpha: 1.0,
		Beta:  0.01,
		Gamma: 0.001,
	},
}

func TestNewQueueAnalyzer(t *testing.T) {
	tests := []struct {
		name        string // description of this test case
		qConfig     *analyzer.Configuration
		requestSize *analyzer.RequestSize
		wantErr     bool
	}{
		{
			name:        "no prefill",
			qConfig:     testConfig,
			requestSize: &analyzer.RequestSize{AvgInputTokens: 0, AvgOutputTokens: 10},
			wantErr:     false,
		},
		{
			name:        "no prefill, one output token",
			qConfig:     testConfig,
			requestSize: &analyzer.RequestSize{AvgInputTokens: 0, AvgOutputTokens: 1},
			wantErr:     false,
		},
		{
			name:        "no decode",
			qConfig:     testConfig,
			requestSize: &analyzer.RequestSize{AvgInputTokens: 100, AvgOutputTokens: 1},
			wantErr:     false,
		},
		{
			name:        "mixed prefill and decode",
			qConfig:     testConfig,
			requestSize: &analyzer.RequestSize{AvgInputTokens: 200, AvgOutputTokens: 20},
			wantErr:     false,
		},
		{
			name:        "zero input and output tokens",
			qConfig:     testConfig,
			requestSize: &analyzer.RequestSize{AvgInputTokens: 0, AvgOutputTokens: 0},
			wantErr:     true,
		},
		{
			name:        "negative tokens",
			qConfig:     testConfig,
			requestSize: &analyzer.RequestSize{AvgInputTokens: -1, AvgOutputTokens: -1},
			wantErr:     true,
		},
		{
			name:        "no decode, no first output token",
			qConfig:     testConfig,
			requestSize: &analyzer.RequestSize{AvgInputTokens: 50, AvgOutputTokens: 0},
			wantErr:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, gotErr := analyzer.NewQueueAnalyzer(tt.qConfig, tt.requestSize)
			if gotErr != nil {
				if !tt.wantErr {
					t.Errorf("NewQueueAnalyzer() failed: %v", gotErr)
				}
				return
			}
			if tt.wantErr {
				t.Fatal("NewQueueAnalyzer() succeeded unexpectedly")
			}
		})
	}
}

func TestConfiguration_Check(t *testing.T) {
	tests := []struct {
		name    string
		config  *analyzer.Configuration
		wantErr bool
	}{
		{
			name:    "valid configuration",
			config:  testConfig,
			wantErr: false,
		},
		{
			name: "zero max batch size",
			config: &analyzer.Configuration{
				MaxBatchSize: 0,
				MaxQueueSize: 16,
				ServiceParms: testConfig.ServiceParms,
			},
			wantErr: true,
		},
		{
			name: "negative max batch size",
			config: &analyzer.Configuration{
				MaxBatchSize: -1,
				MaxQueueSize: 16,
				ServiceParms: testConfig.ServiceParms,
			},
			wantErr: true,
		},
		{
			name: "negative max queue size",
			config: &analyzer.Configuration{
				MaxBatchSize: 8,
				MaxQueueSize: -1,
				ServiceParms: testConfig.ServiceParms,
			},
			wantErr: true,
		},
		{
			name: "nil service parameters",
			config: &analyzer.Configuration{
				MaxBatchSize: 8,
				MaxQueueSize: 16,
				ServiceParms: nil,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			qa, err := analyzer.NewQueueAnalyzer(tt.config, &analyzer.RequestSize{AvgInputTokens: 100, AvgOutputTokens: 10})
			if (err != nil) != tt.wantErr {
				t.Errorf("Configuration check error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && qa == nil {
				t.Error("Expected valid queue analyzer but got nil")
			}
		})
	}
}

func TestRequestSize_Check(t *testing.T) {
	tests := []struct {
		name        string
		requestSize *analyzer.RequestSize
		wantErr     bool
	}{
		{
			name:        "valid request size",
			requestSize: &analyzer.RequestSize{AvgInputTokens: 100, AvgOutputTokens: 10},
			wantErr:     false,
		},
		{
			name:        "zero input tokens",
			requestSize: &analyzer.RequestSize{AvgInputTokens: 0, AvgOutputTokens: 10},
			wantErr:     false,
		},
		{
			name:        "minimum output tokens",
			requestSize: &analyzer.RequestSize{AvgInputTokens: 100, AvgOutputTokens: 1},
			wantErr:     false,
		},
		{
			name:        "negative input tokens",
			requestSize: &analyzer.RequestSize{AvgInputTokens: -1, AvgOutputTokens: 10},
			wantErr:     true,
		},
		{
			name:        "zero output tokens",
			requestSize: &analyzer.RequestSize{AvgInputTokens: 100, AvgOutputTokens: 0},
			wantErr:     true,
		},
		{
			name:        "negative output tokens",
			requestSize: &analyzer.RequestSize{AvgInputTokens: 100, AvgOutputTokens: -1},
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := analyzer.NewQueueAnalyzer(testConfig, tt.requestSize)
			if (err != nil) != tt.wantErr {
				t.Errorf("RequestSize check error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPrefillParms_PrefillTime(t *testing.T) {
	parms := &analyzer.ServiceParms{
		Alpha: 10.0,
		Beta:  0.01,
		Gamma: 0.001,
	}

	tests := []struct {
		name           string
		avgInputTokens int
		batchSize      float32
		expected       float32
	}{
		{
			name:           "no input tokens",
			avgInputTokens: 0,
			batchSize:      4.0,
			expected:       0.0,
		},
		{
			name:           "small batch",
			avgInputTokens: 1000,
			batchSize:      1.0,
			expected:       32.0,
		},
		{
			name:           "large batch",
			avgInputTokens: 2000,
			batchSize:      8.0,
			expected:       208.0,
		},
		{
			name:           "fractional batch size",
			avgInputTokens: 500,
			batchSize:      2.5,
			expected:       29.25,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parms.PrefillTime(&analyzer.RequestSize{
				AvgInputTokens:  float32(tt.avgInputTokens),
				AvgOutputTokens: 0,
			}, tt.batchSize)
			if math.Abs(float64(result-tt.expected)) > 1e-6 {
				t.Errorf("PrefillTime() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestDecodeParms_DecodeTime(t *testing.T) {
	decode := &analyzer.ServiceParms{
		Alpha: 1.0,
		Beta:  0.1,
		Gamma: 0.01,
	}

	tests := []struct {
		name      string
		batchSize float32
		expected  float32
	}{
		{
			name:      "single request",
			batchSize: 1.0,
			expected:  1.23,
		},
		{
			name:      "medium batch",
			batchSize: 4.0,
			expected:  1.575,
		},
		{
			name:      "large batch",
			batchSize: 8.0,
			expected:  2.035,
		},
		{
			name:      "fractional batch size",
			batchSize: 2.5,
			expected:  1.4025,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := decode.DecodeTime(
				&analyzer.RequestSize{
					AvgInputTokens:  1,
					AvgOutputTokens: 1,
				}, tt.batchSize)
			if math.Abs(float64(result-tt.expected)) > 1e-6 {
				t.Errorf("DecodeTime() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestBuildModel(t *testing.T) {
	requestSize := &analyzer.RequestSize{AvgInputTokens: 100, AvgOutputTokens: 10}
	qa := analyzer.BuildModel(testConfig, requestSize)

	// Test that the model is properly constructed
	if qa.MaxBatchSize != testConfig.MaxBatchSize {
		t.Errorf("MaxBatchSize = %v, expected %v", qa.MaxBatchSize, testConfig.MaxBatchSize)
	}

	if qa.MaxQueueSize != testConfig.MaxQueueSize {
		t.Errorf("MaxQueueSize = %v, expected %v", qa.MaxQueueSize, testConfig.MaxQueueSize)
	}

	if qa.ServiceParms != testConfig.ServiceParms {
		t.Error("ServiceParms should point to the same object")
	}

	if qa.RequestSize != requestSize {
		t.Error("RequestSize should point to the same object")
	}

	if qa.Model == nil {
		t.Error("Model should not be nil")
	}

	if qa.RateRange == nil {
		t.Error("RateRange should not be nil")
	}

	// Test that rate range is reasonable
	if qa.RateRange.Min >= qa.RateRange.Max {
		t.Errorf("RateRange.Min (%v) should be less than RateRange.Max (%v)",
			qa.RateRange.Min, qa.RateRange.Max)
	}

	if qa.RateRange.Min <= 0 {
		t.Errorf("RateRange.Min (%v) should be positive", qa.RateRange.Min)
	}
}

func TestQueueAnalyzer_Analyze(t *testing.T) {
	requestSize := &analyzer.RequestSize{AvgInputTokens: 100, AvgOutputTokens: 10}
	qa, err := analyzer.NewQueueAnalyzer(testConfig, requestSize)
	if err != nil {
		t.Fatalf("Failed to create QueueAnalyzer: %v", err)
	}

	tests := []struct {
		name        string
		requestRate float32
		wantErr     bool
	}{
		{
			name:        "zero request rate",
			requestRate: 0.0,
			wantErr:     true,
		},
		{
			name:        "negative request rate",
			requestRate: -1.0,
			wantErr:     true,
		},
		{
			name:        "low request rate",
			requestRate: qa.RateRange.Min * 0.5,
			wantErr:     false,
		},
		{
			name:        "medium request rate",
			requestRate: (qa.RateRange.Min + qa.RateRange.Max) * 0.5,
			wantErr:     false,
		},
		{
			name:        "high request rate within bounds",
			requestRate: qa.RateRange.Max * 0.9,
			wantErr:     false,
		},
		{
			name:        "request rate exceeding maximum",
			requestRate: qa.RateRange.Max * 1.1,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics, err := qa.Analyze(tt.requestRate)
			if (err != nil) != tt.wantErr {
				t.Errorf("Analyze() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				// Validate metrics for successful cases
				if metrics == nil {
					t.Error("Expected metrics but got nil")
					return
				}

				if metrics.Throughput < 0 {
					t.Errorf("Throughput (%v) should be non-negative", metrics.Throughput)
				}

				if metrics.AvgRespTime < 0 {
					t.Errorf("AvgRespTime (%v) should be non-negative", metrics.AvgRespTime)
				}

				if metrics.AvgWaitTime < 0 {
					t.Errorf("AvgWaitTime (%v) should be non-negative", metrics.AvgWaitTime)
				}

				if metrics.AvgNumInServ < 0 {
					t.Errorf("AvgNumInServ (%v) should be non-negative", metrics.AvgNumInServ)
				}

				if metrics.Rho < 0 || metrics.Rho > 1 {
					t.Errorf("Rho (%v) should be between 0 and 1", metrics.Rho)
				}

				if metrics.AvgPrefillTime < 0 {
					t.Errorf("AvgPrefillTime (%v) should be non-negative", metrics.AvgPrefillTime)
				}

				if metrics.AvgTokenTime < 0 {
					t.Errorf("AvgTokenTime (%v) should be non-negative", metrics.AvgTokenTime)
				}
			}
		})
	}
}

func TestQueueAnalyzer_Size(t *testing.T) {
	requestSize := &analyzer.RequestSize{AvgInputTokens: 100, AvgOutputTokens: 10}
	qa, err := analyzer.NewQueueAnalyzer(testConfig, requestSize)
	if err != nil {
		t.Fatalf("Failed to create QueueAnalyzer: %v", err)
	}

	tests := []struct {
		name       string
		targetPerf *analyzer.TargetPerf
		wantErr    bool
	}{
		{
			name: "valid targets",
			targetPerf: &analyzer.TargetPerf{
				TargetTTFT: 50.0,
				TargetITL:  5.0,
				TargetTPS:  100.0,
			},
			wantErr: false,
		},
		{
			name: "zero targets (disabled)",
			targetPerf: &analyzer.TargetPerf{
				TargetTTFT: 0.0,
				TargetITL:  0.0,
				TargetTPS:  0.0,
			},
			wantErr: false,
		},
		{
			name: "negative TTFT target",
			targetPerf: &analyzer.TargetPerf{
				TargetTTFT: -1.0,
				TargetITL:  5.0,
				TargetTPS:  100.0,
			},
			wantErr: true,
		},
		{
			name: "negative ITL target",
			targetPerf: &analyzer.TargetPerf{
				TargetTTFT: 50.0,
				TargetITL:  -1.0,
				TargetTPS:  100.0,
			},
			wantErr: true,
		},
		{
			name: "negative TPS target",
			targetPerf: &analyzer.TargetPerf{
				TargetTTFT: 50.0,
				TargetITL:  5.0,
				TargetTPS:  -1.0,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targetRate, metrics, achieved, err := qa.Size(tt.targetPerf)
			if (err != nil) != tt.wantErr {
				t.Errorf("Size() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if targetRate == nil {
					t.Error("Expected targetRate but got nil")
				}
				if metrics == nil {
					t.Error("Expected metrics but got nil")
				}
				if achieved == nil {
					t.Error("Expected achieved but got nil")
				}

				// Validate target rates
				if targetRate != nil {
					if targetRate.RateTargetTTFT < 0 {
						t.Errorf("RateTargetTTFT (%v) should be non-negative", targetRate.RateTargetTTFT)
					}
					if targetRate.RateTargetITL < 0 {
						t.Errorf("RateTargetITL (%v) should be non-negative", targetRate.RateTargetITL)
					}
					if targetRate.RateTargetTPS < 0 {
						t.Errorf("RateTargetTPS (%v) should be non-negative", targetRate.RateTargetTPS)
					}
				}

				// Validate achieved targets
				if achieved != nil {
					if achieved.TargetTTFT < 0 {
						t.Errorf("Achieved TargetTTFT (%v) should be non-negative", achieved.TargetTTFT)
					}
					if achieved.TargetITL < 0 {
						t.Errorf("Achieved TargetITL (%v) should be non-negative", achieved.TargetITL)
					}
					if achieved.TargetTPS < 0 {
						t.Errorf("Achieved TargetTPS (%v) should be non-negative", achieved.TargetTPS)
					}
				}
			}
		})
	}
}

func TestStringMethods(t *testing.T) {
	config := testConfig
	requestSize := &analyzer.RequestSize{AvgInputTokens: 100, AvgOutputTokens: 10}
	qa, err := analyzer.NewQueueAnalyzer(config, requestSize)
	if err != nil {
		t.Fatalf("Failed to create QueueAnalyzer: %v", err)
	}

	// Test all String() methods
	t.Run("Configuration.String", func(t *testing.T) {
		str := config.String()
		if len(str) == 0 {
			t.Error("Configuration.String() should not be empty")
		}
		if !strings.Contains(str, "maxBatch") {
			t.Error("Configuration.String() should contain maxBatch")
		}
	})

	t.Run("QueueAnalyzer.String", func(t *testing.T) {
		str := qa.String()
		if len(str) == 0 {
			t.Error("QueueAnalyzer.String() should not be empty")
		}
	})

	t.Run("ServiceParms.String", func(t *testing.T) {
		str := qa.ServiceParms.String()
		if len(str) == 0 {
			t.Error("ServiceParms.String() should not be empty")
		}
	})

	t.Run("PrefillParms.String", func(t *testing.T) {
		str := qa.ServiceParms.String()
		if len(str) == 0 {
			t.Error("PrefillParms.String() should not be empty")
		}
	})

	t.Run("DecodeParms.String", func(t *testing.T) {
		str := qa.ServiceParms.String()
		if len(str) == 0 {
			t.Error("DecodeParms.String() should not be empty")
		}
	})

	t.Run("RequestSize.String", func(t *testing.T) {
		str := requestSize.String()
		if len(str) == 0 {
			t.Error("RequestSize.String() should not be empty")
		}
	})

	t.Run("RateRange.String", func(t *testing.T) {
		str := qa.RateRange.String()
		if len(str) == 0 {
			t.Error("RateRange.String() should not be empty")
		}
	})

	// Test metrics and targets if analyzer works
	metrics, err := qa.Analyze(qa.RateRange.Min * 2)
	if err == nil && metrics != nil {
		t.Run("AnalysisMetrics.String", func(t *testing.T) {
			str := metrics.String()
			if len(str) == 0 {
				t.Error("AnalysisMetrics.String() should not be empty")
			}
		})
	}

	targetPerf := &analyzer.TargetPerf{TargetTTFT: 50.0, TargetITL: 5.0, TargetTPS: 100.0}
	t.Run("TargetPerf.String", func(t *testing.T) {
		str := targetPerf.String()
		if len(str) == 0 {
			t.Error("TargetPerf.String() should not be empty")
		}
	})

	targetRate, _, _, err := qa.Size(targetPerf)
	if err == nil && targetRate != nil {
		t.Run("TargetRate.String", func(t *testing.T) {
			str := targetRate.String()
			if len(str) == 0 {
				t.Error("TargetRate.String() should not be empty")
			}
		})
	}
}
