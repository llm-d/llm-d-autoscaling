/*
Copyright 2026 The llm-d Authors

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

package saturation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

func TestNormalizeRole(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"empty maps to both", "", domain.RoleBoth},
		{"both stays both", "both", domain.RoleBoth},
		{"prefill stays prefill", "prefill", "prefill"},
		{"decode stays decode", "decode", "decode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expect, normalizeRole(tc.input))
		})
	}
}

func TestGroupByRole(t *testing.T) {
	t.Run("non-disaggregated model produces single group", func(t *testing.T) {
		states := []domain.VariantReplicaState{
			{VariantName: "v1", Role: "both"},
			{VariantName: "v2", Role: ""},
			{VariantName: "v3", Role: "both"},
		}
		groups := groupByRole(states)
		require.Len(t, groups, 1)
		require.Contains(t, groups, "both")
		assert.Len(t, groups["both"], 3)
	})

	t.Run("disaggregated model produces per-role groups", func(t *testing.T) {
		states := []domain.VariantReplicaState{
			{VariantName: "prefill-h100", Role: "prefill"},
			{VariantName: "prefill-a100", Role: "prefill"},
			{VariantName: "decode-l4", Role: "decode"},
		}
		groups := groupByRole(states)
		require.Len(t, groups, 2)
		assert.Len(t, groups["prefill"], 2)
		assert.Len(t, groups["decode"], 1)
	})

	t.Run("mixed roles produce three groups", func(t *testing.T) {
		states := []domain.VariantReplicaState{
			{VariantName: "prefill-h100", Role: "prefill"},
			{VariantName: "decode-l4", Role: "decode"},
			{VariantName: "both-a100", Role: "both"},
		}
		groups := groupByRole(states)
		require.Len(t, groups, 3)
		assert.Len(t, groups["prefill"], 1)
		assert.Len(t, groups["decode"], 1)
		assert.Len(t, groups["both"], 1)
	})

	t.Run("empty states produce empty map", func(t *testing.T) {
		groups := groupByRole(nil)
		assert.Empty(t, groups)
	})
}

func TestFilterReplicaMetricsByVariants(t *testing.T) {
	metrics := []domain.ReplicaMetrics{
		{VariantName: "prefill-h100", PodName: "pod-1"},
		{VariantName: "prefill-h100", PodName: "pod-2"},
		{VariantName: "decode-l4", PodName: "pod-3"},
		{VariantName: "decode-l4", PodName: "pod-4"},
		{VariantName: "both-a100", PodName: "pod-5"},
	}

	t.Run("filter prefill only", func(t *testing.T) {
		states := []domain.VariantReplicaState{
			{VariantName: "prefill-h100"},
		}
		filtered := filterReplicaMetricsByVariants(metrics, states)
		assert.Len(t, filtered, 2)
		for _, m := range filtered {
			assert.Equal(t, "prefill-h100", m.VariantName)
		}
	})

	t.Run("filter decode only", func(t *testing.T) {
		states := []domain.VariantReplicaState{
			{VariantName: "decode-l4"},
		}
		filtered := filterReplicaMetricsByVariants(metrics, states)
		assert.Len(t, filtered, 2)
		for _, m := range filtered {
			assert.Equal(t, "decode-l4", m.VariantName)
		}
	})

	t.Run("filter all variants returns all metrics", func(t *testing.T) {
		states := []domain.VariantReplicaState{
			{VariantName: "prefill-h100"},
			{VariantName: "decode-l4"},
			{VariantName: "both-a100"},
		}
		filtered := filterReplicaMetricsByVariants(metrics, states)
		assert.Len(t, filtered, 5)
	})

	t.Run("filter no variants returns empty", func(t *testing.T) {
		filtered := filterReplicaMetricsByVariants(metrics, nil)
		assert.Empty(t, filtered)
	})
}

func TestFilterVAsByVariantStates(t *testing.T) {
	vas := []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		{ObjectMeta: metav1.ObjectMeta{Name: "prefill-h100"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "decode-l4"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "both-a100"}},
	}

	t.Run("filter to prefill only", func(t *testing.T) {
		states := []domain.VariantReplicaState{
			{VariantName: "prefill-h100"},
		}
		filtered := filterVAsByVariantStates(vas, states)
		require.Len(t, filtered, 1)
		assert.Equal(t, "prefill-h100", filtered[0].Name)
	})

	t.Run("filter to all returns all", func(t *testing.T) {
		states := []domain.VariantReplicaState{
			{VariantName: "prefill-h100"},
			{VariantName: "decode-l4"},
			{VariantName: "both-a100"},
		}
		filtered := filterVAsByVariantStates(vas, states)
		assert.Len(t, filtered, 3)
	})

	t.Run("empty states returns empty", func(t *testing.T) {
		filtered := filterVAsByVariantStates(vas, nil)
		assert.Empty(t, filtered)
	})
}

func TestVariantNames(t *testing.T) {
	states := []domain.VariantReplicaState{
		{VariantName: "v1"},
		{VariantName: "v2"},
		{VariantName: "v3"},
	}
	names := variantNames(states)
	assert.Equal(t, []string{"v1", "v2", "v3"}, names)
}
