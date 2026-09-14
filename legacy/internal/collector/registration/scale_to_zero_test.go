package registration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// mockPrometheusAPI implements promv1.API for testing
type mockPrometheusAPI struct {
	queryFunc func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error)
}

func (m *mockPrometheusAPI) Query(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
	if m.queryFunc != nil {
		return m.queryFunc(ctx, query, ts, opts...)
	}
	return nil, nil, nil
}

// Implement remaining API methods as no-ops for interface compliance
func (m *mockPrometheusAPI) AlertManagers(ctx context.Context) (v1.AlertManagersResult, error) {
	return v1.AlertManagersResult{}, nil
}
func (m *mockPrometheusAPI) Alerts(ctx context.Context) (v1.AlertsResult, error) {
	return v1.AlertsResult{}, nil
}
func (m *mockPrometheusAPI) Buildinfo(ctx context.Context) (v1.BuildinfoResult, error) {
	return v1.BuildinfoResult{}, nil
}
func (m *mockPrometheusAPI) CleanTombstones(ctx context.Context) error { return nil }
func (m *mockPrometheusAPI) Config(ctx context.Context) (v1.ConfigResult, error) {
	return v1.ConfigResult{}, nil
}
func (m *mockPrometheusAPI) DeleteSeries(ctx context.Context, matches []string, startTime, endTime time.Time) error {
	return nil
}
func (m *mockPrometheusAPI) Flags(ctx context.Context) (v1.FlagsResult, error) {
	return v1.FlagsResult{}, nil
}
func (m *mockPrometheusAPI) LabelNames(ctx context.Context, matches []string, startTime, endTime time.Time, opts ...v1.Option) ([]string, v1.Warnings, error) {
	return nil, nil, nil
}
func (m *mockPrometheusAPI) LabelValues(ctx context.Context, label string, matches []string, startTime, endTime time.Time, opts ...v1.Option) (model.LabelValues, v1.Warnings, error) {
	return nil, nil, nil
}
func (m *mockPrometheusAPI) Metadata(ctx context.Context, metric, limit string) (map[string][]v1.Metadata, error) {
	return nil, nil
}
func (m *mockPrometheusAPI) QueryExemplars(ctx context.Context, query string, startTime, endTime time.Time) ([]v1.ExemplarQueryResult, error) {
	return nil, nil
}
func (m *mockPrometheusAPI) QueryRange(ctx context.Context, query string, r v1.Range, opts ...v1.Option) (model.Value, v1.Warnings, error) {
	return nil, nil, nil
}
func (m *mockPrometheusAPI) Rules(ctx context.Context) (v1.RulesResult, error) {
	return v1.RulesResult{}, nil
}
func (m *mockPrometheusAPI) Runtimeinfo(ctx context.Context) (v1.RuntimeinfoResult, error) {
	return v1.RuntimeinfoResult{}, nil
}
func (m *mockPrometheusAPI) Series(ctx context.Context, matches []string, startTime, endTime time.Time, opts ...v1.Option) ([]model.LabelSet, v1.Warnings, error) {
	return nil, nil, nil
}
func (m *mockPrometheusAPI) Snapshot(ctx context.Context, skipHead bool) (v1.SnapshotResult, error) {
	return v1.SnapshotResult{}, nil
}
func (m *mockPrometheusAPI) Targets(ctx context.Context) (v1.TargetsResult, error) {
	return v1.TargetsResult{}, nil
}
func (m *mockPrometheusAPI) TargetsMetadata(ctx context.Context, matchTarget, metric, limit string) ([]v1.MetricMetadata, error) {
	return nil, nil
}
func (m *mockPrometheusAPI) TSDB(ctx context.Context, opts ...v1.Option) (v1.TSDBResult, error) {
	return v1.TSDBResult{}, nil
}
func (m *mockPrometheusAPI) WalReplay(ctx context.Context) (v1.WalReplayStatus, error) {
	return v1.WalReplayStatus{}, nil
}

var _ = Describe("RegisterScaleToZeroQueries", func() {
	var (
		ctx      context.Context
		registry *source.SourceRegistry
		mockAPI  *mockPrometheusAPI
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
		})

		It("should register the model request count query", func() {
			RegisterScaleToZeroQueries(registry)

			metricsSource := registry.Get("prometheus")
			Expect(metricsSource).NotTo(BeNil())

			query := metricsSource.QueryList().Get(QueryModelRequestCount)
			Expect(query).NotTo(BeNil())
			Expect(query.Name).To(Equal(QueryModelRequestCount))
			Expect(query.Type).To(Equal(source.QueryTypePromQL))
		})
	})

	Context("when prometheus source is not registered", func() {
		It("should not panic", func() {
			Expect(func() {
				RegisterScaleToZeroQueries(registry)
			}).NotTo(Panic())
		})
	})
})

var _ = Describe("CollectModelRequestCount", func() {
	var (
		ctx           context.Context
		registry      *source.SourceRegistry
		mockAPI       *mockPrometheusAPI
		metricsSource source.MetricsSource
	)

	BeforeEach(func() {
		ctx = context.Background()
		registry = source.NewSourceRegistry()
	})

	Context("when metrics are available", func() {
		BeforeEach(func() {
			mockAPI = &mockPrometheusAPI{
				queryFunc: func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
					return &model.Scalar{
						Value:     model.SampleValue(100.5),
						Timestamp: model.TimeFromUnix(time.Now().Unix()),
					}, nil, nil
				},
			}
			metricsSource = prometheus.NewPrometheusSource(ctx, mockAPI, prometheus.DefaultPrometheusSourceConfig())
			err := registry.Register("prometheus", metricsSource)
			Expect(err).NotTo(HaveOccurred())
			RegisterScaleToZeroQueries(registry)
		})

		It("should return the request count", func() {
			count, err := CollectModelRequestCount(ctx, metricsSource, "my-model", "default", 10*time.Minute)

			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(100.5))
		})
	})

	Context("when query succeeds but returns empty result (no requests in retention period)", func() {
		BeforeEach(func() {
			mockAPI = &mockPrometheusAPI{
				queryFunc: func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
					// Prometheus returns empty vector when query succeeds but no time series match
					// This is the normal case when there are genuinely no requests in the retention period
					return model.Vector{}, nil, nil
				},
			}
			metricsSource = prometheus.NewPrometheusSource(ctx, mockAPI, prometheus.DefaultPrometheusSourceConfig())
			err := registry.Register("prometheus", metricsSource)
			Expect(err).NotTo(HaveOccurred())
			RegisterScaleToZeroQueries(registry)
		})

		It("should return an error to prevent premature scale-to-zero", func() {
			count, err := CollectModelRequestCount(ctx, metricsSource, "my-model", "default", 10*time.Minute)

			// Error returned to signal we can't confirm request count,
			// which prevents the enforcer from scaling to zero
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no values"))
			Expect(count).To(Equal(0.0))
		})
	})

	Context("when query returns an error", func() {
		BeforeEach(func() {
			mockAPI = &mockPrometheusAPI{
				queryFunc: func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
					return nil, nil, context.DeadlineExceeded
				},
			}
			metricsSource = prometheus.NewPrometheusSource(ctx, mockAPI, prometheus.DefaultPrometheusSourceConfig())
			err := registry.Register("prometheus", metricsSource)
			Expect(err).NotTo(HaveOccurred())
			RegisterScaleToZeroQueries(registry)
		})

		It("should return an error to prevent premature scale-to-zero", func() {
			count, err := CollectModelRequestCount(ctx, metricsSource, "my-model", "default", 10*time.Minute)

			// Error returned to signal we can't confirm request count,
			// which prevents the enforcer from scaling to zero
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("query"))
			Expect(count).To(Equal(0.0))
		})
	})

	Context("query parameter formatting", func() {
		var capturedQuery string

		BeforeEach(func() {
			mockAPI = &mockPrometheusAPI{
				queryFunc: func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
					capturedQuery = query
					return &model.Scalar{
						Value:     model.SampleValue(50),
						Timestamp: model.TimeFromUnix(time.Now().Unix()),
					}, nil, nil
				},
			}
			metricsSource = prometheus.NewPrometheusSource(ctx, mockAPI, prometheus.DefaultPrometheusSourceConfig())
			err := registry.Register("prometheus", metricsSource)
			Expect(err).NotTo(HaveOccurred())
			RegisterScaleToZeroQueries(registry)
		})

		It("should format retention period correctly in query", func() {
			_, _ = CollectModelRequestCount(ctx, metricsSource, "test-model", "test-ns", 15*time.Minute)

			Expect(capturedQuery).NotTo(BeEmpty())
			Expect(capturedQuery).To(ContainSubstring("[15m]"))
			Expect(capturedQuery).To(ContainSubstring(`model_name="test-model"`))
			Expect(capturedQuery).To(ContainSubstring(`namespace="test-ns"`))
		})
	})

	Context("metrics collection duration tracking", func() {
		var metricsRegistry *promclient.Registry

		BeforeEach(func() {
			// Initialize metrics with a fresh registry for isolation
			metricsRegistry = promclient.NewRegistry()
			err := metrics.InitMetrics(metricsRegistry)
			Expect(err).NotTo(HaveOccurred())

			mockAPI = &mockPrometheusAPI{
				queryFunc: func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
					// Simulate some query latency
					time.Sleep(10 * time.Millisecond)
					return &model.Scalar{
						Value:     model.SampleValue(42),
						Timestamp: model.TimeFromUnix(time.Now().Unix()),
					}, nil, nil
				},
			}
			metricsSource = prometheus.NewPrometheusSource(ctx, mockAPI, prometheus.DefaultPrometheusSourceConfig())
			err = registry.Register("prometheus", metricsSource)
			Expect(err).NotTo(HaveOccurred())
			RegisterScaleToZeroQueries(registry)
		})

		It("should record metrics collection duration", func() {
			// Call the function which should record collection duration
			_, err := CollectModelRequestCount(ctx, metricsSource, "test-model", "test-ns", 5*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			// Gather metrics from the registry
			metricFamilies, err := metricsRegistry.Gather()
			Expect(err).NotTo(HaveOccurred())

			// Find the collection duration histogram
			var found bool
			for _, mf := range metricFamilies {
				if mf.GetName() == constants.WVAMetricsCollectionDurationSeconds {
					found = true

					// Verify the histogram has at least one observation
					Expect(mf.GetMetric()).NotTo(BeEmpty(), "Should have at least one metric series")

					// Find the metric with query_type=request_count label
					var foundRequestCount bool
					for _, m := range mf.GetMetric() {
						// Check labels
						var queryType string
						for _, label := range m.GetLabel() {
							if label.GetName() == constants.LabelQueryType {
								queryType = label.GetValue()
								break
							}
						}

						if queryType == constants.QueryTypeRequestCount {
							foundRequestCount = true
							histogram := m.GetHistogram()
							Expect(histogram).NotTo(BeNil(), "Histogram should not be nil")
							Expect(histogram.GetSampleCount()).To(BeNumerically(">=", 1),
								"Should have recorded at least one observation")
							// Duration should be > 0 (we simulated 10ms delay)
							Expect(histogram.GetSampleSum()).To(BeNumerically(">", 0),
								"Duration should be greater than 0")
							break
						}
					}

					Expect(foundRequestCount).To(BeTrue(),
						"Should have recorded duration for query_type=%s", constants.QueryTypeRequestCount)
					break
				}
			}

			Expect(found).To(BeTrue(), "Should have found %s metric", constants.WVAMetricsCollectionDurationSeconds)
		})
	})
})

var _ = Describe("CollectModelRequestCountForEngine", func() {
	var (
		ctx           context.Context
		registry      *source.SourceRegistry
		metricsSource source.MetricsSource
		capturedQuery string
	)

	BeforeEach(func() {
		ctx = context.Background()
		registry = source.NewSourceRegistry()
		capturedQuery = ""
		mockAPI := &mockPrometheusAPI{
			queryFunc: func(_ context.Context, query string, _ time.Time, _ ...v1.Option) (model.Value, v1.Warnings, error) {
				capturedQuery = query
				return &model.Scalar{Value: model.SampleValue(7), Timestamp: model.TimeFromUnix(0)}, nil, nil
			},
		}
		metricsSource = prometheus.NewPrometheusSource(ctx, mockAPI, prometheus.DefaultPrometheusSourceConfig())
		Expect(registry.Register("prometheus", metricsSource)).NotTo(HaveOccurred())
		RegisterScaleToZeroQueries(registry)
	})

	It("routes EngineSGLang to the sglang:num_requests_total query", func() {
		// The SGLang request-count query is registered under the physical name
		// sglang/model_request_count; this asserts the engine-aware routing reaches it.
		Expect(EngineQuery(inferenceengine.EngineSGLang, QueryModelRequestCount)).To(Equal("sglang/" + QueryModelRequestCount))

		count, err := CollectModelRequestCountForEngine(ctx, metricsSource, inferenceengine.EngineSGLang, "m", "ns", 10*time.Minute)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(7.0))
		Expect(capturedQuery).To(ContainSubstring("sglang:num_requests_total"))
		Expect(capturedQuery).NotTo(ContainSubstring("vllm:"))
	})

	It("routes EngineVLLM to the vllm:request_success_total query", func() {
		count, err := CollectModelRequestCountForEngine(ctx, metricsSource, inferenceengine.EngineVLLM, "m", "ns", 10*time.Minute)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(7.0))
		Expect(capturedQuery).To(ContainSubstring("vllm:request_success_total"))
		Expect(capturedQuery).NotTo(ContainSubstring("sglang:"))
	})
})
