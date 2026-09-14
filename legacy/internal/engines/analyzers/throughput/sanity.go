package throughput

import (
	"math"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// CheckModelMetrics validates the replica metrics for a set of replicas belonging
// to the same variant. It returns a SanityReport summarising any quality issues.
//
// Callers should check report.OK() before proceeding with Observe; a non-OK
// report means the metrics are not reliable enough for shape tracking or
// ITL calibration this cycle.
//
// Issues are deduplicated: the same SanityIssue type appears at most once in
// the report even if multiple pods trigger it.
func CheckModelMetrics(metrics []domain.ReplicaMetrics) SanityReport {
	if len(metrics) == 0 {
		return SanityReport{Issues: []SanityIssue{SanityIssueNoReplicas}}
	}

	issueSet := make(map[SanityIssue]struct{})
	var affectedPods []string

	for _, m := range metrics {
		podIssues := checkReplicaMetrics(m)
		if len(podIssues) > 0 {
			affectedPods = append(affectedPods, m.PodName)
			for _, issue := range podIssues {
				issueSet[issue] = struct{}{}
			}
		}
	}

	if len(issueSet) == 0 {
		return SanityReport{}
	}

	issues := make([]SanityIssue, 0, len(issueSet))
	for issue := range issueSet {
		issues = append(issues, issue)
	}
	return SanityReport{Issues: issues, AffectedPods: affectedPods}
}

// checkReplicaMetrics validates a single replica's metrics and returns the
// list of issues found. An empty slice means the replica is healthy.
func checkReplicaMetrics(m domain.ReplicaMetrics) []SanityIssue {
	var issues []SanityIssue

	// Stale metrics: FreshnessStatus == "stale" means the scrape is behind.
	if m.Metadata != nil && m.Metadata.FreshnessStatus == "stale" {
		issues = append(issues, SanityIssueStaleMetrics)
	}

	// KV cache configuration must be available for capacity calculations.
	if m.TotalKvCapacityTokens <= 0 {
		issues = append(issues, SanityIssueMissingKV)
	}

	// KV utilization (k*) must be a valid fraction.
	if m.KvUsageInstant < 0 || m.KvUsageInstant > 1 || math.IsNaN(m.KvUsageInstant) {
		issues = append(issues, SanityIssueKVOutOfRange)
	}

	// ITL must be positive for calibration.
	if m.AvgITL <= 0 || math.IsNaN(m.AvgITL) {
		issues = append(issues, SanityIssueITLNonPositive)
	}

	// Shape metrics (OL, IL) must be plausible for shape tracking.
	if m.AvgOutputTokens <= DefaultMinTokensPerRequest ||
		m.AvgInputTokens <= DefaultMinTokensPerRequest ||
		math.IsNaN(m.AvgOutputTokens) || math.IsNaN(m.AvgInputTokens) {
		issues = append(issues, SanityIssueMissingShape)
	}

	return issues
}
