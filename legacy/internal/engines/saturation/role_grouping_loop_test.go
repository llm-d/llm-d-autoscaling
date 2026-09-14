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
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// stubAnalyzer is a controllable v1Analyzer for loop-coverage tests.
// It records every AnalyzeModelSaturation call. The analysis (or error)
// returned per call is looked up by walking replicaMetrics in slice order
// and returning on the first VariantName found in errByVariant or
// analysisByVariant; tests keep each role group's metrics to exactly one
// variant so the lookup is unambiguous.
type stubAnalyzer struct {
	// callMetrics captures the replicaMetrics slice handed to each call so
	// tests can assert per-role scoping of analyzer input.
	callMetrics [][]domain.ReplicaMetrics
	// analysisByVariant returns the ModelSaturationAnalysis the stub should
	// produce when a matching variant name appears in the incoming metrics.
	analysisByVariant map[string]*domain.ModelSaturationAnalysis
	// errByVariant returns an error when a matching variant name appears
	// in the incoming metrics.
	errByVariant map[string]error
	// targetsByVariant overrides CalculateSaturationTargets outputs keyed on
	// the first variant name present in variantStates.
	targetsByVariant map[string]map[string]int
}

func (s *stubAnalyzer) AnalyzeModelSaturation(
	_ context.Context,
	modelID, namespace string,
	replicaMetrics []domain.ReplicaMetrics,
	_ config.SaturationScalingConfig,
) (*domain.ModelSaturationAnalysis, error) {
	s.callMetrics = append(s.callMetrics, append([]domain.ReplicaMetrics(nil), replicaMetrics...))
	for _, m := range replicaMetrics {
		if err, ok := s.errByVariant[m.VariantName]; ok {
			return nil, err
		}
		if a, ok := s.analysisByVariant[m.VariantName]; ok {
			return a, nil
		}
	}
	return &domain.ModelSaturationAnalysis{ModelID: modelID, Namespace: namespace}, nil
}

func (s *stubAnalyzer) CalculateSaturationTargets(
	_ context.Context,
	_ *domain.ModelSaturationAnalysis,
	variantStates []domain.VariantReplicaState,
) map[string]int {
	if len(variantStates) > 0 {
		if t, ok := s.targetsByVariant[variantStates[0].VariantName]; ok {
			return t
		}
	}
	// Default: target == current replicas for every state.
	out := make(map[string]int, len(variantStates))
	for _, st := range variantStates {
		out[st.VariantName] = st.CurrentReplicas
	}
	return out
}

// engineWithStub returns an Engine whose v1AnalyzerFactory yields stub on
// every call. Kept per-instance (no package-level state) so tests are
// naturally isolated.
func engineWithStub(stub *stubAnalyzer) *Engine {
	return &Engine{v1AnalyzerFactory: func() v1Analyzer { return stub }}
}

// uniqueVariantCount returns the number of distinct VariantNames in m,
// used to assert that each role group's metrics are scoped to exactly the
// variants in that role.
func uniqueVariantCount(m []domain.ReplicaMetrics) int {
	seen := map[string]struct{}{}
	for _, r := range m {
		seen[r.VariantName] = struct{}{}
	}
	return len(seen)
}

// pdFixture is the standard prefill+decode fixture shared by the P/D
// role-grouping tests. Individual tests mutate the returned slices in
// place if they need different CurrentReplicas, etc.
type pdFixture struct {
	data     *modelData
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling
}

func newPDFixture() pdFixture {
	return pdFixture{
		data: &modelData{
			variantStates: []domain.VariantReplicaState{
				{VariantName: "prefill-h100", Role: "prefill", CurrentReplicas: 1},
				{VariantName: "decode-l4", Role: "decode", CurrentReplicas: 1},
			},
			replicaMetrics: []domain.ReplicaMetrics{
				{VariantName: "prefill-h100", PodName: "pod-p1"},
				{VariantName: "decode-l4", PodName: "pod-d1"},
			},
		},
		modelVAs: []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
			{ObjectMeta: metav1.ObjectMeta{Name: "prefill-h100", Namespace: "ns"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "decode-l4", Namespace: "ns"}},
		},
	}
}

// collectSafetyNetVAs returns a spy emitSafetyNet closure that appends each
// call's VA name set into calls.
func collectSafetyNetVAs(calls *[][]string) safetyNetEmitter {
	return func(
		_ context.Context,
		vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
		_ map[string]*domain.Allocation,
		_ map[string]scaletarget.ScaleTargetAccessor,
	) {
		names := make([]string, 0, len(vas))
		for _, v := range vas {
			names = append(names, v.Name)
		}
		*calls = append(*calls, names)
	}
}

// noopSafetyNet is used when a test does not exercise the failure path.
func noopSafetyNet(
	_ context.Context,
	_ []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	_ map[string]*domain.Allocation,
	_ map[string]scaletarget.ScaleTargetAccessor,
) {
}

func TestAnalyzeRoleGroups_DisaggregatedModel_TwoIndependentAnalyses(t *testing.T) {
	stub := &stubAnalyzer{
		analysisByVariant: map[string]*domain.ModelSaturationAnalysis{
			"prefill-h100": {
				ModelID:   "m",
				Namespace: "ns",
				VariantAnalyses: []domain.VariantSaturationAnalysis{
					{VariantName: "prefill-h100", ReplicaCount: 1},
				},
			},
			"decode-l4": {
				ModelID:   "m",
				Namespace: "ns",
				VariantAnalyses: []domain.VariantSaturationAnalysis{
					{VariantName: "decode-l4", ReplicaCount: 1},
				},
			},
		},
		targetsByVariant: map[string]map[string]int{
			"prefill-h100": {"prefill-h100": 2}, // scale up
			"decode-l4":    {"decode-l4": 1},    // no change
		},
	}
	fx := newPDFixture()
	e := engineWithStub(stub)

	decisions := e.analyzeRoleGroups(
		context.Background(),
		"m", "ns",
		config.SaturationScalingConfig{},
		fx.data, fx.modelVAs, nil,
		noopSafetyNet,
	)

	// Two independent analyzer invocations, one per role group, each scoped
	// to its own variant's metrics only.
	require.Len(t, stub.callMetrics, 2, "expected one AnalyzeModelSaturation call per role group")
	for _, m := range stub.callMetrics {
		require.Equal(t, 1, uniqueVariantCount(m), "each role group should receive exactly one variant's metrics")
	}

	// Both role groups must produce a decision; Role field must propagate.
	byVariant := map[string]domain.VariantDecision{}
	for _, d := range decisions {
		byVariant[d.VariantName] = d
	}
	require.Contains(t, byVariant, "prefill-h100")
	require.Contains(t, byVariant, "decode-l4")
	assert.Equal(t, "prefill", byVariant["prefill-h100"].Role)
	assert.Equal(t, "decode", byVariant["decode-l4"].Role)
	assert.Equal(t, domain.ActionScaleUp, byVariant["prefill-h100"].Action)
	assert.Equal(t, domain.ActionNoChange, byVariant["decode-l4"].Action)
}

func TestAnalyzeRoleGroups_HomogeneousModel_SingleGroup(t *testing.T) {
	stub := &stubAnalyzer{
		analysisByVariant: map[string]*domain.ModelSaturationAnalysis{
			"variant-a": {
				ModelID:   "m",
				Namespace: "ns",
				VariantAnalyses: []domain.VariantSaturationAnalysis{
					{VariantName: "variant-a", ReplicaCount: 2},
					{VariantName: "variant-b", ReplicaCount: 1},
				},
			},
		},
		targetsByVariant: map[string]map[string]int{
			"variant-a": {"variant-a": 2, "variant-b": 1},
		},
	}
	e := engineWithStub(stub)

	data := &modelData{
		variantStates: []domain.VariantReplicaState{
			{VariantName: "variant-a", Role: "", CurrentReplicas: 2},
			{VariantName: "variant-b", Role: "", CurrentReplicas: 1},
		},
		replicaMetrics: []domain.ReplicaMetrics{
			{VariantName: "variant-a", PodName: "a-1"},
			{VariantName: "variant-a", PodName: "a-2"},
			{VariantName: "variant-b", PodName: "b-1"},
		},
	}
	modelVAs := []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		{ObjectMeta: metav1.ObjectMeta{Name: "variant-a", Namespace: "ns"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "variant-b", Namespace: "ns"}},
	}

	decisions := e.analyzeRoleGroups(
		context.Background(),
		"m", "ns",
		config.SaturationScalingConfig{},
		data, modelVAs, nil,
		noopSafetyNet,
	)

	require.Len(t, stub.callMetrics, 1, "homogeneous-role model should collapse to a single analyzer call")
	require.Len(t, decisions, 2)
	// Role defaults to empty for non-disaggregated variants; propagated verbatim.
	byVariant := map[string]domain.VariantDecision{}
	for _, d := range decisions {
		byVariant[d.VariantName] = d
	}
	assert.Equal(t, "", byVariant["variant-a"].Role)
	assert.Equal(t, "", byVariant["variant-b"].Role)
}

func TestAnalyzeRoleGroups_PerRoleFailure_SafetyNetScopedToFailingRole(t *testing.T) {
	analysisErr := errors.New("prefill analyzer blew up")
	stub := &stubAnalyzer{
		errByVariant: map[string]error{"prefill-h100": analysisErr},
		analysisByVariant: map[string]*domain.ModelSaturationAnalysis{
			"decode-l4": {
				ModelID:   "m",
				Namespace: "ns",
				VariantAnalyses: []domain.VariantSaturationAnalysis{
					{VariantName: "decode-l4", ReplicaCount: 1},
				},
			},
		},
		targetsByVariant: map[string]map[string]int{
			"decode-l4": {"decode-l4": 1},
		},
	}
	fx := newPDFixture()
	e := engineWithStub(stub)

	var safetyVAs [][]string
	decisions := e.analyzeRoleGroups(
		context.Background(),
		"m", "ns",
		config.SaturationScalingConfig{},
		fx.data, fx.modelVAs, nil,
		collectSafetyNetVAs(&safetyVAs),
	)

	// Safety-net must be invoked exactly once, with only the failing role's VAs.
	require.Len(t, safetyVAs, 1, "safety-net should fire once for the failing role group")
	assert.Equal(t, []string{"prefill-h100"}, safetyVAs[0],
		"safety-net scope must be limited to the failing role's VAs")

	// Sibling role group still produces its decision.
	require.Len(t, decisions, 1)
	assert.Equal(t, "decode-l4", decisions[0].VariantName)
	assert.Equal(t, "decode", decisions[0].Role)
}

func TestAnalyzeRoleGroups_MergesDecisionsAcrossRoles(t *testing.T) {
	// Covers the reviewer's "scale-to-zero with mixed roles" scenario at the
	// analyzeRoleGroups boundary: both role groups produce decisions, and the
	// merged slice (which optimizeV1 forwards to ScaleToZeroEnforcer) contains
	// entries from every group.
	stub := &stubAnalyzer{
		analysisByVariant: map[string]*domain.ModelSaturationAnalysis{
			"prefill-h100": {
				ModelID:   "m",
				Namespace: "ns",
				VariantAnalyses: []domain.VariantSaturationAnalysis{
					{VariantName: "prefill-h100", ReplicaCount: 1},
				},
			},
			"decode-l4": {
				ModelID:   "m",
				Namespace: "ns",
				VariantAnalyses: []domain.VariantSaturationAnalysis{
					{VariantName: "decode-l4", ReplicaCount: 1},
				},
			},
		},
		targetsByVariant: map[string]map[string]int{
			"prefill-h100": {"prefill-h100": 0}, // scale to zero
			"decode-l4":    {"decode-l4": 0},    // scale to zero
		},
	}
	fx := newPDFixture()
	e := engineWithStub(stub)

	decisions := e.analyzeRoleGroups(
		context.Background(),
		"m", "ns",
		config.SaturationScalingConfig{},
		fx.data, fx.modelVAs, nil,
		noopSafetyNet,
	)

	require.Len(t, decisions, 2, "merged decisions must include both role groups for scale-to-zero enforcement")
	roles := map[string]struct{}{}
	for _, d := range decisions {
		roles[d.Role] = struct{}{}
		assert.Equal(t, domain.ActionScaleDown, d.Action)
	}
	assert.Contains(t, roles, "prefill")
	assert.Contains(t, roles, "decode")
}

func TestAnalyzeRoleGroups_RoleGroupWithoutMetrics_EmitsSafetyNet(t *testing.T) {
	stub := &stubAnalyzer{
		analysisByVariant: map[string]*domain.ModelSaturationAnalysis{
			"decode-l4": {
				ModelID:   "m",
				Namespace: "ns",
				VariantAnalyses: []domain.VariantSaturationAnalysis{
					{VariantName: "decode-l4", ReplicaCount: 1},
				},
			},
		},
		targetsByVariant: map[string]map[string]int{
			"decode-l4": {"decode-l4": 1},
		},
	}
	e := engineWithStub(stub)

	// prefill has states but no metrics; decode has both. The metric-less
	// role must still get safety-net emission (scoped to its own VAs) so
	// HPA/KEDA does not go dark while prefill waits for metrics to appear.
	fx := newPDFixture()
	fx.data.replicaMetrics = []domain.ReplicaMetrics{
		{VariantName: "decode-l4", PodName: "pod-d1"},
	}

	var safetyVAs [][]string
	decisions := e.analyzeRoleGroups(
		context.Background(),
		"m", "ns",
		config.SaturationScalingConfig{},
		fx.data, fx.modelVAs, nil,
		collectSafetyNetVAs(&safetyVAs),
	)

	require.Len(t, stub.callMetrics, 1, "analyzer should only be invoked for the role group that has metrics")
	require.Len(t, safetyVAs, 1, "the metric-less role group must trigger one safety-net emission")
	assert.Equal(t, []string{"prefill-h100"}, safetyVAs[0],
		"safety-net scope must be limited to the metric-less role's VAs")
	require.Len(t, decisions, 1)
	assert.Equal(t, "decode-l4", decisions[0].VariantName)
}

func TestSortedRoleKeys_DeterministicOrder(t *testing.T) {
	// Exercises the precondition promised in sortedRoleKeys: callers pass
	// keys that groupByRole has already normalized, so the resulting order
	// is stable lexicographic.
	groups := map[string][]domain.VariantReplicaState{
		"prefill": nil,
		"both":    nil,
		"decode":  nil,
	}
	got := sortedRoleKeys(groups)
	assert.Equal(t, []string{"both", "decode", "prefill"}, got)

	// Single-group and empty-map cases.
	assert.Equal(t, []string{domain.RoleBoth},
		sortedRoleKeys(map[string][]domain.VariantReplicaState{domain.RoleBoth: nil}))
	assert.Empty(t, sortedRoleKeys(map[string][]domain.VariantReplicaState{}))
}
