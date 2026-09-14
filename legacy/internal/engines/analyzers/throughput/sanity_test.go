package throughput

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// healthyReplica returns a ReplicaMetrics with all fields valid.
func healthyReplica(podName string) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{
		PodName:               podName,
		VariantName:           "v1",
		KvUsageInstant:        0.50,
		TotalKvCapacityTokens: 65536,
		AvgInputTokens:        1024,
		AvgOutputTokens:       256,
		PrefixCacheHitRate:    0.0,
		AvgITL:                0.040,
	}
}

var _ = Describe("CheckModelMetrics", func() {
	Describe("no replicas", func() {
		It("returns SanityIssueNoReplicas", func() {
			report := CheckModelMetrics(nil)
			Expect(report.OK()).To(BeFalse())
			Expect(report.Has(SanityIssueNoReplicas)).To(BeTrue())
		})

		It("returns SanityIssueNoReplicas for empty slice", func() {
			report := CheckModelMetrics([]domain.ReplicaMetrics{})
			Expect(report.OK()).To(BeFalse())
			Expect(report.Has(SanityIssueNoReplicas)).To(BeTrue())
		})
	})

	Describe("healthy metrics", func() {
		It("returns OK for a single healthy replica", func() {
			metrics := []domain.ReplicaMetrics{healthyReplica("pod-0")}
			report := CheckModelMetrics(metrics)
			Expect(report.OK()).To(BeTrue())
			Expect(report.Issues).To(BeEmpty())
			Expect(report.AffectedPods).To(BeEmpty())
		})

		It("returns OK for multiple healthy replicas", func() {
			metrics := []domain.ReplicaMetrics{
				healthyReplica("pod-0"),
				healthyReplica("pod-1"),
				healthyReplica("pod-2"),
			}
			report := CheckModelMetrics(metrics)
			Expect(report.OK()).To(BeTrue())
		})
	})

	Describe("stale metrics", func() {
		It("flags SanityIssueStaleMetrics when FreshnessStatus is stale", func() {
			m := healthyReplica("pod-0")
			m.Metadata = &domain.ReplicaMetricsMetadata{FreshnessStatus: "stale"}
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueStaleMetrics)).To(BeTrue())
			Expect(report.AffectedPods).To(ContainElement("pod-0"))
		})

		It("does not flag stale when FreshnessStatus is fresh", func() {
			m := healthyReplica("pod-0")
			m.Metadata = &domain.ReplicaMetricsMetadata{FreshnessStatus: "fresh"}
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueStaleMetrics)).To(BeFalse())
		})

		It("does not flag stale when Metadata is nil", func() {
			m := healthyReplica("pod-0")
			// Metadata is nil by default in healthyReplica
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueStaleMetrics)).To(BeFalse())
		})
	})

	Describe("missing KV capacity", func() {
		It("flags SanityIssueMissingKV when TotalKvCapacityTokens is zero", func() {
			m := healthyReplica("pod-0")
			m.TotalKvCapacityTokens = 0
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueMissingKV)).To(BeTrue())
			Expect(report.AffectedPods).To(ContainElement("pod-0"))
		})

		It("flags SanityIssueMissingKV when TotalKvCapacityTokens is negative", func() {
			m := healthyReplica("pod-0")
			m.TotalKvCapacityTokens = -1
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueMissingKV)).To(BeTrue())
		})
	})

	Describe("KV utilization out of range", func() {
		It("flags SanityIssueKVOutOfRange when KvUsageInstant (k*) is negative", func() {
			m := healthyReplica("pod-0")
			m.KvUsageInstant = -0.01
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueKVOutOfRange)).To(BeTrue())
			Expect(report.AffectedPods).To(ContainElement("pod-0"))
		})

		It("flags SanityIssueKVOutOfRange when KvUsageInstant (k*) exceeds 1.0", func() {
			m := healthyReplica("pod-0")
			m.KvUsageInstant = 1.01
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueKVOutOfRange)).To(BeTrue())
		})

		It("flags SanityIssueKVOutOfRange for NaN KvUsageInstant (k*)", func() {
			m := healthyReplica("pod-0")
			m.KvUsageInstant = float64NaN()
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueKVOutOfRange)).To(BeTrue())
		})

		It("accepts KvUsageInstant (k*) at the boundary values 0.0 and 1.0", func() {
			m0 := healthyReplica("pod-0")
			m0.KvUsageInstant = 0.0
			m1 := healthyReplica("pod-1")
			m1.KvUsageInstant = 1.0
			Expect(CheckModelMetrics([]domain.ReplicaMetrics{m0}).Has(SanityIssueKVOutOfRange)).To(BeFalse())
			Expect(CheckModelMetrics([]domain.ReplicaMetrics{m1}).Has(SanityIssueKVOutOfRange)).To(BeFalse())
		})
	})

	Describe("non-positive ITL", func() {
		It("flags SanityIssueITLNonPositive when AvgITL is zero", func() {
			m := healthyReplica("pod-0")
			m.AvgITL = 0
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueITLNonPositive)).To(BeTrue())
			Expect(report.AffectedPods).To(ContainElement("pod-0"))
		})

		It("flags SanityIssueITLNonPositive when AvgITL is negative", func() {
			m := healthyReplica("pod-0")
			m.AvgITL = -0.001
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueITLNonPositive)).To(BeTrue())
		})

		It("flags SanityIssueITLNonPositive when AvgITL is NaN", func() {
			m := healthyReplica("pod-0")
			m.AvgITL = float64NaN()
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueITLNonPositive)).To(BeTrue())
		})
	})

	Describe("missing shape metrics", func() {
		It("flags SanityIssueMissingShape when AvgOutputTokens is at threshold", func() {
			m := healthyReplica("pod-0")
			m.AvgOutputTokens = DefaultMinTokensPerRequest
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueMissingShape)).To(BeTrue())
			Expect(report.AffectedPods).To(ContainElement("pod-0"))
		})

		It("flags SanityIssueMissingShape when AvgOutputTokens is zero", func() {
			m := healthyReplica("pod-0")
			m.AvgOutputTokens = 0
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueMissingShape)).To(BeTrue())
		})

		It("flags SanityIssueMissingShape when AvgInputTokens is at threshold", func() {
			m := healthyReplica("pod-0")
			m.AvgInputTokens = DefaultMinTokensPerRequest
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueMissingShape)).To(BeTrue())
		})

		It("flags SanityIssueMissingShape when AvgInputTokens is NaN", func() {
			m := healthyReplica("pod-0")
			m.AvgInputTokens = float64NaN()
			report := CheckModelMetrics([]domain.ReplicaMetrics{m})
			Expect(report.Has(SanityIssueMissingShape)).To(BeTrue())
		})
	})

	Describe("issue deduplication", func() {
		It("reports the same issue once even when multiple pods trigger it", func() {
			m0 := healthyReplica("pod-0")
			m0.AvgITL = 0
			m1 := healthyReplica("pod-1")
			m1.AvgITL = 0

			report := CheckModelMetrics([]domain.ReplicaMetrics{m0, m1})
			count := 0
			for _, issue := range report.Issues {
				if issue == SanityIssueITLNonPositive {
					count++
				}
			}
			Expect(count).To(Equal(1))
			Expect(report.AffectedPods).To(ConsistOf("pod-0", "pod-1"))
		})

		It("collects multiple distinct issues from different pods", func() {
			m0 := healthyReplica("pod-0")
			m0.AvgITL = 0 // ITL issue
			m1 := healthyReplica("pod-1")
			m1.TotalKvCapacityTokens = 0 // KV issue

			report := CheckModelMetrics([]domain.ReplicaMetrics{m0, m1})
			Expect(report.Has(SanityIssueITLNonPositive)).To(BeTrue())
			Expect(report.Has(SanityIssueMissingKV)).To(BeTrue())
			Expect(report.AffectedPods).To(ConsistOf("pod-0", "pod-1"))
		})

		It("a pod with no issues does not appear in AffectedPods", func() {
			good := healthyReplica("pod-good")
			bad := healthyReplica("pod-bad")
			bad.AvgITL = 0

			report := CheckModelMetrics([]domain.ReplicaMetrics{good, bad})
			Expect(report.AffectedPods).To(ConsistOf("pod-bad"))
			Expect(report.AffectedPods).NotTo(ContainElement("pod-good"))
		})
	})

	Describe("SanityReport helpers", func() {
		It("Has returns false for an issue not in the report", func() {
			report := CheckModelMetrics([]domain.ReplicaMetrics{healthyReplica("pod-0")})
			Expect(report.Has(SanityIssueNoReplicas)).To(BeFalse())
		})
	})
})
