package prometheus

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

func TestQueryPrometheusWithBackoff(t *testing.T) {
	t.Parallel()

	const query = "test_query"

	cases := []struct {
		name           string
		failures       int
		expectErr      bool
		expectAttempts int
		description    string
	}{
		{
			name:           "retries_then_succeeds",
			failures:       2,
			expectErr:      false,
			expectAttempts: 3,
			description:    "Transient blips resolve before backoff steps are exhausted and we return the mocked result.",
		},
		{
			name:           "exhausts_retries",
			failures:       constants.PrometheusQueryBackoff.Steps + 5,
			expectErr:      true,
			expectAttempts: constants.PrometheusQueryBackoff.Steps,
			description:    "Every attempt keeps failing so backoff gives up and surfaces the last Prometheus error.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{
					query: model.Vector{
						&model.Sample{
							Value: model.SampleValue(42),
							Metric: model.Metric{
								"__name__": "test_metric",
							},
						},
					},
				},
				QueryFailCounts: map[string]int{
					query: tc.failures,
				},
				QueryCallCounts: make(map[string]int),
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			val, warn, err := QueryPrometheusWithBackoff(ctx, mock, query)

			if tc.expectErr {
				assert.Error(t, err, tc.description)
				assert.Nil(t, val)
				assert.Nil(t, warn)
				t.Log(mock.QueryCallCounts[query])
				assert.Equal(t, mock.QueryCallCounts[query], tc.expectAttempts)
				return
			}

			assert.NoError(t, err, tc.description)
			assert.Equal(t, tc.expectAttempts, mock.QueryCallCounts[query])

			vec, ok := val.(model.Vector)
			if assert.True(t, ok, tc.description) {
				assert.Len(t, vec, 1)
				assert.Equal(t, model.SampleValue(42), vec[0].Value)
			}
			assert.Nil(t, warn)
		})
	}
}
