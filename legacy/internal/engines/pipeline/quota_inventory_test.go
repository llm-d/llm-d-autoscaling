package pipeline

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

func newClusterQuotaInv(quotas map[string]int) *QuotaInventory {
	return NewQuotaInventory(config.QuotaLimiterConfig{
		Name:          "cluster-quota",
		Type:          "quota",
		Scope:         config.QuotaScopeCluster,
		ClusterQuotas: quotas,
	})
}

func newNamespaceQuotaInv(quotas map[string]map[string]int, exclude []string) *QuotaInventory {
	return NewQuotaInventory(config.QuotaLimiterConfig{
		Name:            "namespace-quota",
		Type:            "quota",
		Scope:           config.QuotaScopeNamespace,
		NamespaceQuotas: quotas,
		Exclude:         exclude,
	})
}

func quotaDecisionFor(namespace, accType string) *domain.VariantDecision {
	return &domain.VariantDecision{
		Namespace:       namespace,
		AcceleratorName: accType,
		VariantName:     "v",
	}
}

var _ = Describe("QuotaInventory", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("Inventory interface contract", func() {
		It("satisfies the Inventory interface", func() {
			var _ Inventory = (*QuotaInventory)(nil)
		})

		It("satisfies the NamespaceAwareInventory interface", func() {
			var _ NamespaceAwareInventory = (*QuotaInventory)(nil)
		})

		It("returns the configured name", func() {
			inv := NewQuotaInventory(config.QuotaLimiterConfig{
				Name:          "my-quota",
				Type:          "quota",
				Scope:         config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 1},
			})
			Expect(inv.Name()).To(Equal("my-quota"))
		})

		It("treats Refresh as a no-op", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 1})
			Expect(inv.Refresh(ctx)).To(Succeed())
		})
	})

	Describe("cluster scope", func() {
		It("grants up to the cap then exhausts the pool", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 4, "A100": 2})
			alloc := inv.CreateAllocator(ctx)

			got, err := alloc.TryAllocate(ctx, quotaDecisionFor("team-a", "H100"), 3)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(3))

			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("team-b", "H100"), 5)
			Expect(got).To(Equal(1), "cap=4, used=3, expected 1 remaining")

			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("team-c", "H100"), 1)
			Expect(got).To(Equal(0), "pool exhausted")

			// A100 has its own independent pool.
			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("team-a", "A100"), 2)
			Expect(got).To(Equal(2))
		})

		It("denies unknown types and passes through unlimited", func() {
			inv := newClusterQuotaInv(map[string]int{
				"H100": config.QuotaUnlimited,
				"A100": 2,
			})
			alloc := inv.CreateAllocator(ctx)

			got, _ := alloc.TryAllocate(ctx, quotaDecisionFor("any", "L40S"), 4)
			Expect(got).To(Equal(0), "unknown type denied")

			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("any", "H100"), 999)
			Expect(got).To(Equal(999), "unlimited pass-through")

			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("any", "H100"), 100)
			Expect(got).To(Equal(100), "unlimited has no accounting")
		})

		It("respects SetUsed", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 8})
			inv.SetUsed(map[string]int{"H100": 6})

			alloc := inv.CreateAllocator(ctx)
			got, _ := alloc.TryAllocate(ctx, quotaDecisionFor("any", "H100"), 5)
			Expect(got).To(Equal(2), "cap=8, used=6, expected 2 remaining")

			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("any", "H100"), 1)
			Expect(got).To(Equal(0))
		})
	})

	Describe("namespace scope", func() {
		It("grants up to per-namespace caps and exhausts independently", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a": {"H100": 4},
				"team-b": {"H100": 2},
			}, nil)
			alloc := inv.CreateAllocator(ctx)

			got, _ := alloc.TryAllocate(ctx, quotaDecisionFor("team-a", "H100"), 3)
			Expect(got).To(Equal(3))

			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("team-b", "H100"), 2)
			Expect(got).To(Equal(2), "team-b independent budget")

			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("team-a", "H100"), 5)
			Expect(got).To(Equal(1))

			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("team-a", "H100"), 1)
			Expect(got).To(Equal(0), "team-a exhausted")
		})

		It("passes through excluded namespaces without recording usage", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a": {"H100": 1},
			}, []string{"kube-system"})
			alloc := inv.CreateAllocator(ctx)

			got, _ := alloc.TryAllocate(ctx, quotaDecisionFor("kube-system", "H100"), 50)
			Expect(got).To(Equal(50), "excluded namespace pass-through")

			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("team-a", "H100"), 1)
			Expect(got).To(Equal(1), "team-a unaffected by excluded call")
		})

		It("falls through to the default key with per-namespace budgets", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a":                                {"H100": 4},
				config.QuotaLimiterReservedNamespaceKey: {"H100": 2},
			}, nil)
			alloc := inv.CreateAllocator(ctx)

			// Each unlisted namespace gets its OWN budget at the default level
			// (Kubernetes LimitRange convention, not a shared pool).
			got, _ := alloc.TryAllocate(ctx, quotaDecisionFor("team-z", "H100"), 5)
			Expect(got).To(Equal(2))

			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("team-y", "H100"), 5)
			Expect(got).To(Equal(2), "second unlisted namespace has its own default budget")

			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("team-z", "H100"), 1)
			Expect(got).To(Equal(0), "team-z bucket already exhausted")

			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("team-a", "H100"), 4)
			Expect(got).To(Equal(4), "exact match wins over default")
		})

		It("denies missing namespaces under a strict allowlist (no default)", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a": {"H100": 4},
			}, nil)
			alloc := inv.CreateAllocator(ctx)

			got, _ := alloc.TryAllocate(ctx, quotaDecisionFor("team-b", "H100"), 1)
			Expect(got).To(Equal(0), "strict allowlist: unlisted namespace denied")

			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("team-a", "H100"), 4)
			Expect(got).To(Equal(4))
		})

		It("treats QuotaUnlimited as pass-through per namespace", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-priority": {"H100": config.QuotaUnlimited},
				"team-capped":   {"H100": 2},
			}, nil)
			alloc := inv.CreateAllocator(ctx)

			got, _ := alloc.TryAllocate(ctx, quotaDecisionFor("team-priority", "H100"), 1000)
			Expect(got).To(Equal(1000))

			got, _ = alloc.TryAllocate(ctx, quotaDecisionFor("team-capped", "H100"), 5)
			Expect(got).To(Equal(2), "unlimited usage in another namespace does not leak")
		})

		It("respects SetUsedByNamespace", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a": {"H100": 4},
			}, nil)
			inv.SetUsedByNamespace(map[string]map[string]int{
				"team-a": {"H100": 3},
			})
			alloc := inv.CreateAllocator(ctx)
			got, _ := alloc.TryAllocate(ctx, quotaDecisionFor("team-a", "H100"), 5)
			Expect(got).To(Equal(1), "cap=4, used=3, expected 1 remaining")
		})
	})

	Describe("error and edge cases", func() {
		It("returns an error on a nil decision", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 4})
			alloc := inv.CreateAllocator(ctx)
			_, err := alloc.TryAllocate(ctx, nil, 1)
			Expect(err).To(HaveOccurred())
		})

		It("short-circuits on a zero-GPU request", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 4})
			alloc := inv.CreateAllocator(ctx)
			got, err := alloc.TryAllocate(ctx, quotaDecisionFor("team-a", "H100"), 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(0))
		})

		It("denies (fails closed) when the accelerator type is unresolved", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 4})
			alloc := inv.CreateAllocator(ctx)
			d := quotaDecisionFor("team-a", "")
			got, err := alloc.TryAllocate(ctx, d, 7)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(0), "unresolved accelerator must not bypass the quota")
			Expect(d.DecisionSteps).To(HaveLen(1))
			Expect(d.DecisionSteps[0].Reason).To(ContainSubstring("accelerator type unresolved"))
			Expect(d.DecisionSteps[0].WasConstrained).To(BeTrue())
		})
	})

	Describe("DecisionStep tracing (issue #1002 acceptance)", func() {
		It("records a step formatted as `limited by quota[scope=cluster, type=...]` when the cluster cap binds", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 2})
			alloc := inv.CreateAllocator(ctx)
			d := quotaDecisionFor("team-a", "H100")

			_, err := alloc.TryAllocate(ctx, d, 5)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.DecisionSteps).To(HaveLen(1))
			Expect(d.DecisionSteps[0].Name).To(Equal("cluster-quota"))
			Expect(d.DecisionSteps[0].Reason).To(Equal("limited by quota[scope=cluster, type=H100]"))
			Expect(d.DecisionSteps[0].WasConstrained).To(BeTrue())
		})

		It("records a step formatted as `limited by quota[scope=namespace, namespace=..., type=...]` when the namespace cap binds", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a": {"H100": 2},
			}, nil)
			alloc := inv.CreateAllocator(ctx)
			d := quotaDecisionFor("team-a", "H100")

			_, err := alloc.TryAllocate(ctx, d, 5)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.DecisionSteps).To(HaveLen(1))
			Expect(d.DecisionSteps[0].Reason).To(Equal("limited by quota[scope=namespace, namespace=team-a, type=H100]"))
			Expect(d.DecisionSteps[0].WasConstrained).To(BeTrue())
		})

		It("does NOT record a step for pass-through allocations (full grant)", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 10})
			alloc := inv.CreateAllocator(ctx)
			d := quotaDecisionFor("team-a", "H100")

			_, err := alloc.TryAllocate(ctx, d, 5)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.DecisionSteps).To(BeEmpty(), "no constraint applied, no step")
		})

		It("does NOT record a step for excluded namespaces", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a": {"H100": 1},
			}, []string{"kube-system"})
			alloc := inv.CreateAllocator(ctx)
			d := quotaDecisionFor("kube-system", "H100")

			_, err := alloc.TryAllocate(ctx, d, 50)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.DecisionSteps).To(BeEmpty())
		})

		It("does NOT record a step for unlimited quotas", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": config.QuotaUnlimited})
			alloc := inv.CreateAllocator(ctx)
			d := quotaDecisionFor("team-a", "H100")

			_, err := alloc.TryAllocate(ctx, d, 999)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.DecisionSteps).To(BeEmpty())
		})

		It("denies with a step when a cluster quota of 0 is a hard deny (distinct from unlimited)", func() {
			// Locks in that 0 is a real deny cap, not folded into the unlimited
			// pass-through path — a regression that did so would turn a hard-deny
			// quota into a pass-through and no other spec would catch it.
			inv := newClusterQuotaInv(map[string]int{"H100": 0})
			alloc := inv.CreateAllocator(ctx)
			d := quotaDecisionFor("team-a", "H100")

			got, err := alloc.TryAllocate(ctx, d, 5)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(0), "a cluster cap of 0 denies all allocation")
			Expect(d.DecisionSteps).To(HaveLen(1))
			Expect(d.DecisionSteps[0].Reason).To(Equal("limited by quota[scope=cluster, type=H100]"))
			Expect(d.DecisionSteps[0].WasConstrained).To(BeTrue())
		})

		It("denies with a step when a namespace quota of 0 is a hard deny", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a": {"H100": 0},
			}, nil)
			alloc := inv.CreateAllocator(ctx)
			d := quotaDecisionFor("team-a", "H100")

			got, err := alloc.TryAllocate(ctx, d, 5)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(0), "a namespace cap of 0 denies all allocation")
			Expect(d.DecisionSteps).To(HaveLen(1))
			Expect(d.DecisionSteps[0].Reason).To(Equal("limited by quota[scope=namespace, namespace=team-a, type=H100]"))
			Expect(d.DecisionSteps[0].WasConstrained).To(BeTrue())
		})
	})

	Describe("combined cluster + namespace", func() {
		// Two independent QuotaInventory instances, composed by the test
		// harness in the simplest way (cluster first, then namespace fed the
		// cluster's grant). Verifies the cluster cap is felt across namespace
		// boundaries. Full chain orchestration is sub-issue #1003.
		It("yields min(cluster, namespace) when each is enforced in turn", func() {
			clusterInv := newClusterQuotaInv(map[string]int{"H100": 4})
			nsInv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a": {"H100": 3},
				"team-b": {"H100": 3},
			}, nil)

			clusterAlloc := clusterInv.CreateAllocator(ctx)
			nsAlloc := nsInv.CreateAllocator(ctx)

			tryBoth := func(ns string, req int) int {
				gotCluster, _ := clusterAlloc.TryAllocate(ctx, quotaDecisionFor(ns, "H100"), req)
				gotNS, _ := nsAlloc.TryAllocate(ctx, quotaDecisionFor(ns, "H100"), gotCluster)
				return gotNS
			}

			Expect(tryBoth("team-a", 3)).To(Equal(3), "team-a fits in both caps")
			Expect(tryBoth("team-b", 3)).To(Equal(1), "cluster cap pinches team-b (cluster left=1)")
			Expect(tryBoth("team-b", 1)).To(Equal(0), "cluster exhausted")
		})
	})

	Describe("GetResourcePools", func() {
		It("returns per-type pools for cluster scope, emitting unlimited as a sentinel and keeping zero caps", func() {
			inv := newClusterQuotaInv(map[string]int{
				"H100": 8,
				"A100": config.QuotaUnlimited,
				"L40S": 0,
			})
			inv.SetUsed(map[string]int{"H100": 3})

			pools := inv.GetResourcePools()
			Expect(pools).To(HaveKey("H100"))
			Expect(pools["H100"].Limit).To(Equal(8))
			Expect(pools["H100"].Used).To(Equal(3))
			Expect(pools).To(HaveKey("A100"), "unlimited is emitted as a sentinel so the V2 optimizer can distinguish it from an unconfigured (deny) type")
			Expect(pools["A100"].Limit).To(Equal(config.QuotaUnlimited), "unlimited is carried as the QuotaUnlimited sentinel")
			Expect(pools).To(HaveKey("L40S"))
			Expect(pools["L40S"].Limit).To(Equal(0), "a quota of 0 is a real deny cap, distinct from unlimited")
		})

		It("sums per-type across listed namespaces for namespace scope, skipping default and excluded", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a":                                {"H100": 4},
				"team-b":                                {"H100": 2, "A100": 3},
				"kube-system":                           {"H100": 99},
				config.QuotaLimiterReservedNamespaceKey: {"H100": 1},
			}, []string{"kube-system"})

			pools := inv.GetResourcePools()
			Expect(pools["H100"].Limit).To(Equal(6), "team-a(4)+team-b(2); default and excluded kube-system skipped")
			Expect(pools["A100"].Limit).To(Equal(3))
			Expect(pools).NotTo(HaveKey("team-a/H100"), "namespace scope no longer emits composite keys")
		})
	})

	Describe("NamespaceResourcePools", func() {
		It("returns per-(namespace,type) caps, applying default fall-through and excludes, with unlimited as a sentinel", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a":                                {"H100": 4, "A100": config.QuotaUnlimited},
				"kube-system":                           {"H100": 99},
				config.QuotaLimiterReservedNamespaceKey: {"H100": 2},
			}, []string{"kube-system"})
			inv.SetUsedByNamespace(map[string]map[string]int{"team-a": {"H100": 1}})

			// team-a explicit; team-z unlisted (falls through to default);
			// kube-system excluded.
			pools := inv.NamespaceResourcePools([]string{"team-a", "team-z", "kube-system"})

			Expect(pools["team-a"]).To(HaveKeyWithValue("H100", ResourcePool{Limit: 4, Used: 1}))
			Expect(pools["team-a"]).To(HaveKeyWithValue("A100", ResourcePool{Limit: config.QuotaUnlimited, Used: 0}),
				"unlimited cap emitted as a sentinel, not omitted, so it stays distinguishable from an unlisted (denied) type")
			Expect(pools["team-z"]).To(HaveKeyWithValue("H100", ResourcePool{Limit: 2, Used: 0}), "default fall-through cap")
			Expect(pools).NotTo(HaveKey("kube-system"), "excluded namespace omitted (open / pass-through)")
		})

		It("materializes a namespace with neither an explicit quota nor a default as an empty deny-all allowlist", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a": {"H100": 4},
			}, nil)

			// team-z is unlisted and there is no "default" fall-through.
			pools := inv.NamespaceResourcePools([]string{"team-a", "team-z"})

			Expect(pools).To(HaveKey("team-z"), "present (closed allowlist) so the optimizer denies it")
			Expect(pools["team-z"]).To(BeEmpty(), "no listed types — a real deny-all")
		})

		It("returns nil for cluster scope", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 8})
			Expect(inv.NamespaceResourcePools([]string{"team-a"})).To(BeNil())
		})
	})

	Describe("totals", func() {
		It("excludes unlimited entries from cluster scope totals", func() {
			inv := newClusterQuotaInv(map[string]int{
				"H100": 8,
				"A100": config.QuotaUnlimited,
				"L40S": 2,
			})
			inv.SetUsed(map[string]int{"H100": 3, "L40S": 1})

			Expect(inv.TotalLimit()).To(Equal(10))
			Expect(inv.TotalUsed()).To(Equal(4))
			Expect(inv.TotalAvailable()).To(Equal(6))
		})

		It("skips default and excluded namespaces in namespace scope", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a":                                {"H100": 4},
				"team-b":                                {"H100": 2},
				"kube-system":                           {"H100": 99},
				config.QuotaLimiterReservedNamespaceKey: {"H100": 1},
			}, []string{"kube-system"})

			Expect(inv.TotalLimit()).To(Equal(6), "sum of team-a + team-b only")
		})
	})

	Describe("Remaining", func() {
		It("sums available across pools and treats unlimited as zero contribution", func() {
			inv := newClusterQuotaInv(map[string]int{
				"H100": 4,
				"A100": 2,
				"L40S": config.QuotaUnlimited,
			})
			alloc := inv.CreateAllocator(ctx)

			Expect(alloc.Remaining()).To(Equal(6))

			_, err := alloc.TryAllocate(ctx, quotaDecisionFor("x", "H100"), 3)
			Expect(err).NotTo(HaveOccurred())
			Expect(alloc.Remaining()).To(Equal(3))
		})

		It("namespace scope: sums listed namespaces only, skipping excluded and unlimited", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a":                                {"H100": 4, "A100": config.QuotaUnlimited},
				"kube-system":                           {"H100": 99},
				config.QuotaLimiterReservedNamespaceKey: {"H100": 1},
			}, []string{"kube-system"})
			alloc := inv.CreateAllocator(ctx)

			// team-a H100 (4) only; A100 unlimited contributes 0; excluded
			// kube-system and the reserved default key contribute 0.
			Expect(alloc.Remaining()).To(Equal(4))
		})

		It("namespace scope: over-reports when a default-fallback namespace allocates (documented limitation)", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a":                                {"H100": 4},
				config.QuotaLimiterReservedNamespaceKey: {"H100": 2},
			}, nil)
			alloc := inv.CreateAllocator(ctx)

			// team-z is unlisted and allocates 2 via the "default" fallback.
			got, err := alloc.TryAllocate(ctx, quotaDecisionFor("team-z", "H100"), 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(2))

			// Remaining iterates the listed NamespaceQuotas keys only (team-a, and
			// the reserved default key which is skipped), so team-z's usage is not
			// subtracted. It reports team-a's full 4 — the documented over-report.
			Expect(alloc.Remaining()).To(Equal(4),
				"default-fallback namespace usage is not subtracted from Remaining")
		})
	})

	Describe("SetUsed / SetUsedByNamespace scope guards", func() {
		It("namespace-scoped inventory ignores cluster-wide SetUsed and uses per-namespace usage", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{"team-a": {"H100": 4}}, nil)

			// Cluster-wide SetUsed must be a no-op for a namespace-scoped inventory.
			inv.SetUsed(map[string]int{"H100": 99})
			Expect(inv.TotalUsed()).To(Equal(0), "cluster SetUsed ignored at namespace scope")

			inv.SetUsedByNamespace(map[string]map[string]int{"team-a": {"H100": 3}})
			alloc := inv.CreateAllocator(ctx)
			// 4 cap − 3 used = 1 remaining for team-a/H100.
			got, err := alloc.TryAllocate(ctx, quotaDecisionFor("team-a", "H100"), 5)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(1), "only SetUsedByNamespace usage applies")
		})

		It("cluster-scoped inventory ignores per-namespace SetUsedByNamespace", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 4})

			// Per-namespace usage must be a no-op for a cluster-scoped inventory.
			inv.SetUsedByNamespace(map[string]map[string]int{"team-a": {"H100": 99}})
			alloc := inv.CreateAllocator(ctx)
			// Full 4 still available — the bogus per-namespace usage was ignored.
			got, err := alloc.TryAllocate(ctx, quotaDecisionFor("team-a", "H100"), 4)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(4), "cluster scope ignores SetUsedByNamespace")
		})
	})

	Describe("more edge cases", func() {
		It("TotalAvailable clamps to 0 when usage exceeds the configured quota", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 2})
			inv.SetUsed(map[string]int{"H100": 5}) // over-used
			Expect(inv.TotalAvailable()).To(Equal(0), "never reports negative availability")
		})

		It("TryAllocate errors on an unknown allocator scope", func() {
			inv := NewQuotaInventory(config.QuotaLimiterConfig{
				Name: "bad", Type: "quota", Scope: config.QuotaScope("bogus"),
			})
			alloc := inv.CreateAllocator(ctx)
			_, err := alloc.TryAllocate(ctx, quotaDecisionFor("team-a", "H100"), 1)
			Expect(err).To(MatchError(ContainSubstring("unknown scope")))
		})
	})
})
