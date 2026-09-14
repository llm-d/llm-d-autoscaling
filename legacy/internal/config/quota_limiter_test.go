package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("QuotaLimiterConfig", func() {

	Context("IsExcluded", func() {
		DescribeTable("namespace exclusion",
			func(entry QuotaLimiterConfig, namespace string, want bool) {
				Expect(entry.IsExcluded(namespace)).To(Equal(want))
			},
			Entry("cluster scope never excludes",
				QuotaLimiterConfig{Scope: QuotaScopeCluster, Exclude: []string{"any"}},
				"any", false),
			Entry("namespace scope matches exact entry",
				QuotaLimiterConfig{Scope: QuotaScopeNamespace, Exclude: []string{"kube-system", "llm-d-system"}},
				"kube-system", true),
			Entry("namespace scope returns false for unlisted namespace",
				QuotaLimiterConfig{Scope: QuotaScopeNamespace, Exclude: []string{"kube-system"}},
				"team-a", false),
		)
	})

	Context("QuotaForNamespace", func() {
		var entry QuotaLimiterConfig

		BeforeEach(func() {
			entry = QuotaLimiterConfig{
				Name:  "ns-quota",
				Type:  "quota",
				Scope: QuotaScopeNamespace,
				Exclude: []string{
					"kube-system",
				},
				NamespaceQuotas: map[string]map[string]int{
					"team-a": {
						"H100": 8,
						"A100": 4,
					},
					"team-priority": {
						"H100": QuotaUnlimited,
					},
					QuotaLimiterReservedNamespaceKey: {
						"H100": 2,
					},
				},
			}
		})

		It("returns excluded=true and nil map for excluded namespace", func() {
			quotas, excluded := entry.QuotaForNamespace("kube-system")
			Expect(excluded).To(BeTrue())
			Expect(quotas).To(BeNil())
		})

		It("returns exact match in preference to the default key", func() {
			quotas, excluded := entry.QuotaForNamespace("team-a")
			Expect(excluded).To(BeFalse())
			Expect(quotas).To(HaveKeyWithValue("H100", 8))
		})

		It("preserves QuotaUnlimited verbatim", func() {
			quotas, _ := entry.QuotaForNamespace("team-priority")
			Expect(quotas).To(HaveKeyWithValue("H100", QuotaUnlimited))
		})

		It("falls through to the default key for unlisted namespaces", func() {
			quotas, _ := entry.QuotaForNamespace("unknown")
			Expect(quotas).To(HaveKeyWithValue("H100", 2))
		})

		It("returns an empty map for strict allowlist (no default key)", func() {
			strict := QuotaLimiterConfig{
				Scope: QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{
					"team-a": {"H100": 4},
				},
			}
			quotas, excluded := strict.QuotaForNamespace("team-b")
			Expect(excluded).To(BeFalse())
			Expect(quotas).To(BeEmpty())
		})

		It("returns (nil, false) for cluster scope", func() {
			cluster := QuotaLimiterConfig{
				Scope:         QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 16},
			}
			quotas, excluded := cluster.QuotaForNamespace("any")
			Expect(excluded).To(BeFalse())
			Expect(quotas).To(BeNil())
		})
	})
})

var _ = Describe("QuotaLimiterEntries.Validate", func() {

	It("accepts a combined cluster + namespace config", func() {
		entries := &QuotaLimiterEntries{
			Limiters: []QuotaLimiterConfig{
				{
					Name:          "cluster-quota",
					Type:          "quota",
					Scope:         QuotaScopeCluster,
					ClusterQuotas: map[string]int{"H100": 16, "A100": 8},
				},
				{
					Name:    "namespace-quota",
					Type:    "quota",
					Scope:   QuotaScopeNamespace,
					Exclude: []string{"kube-system"},
					NamespaceQuotas: map[string]map[string]int{
						"team-a":                         {"H100": 8},
						"team-b":                         {"H100": QuotaUnlimited},
						QuotaLimiterReservedNamespaceKey: {"H100": 2},
					},
				},
			},
		}
		warnings, err := entries.Validate()
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(BeEmpty())
	})

	It("warns (non-fatal) when a namespace is in both exclude and namespaceQuotas", func() {
		entries := &QuotaLimiterEntries{
			Limiters: []QuotaLimiterConfig{
				{
					Name:    "ns",
					Type:    "quota",
					Scope:   QuotaScopeNamespace,
					Exclude: []string{"team-a"},
					NamespaceQuotas: map[string]map[string]int{
						"team-a": {"H100": 4},
					},
				},
			},
		}
		warnings, err := entries.Validate()
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(HaveLen(1))
		Expect(warnings[0]).To(ContainSubstring("team-a"))
	})

	It("accumulates multiple errors instead of failing fast", func() {
		// Two broken entries: one with wrong type, one with bad scope.
		// errors.Join joins messages with newlines; both should appear.
		entries := &QuotaLimiterEntries{
			Limiters: []QuotaLimiterConfig{
				{Name: "a", Type: "reservation", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 1}},
				{Name: "b", Type: "quota", Scope: "global"},
			},
		}
		_, err := entries.Validate()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`type must be "quota"`))
		Expect(err.Error()).To(ContainSubstring(`scope must be`))
	})

	DescribeTable("error cases",
		func(entries *QuotaLimiterEntries, want string) {
			_, err := entries.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(want))
		},
		Entry("empty name rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{Name: "", Type: "quota", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8}},
			}}, "name must not be empty"),
		Entry("duplicate name rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{Name: "x", Type: "quota", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8}},
				{Name: "x", Type: "quota", Scope: QuotaScopeNamespace, NamespaceQuotas: map[string]map[string]int{"a": {"H100": 1}}},
			}}, "duplicate limiter name"),
		Entry("wrong type rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{Name: "x", Type: "reservation", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8}},
			}}, `type must be "quota"`),
		Entry("unknown scope rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{Name: "x", Type: "quota", Scope: "global"},
			}}, "scope must be"),
		Entry("cluster scope rejects namespaceQuotas",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeCluster,
					NamespaceQuotas: map[string]map[string]int{"a": {"H100": 1}},
				},
			}}, "namespaceQuotas is invalid for cluster scope"),
		Entry("cluster scope rejects exclude",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeCluster,
					ClusterQuotas: map[string]int{"H100": 1},
					Exclude:       []string{"kube-system"},
				},
			}}, "exclude is invalid for cluster scope"),
		Entry("namespace scope rejects cluster quotas",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeNamespace,
					ClusterQuotas: map[string]int{"H100": 8},
				},
			}}, "quotas is invalid for namespace scope"),
		Entry("value < -1 rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeCluster,
					ClusterQuotas: map[string]int{"H100": -5},
				},
			}}, "only -1 is allowed as negative"),
		Entry("value < -1 in namespaceQuotas inner map rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeNamespace,
					NamespaceQuotas: map[string]map[string]int{"team-a": {"H100": -5}},
				},
			}}, "only -1 is allowed as negative"),
		Entry("value above MaxQuotaValue rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeCluster,
					ClusterQuotas: map[string]int{"H100": MaxQuotaValue + 1},
				},
			}}, "exceeds the maximum"),
		Entry("empty accelerator type rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeCluster,
					ClusterQuotas: map[string]int{"": 8},
				},
			}}, "accelerator type name must not be empty"),
		Entry("empty namespace name rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeNamespace,
					NamespaceQuotas: map[string]map[string]int{"": {"H100": 1}},
				},
			}}, "namespaceQuotas contains an empty namespace key"),
		Entry("empty exclude name rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeNamespace,
					Exclude:         []string{""},
					NamespaceQuotas: map[string]map[string]int{"team-a": {"H100": 1}},
				},
			}}, "exclude contains an empty namespace name"),
	)
})
