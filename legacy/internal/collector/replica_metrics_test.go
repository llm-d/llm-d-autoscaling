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

package collector

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// Shared test literals (extracted to satisfy goconst across the new SGLang cases).
const (
	sglangScaleTargetKey = "test-ns/sglang-decode"
	mixedVLLMPodName     = "vllm-0"
	testServerContainer  = "server"
	sglangVariantLabel   = "va-sglang"
	sglangKVUsageKey     = "sglang/kv_cache_usage"
)

// mockMetricsSource is a mock implementation of source.MetricsSource for testing
type mockMetricsSource struct {
	refreshFunc  func(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error)
	refreshError error
	results      map[string]*source.MetricResult
}

func (m *mockMetricsSource) QueryList() *source.QueryList {
	return source.NewQueryList()
}

func (m *mockMetricsSource) Refresh(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	// If refreshFunc is set, use it (takes precedence)
	if m.refreshFunc != nil {
		return m.refreshFunc(ctx, spec)
	}
	// Otherwise use the error/results fields
	if m.refreshError != nil {
		return nil, m.refreshError
	}
	if m.results != nil {
		return m.results, nil
	}
	// Return empty results by default
	emptyResults := make(map[string]*source.MetricResult)
	for _, query := range spec.Queries {
		emptyResults[query] = &source.MetricResult{
			QueryName: query,
			Values:    []source.MetricValue{},
		}
	}
	return emptyResults, nil
}

func (m *mockMetricsSource) Get(queryName string, params map[string]string) *source.CachedValue {
	return nil
}

func TestRecordMetricsUnavailableEvent(t *testing.T) {
	tests := []struct {
		name         string
		numVAs       int
		expectedEvts int
	}{
		{
			name:         "records event for single VA",
			numVAs:       1,
			expectedEvts: 1,
		},
		{
			name:         "records event for multiple VAs",
			numVAs:       3,
			expectedEvts: 3,
		},
		{
			name:         "handles empty VA map",
			numVAs:       0,
			expectedEvts: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeRecorder := record.NewFakeRecorder(100)
			mockSource := &mockMetricsSource{}
			collector := NewReplicaMetricsCollector(mockSource, nil, fakeRecorder, nil)

			variantAutoscalings := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)
			for i := 0; i < tt.numVAs; i++ {
				vaName := "test-va"
				if i > 0 {
					vaName = "test-va-" + string(rune('a'+i))
				}
				variantAutoscalings["default/"+vaName] = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					ObjectMeta: metav1.ObjectMeta{
						Name:      vaName,
						Namespace: "default",
					},
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
							Kind: "Deployment",
							Name: vaName + "-deployment",
						},
						ModelID:     "test-model",
						MaxReplicas: 5,
					},
				}
			}

			collector.recordMetricsUnavailableEvent(variantAutoscalings, nil, "Test metrics unavailable")

			// Count recorded events
			eventCount := 0
			for {
				select {
				case event := <-fakeRecorder.Events:
					assert.Contains(t, event, constants.K8SEventMetricsUnavailable,
						"Event should contain K8SEventMetricsUnavailable constant")
					assert.Contains(t, event, "Test metrics unavailable",
						"Event should contain the reason message")
					eventCount++
				default:
					goto done
				}
			}
		done:
			assert.Equal(t, tt.expectedEvts, eventCount,
				"Should record correct number of events")
		})
	}
}

func TestCollectReplicaMetrics_ErrorRecordsEvent(t *testing.T) {
	// This test verifies edge-triggered event emission for metrics collection errors.
	// Note: Without actual pod data in the k8s client, replicaMetrics is always empty,
	// so we can't test the full "available → error" transition. This test focuses on
	// verifying that repeated errors don't flood the event stream.

	ctx := context.Background()
	fakeRecorder := record.NewFakeRecorder(100)

	variantAutoscalings := map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		"default/test-va": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-va",
				Namespace: "default",
			},
			Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "test-deployment",
				},
				ModelID:     "test-model",
				MaxReplicas: 5,
			},
		},
	}

	scaleTargets := make(map[string]scaletarget.ScaleTargetAccessor)
	variantCosts := make(map[string]float64)

	// Simulate metrics collection failure
	mockSource := &mockMetricsSource{
		refreshError: errors.New("prometheus connection failed"),
	}
	collector := NewReplicaMetricsCollector(mockSource, nil, fakeRecorder, nil)

	// First call with error: no event (first observation, unknown previous state)
	metrics, err := collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.Error(t, err, "Should return error when refresh fails")
	require.Nil(t, metrics, "Should return nil metrics on error")

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("No event expected on first observation: %s", event)
	default:
		// Expected: no event
	}

	// Second call: metrics still fail, should NOT emit event (no state transition)
	_, err = collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.Error(t, err, "Should still return error")

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("No event expected when metrics remain unavailable: %s", event)
	default:
		// Expected: no event
	}
}

func TestCollectReplicaMetrics_NoMetricsRecordsEvent(t *testing.T) {
	// This test verifies edge-triggered event emission when no metrics are available.
	// Simulates a VA scaled to zero (no pods = no metrics) to verify that repeated
	// "no metrics" states don't flood the event stream.

	ctx := context.Background()
	fakeRecorder := record.NewFakeRecorder(100)

	variantAutoscalings := map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		"default/test-va": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-va",
				Namespace: "default",
			},
			Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "test-deployment",
				},
				ModelID:     "test-model",
				MaxReplicas: 5,
			},
		},
	}

	scaleTargets := make(map[string]scaletarget.ScaleTargetAccessor)
	variantCosts := make(map[string]float64)

	// Mock source with no metrics (e.g., VA scaled to zero)
	mockSource := &mockMetricsSource{
		results: make(map[string]*source.MetricResult),
	}
	collector := NewReplicaMetricsCollector(mockSource, nil, fakeRecorder, nil)

	// First call: no metrics, should NOT emit event (first observation, unknown previous state)
	metrics, err := collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.NoError(t, err, "Should not return error when no metrics available")
	require.Empty(t, metrics, "Should return empty metrics slice")

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("No event expected on first observation: %s", event)
	default:
		// Expected: no event
	}

	// Second call: still no metrics, should NOT emit event (no state transition)
	_, err = collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.NoError(t, err, "Should not return error")

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("No event expected when metrics remain unavailable: %s", event)
	default:
		// Expected: no event
	}

	// Third call: still no metrics, should NOT emit event (no state transition)
	_, err = collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.NoError(t, err, "Should not return error")

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("No event expected when metrics remain unavailable: %s", event)
	default:
		// Expected: no event
	}
}

func TestK8SEventMetricsUnavailableConstant(t *testing.T) {
	// Verify the constant is correctly defined
	assert.Equal(t, "MetricsUnavailable", constants.K8SEventMetricsUnavailable,
		"K8SEventMetricsUnavailable constant should match expected value")
}

func TestCollectReplicaMetrics_EdgeTriggeredEvents(t *testing.T) {
	// This test verifies the core edge-triggered behavior: events are emitted only on
	// state transitions, not on every cycle with unavailable metrics. This prevents
	// event flooding when a VA is legitimately scaled to zero.

	ctx := context.Background()
	fakeRecorder := record.NewFakeRecorder(100)

	variantAutoscalings := map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		"default/test-va": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-va",
				Namespace: "default",
			},
			Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "test-deployment",
				},
				ModelID:     "test-model",
				MaxReplicas: 5,
			},
		},
	}

	scaleTargets := make(map[string]scaletarget.ScaleTargetAccessor)
	variantCosts := make(map[string]float64)

	// Mock source that starts with no metrics (simulates VA scaled to zero)
	mockSource := &mockMetricsSource{
		results: make(map[string]*source.MetricResult),
	}
	collector := NewReplicaMetricsCollector(mockSource, nil, fakeRecorder, nil)

	// First call: metrics unavailable, should NOT emit event (first observation, unknown previous state)
	_, err := collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.NoError(t, err)

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("First call should not emit event (unknown previous state): %s", event)
	default:
		// Expected: no event - prevents false positive for VAs that start at zero
	}

	// Second call: metrics still unavailable, should NOT emit event (no state transition)
	_, err = collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.NoError(t, err)

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("Second call should not emit event when metrics remain unavailable: %s", event)
	default:
		// Expected: no event - prevents flooding on every optimization cycle
	}

	// Third call: still unavailable, should NOT emit event
	_, err = collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.NoError(t, err)

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("Third call should not emit event when metrics remain unavailable: %s", event)
	default:
		// Expected: no event
	}
}

func TestCollectReplicaMetrics_MetricsObservation(t *testing.T) {
	// Initialize metrics with a fresh registry
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

	// Create a mock source that returns empty results
	mockSource := &mockMetricsSource{
		refreshFunc: func(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
			// Simulate some query latency
			time.Sleep(10 * time.Millisecond)
			// Return empty results
			return make(map[string]*source.MetricResult), nil
		},
	}

	// Create test dependencies
	scheme := runtime.NewScheme()
	err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme)
	if err != nil {
		t.Fatalf("Failed to add scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	fakeRecorder := record.NewFakeRecorder(100)
	collector := NewReplicaMetricsCollector(mockSource, k8sClient, fakeRecorder, nil)

	// Call the function
	_, err = collector.CollectReplicaMetrics(
		context.Background(),
		"test-model",
		"test-namespace",
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
		make(map[string]float64),
	)
	if err != nil {
		t.Fatalf("CollectReplicaMetrics failed: %v", err)
	}

	// Gather metrics from the registry
	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Verify ObserveMetricsCollectionDuration was called for all query types
	var foundDurationMetric bool
	expectedQueryTypes := map[string]bool{
		constants.QueryTypeKVCache:     false,
		constants.QueryTypeQueueLength: false,
		constants.QueryTypeCacheConfig: false,
	}

	for _, mf := range metricFamilies {
		if mf.GetName() == constants.WVAMetricsCollectionDurationSeconds {
			foundDurationMetric = true

			// Check each metric series
			for _, m := range mf.GetMetric() {
				// Find query_type label
				for _, label := range m.GetLabel() {
					if label.GetName() == constants.LabelQueryType {
						queryType := label.GetValue()
						if _, exists := expectedQueryTypes[queryType]; exists {
							expectedQueryTypes[queryType] = true
							histogram := m.GetHistogram()
							if histogram == nil {
								t.Errorf("Expected histogram for query_type=%s", queryType)
								continue
							}
							if histogram.GetSampleCount() == 0 {
								t.Errorf("Expected at least one observation for query_type=%s", queryType)
							}
							if histogram.GetSampleSum() <= 0 {
								t.Errorf("Expected positive duration for query_type=%s", queryType)
							}
						}
					}
				}
			}
		}
	}

	if !foundDurationMetric {
		t.Errorf("Metric %s not found", constants.WVAMetricsCollectionDurationSeconds)
	}

	// Verify all expected query types were recorded
	for queryType, found := range expectedQueryTypes {
		if !found {
			t.Errorf("Expected duration metric for query_type=%s but was not found", queryType)
		}
	}

	// Verify SetMetricsPodsDiscovered was called
	var foundPodsMetric bool
	for _, mf := range metricFamilies {
		if mf.GetName() == constants.WVAMetricsPodsDiscovered {
			foundPodsMetric = true
			// Should have at least one metric (for test-namespace)
			if len(mf.GetMetric()) == 0 {
				t.Error("Expected at least one pods discovered metric")
			}
		}
	}

	if !foundPodsMetric {
		t.Errorf("Metric %s not found", constants.WVAMetricsPodsDiscovered)
	}
}

func TestCollectReplicaMetrics_ErrorMetrics(t *testing.T) {
	// Initialize metrics with a fresh registry
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

	// Create a mock source that returns an error
	testErr := context.DeadlineExceeded
	mockSource := &mockMetricsSource{
		refreshFunc: func(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return nil, testErr
		},
	}

	// Create test dependencies
	scheme := runtime.NewScheme()
	err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme)
	if err != nil {
		t.Fatalf("Failed to add scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	fakeRecorder := record.NewFakeRecorder(100)
	collector := NewReplicaMetricsCollector(mockSource, k8sClient, fakeRecorder, nil)

	// Call the function - should return error
	_, err = collector.CollectReplicaMetrics(
		context.Background(),
		"test-model",
		"test-namespace",
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
		make(map[string]float64),
	)
	if err == nil {
		t.Fatal("Expected error but got nil")
	}

	// Gather metrics from the registry
	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Verify IncMetricsCollectionErrors was called for all query types
	var foundErrorMetric bool
	expectedQueryTypes := map[string]bool{
		constants.QueryTypeKVCache:     false,
		constants.QueryTypeQueueLength: false,
		constants.QueryTypeCacheConfig: false,
	}

	for _, mf := range metricFamilies {
		if mf.GetName() == constants.WVAMetricsCollectionErrorsTotal {
			foundErrorMetric = true

			// Check each metric series
			for _, m := range mf.GetMetric() {
				// Find query_type label
				var queryType string
				for _, label := range m.GetLabel() {
					if label.GetName() == constants.LabelQueryType {
						queryType = label.GetValue()
						break
					}
				}

				if _, exists := expectedQueryTypes[queryType]; exists {
					expectedQueryTypes[queryType] = true
					counter := m.GetCounter()
					if counter == nil {
						t.Errorf("Expected counter for query_type=%s", queryType)
						continue
					}
					if counter.GetValue() != 1.0 {
						t.Errorf("Expected error count 1 for query_type=%s, got %f", queryType, counter.GetValue())
					}
				}
			}
		}
	}

	if !foundErrorMetric {
		t.Errorf("Metric %s not found", constants.WVAMetricsCollectionErrorsTotal)
	}

	// Verify all expected query types were recorded
	for queryType, found := range expectedQueryTypes {
		if !found {
			t.Errorf("Expected error metric for query_type=%s but was not found", queryType)
		}
	}
}

// TestCollectReplicaMetrics_ThroughputKeyMerge verifies that when the KV-cache
// query and the throughput queries (GenerationTokenRate, KvUsageInstant,
// RequestRate) return results for the same pod, they merge into a single
// ReplicaMetrics entry with all fields non-zero.
//
// Before the Bug A fix, throughput loops used the bare pod name as the podData
// key while all other loops used buildInstanceKey's composite key (pod:port).
// The entries never merged and the throughput fields were always zero.
func TestCollectReplicaMetrics_ThroughputKeyMerge(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	podLabels := map[string]string{
		"pod":                               "pod-abc",
		"instance":                          "10.0.0.1:8000",
		constants.VariantLabelPrometheusKey: "va-1",
	}
	ts := time.Now()

	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {
					Values: []source.MetricValue{
						{Labels: podLabels, Value: 0.55, Timestamp: ts},
					},
				},
				"generation_token_rate": {
					Values: []source.MetricValue{
						{Labels: podLabels, Value: 1500.0, Timestamp: ts},
					},
				},
				"kv_usage_instant": {
					Values: []source.MetricValue{
						{Labels: podLabels, Value: 0.50, Timestamp: ts},
					},
				},
				"request_rate": {
					Values: []source.MetricValue{
						{Labels: podLabels, Value: 7.0, Timestamp: ts},
					},
				},
			}, nil
		},
	}

	collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil)
	results, err := collector.CollectReplicaMetrics(
		context.Background(),
		"test-model",
		"test-ns",
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
		make(map[string]float64),
	)
	if err != nil {
		t.Fatalf("CollectReplicaMetrics: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected exactly 1 ReplicaMetrics entry (key merge), got %d", len(results))
	}

	m := results[0]
	if m.GenerationTokenRate == 0 {
		t.Errorf("GenerationTokenRate is zero — throughput key merge failed")
	}
	if m.KvUsageInstant == 0 {
		t.Errorf("KvUsageInstant is zero — throughput key merge failed")
	}
	if m.RequestRate == 0 {
		t.Errorf("RequestRate is zero — throughput key merge failed")
	}
	if m.KvCacheUsage == 0 {
		t.Errorf("KvCacheUsage is zero — KV cache result not merged")
	}
}

// TestCollectReplicaMetrics_ArrivalRatePerPodRetained is a regression guard for
// PR C (model-level TA demand): that PR adds a model-level arrival-rate query for
// the throughput analyzer but must not touch the existing per-pod
// scheduler_dispatch_rate collection — queueingmodel and internal/utils/allocation
// still read ReplicaMetrics.ArrivalRate per-pod. This pins that the per-pod value
// keeps flowing through CollectReplicaMetrics unchanged.
func TestCollectReplicaMetrics_ArrivalRatePerPodRetained(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	// KV-cache side: keyed by pod+instance (buildInstanceKey derives "pod-known:8000").
	kvLabels := map[string]string{
		"pod":                               "pod-known",
		"instance":                          "10.0.0.1:8000",
		constants.VariantLabelPrometheusKey: "va-1",
	}
	// Scheduler side: keyed by pod_name+port directly ("pod-known:8000" — same key).
	schedulerLabels := map[string]string{
		"pod_name": "pod-known",
		"port":     "8000",
	}
	ts := time.Now()

	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {
					Values: []source.MetricValue{{Labels: kvLabels, Value: 0.5, Timestamp: ts}},
				},
				"scheduler_dispatch_rate": {
					Values: []source.MetricValue{{Labels: schedulerLabels, Value: 3.5, Timestamp: ts}},
				},
			}, nil
		},
	}

	collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil)
	results, err := collector.CollectReplicaMetrics(
		context.Background(),
		"test-model",
		"test-ns",
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
		make(map[string]float64),
	)
	if err != nil {
		t.Fatalf("CollectReplicaMetrics: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 ReplicaMetrics entry (key merge), got %d", len(results))
	}
	if results[0].ArrivalRate != 3.5 {
		t.Errorf("ArrivalRate is %v, want 3.5 — per-pod scheduler_dispatch_rate merge must still populate it "+
			"(queueingmodel and internal/utils/allocation depend on this field)", results[0].ArrivalRate)
	}
}

// TestCollectReplicaMetrics_Freshness verifies that the per-replica
// ReplicaMetricsMetadata.FreshnessStatus and Age are derived from the actual
// metric scrape timestamps rather than hardcoded to "fresh"/0: a pod whose
// driving metrics are old enough to cross the stale threshold is reported
// "stale" with a non-zero Age; a pod with fresh timestamps is reported "fresh".
func TestCollectReplicaMetrics_Freshness(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	// fixture returns a source that reports all nine tracked metrics at timestamp
	// ts. Any query names passed in omit are dropped from the result set, leaving
	// their per-replica timestamps zero — used to model metrics that are absent by
	// design (e.g. scheduler_dispatch_rate when no EPP is deployed).
	fixture := func(ts time.Time, omit ...string) *mockMetricsSource {
		// scheduler_dispatch_rate keys its instance by pod_name(or pod)+port (a
		// dedicated "port" label, not the "instance" label's embedded port used by
		// buildInstanceKey) — include "port" so it resolves to the same instance
		// key ("pod-abc:8000") as every other query below.
		podLabels := map[string]string{
			"pod":                               "pod-abc",
			"instance":                          "10.0.0.1:8000",
			"port":                              "8000",
			constants.VariantLabelPrometheusKey: "va-1",
		}
		// cache_config_info populates cacheConfigTimestamp only when both label
		// values parse to positive integers (see the collector's cache-config block).
		cacheConfigLabels := map[string]string{
			"pod":                               "pod-abc",
			"instance":                          "10.0.0.1:8000",
			constants.VariantLabelPrometheusKey: "va-1",
			"num_gpu_blocks":                    "1000",
			"block_size":                        "16",
		}
		return &mockMetricsSource{
			refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
				results := map[string]*source.MetricResult{
					"kv_cache_usage":          {Values: []source.MetricValue{{Labels: podLabels, Value: 0.55, Timestamp: ts}}},
					"queue_length":            {Values: []source.MetricValue{{Labels: podLabels, Value: 2, Timestamp: ts}}},
					"avg_output_tokens":       {Values: []source.MetricValue{{Labels: podLabels, Value: 100, Timestamp: ts}}},
					"avg_input_tokens":        {Values: []source.MetricValue{{Labels: podLabels, Value: 50, Timestamp: ts}}},
					"prefix_cache_hit_rate":   {Values: []source.MetricValue{{Labels: podLabels, Value: 0.1, Timestamp: ts}}},
					"cache_config_info":       {Values: []source.MetricValue{{Labels: cacheConfigLabels, Value: 1, Timestamp: ts}}},
					"scheduler_dispatch_rate": {Values: []source.MetricValue{{Labels: podLabels, Value: 5.0, Timestamp: ts}}},
					"avg_ttft":                {Values: []source.MetricValue{{Labels: podLabels, Value: 0.2, Timestamp: ts}}},
					"avg_itl":                 {Values: []source.MetricValue{{Labels: podLabels, Value: 0.04, Timestamp: ts}}},
				}
				for _, k := range omit {
					delete(results, k)
				}
				return results, nil
			},
		}
	}

	collect := func(src *mockMetricsSource) []domain.ReplicaMetrics {
		t.Helper()
		collector := NewReplicaMetricsCollector(src, k8sClient, nil, nil)
		results, err := collector.CollectReplicaMetrics(
			context.Background(), "test-model", "test-ns",
			make(map[string]scaletarget.ScaleTargetAccessor),
			make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
			nil, make(map[string]float64),
		)
		if err != nil {
			t.Fatalf("CollectReplicaMetrics: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("expected exactly 1 ReplicaMetrics entry, got %d", len(results))
		}
		return results
	}

	const (
		statusFresh = "fresh"
		statusStale = "stale"
	)

	t.Run("stale", func(t *testing.T) {
		staleTs := time.Now().Add(-3 * time.Minute) // within [StaleThreshold, UnavailableThreshold)
		m := collect(fixture(staleTs))[0]
		if m.Metadata == nil || m.Metadata.FreshnessStatus != statusStale {
			t.Fatalf("expected FreshnessStatus=stale, got %+v", m.Metadata)
		}
		if m.Metadata.Age <= 0 {
			t.Errorf("expected non-zero Age for stale metrics, got %v", m.Metadata.Age)
		}
	})

	t.Run("fresh", func(t *testing.T) {
		m := collect(fixture(time.Now()))[0]
		if m.Metadata == nil || m.Metadata.FreshnessStatus != statusFresh {
			t.Fatalf("expected FreshnessStatus=fresh, got %+v", m.Metadata)
		}
	})

	// A metric that is absent by design (here scheduler_dispatch_rate, unscraped
	// when no EPP is deployed) leaves its timestamp zero. That must not make the
	// replica "missing": the worst status is taken over present timestamps only.
	t.Run("fresh when an absent-by-design metric leaves a zero timestamp", func(t *testing.T) {
		m := collect(fixture(time.Now(), "scheduler_dispatch_rate"))[0]
		if m.Metadata == nil || m.Metadata.FreshnessStatus != statusFresh {
			t.Fatalf("expected FreshnessStatus=fresh with an absent metric, got %+v", m.Metadata)
		}
	})

	// Because "missing" outranks "stale" in the severity order, an absent metric
	// must not mask a genuinely stale driving metric — otherwise the rollup would
	// report "missing" and the CheckModelMetrics stale-metrics gate (which keys on
	// == "stale") would never fire.
	t.Run("stale driving metric is not masked by an absent-by-design metric", func(t *testing.T) {
		staleTs := time.Now().Add(-3 * time.Minute)
		m := collect(fixture(staleTs, "scheduler_dispatch_rate"))[0]
		if m.Metadata == nil || m.Metadata.FreshnessStatus != statusStale {
			t.Fatalf("expected FreshnessStatus=stale despite an absent metric, got %+v", m.Metadata)
		}
	})
}

// TestCollectReplicaMetrics_SGLangCacheConfig verifies the SGLang cache-config
// pass: SGLang exposes total KV-cache token capacity directly via
// sglang:max_total_num_tokens (queried as sglang/cache_config_info), and the
// collector must surface that value as ReplicaMetrics.TotalKvCapacityTokens —
// unlike vLLM, which derives capacity from num_gpu_blocks × block_size.
func TestCollectReplicaMetrics_SGLangCacheConfig(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	podLabels := map[string]string{
		"pod":                               "sglang-pod-0",
		"instance":                          "10.0.0.3:8000",
		constants.VariantLabelPrometheusKey: sglangVariantLabel,
	}
	ts := time.Now()
	const capacity = 100000.0

	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			// Physical SGLang keys. The KV query makes the pod discoverable;
			// cache_config_info carries the directly-exposed token capacity.
			return map[string]*source.MetricResult{
				sglangKVUsageKey: {
					Values: []source.MetricValue{{Labels: podLabels, Value: 0.85, Timestamp: ts}},
				},
				"sglang/cache_config_info": {
					Values: []source.MetricValue{{Labels: podLabels, Value: capacity, Timestamp: ts}},
				},
			}, nil
		},
	}

	// An SGLang scale target so inferenceengine.Present detects SGLang and the
	// collector runs the SGLang cache-config pass.
	sglangTarget := scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: testServerContainer, Image: "lmsysorg/sglang:latest"}},
				},
			},
		},
	})

	collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil)
	results, err := collector.CollectReplicaMetrics(
		context.Background(),
		"test-model",
		"test-ns",
		map[string]scaletarget.ScaleTargetAccessor{sglangScaleTargetKey: sglangTarget},
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
		make(map[string]float64),
	)
	if err != nil {
		t.Fatalf("CollectReplicaMetrics: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 ReplicaMetrics entry, got %d", len(results))
	}
	if got := results[0].TotalKvCapacityTokens; got != int64(capacity) {
		t.Errorf("TotalKvCapacityTokens = %d, want %d (from sglang:max_total_num_tokens)", got, int64(capacity))
	}
}

// TestCollectReplicaMetrics_SGLangPrefixCacheHitRate verifies the SGLang
// prefix-cache-hit-rate value flows end-to-end (through mergeEngineResults
// re-keying) into ReplicaMetrics.PrefixCacheHitRate, and that a NaN ratio
// (0/0 when prompt_tokens_total has not increased) is dropped to 0 rather than
// poisoning the field.
func TestCollectReplicaMetrics_SGLangPrefixCacheHitRate(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}
	scheme := runtime.NewScheme()
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	podLabels := map[string]string{
		"pod":                               "sglang-pod-0",
		"instance":                          "10.0.0.4:8000",
		constants.VariantLabelPrometheusKey: sglangVariantLabel,
	}
	sglangTarget := scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: testServerContainer, Image: "lmsysorg/sglang:latest"}},
				},
			},
		},
	})

	cases := []struct {
		name string
		hit  float64
		want float64
	}{
		{"valid ratio flows into PrefixCacheHitRate", 0.3, 0.3},
		{"NaN ratio (0/0) is dropped to 0", math.NaN(), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			ts := time.Now()
			mockSource := &mockMetricsSource{
				refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
					return map[string]*source.MetricResult{
						// KV query makes the pod discoverable.
						sglangKVUsageKey: {
							Values: []source.MetricValue{{Labels: podLabels, Value: 0.85, Timestamp: ts}},
						},
						"sglang/prefix_cache_hit_rate": {
							Values: []source.MetricValue{{Labels: podLabels, Value: tc.hit, Timestamp: ts}},
						},
					}, nil
				},
			}
			collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil)
			results, err := collector.CollectReplicaMetrics(
				context.Background(), "test-model", "test-ns",
				map[string]scaletarget.ScaleTargetAccessor{sglangScaleTargetKey: sglangTarget},
				make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
				nil, make(map[string]float64),
			)
			if err != nil {
				t.Fatalf("CollectReplicaMetrics: %v", err)
			}
			if len(results) != 1 {
				t.Fatalf("expected 1 ReplicaMetrics entry, got %d", len(results))
			}
			if got := results[0].PrefixCacheHitRate; got != tc.want {
				t.Errorf("PrefixCacheHitRate = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCollectReplicaMetrics_MixedEngine drives the mixed vLLM+SGLang collection
// path end-to-end: both engines' series are merged under the logical names, and
// each pod must get its capacity from its own engine's source — the SGLang
// cache-config pass (capacity = sglang:max_total_num_tokens) must not corrupt the
// vLLM pod's num_gpu_blocks × block_size capacity, and vice versa.
func TestCollectReplicaMetrics_MixedEngine(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}
	scheme := runtime.NewScheme()
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	ts := time.Now()

	vllmPod := map[string]string{"pod": mixedVLLMPodName, "instance": "10.0.1.1:8000", constants.VariantLabelPrometheusKey: "va-vllm"}
	sglangPod := map[string]string{"pod": "sglang-0", "instance": "10.0.2.1:8000", constants.VariantLabelPrometheusKey: sglangVariantLabel}
	vllmCacheLabels := map[string]string{"pod": mixedVLLMPodName, "instance": "10.0.1.1:8000", "num_gpu_blocks": "1000", "block_size": "16"}

	const sglangCapacity = 100000.0

	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				// vLLM uses the bare logical keys.
				"kv_cache_usage":    {Values: []source.MetricValue{{Labels: vllmPod, Value: 0.5, Timestamp: ts}}},
				"cache_config_info": {Values: []source.MetricValue{{Labels: vllmCacheLabels, Value: 1, Timestamp: ts}}},
				// SGLang uses the engine-scoped physical keys.
				sglangKVUsageKey:           {Values: []source.MetricValue{{Labels: sglangPod, Value: 0.85, Timestamp: ts}}},
				"sglang/cache_config_info": {Values: []source.MetricValue{{Labels: sglangPod, Value: sglangCapacity, Timestamp: ts}}},
			}, nil
		},
	}

	target := func(image string) scaletarget.ScaleTargetAccessor {
		return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: testServerContainer, Image: image}}},
				},
			},
		})
	}

	collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil)
	results, err := collector.CollectReplicaMetrics(
		context.Background(), "test-model", "test-ns",
		map[string]scaletarget.ScaleTargetAccessor{
			"test-ns/vllm-decode": target("vllm/vllm-openai:latest"),
			sglangScaleTargetKey:  target("lmsysorg/sglang:latest"),
		},
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil, make(map[string]float64),
	)
	if err != nil {
		t.Fatalf("CollectReplicaMetrics: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 ReplicaMetrics entries (one per engine), got %d", len(results))
	}

	byPod := make(map[string]int64, 2)
	for _, m := range results {
		byPod[m.PodName] = m.TotalKvCapacityTokens
	}
	if got := byPod[mixedVLLMPodName]; got != 1000*16 {
		t.Errorf("vLLM pod TotalKvCapacityTokens = %d, want %d (num_gpu_blocks × block_size)", got, 1000*16)
	}
	if got := byPod["sglang-0"]; got != int64(sglangCapacity) {
		t.Errorf("SGLang pod TotalKvCapacityTokens = %d, want %d (sglang:max_total_num_tokens)", got, int64(sglangCapacity))
	}
}

// mockScaleTargetAccessor implements scaletarget.ScaleTargetAccessor for testing.
// Only GetStatusReadyReplicas is meaningful; all other methods return zero/nil.
type mockScaleTargetAccessor struct {
	readyReplicas int32
}

func (m *mockScaleTargetAccessor) GetName() string                                   { return "" }
func (m *mockScaleTargetAccessor) GetNamespace() string                              { return "" }
func (m *mockScaleTargetAccessor) GetReplicas() *int32                               { return nil }
func (m *mockScaleTargetAccessor) GetDeletionTimestamp() *metav1.Time                { return nil }
func (m *mockScaleTargetAccessor) GetStatusReplicas() int32                          { return 0 }
func (m *mockScaleTargetAccessor) GetStatusReadyReplicas() int32                     { return m.readyReplicas }
func (m *mockScaleTargetAccessor) GetTotalGPUsPerReplica() int                       { return 0 }
func (m *mockScaleTargetAccessor) GetLeaderPodTemplateSpec() *corev1.PodTemplateSpec { return nil }
func (m *mockScaleTargetAccessor) GetWorkerPodTemplateSpec() *corev1.PodTemplateSpec { return nil }
func (m *mockScaleTargetAccessor) GetGroupSize() int32                               { return 1 }

// TestCollectReplicaMetrics_UnattributedReadyPodsEvent verifies that when a VA
// has Ready pods but none are attributed this cycle, a Warning/UnattributedReadyPods
// K8s event is emitted exactly once (deduped via vaEventTracker on second call).
func TestCollectReplicaMetrics_UnattributedReadyPodsEvent(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))

	scheme := runtime.NewScheme()
	require.NoError(t, llmdVariantAutoscalingV1alpha1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	fakeRecorder := record.NewFakeRecorder(10)

	// One pod attributed to "va-other", not to "va-target".
	podLabels := map[string]string{
		"pod":                               "pod-other",
		"instance":                          "10.0.0.2:8000",
		constants.VariantLabelPrometheusKey: "va-other",
	}
	ts := time.Now()
	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {
					Values: []source.MetricValue{{Labels: podLabels, Value: 0.5, Timestamp: ts}},
				},
			}, nil
		},
	}

	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: "va-target", Namespace: "default"},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "dep-target"},
			ModelID:        "test-model",
			MaxReplicas:    5,
		},
	}
	variantAutoscalings := map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		"default/va-target": va,
	}
	scaleTargets := map[string]scaletarget.ScaleTargetAccessor{
		"default/dep-target": &mockScaleTargetAccessor{readyReplicas: 2},
	}
	variantCosts := make(map[string]float64)

	collector := NewReplicaMetricsCollector(mockSource, k8sClient, fakeRecorder, nil)

	// First call: metrics present for a different VA → va-target has 0 attributed but 2 ready.
	vaEventTracker := make(map[string]bool)
	results, err := collector.CollectReplicaMetrics(
		context.Background(), "test-model", "default",
		scaleTargets, variantAutoscalings, vaEventTracker, variantCosts,
	)
	require.NoError(t, err)
	assert.NotEmpty(t, results, "expected attributed results for va-other")

	select {
	case event := <-fakeRecorder.Events:
		assert.Contains(t, event, constants.K8SEventUnattributedReadyPods)
		assert.Contains(t, event, "va-target")
	default:
		t.Error("expected UnattributedReadyPods event but none received")
	}

	// Second call with same vaEventTracker: event must NOT be re-emitted (deduped).
	_, err = collector.CollectReplicaMetrics(
		context.Background(), "test-model", "default",
		scaleTargets, variantAutoscalings, vaEventTracker, variantCosts,
	)
	require.NoError(t, err)
	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("unexpected duplicate event: %s", event)
	default:
		// Expected: no second event
	}
}

// TestCollectReplicaMetrics_ThroughputOrphanSkipped verifies that a throughput
// query result for an instance that has no KV-cache entry (scrape skew or
// throughput-only pod) does not create an orphan podData entry and does not
// appear in the assembled ReplicaMetrics slice.
func TestCollectReplicaMetrics_ThroughputOrphanSkipped(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))

	scheme := runtime.NewScheme()
	require.NoError(t, llmdVariantAutoscalingV1alpha1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	// KV-cache: only pod-known at 10.0.0.1:8000.
	kvLabels := map[string]string{
		"pod":                               "pod-known",
		"instance":                          "10.0.0.1:8000",
		constants.VariantLabelPrometheusKey: "va-1",
	}
	// Throughput: pod-orphan at 10.0.0.2:8000 — NOT in the KV query results.
	orphanLabels := map[string]string{
		"pod":                               "pod-orphan",
		"instance":                          "10.0.0.2:8000",
		constants.VariantLabelPrometheusKey: "va-1",
	}
	ts := time.Now()

	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {
					Values: []source.MetricValue{{Labels: kvLabels, Value: 0.5, Timestamp: ts}},
				},
				"generation_token_rate": {
					Values: []source.MetricValue{{Labels: orphanLabels, Value: 1000.0, Timestamp: ts}},
				},
			}, nil
		},
	}

	collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil)
	results, err := collector.CollectReplicaMetrics(
		context.Background(), "test-model", "test-ns",
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
		make(map[string]float64),
	)
	require.NoError(t, err)

	// Only pod-known should be present; pod-orphan must be skipped.
	require.Len(t, results, 1, "orphan throughput-only pod must not produce a ReplicaMetrics entry")
	assert.Equal(t, "pod-known", results[0].PodName)
	assert.Equal(t, float64(0), results[0].GenerationTokenRate, "orphan entry must not contaminate pod-known")
}

func TestIsLWSWorker(t *testing.T) {
	tests := []struct {
		name       string
		podLabels  map[string]string
		want       bool
		nilLocator bool
	}{
		{
			name: "LWS leader pod (worker-index=0)",
			podLabels: map[string]string{
				"app":                     "test",
				lwsv1.WorkerIndexLabelKey: "0",
			},
			want: false, // leaders are not workers
		},
		{
			name: "LWS worker pod (worker-index=1)",
			podLabels: map[string]string{
				"app":                     "test",
				lwsv1.WorkerIndexLabelKey: "1",
			},
			want: true,
		},
		{
			name: "LWS worker pod (worker-index=2)",
			podLabels: map[string]string{
				"app":                     "test",
				lwsv1.WorkerIndexLabelKey: "2",
			},
			want: true,
		},
		{
			name: "non-LWS pod (no worker-index label)",
			podLabels: map[string]string{
				"app": "test",
			},
			want: false,
		},
		{
			name:      "pod with no labels",
			podLabels: map[string]string{},
			want:      false,
		},
		{
			name:       "nil locator",
			podLabels:  nil,
			nilLocator: true,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mockLoc *mockLocator
			if !tt.nilLocator {
				mockLoc = &mockLocator{
					getPodLabelsFunc: func(ctx context.Context, namespace, podName string) map[string]string {
						return tt.podLabels
					},
				}
			}

			collector := NewReplicaMetricsCollector(nil, nil, nil, mockLoc)
			got := collector.isLWSWorker(context.Background(), "test-ns", "test-pod")

			if got != tt.want {
				t.Errorf("isLWSWorker() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsLWSWorker_EmptyPodName(t *testing.T) {
	mockLoc := &mockLocator{
		getPodLabelsFunc: func(ctx context.Context, namespace, podName string) map[string]string {
			t.Error("GetPodLabels should not be called for empty pod name")
			return nil
		},
	}

	collector := NewReplicaMetricsCollector(nil, nil, nil, mockLoc)
	got := collector.isLWSWorker(context.Background(), "test-ns", "")

	if got != false {
		t.Errorf("isLWSWorker() with empty podName = %v, want false", got)
	}
}

// TestIsLWSWorker_MetricEmissionPatterns tests the isLWSWorker filter logic
// in isolation across various real-world metric emission patterns:
// - leader-only emits (ideal case)
// - all-pods-emit (workers incorrectly emit metrics)
// - leader-missing-emit (resilience when leader doesn't emit)
// - mixed scenarios across multiple replicas
func TestIsLWSWorker_MetricEmissionPatterns(t *testing.T) {
	tests := []struct {
		name           string
		metricPods     []string // pods that emit metrics
		podLabels      map[string]map[string]string
		expectedFilter []string // pods that should pass the filter (not workers)
	}{
		{
			name:       "leader-only emits (ideal case)",
			metricPods: []string{"leader-0", "leader-1"},
			podLabels: map[string]map[string]string{
				"leader-0": {lwsv1.WorkerIndexLabelKey: "0"},
				"leader-1": {lwsv1.WorkerIndexLabelKey: "0"},
			},
			expectedFilter: []string{"leader-0", "leader-1"},
		},
		{
			name: "all-pods-emit (workers incorrectly emit)",
			metricPods: []string{
				"leader-0", "worker-0-1", "worker-0-2",
				"leader-1", "worker-1-1", "worker-1-2",
			},
			podLabels: map[string]map[string]string{
				"leader-0":   {lwsv1.WorkerIndexLabelKey: "0"},
				"worker-0-1": {lwsv1.WorkerIndexLabelKey: "1"},
				"worker-0-2": {lwsv1.WorkerIndexLabelKey: "2"},
				"leader-1":   {lwsv1.WorkerIndexLabelKey: "0"},
				"worker-1-1": {lwsv1.WorkerIndexLabelKey: "1"},
				"worker-1-2": {lwsv1.WorkerIndexLabelKey: "2"},
			},
			expectedFilter: []string{"leader-0", "leader-1"},
		},
		{
			name: "leader-missing-emit (resilience)",
			metricPods: []string{
				// leader-0 is missing (crashed, not emitting yet, etc.)
				"worker-0-1", "worker-0-2",
				"leader-1", "worker-1-1", "worker-1-2",
			},
			podLabels: map[string]map[string]string{
				"worker-0-1": {lwsv1.WorkerIndexLabelKey: "1"},
				"worker-0-2": {lwsv1.WorkerIndexLabelKey: "2"},
				"leader-1":   {lwsv1.WorkerIndexLabelKey: "0"},
				"worker-1-1": {lwsv1.WorkerIndexLabelKey: "1"},
				"worker-1-2": {lwsv1.WorkerIndexLabelKey: "2"},
			},
			// Workers filtered out even though leader-0 is missing
			expectedFilter: []string{"leader-1"},
		},
		{
			name: "mixed: some replicas all-emit, some leader-only",
			metricPods: []string{
				// Replica 0: all pods emit
				"leader-0", "worker-0-1", "worker-0-2",
				// Replica 1: leader only
				"leader-1",
				// Replica 2: leader missing
				"worker-2-1", "worker-2-2",
			},
			podLabels: map[string]map[string]string{
				"leader-0":   {lwsv1.WorkerIndexLabelKey: "0"},
				"worker-0-1": {lwsv1.WorkerIndexLabelKey: "1"},
				"worker-0-2": {lwsv1.WorkerIndexLabelKey: "2"},
				"leader-1":   {lwsv1.WorkerIndexLabelKey: "0"},
				"worker-2-1": {lwsv1.WorkerIndexLabelKey: "1"},
				"worker-2-2": {lwsv1.WorkerIndexLabelKey: "2"},
			},
			expectedFilter: []string{"leader-0", "leader-1"},
		},
		{
			name: "non-LWS pods mixed with LWS (no worker-index label)",
			metricPods: []string{
				"leader-0", "worker-0-1",
				"regular-pod-1", "regular-pod-2", // non-LWS pods
			},
			podLabels: map[string]map[string]string{
				"leader-0":      {lwsv1.WorkerIndexLabelKey: "0"},
				"worker-0-1":    {lwsv1.WorkerIndexLabelKey: "1"},
				"regular-pod-1": {}, // no worker-index label
				"regular-pod-2": {}, // no worker-index label
			},
			// Non-LWS pods pass through (not workers), LWS workers filtered
			expectedFilter: []string{"leader-0", "regular-pod-1", "regular-pod-2"},
		},
		{
			name:       "single leader, no workers (size=1 LWS)",
			metricPods: []string{"leader-0", "leader-1"},
			podLabels: map[string]map[string]string{
				"leader-0": {lwsv1.WorkerIndexLabelKey: "0"},
				"leader-1": {lwsv1.WorkerIndexLabelKey: "0"},
			},
			expectedFilter: []string{"leader-0", "leader-1"},
		},
		{
			name: "Deployment pods (no worker-index label) - unchanged behavior",
			metricPods: []string{
				"deploy-pod-abc123", "deploy-pod-def456", "deploy-pod-ghi789",
			},
			podLabels: map[string]map[string]string{
				"deploy-pod-abc123": {"app": "my-app", "version": "v1"},
				"deploy-pod-def456": {"app": "my-app", "version": "v1"},
				"deploy-pod-ghi789": {"app": "my-app", "version": "v1"},
				// No worker-index label - regular Deployment pods
			},
			// All Deployment pods pass through unchanged
			expectedFilter: []string{"deploy-pod-abc123", "deploy-pod-def456", "deploy-pod-ghi789"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockLoc := &mockLocator{
				getPodLabelsFunc: func(ctx context.Context, namespace, podName string) map[string]string {
					return tt.podLabels[podName]
				},
			}

			collector := NewReplicaMetricsCollector(nil, nil, nil, mockLoc)

			// Simulate filtering: for each pod that emits metrics,
			// check if it would be filtered out by isLWSWorker
			var filtered []string
			for _, podName := range tt.metricPods {
				if !collector.isLWSWorker(context.Background(), "test-ns", podName) {
					filtered = append(filtered, podName)
				}
			}

			// Verify the filtered result matches expectations
			if len(filtered) != len(tt.expectedFilter) {
				t.Errorf("filtered count mismatch: got %d, want %d\nGot: %v\nWant: %v",
					len(filtered), len(tt.expectedFilter), filtered, tt.expectedFilter)
				return
			}

			// Check each expected pod is in the filtered list
			filteredSet := make(map[string]bool)
			for _, pod := range filtered {
				filteredSet[pod] = true
			}

			for _, expectedPod := range tt.expectedFilter {
				if !filteredSet[expectedPod] {
					t.Errorf("expected pod %q to pass filter, but it was filtered out", expectedPod)
				}
			}

			// Check no unexpected pods in filtered list
			expectedSet := make(map[string]bool)
			for _, pod := range tt.expectedFilter {
				expectedSet[pod] = true
			}

			for _, pod := range filtered {
				if !expectedSet[pod] {
					t.Errorf("unexpected pod %q in filtered list", pod)
				}
			}
		})
	}
}

// TestCollectReplicaMetrics_LWSWorkerPodsFiltered verifies that when collecting
// metrics from an LWS with replicas=2, size=3 (6 total pods: 2 leaders + 4 workers),
// only the leader pods contribute to ReplicaCount. Worker pods (worker-index != "0")
// are filtered out even though they emit metrics.
//
// This is a regression test for the LWS worker pod filtering feature: LWS worker
// pods should not be counted in ReplicaMetrics because each LWS leader already
// represents the full replica (leader + workers) and emits aggregate metrics.
func TestCollectReplicaMetrics_LWSWorkerPodsFiltered(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}

	ts := time.Now()
	vaName := "lws-variant"
	namespace := "test-ns"

	// Create 6 pods total: 2 LWS replicas with size=3 (1 leader + 2 workers each)
	// Replica 0: leader-0, worker-0-1, worker-0-2
	// Replica 1: leader-1, worker-1-1, worker-1-2
	pods := []runtime.Object{
		// Replica 0 - leader
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "leader-0",
				Namespace: namespace,
				Labels: map[string]string{
					"app":                     "lws-app",
					constants.VariantLabelKey: vaName,
					lwsv1.WorkerIndexLabelKey: "0", // leader
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.0.0.1",
			},
		},
		// Replica 0 - worker 1
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "worker-0-1",
				Namespace: namespace,
				Labels: map[string]string{
					"app":                     "lws-app",
					constants.VariantLabelKey: vaName,
					lwsv1.WorkerIndexLabelKey: "1", // worker
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.0.0.2",
			},
		},
		// Replica 0 - worker 2
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "worker-0-2",
				Namespace: namespace,
				Labels: map[string]string{
					"app":                     "lws-app",
					constants.VariantLabelKey: vaName,
					lwsv1.WorkerIndexLabelKey: "2", // worker
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.0.0.3",
			},
		},
		// Replica 1 - leader
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "leader-1",
				Namespace: namespace,
				Labels: map[string]string{
					"app":                     "lws-app",
					constants.VariantLabelKey: vaName,
					lwsv1.WorkerIndexLabelKey: "0", // leader
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.0.1.1",
			},
		},
		// Replica 1 - worker 1
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "worker-1-1",
				Namespace: namespace,
				Labels: map[string]string{
					"app":                     "lws-app",
					constants.VariantLabelKey: vaName,
					lwsv1.WorkerIndexLabelKey: "1", // worker
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.0.1.2",
			},
		},
		// Replica 1 - worker 2
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "worker-1-2",
				Namespace: namespace,
				Labels: map[string]string{
					"app":                     "lws-app",
					constants.VariantLabelKey: vaName,
					lwsv1.WorkerIndexLabelKey: "2", // worker
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.0.1.3",
			},
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(pods...).Build()

	// Mock Prometheus source returns metrics for ALL 6 pods (leaders + workers)
	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {
					Values: []source.MetricValue{
						// Leader pods emit metrics
						{Labels: map[string]string{"pod": "leader-0", "instance": "10.0.0.1:8000", constants.VariantLabelPrometheusKey: vaName}, Value: 0.5, Timestamp: ts},
						{Labels: map[string]string{"pod": "leader-1", "instance": "10.0.1.1:8000", constants.VariantLabelPrometheusKey: vaName}, Value: 0.6, Timestamp: ts},
						// Worker pods also emit metrics (should be filtered out)
						{Labels: map[string]string{"pod": "worker-0-1", "instance": "10.0.0.2:8000", constants.VariantLabelPrometheusKey: vaName}, Value: 0.7, Timestamp: ts},
						{Labels: map[string]string{"pod": "worker-0-2", "instance": "10.0.0.3:8000", constants.VariantLabelPrometheusKey: vaName}, Value: 0.8, Timestamp: ts},
						{Labels: map[string]string{"pod": "worker-1-1", "instance": "10.0.1.2:8000", constants.VariantLabelPrometheusKey: vaName}, Value: 0.9, Timestamp: ts},
						{Labels: map[string]string{"pod": "worker-1-2", "instance": "10.0.1.3:8000", constants.VariantLabelPrometheusKey: vaName}, Value: 0.95, Timestamp: ts},
					},
				},
			}, nil
		},
	}

	// Mock locator that returns pod labels from the K8s client
	mockLoc := &mockLocator{
		getPodLabelsFunc: func(ctx context.Context, ns, podName string) map[string]string {
			pod := &corev1.Pod{}
			key := client.ObjectKey{Namespace: ns, Name: podName}
			if err := k8sClient.Get(ctx, key, pod); err != nil {
				return nil
			}
			return pod.Labels
		},
	}

	collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, mockLoc)

	results, err := collector.CollectReplicaMetrics(
		context.Background(),
		"test-model",
		namespace,
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
		make(map[string]float64),
	)
	if err != nil {
		t.Fatalf("CollectReplicaMetrics: %v", err)
	}

	// Assert: Only 2 ReplicaMetrics entries (one per leader pod), not 6
	if len(results) != 2 {
		t.Errorf("expected 2 ReplicaMetrics entries (leaders only), got %d", len(results))
		for i, r := range results {
			t.Logf("  [%d] PodName=%s, VariantName=%s, KvCacheUsage=%f", i, r.PodName, r.VariantName, r.KvCacheUsage)
		}
	}

	// Verify that only leader pods are in the results
	foundPods := make(map[string]bool)
	for _, r := range results {
		foundPods[r.PodName] = true
	}

	expectedLeaders := map[string]bool{"leader-0": true, "leader-1": true}
	for leader := range expectedLeaders {
		if !foundPods[leader] {
			t.Errorf("expected leader pod %q in results, but not found", leader)
		}
	}

	// Verify NO worker pods in results
	workers := []string{"worker-0-1", "worker-0-2", "worker-1-1", "worker-1-2"}
	for _, worker := range workers {
		if foundPods[worker] {
			t.Errorf("worker pod %q should be filtered out, but found in results", worker)
		}
	}

	// Verify that KvCacheUsage values are only from leader pods (0.5, 0.6),
	// NOT from worker pods (0.7, 0.8, 0.9, 0.95). This ensures metrics are
	// not double-counted across leader + workers of the same replica.
	leaderMetrics := map[string]float64{
		"leader-0": 0.5,
		"leader-1": 0.6,
	}
	workerMetrics := []float64{0.7, 0.8, 0.9, 0.95}

	for _, r := range results {
		expectedValue, isLeader := leaderMetrics[r.PodName]
		if !isLeader {
			t.Errorf("pod %q should not be in results (not a leader)", r.PodName)
			continue
		}

		if r.KvCacheUsage != expectedValue {
			t.Errorf("pod %q: expected KvCacheUsage=%f (leader value), got %f",
				r.PodName, expectedValue, r.KvCacheUsage)
		}

		// Verify this pod's metric doesn't match any worker metric value
		for _, workerValue := range workerMetrics {
			if r.KvCacheUsage == workerValue {
				t.Errorf("pod %q has KvCacheUsage=%f which matches worker pod metric - double counting detected!",
					r.PodName, workerValue)
			}
		}
	}
}

// TestCollectReplicaMetrics_DeploymentUnchanged verifies that the LWS worker
// filtering logic does NOT affect regular Deployment pods. In a Deployment with
// 3 replicas (3 pods, one per replica), all 3 pods should contribute to
// ReplicaMetrics with their metrics flowing through unchanged.
//
// This is a regression test ensuring that adding LWS worker filtering doesn't
// break the existing Deployment behavior: ReplicaCount should equal the
// Deployment's replica count and per-pod metrics should flow through unchanged.
func TestCollectReplicaMetrics_DeploymentUnchanged(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}

	ts := time.Now()
	vaName := "deployment-variant"
	namespace := "test-ns"

	// Create 3 regular Deployment pods (no worker-index label)
	pods := []runtime.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "deploy-pod-abc123",
				Namespace: namespace,
				Labels: map[string]string{
					"app":                     "my-app",
					constants.VariantLabelKey: vaName,
					"pod-template-hash":       "abc123",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.1.0.1",
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "deploy-pod-def456",
				Namespace: namespace,
				Labels: map[string]string{
					"app":                     "my-app",
					constants.VariantLabelKey: vaName,
					"pod-template-hash":       "def456",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.1.0.2",
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "deploy-pod-ghi789",
				Namespace: namespace,
				Labels: map[string]string{
					"app":                     "my-app",
					constants.VariantLabelKey: vaName,
					"pod-template-hash":       "ghi789",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.1.0.3",
			},
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(pods...).Build()

	// Mock Prometheus returns metrics for all 3 Deployment pods
	// Each pod has distinct metric values
	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {
					Values: []source.MetricValue{
						{Labels: map[string]string{"pod": "deploy-pod-abc123", "instance": "10.1.0.1:8000", constants.VariantLabelPrometheusKey: vaName}, Value: 0.4, Timestamp: ts},
						{Labels: map[string]string{"pod": "deploy-pod-def456", "instance": "10.1.0.2:8000", constants.VariantLabelPrometheusKey: vaName}, Value: 0.5, Timestamp: ts},
						{Labels: map[string]string{"pod": "deploy-pod-ghi789", "instance": "10.1.0.3:8000", constants.VariantLabelPrometheusKey: vaName}, Value: 0.6, Timestamp: ts},
					},
				},
			}, nil
		},
	}

	// Mock locator that returns pod labels from the K8s client
	mockLoc := &mockLocator{
		getPodLabelsFunc: func(ctx context.Context, ns, podName string) map[string]string {
			pod := &corev1.Pod{}
			key := client.ObjectKey{Namespace: ns, Name: podName}
			if err := k8sClient.Get(ctx, key, pod); err != nil {
				return nil
			}
			return pod.Labels
		},
	}

	collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, mockLoc)

	results, err := collector.CollectReplicaMetrics(
		context.Background(),
		"test-model",
		namespace,
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
		make(map[string]float64),
	)
	if err != nil {
		t.Fatalf("CollectReplicaMetrics: %v", err)
	}

	// Assert: Exactly 3 ReplicaMetrics entries (one per Deployment pod)
	// ReplicaCount equals Deployment's replica count
	if len(results) != 3 {
		t.Errorf("expected 3 ReplicaMetrics entries (Deployment replicas), got %d", len(results))
		for i, r := range results {
			t.Logf("  [%d] PodName=%s, VariantName=%s, KvCacheUsage=%f", i, r.PodName, r.VariantName, r.KvCacheUsage)
		}
	}

	// Verify that all Deployment pods are in the results
	foundPods := make(map[string]bool)
	for _, r := range results {
		foundPods[r.PodName] = true
	}

	expectedPods := []string{"deploy-pod-abc123", "deploy-pod-def456", "deploy-pod-ghi789"}
	for _, podName := range expectedPods {
		if !foundPods[podName] {
			t.Errorf("expected Deployment pod %q in results, but not found", podName)
		}
	}

	// Verify per-pod metrics flow through unchanged
	expectedMetrics := map[string]float64{
		"deploy-pod-abc123": 0.4,
		"deploy-pod-def456": 0.5,
		"deploy-pod-ghi789": 0.6,
	}

	for _, r := range results {
		expectedValue, ok := expectedMetrics[r.PodName]
		if !ok {
			t.Errorf("unexpected pod %q in results", r.PodName)
			continue
		}

		if r.KvCacheUsage != expectedValue {
			t.Errorf("pod %q: expected KvCacheUsage=%f (original value), got %f - metrics not flowing through unchanged!",
				r.PodName, expectedValue, r.KvCacheUsage)
		}
	}

	// Verify no pods are missing (all 3 should be present)
	if len(foundPods) != len(expectedPods) {
		t.Errorf("expected %d pods in results, got %d - Deployment behavior changed!",
			len(expectedPods), len(foundPods))
	}
}
