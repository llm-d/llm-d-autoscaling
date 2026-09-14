package pipeline

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/testutil"
)

// boolPtr is a helper to create a pointer to a bool value
func boolPtr(b bool) *bool {
	return &b
}

var _ = Describe("Enforcer", func() {
	var (
		ctx      context.Context
		enforcer *Enforcer
	)

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("EnforcePolicyOnDecisions", func() {

		Context("when scale-to-zero is enabled", func() {

			Context("and there are no requests", func() {
				It("should set all matching decisions to zero", func() {
					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 0, nil
					})
					decisions := []domain.VariantDecision{
						{VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 2, Action: domain.ActionNoChange},
						{VariantName: "variant-b", ModelID: "test-model", Namespace: "test-ns", Cost: 2.0, CurrentReplicas: 1, TargetReplicas: 3, Action: domain.ActionScaleUp},
					}
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {EnableScaleToZero: boolPtr(true), RetentionPeriod: "10m"},
					}

					applied := enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, nil, "cost-aware")

					Expect(applied).To(BeTrue())
					Expect(decisions[0].TargetReplicas).To(Equal(0))
					Expect(decisions[0].Action).To(Equal(domain.ActionScaleDown))
					Expect(decisions[1].TargetReplicas).To(Equal(0))
					Expect(decisions[1].Action).To(Equal(domain.ActionScaleDown))
				})
			})

			Context("and there are requests", func() {
				It("should keep decisions unchanged", func() {
					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 10, nil
					})
					decision := domain.VariantDecision{
						VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 3, Action: domain.ActionScaleUp,
					}
					decision.SetDecisionReason(domain.ActionScaleUp, domain.DecisionReasonTest, string(domain.DecisionReasonTest))
					decisions := []domain.VariantDecision{decision}
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {EnableScaleToZero: boolPtr(true), RetentionPeriod: "10m"},
					}

					applied := enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, nil, "cost-aware")

					Expect(applied).To(BeFalse())
					Expect(decisions[0].TargetReplicas).To(Equal(3))
					Expect(decisions[0].Action).To(Equal(domain.ActionScaleUp))
					Expect(decisions[0].Reason()).To(Equal(string(domain.DecisionReasonTest)))
				})
			})

			Context("and request count query fails", func() {
				It("should keep decisions unchanged and record error metric", func() {
					// Create fresh registry for this test
					registry := prometheus.NewRegistry()
					Expect(metrics.InitMetrics(registry)).To(Succeed())

					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 0, errors.New("prometheus unavailable")
					})
					decisions := []domain.VariantDecision{
						{VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 2},
					}
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {EnableScaleToZero: boolPtr(true), RetentionPeriod: "10m"},
					}

					applied := enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, nil, "cost-aware")

					Expect(applied).To(BeFalse())
					Expect(decisions[0].TargetReplicas).To(Equal(2))

					// Verify error metric was recorded
					count := testutil.GetErrorMetricValue(registry, constants.ComponentEnforcer, "Failed to get request count, keeping current decisions")
					Expect(count).To(BeNumerically(">", 0))
				})
			})
		})

		Context("when scale-to-zero is disabled", func() {

			Context("and all targets are zero", func() {
				It("should preserve minimum replica on the cheapest variant", func() {
					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 0, nil
					})
					decisions := []domain.VariantDecision{
						{VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 2.0, CurrentReplicas: 0, TargetReplicas: 0},
						{VariantName: "variant-b", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 0, TargetReplicas: 0},
					}
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {EnableScaleToZero: boolPtr(false)},
					}

					enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, nil, "cost-aware")

					Expect(decisions[0].TargetReplicas).To(Equal(0)) // expensive
					Expect(decisions[1].TargetReplicas).To(Equal(1)) // cheapest gets 1
					Expect(decisions[1].Action).To(Equal(domain.ActionScaleUp))
				})
			})

			Context("and some targets have replicas", func() {
				It("should keep decisions unchanged", func() {
					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 0, nil
					})
					decision1 := domain.VariantDecision{
						VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 2.0, CurrentReplicas: 2, TargetReplicas: 2, Action: domain.ActionNoChange,
					}
					decision1.SetDecisionReason(domain.ActionNoChange, domain.DecisionReasonTest, string(domain.DecisionReasonTest))
					decisions := []domain.VariantDecision{
						decision1,
						{VariantName: "variant-b", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 0, TargetReplicas: 0},
					}
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {EnableScaleToZero: boolPtr(false)},
					}

					enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, nil, "cost-aware")

					Expect(decisions[0].TargetReplicas).To(Equal(2))
					Expect(decisions[0].Reason()).To(Equal(string(domain.DecisionReasonTest)))
					Expect(decisions[1].TargetReplicas).To(Equal(0))
				})
			})

			Context("and variants have equal cost", func() {
				It("should use alphabetical order as tiebreaker", func() {
					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 0, nil
					})
					decisions := []domain.VariantDecision{
						{VariantName: "variant-z", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 0, TargetReplicas: 0},
						{VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 0, TargetReplicas: 0},
					}
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {EnableScaleToZero: boolPtr(false)},
					}

					enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, nil, "cost-aware")

					Expect(decisions[0].TargetReplicas).To(Equal(0)) // variant-z
					Expect(decisions[1].TargetReplicas).To(Equal(1)) // variant-a (alphabetically first)
				})
			})
		})

		Context("model filtering", func() {

			It("should only modify decisions matching modelID and namespace", func() {
				enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
					return 0, nil
				})
				d1 := domain.VariantDecision{
					VariantName: "v1", ModelID: "model-1", Namespace: "ns-1", Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 3, Action: domain.ActionScaleUp,
				}
				d1.SetDecisionReason(domain.ActionScaleUp, domain.DecisionReasonTest, string(domain.DecisionReasonTest))
				d2 := domain.VariantDecision{
					VariantName: "v2", ModelID: "model-2", Namespace: "ns-1", Cost: 1.0, CurrentReplicas: 1, TargetReplicas: 2, Action: domain.ActionScaleUp,
				}
				d2.SetDecisionReason(domain.ActionScaleUp, domain.DecisionReasonTest, string(domain.DecisionReasonTest))
				d3 := domain.VariantDecision{
					VariantName: "v3", ModelID: "model-1", Namespace: "ns-2", Cost: 1.0, CurrentReplicas: 1, TargetReplicas: 1, Action: domain.ActionNoChange,
				}
				d3.SetDecisionReason(domain.ActionNoChange, domain.DecisionReasonTest, string(domain.DecisionReasonTest))
				decisions := []domain.VariantDecision{d1, d2, d3}
				scaleToZeroConfig := config.ScaleToZeroConfigData{
					"model-1": {EnableScaleToZero: boolPtr(true), RetentionPeriod: "10m"},
				}

				applied := enforcer.EnforcePolicyOnDecisions(ctx, "model-1", "ns-1", decisions, scaleToZeroConfig, nil, "cost-aware")

				Expect(applied).To(BeTrue())
				// model-1/ns-1 → scaled to zero
				Expect(decisions[0].TargetReplicas).To(Equal(0))
				Expect(decisions[0].Action).To(Equal(domain.ActionScaleDown))
				// model-2/ns-1 → untouched
				Expect(decisions[1].TargetReplicas).To(Equal(2))
				Expect(decisions[1].Action).To(Equal(domain.ActionScaleUp))
				Expect(decisions[1].Reason()).To(Equal(string(domain.DecisionReasonTest)))
				// model-1/ns-2 → untouched (different namespace)
				Expect(decisions[2].TargetReplicas).To(Equal(1))
				Expect(decisions[2].Action).To(Equal(domain.ActionNoChange))
				Expect(decisions[2].Reason()).To(Equal(string(domain.DecisionReasonTest)))
			})
		})

		Context("reason strings", func() {

			It("should include optimizer name in enforced reason", func() {
				enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
					return 0, nil
				})
				decisions := []domain.VariantDecision{
					{VariantName: "v1", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 2},
				}
				scaleToZeroConfig := config.ScaleToZeroConfigData{
					"test-model": {EnableScaleToZero: boolPtr(true), RetentionPeriod: "10m"},
				}

				enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, nil, "greedy-by-saturation")

				Expect(decisions[0].Reason()).To(ContainSubstring("greedy-by-saturation"))
				Expect(decisions[0].Reason()).To(ContainSubstring("enforced"))
			})
		})

		Context("metrics emission", func() {
			var (
				registry *prometheus.Registry
			)

			BeforeEach(func() {
				// Create a fresh registry for each test
				registry = prometheus.NewRegistry()
				err := metrics.InitMetrics(registry)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should emit metric when enforcing scale-to-zero", func() {
				enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
					return 0, nil
				})
				decisions := []domain.VariantDecision{
					{VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 2},
				}
				scaleToZeroConfig := config.ScaleToZeroConfigData{
					"test-model": {EnableScaleToZero: boolPtr(true), RetentionPeriod: "10m"},
				}

				enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, nil, "cost-aware")

				// Verify metric was emitted
				metricFamilies, err := registry.Gather()
				Expect(err).NotTo(HaveOccurred())

				var found bool
				for _, mf := range metricFamilies {
					if mf.GetName() == constants.WVAEnforcerModificationsTotal {
						found = true
						// Should have at least one metric
						Expect(mf.GetMetric()).NotTo(BeEmpty())
						// Check for scale_to_zero policy type
						for _, m := range mf.GetMetric() {
							for _, label := range m.GetLabel() {
								if label.GetName() == constants.LabelPolicyType && label.GetValue() == "scale_to_zero" {
									counter := m.GetCounter()
									Expect(counter).NotTo(BeNil())
									Expect(counter.GetValue()).To(BeNumerically(">", 0))
								}
							}
						}
					}
				}
				Expect(found).To(BeTrue(), "enforcer metric should be emitted")
			})

			It("should emit metric when enforcing minimum replica", func() {
				enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
					return 0, nil
				})
				decisions := []domain.VariantDecision{
					{VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 0, TargetReplicas: 0},
				}
				scaleToZeroConfig := config.ScaleToZeroConfigData{
					"test-model": {EnableScaleToZero: boolPtr(false)},
				}

				enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, nil, "cost-aware")

				// Verify metric was emitted
				metricFamilies, err := registry.Gather()
				Expect(err).NotTo(HaveOccurred())

				var found bool
				for _, mf := range metricFamilies {
					if mf.GetName() == constants.WVAEnforcerModificationsTotal {
						found = true
						// Check for minimum_replicas policy type
						for _, m := range mf.GetMetric() {
							for _, label := range m.GetLabel() {
								if label.GetName() == constants.LabelPolicyType && label.GetValue() == "minimum_replicas" {
									counter := m.GetCounter()
									Expect(counter).NotTo(BeNil())
									Expect(counter.GetValue()).To(BeNumerically(">", 0))
								}
							}
						}
					}
				}
				Expect(found).To(BeTrue(), "enforcer metric should be emitted")
			})

			It("should not emit metric when no enforcement is needed", func() {
				enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
					return 10, nil // Has requests
				})
				decisions := []domain.VariantDecision{
					{VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 3},
				}
				scaleToZeroConfig := config.ScaleToZeroConfigData{
					"test-model": {EnableScaleToZero: boolPtr(true), RetentionPeriod: "10m"},
				}

				enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, nil, "cost-aware")

				// Verify no metric was emitted (counter should be empty or zero)
				metricFamilies, err := registry.Gather()
				Expect(err).NotTo(HaveOccurred())

				for _, mf := range metricFamilies {
					if mf.GetName() == constants.WVAEnforcerModificationsTotal {
						// Metrics should be empty since no enforcement was applied
						Expect(mf.GetMetric()).To(BeEmpty(), "no enforcement should mean no metrics emitted")
					}
				}
			})
		})
	})
})
