package config

import (
	"testing"
)

func TestSaturatedAllocationPolicy_String(t *testing.T) {
	tests := []struct {
		name   string
		policy SaturatedAllocationPolicy
		want   string
	}{
		{
			name:   "No policy",
			policy: None,
			want:   "None",
		},
		{
			name:   "PriorityExhaustive policy",
			policy: PriorityExhaustive,
			want:   "PriorityExhaustive",
		},
		{
			name:   "PriorityRoundRobin policy",
			policy: PriorityRoundRobin,
			want:   "PriorityRoundRobin",
		},
		{
			name:   "RoundRobin policy",
			policy: RoundRobin,
			want:   "RoundRobin",
		},
		{
			name:   "Unknown policy",
			policy: SaturatedAllocationPolicy(999),
			want:   "Unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.policy.String(); got != tt.want {
				t.Errorf("SaturatedAllocationPolicy.String() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSaturatedAllocationPolicyEnum(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  SaturatedAllocationPolicy
	}{
		{
			name:  "No policy string",
			input: "None",
			want:  None,
		},
		{
			name:  "PriorityExhaustive string",
			input: "PriorityExhaustive",
			want:  PriorityExhaustive,
		},
		{
			name:  "PriorityRoundRobin string",
			input: "PriorityRoundRobin",
			want:  PriorityRoundRobin,
		},
		{
			name:  "RoundRobin string",
			input: "RoundRobin",
			want:  RoundRobin,
		},
		{
			name:  "Unknown policy string returns default",
			input: "InvalidPolicy",
			want:  DefaultSaturatedAllocationPolicy,
		},
		{
			name:  "Empty policy string returns default",
			input: "",
			want:  DefaultSaturatedAllocationPolicy,
		},
		{
			name:  "Case sensitive - lowercase (non-existent) returns default",
			input: "none",
			want:  DefaultSaturatedAllocationPolicy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SaturatedAllocationPolicyEnum(tt.input); got != tt.want {
				t.Errorf("SaturatedAllocationPolicyEnum(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestSaturatedAllocationPolicy_RoundTrip(t *testing.T) {
	policies := []SaturatedAllocationPolicy{
		None,
		PriorityExhaustive,
		PriorityRoundRobin,
		RoundRobin,
	}

	for _, policy := range policies {
		t.Run(policy.String(), func(t *testing.T) {
			str := policy.String()
			roundTrip := SaturatedAllocationPolicyEnum(str)
			if roundTrip != policy {
				t.Errorf("Round trip failed: %v -> %v -> %v", policy, str, roundTrip)
			}
		})
	}
}
