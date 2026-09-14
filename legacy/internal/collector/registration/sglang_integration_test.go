package registration

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// This integration test drives the real PrometheusSource end to end through the
// registered SGLang query templates: it builds each engine's KV-usage query from
// its template + params, executes it against a mock Prometheus that only answers
// the engine-appropriate metric name, and asserts the value flows back. It proves
// the SGLang templates are registered, parameter-substituted, and queryable —
// and that the vLLM path is unchanged.
var _ = Describe("SGLang query path (integration)", func() {
	var registry *source.SourceRegistry

	// vector returns a single-sample PromQL vector result with the given value.
	vector := func(val float64) model.Value {
		return model.Vector{&model.Sample{
			Metric:    model.Metric{"instance": "10.0.0.1:8000", "pod": "server-0", "model_name": "m"},
			Value:     model.SampleValue(val),
			Timestamp: model.Now(),
		}}
	}

	// newSource wires a PrometheusSource whose mock answers `vllm:kv_cache_usage_perc`
	// with 0.50 and `sglang:token_usage` with 0.85, and everything else empty.
	newSource := func() {
		ctx := context.Background()
		registry = source.NewSourceRegistry()
		mock := &mockPrometheusAPI{
			queryFunc: func(_ context.Context, query string, _ time.Time, _ ...v1.Option) (model.Value, v1.Warnings, error) {
				switch {
				case strings.Contains(query, "vllm:kv_cache_usage_perc"):
					return vector(0.50), nil, nil
				case strings.Contains(query, "sglang:token_usage"):
					return vector(0.85), nil, nil
				default:
					return model.Vector{}, nil, nil
				}
			},
		}
		Expect(registry.Register("prometheus", prometheus.NewPrometheusSource(ctx, mock, prometheus.DefaultPrometheusSourceConfig()))).NotTo(HaveOccurred())
		RegisterSaturationQueries(registry)
	}

	refresh := func(engine inferenceengine.Engine) *source.MetricResult {
		name := EngineQuery(engine, QueryKvCacheUsage)
		results, err := registry.Get("prometheus").Refresh(context.Background(), source.RefreshSpec{
			Queries: []string{name},
			Params:  map[string]string{source.ParamNamespace: "ns", source.ParamModelID: "m"},
		})
		Expect(err).NotTo(HaveOccurred())
		return results[name]
	}

	BeforeEach(newSource)

	It("executes the SGLang KV-usage template and returns sglang:token_usage", func() {
		res := refresh(inferenceengine.EngineSGLang)
		Expect(res).NotTo(BeNil())
		Expect(res.HasError()).To(BeFalse())
		Expect(res.FirstValue().Value).To(BeNumerically("~", 0.85, 1e-9))
	})

	It("keeps the vLLM KV-usage template working (unchanged behavior)", func() {
		res := refresh(inferenceengine.EngineVLLM)
		Expect(res).NotTo(BeNil())
		Expect(res.HasError()).To(BeFalse())
		Expect(res.FirstValue().Value).To(BeNumerically("~", 0.50, 1e-9))
	})
})
