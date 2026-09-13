package registration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	promsource "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
)

// TestEPPMetricFallback evaluates the registered queries using promtool.
// Set PROMTOOL_IMAGE to run with Docker instead of a local promtool binary.
func TestEPPMetricFallback(t *testing.T) {
	image := os.Getenv("PROMTOOL_IMAGE")
	bin, err := exec.LookPath("promtool")
	if err != nil && image == "" {
		t.Skip("install promtool or set PROMTOOL_IMAGE to run PromQL compatibility tests")
	}
	registry := source.NewSourceRegistry()
	require.NoError(t, registry.Register("prometheus", promsource.NewPrometheusSource(context.Background(), &mockPrometheusAPI{}, promsource.DefaultPrometheusSourceConfig())))
	RegisterSaturationQueries(registry)
	RegisterQueueingModelQueries(registry)
	RegisterThroughputAnalyzerQueries(registry)
	queries := registry.Get("prometheus").QueryList()
	type sample struct {
		Labels string  `yaml:"labels"`
		Value  float64 `yaml:"value"`
	}
	type series struct {
		Series string `yaml:"series"`
		Values string `yaml:"values"`
	}
	for _, query := range []struct {
		name, metric, labels string
		counter              bool
	}{
		{QuerySchedulerQueueSize, "flow_control_queue_size", "{}", false},
		{QuerySchedulerQueueBytes, "flow_control_queue_bytes", "{}", false},
		{QuerySchedulerDispatchRate, "scheduler_attempts_total", `{namespace="ns",pod_name="backend",port="8000"}`, true},
		{QueryModelArrivalRate, "scheduler_attempts_total", `{namespace="ns"}`, true},
	} {
		for _, tc := range []struct {
			name                                 string
			current, legacy, mixed, zero, absent bool
			want                                 float64
		}{
			{name: "current only", current: true, want: 2},
			{name: "legacy only", legacy: true, want: 5},
			{name: "dual emission", current: true, legacy: true, want: 2},
			{name: "mixed sources", current: true, legacy: true, mixed: true, want: 7},
			{name: "current zero wins", current: true, legacy: true, zero: true, want: 0},
			{name: "missing metrics", absent: true},
		} {
			t.Run(query.name+"/"+tc.name, func(t *testing.T) {
				var inputs []series
				add := func(prefix, instance string, value int) {
					labels := `namespace="ns",instance="` + instance + `",target_model_name="model"`
					values := fmt.Sprintf("%d+0x4", value)
					if query.counter {
						endpointLabel := "pod_name"
						if prefix == "llm_d_epp_" {
							endpointLabel = "endpoint_name"
						}
						labels += `,status="success",port="8000",` + endpointLabel + `="backend"`
						values = fmt.Sprintf("0+%dx4", value*15)
					}
					inputs = append(inputs, series{prefix + query.metric + "{" + labels + "}", values})
				}
				if tc.current {
					value := 2
					if tc.zero {
						value = 0
					}
					add("llm_d_epp_", "epp-a", value)
				}
				if tc.legacy {
					add("inference_extension_", "epp-a", 5)
				}
				if tc.mixed {
					add("inference_extension_", "epp-b", 5)
				}
				expr := strings.NewReplacer("{{.namespace}}", "ns", "{{.modelID}}", "model").Replace(queries.Get(query.name).Template)
				expected := []sample{}
				if !tc.absent {
					expected = append(expected, sample{query.labels, tc.want})
				}
				doc := map[string]any{
					"fuzzy_compare":       true,
					"evaluation_interval": "15s",
					"tests": []any{map[string]any{
						"interval": "15s", "input_series": inputs,
						"promql_expr_test": []any{map[string]any{"expr": expr, "eval_time": "1m", "exp_samples": expected}},
					}},
				}
				if !query.counter {
					fallbackInputs := make([]series, len(inputs))
					for i, input := range inputs {
						fallbackInputs[i] = series{strings.ReplaceAll(input.Series, `target_model_name="model"`, `target_model_name="",model_name="model"`), input.Values}
					}
					doc["tests"] = append(doc["tests"].([]any), map[string]any{
						"interval": "15s", "input_series": fallbackInputs,
						"promql_expr_test": []any{map[string]any{"expr": expr, "eval_time": "1m", "exp_samples": expected}},
					})
				}
				data, err := yaml.Marshal(doc)
				require.NoError(t, err)
				dir := t.TempDir()
				path := filepath.Join(dir, "rules.test.yaml")
				require.NoError(t, os.WriteFile(path, data, 0o600))
				cmd := exec.CommandContext(t.Context(), bin, "test", "rules", path)
				if image != "" {
					cmd = exec.CommandContext(t.Context(), "docker", "run", "--rm", "--user", "0", "-v", dir+":/tests:ro", "--entrypoint", "/bin/promtool", image, "test", "rules", "/tests/rules.test.yaml")
				}
				output, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", output)
			})
		}
	}
}
