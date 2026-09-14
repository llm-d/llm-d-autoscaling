package prometheus

import (
	"context"
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	sourcepkg "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
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

var _ = Describe("PrometheusSource", func() {
	var (
		mockAPI  *mockPrometheusAPI
		registry *sourcepkg.QueryList
		source   *PrometheusSource
		ctx      context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("Refresh", func() {
		BeforeEach(func() {
			mockAPI = &mockPrometheusAPI{
				queryFunc: func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
					return model.Vector{
						&model.Sample{
							Metric:    model.Metric{"pod": "test-pod-1"},
							Value:     0.75,
							Timestamp: model.TimeFromUnix(time.Now().Unix()),
						},
						&model.Sample{
							Metric:    model.Metric{"pod": "test-pod-2"},
							Value:     0.50,
							Timestamp: model.TimeFromUnix(time.Now().Unix()),
						},
					}, nil, nil
				},
			}

			source = NewPrometheusSource(context.Background(), mockAPI, PrometheusSourceConfig{
				DefaultTTL:   30 * time.Second,
				QueryTimeout: 5 * time.Second,
			})
			registry = source.QueryList()
			err := registry.Register(sourcepkg.QueryTemplate{
				Name:        "test_query",
				Type:        sourcepkg.QueryTypePromQL,
				Template:    `test_metric{namespace="{{.namespace}}"}`,
				Params:      []string{"namespace"},
				Description: "Test query",
			})
			Expect(err).NotTo(HaveOccurred())

		})

		It("should execute registered queries and return results", func() {
			results, err := source.Refresh(ctx, sourcepkg.RefreshSpec{
				Params: map[string]string{"namespace": "test-ns"},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveKey("test_query"))

			result := results["test_query"]
			Expect(result.Error).NotTo(HaveOccurred())
			Expect(result.Values).To(HaveLen(2))
			Expect(result.Values[0].Value).To(Equal(0.75))
			Expect(result.Values[0].Labels["pod"]).To(Equal("test-pod-1"))
		})

		It("should escape params so PromQL is safe (no injection)", func() {
			var capturedQuery string
			mockAPI.queryFunc = func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
				capturedQuery = query
				return model.Vector{
					&model.Sample{
						Metric:    model.Metric{"pod": "p1"},
						Value:     0.5,
						Timestamp: model.TimeFromUnix(time.Now().Unix()),
					},
				}, nil, nil
			}

			err := registry.Register(sourcepkg.QueryTemplate{
				Name:        "injection_test",
				Type:        sourcepkg.QueryTypePromQL,
				Template:    `metric{namespace="{{.namespace}}",model_name="{{.modelID}}"}`,
				Params:      []string{"namespace", "modelID"},
				Description: "Test escaping",
			})
			Expect(err).NotTo(HaveOccurred())

			// Params that would break PromQL or inject labels if unescaped
			params := map[string]string{
				"namespace": `safe-ns`,
				"modelID":   `x",namespace="other"`,
			}
			_, err = source.Refresh(ctx, sourcepkg.RefreshSpec{
				Queries: []string{"injection_test"},
				Params:  params,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(capturedQuery).NotTo(BeEmpty())
			// Escaped: quote and backslash in modelID become \"
			Expect(capturedQuery).To(ContainSubstring(`model_name="x\",namespace=\"other\""`))
			// Should not contain unescaped injection (extra namespace=)
			Expect(capturedQuery).NotTo(MatchRegexp(`namespace="other"`))
		})
	})

	Describe("Caching", func() {
		var callCount int

		BeforeEach(func() {
			callCount = 0

			mockAPI = &mockPrometheusAPI{
				queryFunc: func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
					callCount++
					return model.Vector{
						&model.Sample{
							Value:     model.SampleValue(callCount),
							Timestamp: model.TimeFromUnix(time.Now().Unix()),
						},
					}, nil, nil
				},
			}

			source = NewPrometheusSource(context.Background(), mockAPI, PrometheusSourceConfig{
				DefaultTTL:   1 * time.Hour,
				QueryTimeout: 5 * time.Second,
			})
			registry = source.QueryList()

			err := registry.Register(sourcepkg.QueryTemplate{
				Name:        "cached_query",
				Type:        sourcepkg.QueryTypePromQL,
				Template:    `cached_metric{namespace="{{.namespace}}"}`,
				Params:      []string{"namespace"},
				Description: "Cached query test",
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should cache results after refresh", func() {
			params := map[string]string{"namespace": "test-ns"}
			_, err := source.Refresh(ctx, sourcepkg.RefreshSpec{
				Params: params,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(callCount).To(Equal(1))

			cached := source.Get("cached_query", params)
			Expect(cached).NotTo(BeNil())
			Expect(cached.IsExpired()).To(BeFalse())
			Expect(cached.Result.FirstValue().Value).To(Equal(1.0))
		})

		It("should return cached values without re-querying", func() {
			params := map[string]string{"namespace": "test-ns"}
			_, err := source.Refresh(ctx, sourcepkg.RefreshSpec{
				Params: params,
			})
			Expect(err).NotTo(HaveOccurred())

			// Access cache multiple times
			_ = source.Get("cached_query", params)
			_ = source.Get("cached_query", params)

			Expect(callCount).To(Equal(1), "should not re-query Prometheus")
		})

		It("should cache separately for different params", func() {
			params1 := map[string]string{"namespace": "ns-1"}
			params2 := map[string]string{"namespace": "ns-2"}

			// Refresh with first params
			_, err := source.Refresh(ctx, sourcepkg.RefreshSpec{Params: params1})
			Expect(err).NotTo(HaveOccurred())
			Expect(callCount).To(Equal(1))

			// Refresh with second params
			_, err = source.Refresh(ctx, sourcepkg.RefreshSpec{Params: params2})
			Expect(err).NotTo(HaveOccurred())
			Expect(callCount).To(Equal(2))

			// Both should be cached separately
			cached1 := source.Get("cached_query", params1)
			cached2 := source.Get("cached_query", params2)
			Expect(cached1).NotTo(BeNil())
			Expect(cached2).NotTo(BeNil())
			Expect(cached1.Result.FirstValue().Value).To(Equal(1.0))
			Expect(cached2.Result.FirstValue().Value).To(Equal(2.0))
		})
	})

	Describe("Invalidate", func() {
		BeforeEach(func() {
			mockAPI = &mockPrometheusAPI{
				queryFunc: func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
					return model.Vector{
						&model.Sample{
							Value:     1.0,
							Timestamp: model.TimeFromUnix(time.Now().Unix()),
						},
					}, nil, nil
				},
			}

			source = NewPrometheusSource(context.Background(), mockAPI, DefaultPrometheusSourceConfig())
			registry = source.QueryList()

			err := registry.Register(sourcepkg.QueryTemplate{
				Name:     "query1",
				Type:     sourcepkg.QueryTypePromQL,
				Template: `metric1`,
			})
			Expect(err).ToNot(HaveOccurred())
			err = registry.Register(sourcepkg.QueryTemplate{
				Name:     "query2",
				Type:     sourcepkg.QueryTypePromQL,
				Template: `metric2`,
			})
			Expect(err).ToNot(HaveOccurred())
		})

	})

	Describe("Parsing", func() {
		BeforeEach(func() {
			source = &PrometheusSource{}
		})

		Describe("parseVector", func() {
			It("should parse vector results with labels", func() {
				ts := model.TimeFromUnix(time.Now().Unix())
				vec := model.Vector{
					&model.Sample{
						Metric:    model.Metric{"pod": "pod-1", "namespace": "ns-1"},
						Value:     0.5,
						Timestamp: ts,
					},
				}

				values := source.parseVector(vec)

				Expect(values).To(HaveLen(1))
				Expect(values[0].Value).To(Equal(0.5))
				Expect(values[0].Labels["pod"]).To(Equal("pod-1"))
				Expect(values[0].Labels["namespace"]).To(Equal("ns-1"))
			})
		})

		Describe("parseScalar", func() {
			It("should parse scalar results", func() {
				ts := model.TimeFromUnix(time.Now().Unix())
				scalar := &model.Scalar{
					Value:     42.0,
					Timestamp: ts,
				}

				values := source.parseScalar(scalar)

				Expect(values).To(HaveLen(1))
				Expect(values[0].Value).To(Equal(42.0))
			})
		})

		Describe("handling NaN and Inf", func() {
			It("should convert NaN to zero", func() {
				ts := model.TimeFromUnix(time.Now().Unix())
				vec := model.Vector{
					&model.Sample{
						Value:     model.SampleValue(math.NaN()),
						Timestamp: ts,
					},
				}

				values := source.parseVector(vec)

				Expect(values[0].Value).To(Equal(0.0))
			})

			It("should convert Inf to zero", func() {
				ts := model.TimeFromUnix(time.Now().Unix())
				vec := model.Vector{
					&model.Sample{
						Value:     model.SampleValue(math.Inf(1)),
						Timestamp: ts,
					},
				}

				values := source.parseVector(vec)

				Expect(values[0].Value).To(Equal(0.0))
			})
		})
	})
})

var _ = Describe("MetricResult", func() {
	Describe("IsStale", func() {
		Context("when result has values with recent timestamps", func() {
			It("should not be stale within threshold", func() {
				result := &sourcepkg.MetricResult{
					QueryName:   "test",
					CollectedAt: time.Now(),
					Values: []sourcepkg.MetricValue{
						{
							Value:     1.0,
							Timestamp: time.Now().Add(-5 * time.Second),
						},
					},
				}

				Expect(result.IsStale(10 * time.Second)).To(BeFalse())
			})
		})

		Context("when result has values with old timestamps", func() {
			It("should be stale beyond threshold", func() {
				result := &sourcepkg.MetricResult{
					QueryName:   "test",
					CollectedAt: time.Now(),
					Values: []sourcepkg.MetricValue{
						{
							Value:     1.0,
							Timestamp: time.Now().Add(-5 * time.Second),
						},
					},
				}

				Expect(result.IsStale(3 * time.Second)).To(BeTrue())
			})
		})

		Context("when result has no values", func() {
			It("should be considered stale", func() {
				result := &sourcepkg.MetricResult{QueryName: "empty"}

				Expect(result.IsStale(10 * time.Second)).To(BeTrue())
			})
		})

		Context("when result is nil", func() {
			It("should be considered stale", func() {
				var result *sourcepkg.MetricResult

				Expect(result.IsStale(10 * time.Second)).To(BeTrue())
			})
		})
	})

	Describe("FirstValue", func() {
		It("should return the first metric value", func() {
			expectedTime := time.Now().Truncate(time.Second)
			result := &sourcepkg.MetricResult{
				QueryName: "test",
				Values: []sourcepkg.MetricValue{
					{
						Value:     0.85,
						Timestamp: expectedTime,
						Labels:    map[string]string{"pod": "test-pod"},
					},
				},
				CollectedAt: time.Now(),
			}

			first := result.FirstValue()

			Expect(first.Value).To(Equal(0.85))
			Expect(first.Timestamp).To(Equal(expectedTime))
			Expect(first.Labels["pod"]).To(Equal("test-pod"))
		})
	})
})

var _ = Describe("sourcepkg.MetricValue", func() {
	Describe("IsStale", func() {
		Context("when timestamp is within threshold", func() {
			It("should not be stale", func() {
				value := sourcepkg.MetricValue{
					Value:     1.0,
					Timestamp: time.Now().Add(-2 * time.Second),
				}

				Expect(value.IsStale(5 * time.Second)).To(BeFalse())
			})
		})

		Context("when timestamp exceeds threshold", func() {
			It("should be stale", func() {
				value := sourcepkg.MetricValue{
					Value:     1.0,
					Timestamp: time.Now().Add(-2 * time.Second),
				}

				Expect(value.IsStale(1 * time.Second)).To(BeTrue())
			})
		})

		Context("when timestamp is zero", func() {
			It("should always be considered stale", func() {
				value := sourcepkg.MetricValue{Value: 1.0}

				Expect(value.IsStale(1 * time.Hour)).To(BeTrue())
			})
		})
	})
})
