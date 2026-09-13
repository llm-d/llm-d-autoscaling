package scalefromzero

import (
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
)

func TestPendingQueueMetric(t *testing.T) {
	metric := func(name, instance string, value float64) source.MetricValue {
		return source.MetricValue{Labels: map[string]string{"__name__": name, "instance": instance, "target_model_name": "model"}, Value: value}
	}
	current := metric(targetEPPMetricName, "a", 2)
	zero := metric(targetEPPMetricName, "a", 0)
	legacy := metric(legacyEPPMetricName, "a", 5)
	for _, tc := range []struct {
		name   string
		values []source.MetricValue
		want   bool
		value  float64
	}{
		{"current", []source.MetricValue{current}, true, 2},
		{"legacy", []source.MetricValue{legacy}, true, 5},
		{"dual", []source.MetricValue{legacy, current}, true, 2},
		{"dual reversed", []source.MetricValue{current, legacy}, true, 2},
		{"zero wins", []source.MetricValue{legacy, zero}, false, 0},
		{"mixed sources", []source.MetricValue{zero, legacy, metric(legacyEPPMetricName, "b", 5)}, true, 5},
		{"absent", nil, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, found := pendingQueueMetric(tc.values, "model")
			if found != tc.want || value.Value != tc.value {
				t.Fatalf("got (%v, %v), want (%v, %v)", found, value.Value, tc.want, tc.value)
			}
			if _, found := pendingQueueMetric(tc.values, "other-model"); found {
				t.Fatal("matched another model")
			}
		})
	}
}
