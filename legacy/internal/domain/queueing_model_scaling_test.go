package domain

import (
	"testing"
)

func TestQueueingModelScalingConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		config  QueueingModelScalingConfig
		wantErr bool
	}{
		{
			name:    "valid defaults (all zero values)",
			config:  QueueingModelScalingConfig{},
			wantErr: false,
		},
		{
			name:    "valid explicit sloMultiplier",
			config:  QueueingModelScalingConfig{SLOMultiplier: 3.0},
			wantErr: false,
		},
		{
			name:    "invalid sloMultiplier equals 1",
			config:  QueueingModelScalingConfig{SLOMultiplier: 1.0},
			wantErr: true,
		},
		{
			name:    "invalid sloMultiplier less than 1",
			config:  QueueingModelScalingConfig{SLOMultiplier: 0.5},
			wantErr: true,
		},
		{
			name:    "valid both SLO targets set",
			config:  QueueingModelScalingConfig{TargetTTFT: 500.0, TargetITL: 50.0},
			wantErr: false,
		},
		{
			name:    "invalid partial SLO - only TTFT",
			config:  QueueingModelScalingConfig{TargetTTFT: 500.0},
			wantErr: true,
		},
		{
			name:    "invalid partial SLO - only ITL",
			config:  QueueingModelScalingConfig{TargetITL: 50.0},
			wantErr: true,
		},
		{
			name:    "invalid negative TTFT",
			config:  QueueingModelScalingConfig{TargetTTFT: -1.0, TargetITL: 50.0},
			wantErr: true,
		},
		{
			name:    "invalid negative ITL",
			config:  QueueingModelScalingConfig{TargetTTFT: 500.0, TargetITL: -1.0},
			wantErr: true,
		},
		{
			name: "valid per-model override with model_id and namespace",
			config: QueueingModelScalingConfig{
				ModelID:    "my-model",
				Namespace:  "my-ns",
				TargetTTFT: 500.0,
				TargetITL:  50.0,
			},
			wantErr: false,
		},
		{
			name: "invalid per-model override - model_id without namespace",
			config: QueueingModelScalingConfig{
				ModelID:    "my-model",
				TargetTTFT: 500.0,
				TargetITL:  50.0,
			},
			wantErr: true,
		},
		{
			name: "invalid per-model override - namespace without model_id",
			config: QueueingModelScalingConfig{
				Namespace:  "my-ns",
				TargetTTFT: 500.0,
				TargetITL:  50.0,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestQueueingModelScalingConfig_GetAnalyzerName(t *testing.T) {
	cfg := QueueingModelScalingConfig{}
	if cfg.GetAnalyzerName() != "queueing-model" {
		t.Errorf("GetAnalyzerName() = %q, want %q", cfg.GetAnalyzerName(), "queueing-model")
	}
}
