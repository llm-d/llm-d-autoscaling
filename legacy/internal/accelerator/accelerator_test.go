package accelerator

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeAcceleratorName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		fullName          string
		expectedShortName string
	}{
		{
			name:              "NVIDIA A100",
			fullName:          "NVIDIA-A100-PCIE-80GB",
			expectedShortName: "A100",
		},
		{
			name:              "NVIDIA H100",
			fullName:          "NVIDIA-H100-SXM5-80GB",
			expectedShortName: "H100",
		},
		{
			name:              "NVIDIA L40S",
			fullName:          "NVIDIA-L40S-48GB",
			expectedShortName: "L40S",
		},
		{
			name:              "AMD MI300X",
			fullName:          "AMD-MI300X-192G",
			expectedShortName: "MI300X",
		},
		{
			name:              "Intel Gaudi 2",
			fullName:          "Intel-Gaudi-2-96GB",
			expectedShortName: "Gaudi-2",
		},
		{
			name:              "already short - A100",
			fullName:          "A100",
			expectedShortName: "A100",
		},
		{
			name:              "already short - H100",
			fullName:          "H100",
			expectedShortName: "H100",
		},
		{
			name:              "lowercase nvidia",
			fullName:          "nvidia-A100-PCIE-80GB",
			expectedShortName: "A100",
		},
		{
			name:              "unknown vendor fallback",
			fullName:          "Unknown-GPU-Model-123",
			expectedShortName: "GPU",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeAcceleratorName(tt.fullName)
			assert.Equal(t, tt.expectedShortName, result)
		})
	}
}
