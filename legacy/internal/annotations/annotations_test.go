/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package annotations_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
)

func obj(ann map[string]string) metav1.Object {
	return &metav1.ObjectMeta{Annotations: ann}
}

func TestIsManaged(t *testing.T) {
	tests := []struct {
		name string
		ann  map[string]string
		want bool
	}{
		{"managed true", map[string]string{annotations.Managed: "true"}, true},
		{"managed false", map[string]string{annotations.Managed: "false"}, false},
		{"missing", map[string]string{}, false},
		{"nil annotations", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := annotations.IsManaged(obj(tc.ann)); got != tc.want {
				t.Errorf("IsManaged() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name        string
		ann         map[string]string
		wantModelID string
		wantCost    string
		wantErr     bool
	}{
		{
			name:        "all fields",
			ann:         map[string]string{annotations.Managed: "true", annotations.ModelID: "ibm/granite-13b", annotations.VariantCost: "40.0"},
			wantModelID: "ibm/granite-13b",
			wantCost:    "40.0",
		},
		{
			name:        "cost defaults to 10.0",
			ann:         map[string]string{annotations.Managed: "true", annotations.ModelID: "ibm/granite-13b"},
			wantModelID: "ibm/granite-13b",
			wantCost:    "10.0",
		},
		{
			name:    "missing managed",
			ann:     map[string]string{annotations.ModelID: "ibm/granite-13b"},
			wantErr: true,
		},
		{
			name:    "managed false",
			ann:     map[string]string{annotations.Managed: "false", annotations.ModelID: "ibm/granite-13b"},
			wantErr: true,
		},
		{
			name:    "missing model-id",
			ann:     map[string]string{annotations.Managed: "true"},
			wantErr: true,
		},
		{
			name:    "invalid cost",
			ann:     map[string]string{annotations.Managed: "true", annotations.ModelID: "ibm/granite-13b", annotations.VariantCost: "not-a-number"},
			wantErr: true,
		},
		{
			name:        "integer cost",
			ann:         map[string]string{annotations.Managed: "true", annotations.ModelID: "m", annotations.VariantCost: "5"},
			wantModelID: "m",
			wantCost:    "5",
		},
		{
			name:    "negative cost rejected",
			ann:     map[string]string{annotations.Managed: "true", annotations.ModelID: "m", annotations.VariantCost: "-5.0"},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := annotations.Parse(obj(tc.ann))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Parse() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if got.ModelID != tc.wantModelID {
				t.Errorf("ModelID = %q, want %q", got.ModelID, tc.wantModelID)
			}
			if got.VariantCost != tc.wantCost {
				t.Errorf("VariantCost = %q, want %q", got.VariantCost, tc.wantCost)
			}
		})
	}
}
