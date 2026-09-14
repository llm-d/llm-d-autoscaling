/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aggregation_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
)

func TestAggregation(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Aggregation Suite")
}

var _ = Describe("aggregation helpers", func() {

	Describe("SumTotalSupply", func() {
		It("returns 0 for empty input", func() {
			Expect(aggregation.SumTotalSupply(nil)).To(BeZero())
			Expect(aggregation.SumTotalSupply([]domain.VariantCapacity{})).To(BeZero())
		})

		It("sums ReplicaCount × PerReplicaCapacity across variants", func() {
			vcs := []domain.VariantCapacity{
				{VariantName: "v1", ReplicaCount: 2, PendingReplicas: 1, PerReplicaCapacity: 5000},
				{VariantName: "v2", ReplicaCount: 3, PendingReplicas: 0, PerReplicaCapacity: 8000},
			}
			// 2×5000 + 3×8000 = 10000 + 24000 = 34000 (pending NOT included)
			Expect(aggregation.SumTotalSupply(vcs)).To(Equal(34000.0))
		})

		It("ignores pending replicas", func() {
			vcs := []domain.VariantCapacity{
				{ReplicaCount: 1, PendingReplicas: 99, PerReplicaCapacity: 1000},
			}
			Expect(aggregation.SumTotalSupply(vcs)).To(Equal(1000.0))
		})

		It("handles zero PRC", func() {
			vcs := []domain.VariantCapacity{
				{ReplicaCount: 5, PerReplicaCapacity: 0},
			}
			Expect(aggregation.SumTotalSupply(vcs)).To(BeZero())
		})
	})

	Describe("SumTotalAnticipatedSupply", func() {
		It("returns 0 for empty input", func() {
			Expect(aggregation.SumTotalAnticipatedSupply(nil)).To(BeZero())
		})

		It("sums (ReplicaCount + PendingReplicas) × PRC across variants", func() {
			vcs := []domain.VariantCapacity{
				{ReplicaCount: 2, PendingReplicas: 1, PerReplicaCapacity: 5000},
				{ReplicaCount: 3, PendingReplicas: 0, PerReplicaCapacity: 8000},
			}
			// (2+1)×5000 + (3+0)×8000 = 15000 + 24000 = 39000
			Expect(aggregation.SumTotalAnticipatedSupply(vcs)).To(Equal(39000.0))
		})

		It("equals SumTotalSupply when no variants have pending replicas", func() {
			vcs := []domain.VariantCapacity{
				{ReplicaCount: 3, PendingReplicas: 0, PerReplicaCapacity: 10000},
			}
			Expect(aggregation.SumTotalAnticipatedSupply(vcs)).To(Equal(aggregation.SumTotalSupply(vcs)))
		})
	})

	Describe("SumTotalDemand", func() {
		It("returns 0 for empty input", func() {
			Expect(aggregation.SumTotalDemand(nil)).To(BeZero())
		})

		It("sums TotalDemand across variants", func() {
			vcs := []domain.VariantCapacity{
				{TotalDemand: 3000},
				{TotalDemand: 7000},
			}
			Expect(aggregation.SumTotalDemand(vcs)).To(Equal(10000.0))
		})
	})

	Describe("AggregateByRole", func() {
		It("returns empty map for empty input", func() {
			Expect(aggregation.AggregateByRole(nil)).To(BeEmpty())
		})

		It("canonicalizes empty role to RoleBoth", func() {
			vcs := []domain.VariantCapacity{
				{Role: "", ReplicaCount: 1, PerReplicaCapacity: 1000, TotalDemand: 500},
			}
			result := aggregation.AggregateByRole(vcs)
			Expect(result).To(HaveKey(domain.RoleBoth))
			Expect(result).NotTo(HaveKey(""))
		})

		It("groups variants by role and computes ScopeTotals for each", func() {
			vcs := []domain.VariantCapacity{
				{Role: "prefill", ReplicaCount: 2, PendingReplicas: 1, PerReplicaCapacity: 5000, TotalDemand: 8000},
				{Role: "decode", ReplicaCount: 3, PendingReplicas: 0, PerReplicaCapacity: 8000, TotalDemand: 9000},
				{Role: "decode", ReplicaCount: 1, PendingReplicas: 0, PerReplicaCapacity: 4000, TotalDemand: 2000},
			}
			result := aggregation.AggregateByRole(vcs)

			Expect(result).To(HaveLen(2))

			prefill := result["prefill"]
			Expect(prefill.TotalSupply).To(Equal(10000.0))            // 2×5000
			Expect(prefill.TotalAnticipatedSupply).To(Equal(15000.0)) // (2+1)×5000
			Expect(prefill.TotalDemand).To(Equal(8000.0))

			decode := result["decode"]
			Expect(decode.TotalSupply).To(Equal(28000.0))            // 3×8000 + 1×4000
			Expect(decode.TotalAnticipatedSupply).To(Equal(28000.0)) // no pending
			Expect(decode.TotalDemand).To(Equal(11000.0))            // 9000 + 2000
		})

		It("handles a single non-disaggregated variant (empty role → RoleBoth)", func() {
			vcs := []domain.VariantCapacity{
				{Role: "", ReplicaCount: 2, PendingReplicas: 0, PerReplicaCapacity: 10000, TotalDemand: 15000},
			}
			result := aggregation.AggregateByRole(vcs)
			both := result[domain.RoleBoth]
			Expect(both.TotalSupply).To(Equal(20000.0))
			Expect(both.TotalAnticipatedSupply).To(Equal(20000.0))
			Expect(both.TotalDemand).To(Equal(15000.0))
		})

		It("handles zero ReplicaCount", func() {
			vcs := []domain.VariantCapacity{
				{Role: "prefill", ReplicaCount: 0, PendingReplicas: 0, PerReplicaCapacity: 5000, TotalDemand: 0},
			}
			result := aggregation.AggregateByRole(vcs)
			prefill := result["prefill"]
			Expect(prefill.TotalSupply).To(BeZero())
			Expect(prefill.TotalAnticipatedSupply).To(BeZero())
			Expect(prefill.TotalDemand).To(BeZero())
		})
	})
})
