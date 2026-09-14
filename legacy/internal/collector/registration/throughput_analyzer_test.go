package registration

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
)

var _ = Describe("RegisterThroughputAnalyzerQueries", func() {
	var (
		ctx       context.Context
		registry  *source.SourceRegistry
		mockAPI   *mockPrometheusAPI
		queryList *source.QueryList
	)

	BeforeEach(func() {
		ctx = context.Background()
		registry = source.NewSourceRegistry()
		mockAPI = &mockPrometheusAPI{}
	})

	Context("when prometheus source is registered", func() {
		BeforeEach(func() {
			metricsSource := prometheus.NewPrometheusSource(ctx, mockAPI, prometheus.DefaultPrometheusSourceConfig())
			err := registry.Register("prometheus", metricsSource)
			Expect(err).NotTo(HaveOccurred())
			RegisterThroughputAnalyzerQueries(registry)
			queryList = registry.Get("prometheus").QueryList()
		})

		It("should panic when RegisterThroughputAnalyzerQueries is called twice on the same registry", func() {
			// MustRegister panics on duplicate names; calling the function a second
			// time on the same registry (queries already registered by BeforeEach)
			// must trigger that panic.
			Expect(func() {
				RegisterThroughputAnalyzerQueries(registry)
			}).To(Panic())
		})

		It("should register exactly the three TA-exclusive queries", func() {
			expectedQueries := []string{
				QueryGenerationTokenRate,
				QueryKvUsageInstant,
				QueryRequestRate,
			}
			for _, name := range expectedQueries {
				q := queryList.Get(name)
				Expect(q).NotTo(BeNil(), "expected query %q to be registered", name)
				Expect(q.Name).To(Equal(name))
				Expect(q.Type).To(Equal(source.QueryTypePromQL))
			}
		})

		It("should build QueryGenerationTokenRate with namespace and model substituted", func() {
			rendered, err := queryList.Build(QueryGenerationTokenRate, map[string]string{
				source.ParamNamespace: "test-ns",
				source.ParamModelID:   "test-model",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(rendered).To(ContainSubstring(`namespace="test-ns"`))
			Expect(rendered).To(ContainSubstring(`model_name="test-model"`))
			Expect(rendered).To(ContainSubstring(`[1m]`))
			Expect(rendered).To(ContainSubstring(`vllm:request_generation_tokens_sum`))
		})

		It("should build QueryKvUsageInstant without max_over_time", func() {
			rendered, err := queryList.Build(QueryKvUsageInstant, map[string]string{
				source.ParamNamespace: "test-ns",
				source.ParamModelID:   "test-model",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(rendered).To(ContainSubstring(`vllm:kv_cache_usage_perc`))
			Expect(rendered).NotTo(ContainSubstring(`max_over_time`))
			Expect(rendered).To(ContainSubstring(`namespace="test-ns"`))
			Expect(rendered).To(ContainSubstring(`model_name="test-model"`))
		})

		It("should build QueryRequestRate with 1m window over token count", func() {
			rendered, err := queryList.Build(QueryRequestRate, map[string]string{
				source.ParamNamespace: "test-ns",
				source.ParamModelID:   "test-model",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(rendered).To(ContainSubstring(`vllm:request_generation_tokens_count`))
			Expect(rendered).To(ContainSubstring(`[1m]`))
			Expect(rendered).To(ContainSubstring(`namespace="test-ns"`))
			Expect(rendered).To(ContainSubstring(`model_name="test-model"`))
		})
	})

	Context("when prometheus source is not registered", func() {
		It("should not panic", func() {
			Expect(func() {
				RegisterThroughputAnalyzerQueries(registry)
			}).NotTo(Panic())
		})
	})
})
