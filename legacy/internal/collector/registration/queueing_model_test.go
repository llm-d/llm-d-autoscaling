package registration

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
)

var _ = Describe("RegisterQueueingModelQueries", func() {
	var (
		ctx      context.Context
		registry *source.SourceRegistry
		mockAPI  *mockPrometheusAPI
	)

	BeforeEach(func() {
		ctx = context.Background()
		registry = source.NewSourceRegistry()
		mockAPI = &mockPrometheusAPI{}
		metricsSource := prometheus.NewPrometheusSource(ctx, mockAPI, prometheus.DefaultPrometheusSourceConfig())
		err := registry.Register("prometheus", metricsSource)
		Expect(err).NotTo(HaveOccurred())
		RegisterQueueingModelQueries(registry)
	})

	Describe("scheduler dispatch rate query", func() {
		It("uses pod_name label to identify the vLLM endpoint pod", func() {
			q := registry.Get("prometheus").QueryList().Get(QuerySchedulerDispatchRate)
			Expect(q).NotTo(BeNil())
			// The metric labels both "pod" (EPP scrape target) and "pod_name" (vLLM endpoint).
			// We must group by pod_name so results join correctly with vLLM TTFT/ITL metrics.
			Expect(q.Template).To(ContainSubstring("pod_name"))
			Expect(q.Template).To(ContainSubstring("inference_extension_scheduler_attempts_total"))
			Expect(q.Template).To(ContainSubstring(`status="success"`))
		})
	})

	Describe("average TTFT query", func() {
		It("queries vllm:time_to_first_token_seconds histogram", func() {
			q := registry.Get("prometheus").QueryList().Get(QueryAvgTTFT)
			Expect(q).NotTo(BeNil())
			Expect(q.Template).To(ContainSubstring("vllm:time_to_first_token_seconds_sum"))
			Expect(q.Template).To(ContainSubstring("vllm:time_to_first_token_seconds_count"))
		})
	})

	Describe("average ITL query", func() {
		It("queries vllm:inter_token_latency_seconds histogram", func() {
			q := registry.Get("prometheus").QueryList().Get(QueryAvgITL)
			Expect(q).NotTo(BeNil())
			// vLLM exports inter-token latency as inter_token_latency_seconds.
			Expect(q.Template).To(ContainSubstring("vllm:inter_token_latency_seconds_sum"))
			Expect(q.Template).To(ContainSubstring("vllm:inter_token_latency_seconds_count"))
		})

		It("does not use the old time_per_output_token_seconds metric name", func() {
			q := registry.Get("prometheus").QueryList().Get(QueryAvgITL)
			Expect(q).NotTo(BeNil())
			Expect(q.Template).NotTo(ContainSubstring("time_per_output_token_seconds"))
		})
	})
})
