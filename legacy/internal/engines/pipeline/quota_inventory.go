package pipeline

import (
	"context"
	"fmt"
	"maps"
	"sync"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// QuotaInventory enforces operator-declared GPU quotas independent of physical
// cluster inventory. It implements the Inventory interface for use in the
// limiter pipeline. Each instance is tied to exactly one QuotaLimiterConfig
// scope — a deployment that needs both cluster and namespace caps composes two
// QuotaInventory instances via the limiter chain (sub-issue #1003).
//
// Quota values are static; QuotaInventory.Refresh is a no-op because there's
// no external source to discover. Usage is tracked the same way as
// TypeInventory: callers invoke SetUsed before each cycle, then CreateAllocator
// for the per-cycle ResourceAllocator.
//
// QuotaInventory is decoupled from physical inventory. The limiter chain
// composes a QuotaInventory with TypeInventory so allocations are bounded by
// min(physical, quota); used standalone, no physical bound is enforced.
type QuotaInventory struct {
	name string
	// cfg is a value copy taken at construction and is immutable thereafter, so
	// it is safe to read (incl. cfg.IsExcluded / cfg.QuotaForNamespace) without
	// holding mu. Only usedByType / usedByNS are mutable and mu-protected.
	cfg config.QuotaLimiterConfig

	mu sync.RWMutex
	// usedByType is the cluster-scoped usage map (Scope == QuotaScopeCluster).
	usedByType map[string]int
	// usedByNS is the per-namespace usage map (Scope == QuotaScopeNamespace).
	// Outer key: namespace; inner key: accelerator type.
	usedByNS map[string]map[string]int
}

// NewQuotaInventory constructs an inventory from a validated config entry.
// The caller is responsible for running config.QuotaLimiterEntries.Validate()
// before this call — the constructor accepts the entry as-is and trusts it.
func NewQuotaInventory(cfg config.QuotaLimiterConfig) *QuotaInventory {
	return &QuotaInventory{
		name:       cfg.Name,
		cfg:        cfg,
		usedByType: make(map[string]int),
		usedByNS:   make(map[string]map[string]int),
	}
}

// Compile-time interface assertions.
var (
	_ Inventory               = (*QuotaInventory)(nil)
	_ NamespaceAwareInventory = (*QuotaInventory)(nil)
	_ ResourceAllocator       = (*quotaAllocator)(nil)
)

// Name returns the limiter identifier (e.g., "cluster-quota"). Used in logs
// and DecisionStep traces (the trace format is `limited by quota[scope=...]`,
// per issue #1002).
func (q *QuotaInventory) Name() string {
	return q.name
}

// Refresh is a no-op for QuotaInventory: quotas are operator-declared via
// ConfigMap and don't have an external source to refresh from. The method
// satisfies the Inventory interface and returns nil to allow uniform
// pipeline orchestration.
func (q *QuotaInventory) Refresh(_ context.Context) error {
	return nil
}

// SetUsed updates the cluster-scoped usage map for cluster-scoped inventories.
// For namespace-scoped inventories this method is a no-op — namespace-scoped
// inventories need per-namespace usage and should use SetUsedByNamespace
// instead (the Inventory interface predates the per-namespace case; rather
// than widen the interface we accept both shapes and only the relevant one
// is consulted).
func (q *QuotaInventory) SetUsed(usedByType map[string]int) {
	if q.cfg.Scope != config.QuotaScopeCluster {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.usedByType = maps.Clone(usedByType)
}

// SetUsedByNamespace updates the per-namespace usage map used by
// namespace-scoped inventories. The outer key is the namespace; the inner
// key is the accelerator type. Excluded namespaces are still tracked but
// CreateAllocator skips enforcement for them.
//
// For cluster-scoped inventories this method is a no-op.
func (q *QuotaInventory) SetUsedByNamespace(usedByNS map[string]map[string]int) {
	if q.cfg.Scope != config.QuotaScopeNamespace {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.usedByNS = copyNestedIntMap(usedByNS)
}

// CreateAllocator returns a ResourceAllocator that enforces this inventory's
// quotas. The allocator is per-cycle: callers create one fresh after each
// SetUsed / SetUsedByNamespace, exhaust it during the allocation pass, then
// discard it.
func (q *QuotaInventory) CreateAllocator(_ context.Context) ResourceAllocator {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return &quotaAllocator{
		name:  q.name,
		scope: q.cfg.Scope,
		// Snapshot the config to keep the allocator self-contained.
		cfg:        q.cfg,
		usedByType: maps.Clone(q.usedByType),
		usedByNS:   copyNestedIntMap(q.usedByNS),
	}
}

// TotalLimit returns the cluster-wide sum of quotas across accelerator types
// for the cluster scope, or the sum of all per-namespace quotas (excluding
// excluded namespaces and the reserved default key) for the namespace scope.
// QuotaUnlimited (-1) is excluded from sums — there is no finite total.
func (q *QuotaInventory) TotalLimit() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.totalLimitLocked()
}

// totalLimitLocked computes TotalLimit. Callers must hold q.mu (read or write).
func (q *QuotaInventory) totalLimitLocked() int {
	total := 0
	switch q.cfg.Scope {
	case config.QuotaScopeCluster:
		for _, v := range q.cfg.ClusterQuotas {
			if v == config.QuotaUnlimited {
				continue
			}
			total += v
		}
	case config.QuotaScopeNamespace:
		for ns, perType := range q.cfg.NamespaceQuotas {
			if ns == config.QuotaLimiterReservedNamespaceKey {
				continue
			}
			if q.cfg.IsExcluded(ns) {
				continue
			}
			for _, v := range perType {
				if v == config.QuotaUnlimited {
					continue
				}
				total += v
			}
		}
	}
	return total
}

// TotalUsed returns the sum of recorded usage, counting only the key set that
// TotalLimit does so the two stay symmetric and TotalAvailable does not
// under-report. At cluster scope that is the accelerator types with a finite
// ClusterQuota; at namespace scope it is the non-excluded, non-"default"
// namespaces and their finite-quota types. Usage of unquotaed, unlimited (-1),
// excluded, or default-fall-through entries is therefore excluded — matching
// TotalLimit, which does not count their caps.
func (q *QuotaInventory) TotalUsed() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.totalUsedLocked()
}

// totalUsedLocked computes TotalUsed. Callers must hold q.mu (read or write).
// Both branches iterate the SAME key set as totalLimitLocked so that TotalUsed
// and TotalLimit cover identical entries.
func (q *QuotaInventory) totalUsedLocked() int {
	total := 0
	switch q.cfg.Scope {
	case config.QuotaScopeCluster:
		// Count usage only for types with a finite cluster quota, mirroring
		// totalLimitLocked. Usage of unquotaed types (real replicas with no
		// configured cap) must not be subtracted from the quotaed budget.
		for accType, limit := range q.cfg.ClusterQuotas {
			if limit == config.QuotaUnlimited {
				continue
			}
			total += q.usedByType[accType]
		}
	case config.QuotaScopeNamespace:
		// Mirror totalLimitLocked: skip the reserved "default" key, excluded
		// namespaces, and unlimited types, so excluded/unlimited/default-
		// fall-through usage is not charged against the finite namespace budget.
		for ns, perType := range q.cfg.NamespaceQuotas {
			if ns == config.QuotaLimiterReservedNamespaceKey {
				continue
			}
			if q.cfg.IsExcluded(ns) {
				continue
			}
			for accType, limit := range perType {
				if limit == config.QuotaUnlimited {
					continue
				}
				total += q.usedByNS[ns][accType]
			}
		}
	}
	return total
}

// TotalAvailable returns max(0, TotalLimit - TotalUsed). Notes the same
// caveat as TotalLimit: unlimited (-1) entries are excluded from both terms,
// so TotalAvailable underreports availability when unlimited quotas are
// configured. Limit and used are read under a single lock so the pair is
// internally consistent.
func (q *QuotaInventory) TotalAvailable() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	avail := q.totalLimitLocked() - q.totalUsedLocked()
	if avail < 0 {
		return 0
	}
	return avail
}

// GetResourcePools returns per-accelerator-type pools. For the cluster scope it
// is one pool per configured type. For the namespace scope it is the per-type
// SUM across explicitly-listed (non-excluded, non-"default") namespaces — i.e.
// the aggregate cluster budget that the per-namespace caps partition. The
// per-(namespace, type) breakdown is exposed separately via
// NamespaceResourcePools; the optimizer combines them as
// min(per-type total, that namespace's cap).
//
// Quota value semantics on the emitted Limit:
//   - Unlimited (-1) entries are emitted as a sentinel ResourcePool{Limit ==
//     QuotaUnlimited} (matching NamespaceResourcePools), so the V2 optimizer can
//     tell "unlimited" (allow up to demand) apart from a type with no configured
//     quota (absent → deny). mergeConstraints branches on Limit < 0 and treats
//     the sentinel as unbounded; TotalLimit/TotalUsed/TotalAvailable exclude it,
//     so the reported totals are unchanged.
//   - A quota of 0 is a real cap (deny) and is emitted with Limit == 0.
func (q *QuotaInventory) GetResourcePools() map[string]ResourcePool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	pools := make(map[string]ResourcePool)
	switch q.cfg.Scope {
	case config.QuotaScopeCluster:
		for accType, limit := range q.cfg.ClusterQuotas {
			// Emit every configured type, including unlimited (-1). An omitted
			// type is read as 0 available by the V2 optimizer and silently
			// denied, which would invert the -1 = unlimited semantic (see the
			// function doc above). The sentinel is carried through
			// mergeConstraints as an unbounded budget.
			pools[accType] = ResourcePool{
				Limit: limit,
				Used:  q.usedByType[accType],
			}
		}
	case config.QuotaScopeNamespace:
		for ns, perType := range q.cfg.NamespaceQuotas {
			if ns == config.QuotaLimiterReservedNamespaceKey {
				continue
			}
			if q.cfg.IsExcluded(ns) {
				continue
			}
			for accType, limit := range perType {
				if limit == config.QuotaUnlimited {
					continue
				}
				pool := pools[accType]
				pool.Limit += limit
				pool.Used += q.usedByNS[ns][accType]
				pools[accType] = pool
			}
		}
	}
	return pools
}

// NamespaceResourcePools implements NamespaceAwareInventory. For the cluster
// scope it returns nil (no namespace dimension). For the namespace scope it
// exposes each active namespace as a CLOSED allowlist that mirrors the V1
// tryAllocateNamespace contract, so namespace quota is enforced identically on
// the V1 Limit() and V2 optimizer paths:
//
//   - An excluded namespace is OMITTED from the result entirely. Its absence
//     signals "open" to the optimizer (no namespace cap; only the cluster/
//     per-type constraint applies), matching V1's pass-through for excludes.
//   - Every other active namespace is always present (even with an empty inner
//     map), which marks it as a closed allowlist. An empty map is a real
//     deny-all (a namespace with neither an explicit quota nor a "default"
//     fall-through): the optimizer allocates it nothing.
//   - Within a present namespace, each listed accelerator type is emitted: a
//     finite cap as ResourcePool{Limit: n} (including 0, a real deny cap), and
//     an unlimited (-1) cap as a sentinel ResourcePool{Limit: QuotaUnlimited}
//     so the optimizer can tell "unlimited" (allow up to the cluster budget)
//     apart from "type not listed" (deny). A type the namespace does not list
//     is simply absent, which the optimizer denies — it does NOT fall through
//     to the cluster aggregate (that fall-through was the cross-namespace leak).
//
// Used counts come from the most recent SetUsedByNamespace call.
func (q *QuotaInventory) NamespaceResourcePools(activeNamespaces []string) map[string]map[string]ResourcePool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.cfg.Scope != config.QuotaScopeNamespace {
		return nil
	}
	out := make(map[string]map[string]ResourcePool)
	for _, ns := range activeNamespaces {
		quotas, excluded := q.cfg.QuotaForNamespace(ns)
		if excluded {
			// Omit: an absent namespace is "open" (pass-through), so the
			// model is bound only by the cluster/per-type constraint.
			continue
		}
		// Always materialize the namespace so it is a closed allowlist; an
		// empty map is a deny-all namespace.
		perType, ok := out[ns]
		if !ok {
			perType = make(map[string]ResourcePool)
			out[ns] = perType
		}
		for accType, limit := range quotas {
			// Emit unlimited as a sentinel (Limit == QuotaUnlimited) rather
			// than dropping it, so the optimizer distinguishes it from a
			// type the namespace does not list (which it must deny).
			perType[accType] = ResourcePool{
				Limit: limit,
				Used:  q.usedByNS[ns][accType],
			}
		}
	}
	return out
}

// quotaAllocator is the per-cycle ResourceAllocator returned by
// QuotaInventory.CreateAllocator. It holds a snapshot of the config and
// current usage; TryAllocate decrements the in-memory usage counters as
// allocations are granted.
//
// The allocator is intentionally tied to one scope. Composing cluster +
// namespace quotas is the limiter chain's job (sub-issue #1003) — one
// allocator per scope, both consulted before a decision is committed.
type quotaAllocator struct {
	name       string
	scope      config.QuotaScope
	cfg        config.QuotaLimiterConfig
	usedByType map[string]int            // cluster scope only
	usedByNS   map[string]map[string]int // namespace scope only
}

// TryAllocate enforces this allocator's quota against the given decision.
// Returns the number of GPUs that may be allocated (between 0 and
// gpusRequested), accounting for both the configured cap and the current
// usage snapshot. On grant, the in-memory usage counter is incremented so
// subsequent calls in the same cycle see the updated remaining quota.
//
// Per-scope semantics:
//   - Cluster: cap is q.cfg.ClusterQuotas[type]; if missing or zero, returns 0.
//   - Namespace: applies the namespace lookup rules from issue #1002:
//     excluded → pass-through (return gpusRequested without recording usage,
//     so this limiter doesn't double-count against another limiter that
//     does enforce); exact match → cap; fall-through to `default` → cap;
//     missing → 0.
//   - Unlimited (-1) on either scope: pass-through.
func (a *quotaAllocator) TryAllocate(ctx context.Context, decision *domain.VariantDecision, gpusRequested int) (int, error) {
	if gpusRequested <= 0 {
		return 0, nil
	}
	if decision == nil {
		return 0, fmt.Errorf("quota allocator %q: decision is nil", a.name)
	}

	accType := decision.AcceleratorName
	if accType == "" {
		// Without a resolved accelerator type we cannot match a per-type quota.
		// Fail CLOSED (deny) rather than passing through: DefaultLimiter runs
		// resolveUnknownAccelerators first, but that only resolves a single-type
		// inventory — a multi-type quota config (the case quotas exist to bound)
		// leaves the type unresolved here, and passing through would let an
		// unattributed request bypass the cap entirely.
		ctrl.LoggerFrom(ctx).WithName("quotaAllocator").V(logging.DEBUG).Info(
			"Denying allocation: accelerator unresolved, cannot enforce quota",
			"limiter", a.name,
			"variant", decision.VariantName,
			"namespace", decision.Namespace,
		)
		decision.AddDecisionStep(a.name,
			fmt.Sprintf("denied by quota[scope=%s]: accelerator type unresolved", a.scope), true)
		return 0, nil
	}

	var granted int
	switch a.scope {
	case config.QuotaScopeCluster:
		granted = a.tryAllocateCluster(accType, gpusRequested)
	case config.QuotaScopeNamespace:
		granted = a.tryAllocateNamespace(decision.Namespace, accType, gpusRequested)
	default:
		return 0, fmt.Errorf("quota allocator %q: unknown scope %q", a.name, a.scope)
	}

	// Record a DecisionStep when the allocator actually capped the request.
	// Pass-through paths (excluded namespace, unlimited quota) do not record
	// a step because they did not constrain the decision. Per issue #1002 the
	// reason is formatted as `limited by quota[scope=..., type=...]` (and
	// `namespace=...` for the namespace scope) so traces are filterable.
	if granted < gpusRequested {
		decision.AddDecisionStep(a.name, a.formatLimitReason(decision.Namespace, accType), true)
	}
	return granted, nil
}

// formatLimitReason returns the human-readable trace string emitted on a
// DecisionStep when this allocator caps a request. The format mirrors the
// one documented in issue #1002 and the developer guide.
func (a *quotaAllocator) formatLimitReason(namespace, accType string) string {
	switch a.scope {
	case config.QuotaScopeNamespace:
		return fmt.Sprintf("limited by quota[scope=namespace, namespace=%s, type=%s]", namespace, accType)
	default:
		return fmt.Sprintf("limited by quota[scope=%s, type=%s]", a.scope, accType)
	}
}

func (a *quotaAllocator) tryAllocateCluster(accType string, gpusRequested int) int {
	limit, ok := a.cfg.ClusterQuotas[accType]
	if !ok {
		// No entry == zero quota for this type at cluster scope.
		return 0
	}
	if limit == config.QuotaUnlimited {
		return gpusRequested
	}
	available := limit - a.usedByType[accType]
	if available <= 0 {
		return 0
	}
	// usedByType is always non-nil here: NewQuotaInventory initializes it and
	// CreateAllocator clones it from a non-nil source.
	granted := min(gpusRequested, available)
	a.usedByType[accType] += granted
	return granted
}

func (a *quotaAllocator) tryAllocateNamespace(ns, accType string, gpusRequested int) int {
	quotas, excluded := a.cfg.QuotaForNamespace(ns)
	if excluded {
		// Pass-through: namespace bypasses this limiter entirely. Caller's
		// other limiters (e.g., cluster scope, physical inventory) still
		// apply.
		return gpusRequested
	}
	limit, ok := quotas[accType]
	if !ok {
		return 0
	}
	if limit == config.QuotaUnlimited {
		return gpusRequested
	}
	if a.usedByNS[ns] == nil {
		a.usedByNS[ns] = make(map[string]int)
	}
	available := limit - a.usedByNS[ns][accType]
	if available <= 0 {
		return 0
	}
	granted := min(gpusRequested, available)
	a.usedByNS[ns][accType] += granted
	return granted
}

// Remaining returns the sum of available GPUs across all pools the allocator
// tracks. Unlimited (-1) entries contribute 0 — the value is reserved for
// post-allocation reporting, not for capacity comparisons.
//
// Namespace scope caveat: this sums only the explicitly-listed NamespaceQuotas
// keys. Headroom consumed by unlisted namespaces that allocate via the "default"
// per-namespace fallback is not subtracted here, so the namespace-scope total
// can over-report remaining headroom. This affects reporting only — allocation
// is unaffected, since TryAllocate tracks each namespace's own budget.
func (a *quotaAllocator) Remaining() int {
	total := 0
	switch a.scope {
	case config.QuotaScopeCluster:
		for accType, limit := range a.cfg.ClusterQuotas {
			if limit == config.QuotaUnlimited {
				continue
			}
			avail := limit - a.usedByType[accType]
			if avail > 0 {
				total += avail
			}
		}
	case config.QuotaScopeNamespace:
		for ns, perType := range a.cfg.NamespaceQuotas {
			if ns == config.QuotaLimiterReservedNamespaceKey {
				continue
			}
			if a.cfg.IsExcluded(ns) {
				continue
			}
			for accType, limit := range perType {
				if limit == config.QuotaUnlimited {
					continue
				}
				avail := limit - a.usedByNS[ns][accType]
				if avail > 0 {
					total += avail
				}
			}
		}
	}
	return total
}

// copyNestedIntMap returns a deep copy of a two-level nested int map.
// maps.Clone is shallow, so the inner maps are cloned explicitly.
func copyNestedIntMap(src map[string]map[string]int) map[string]map[string]int {
	dst := make(map[string]map[string]int, len(src))
	for outer, inner := range src {
		dst[outer] = maps.Clone(inner)
	}
	return dst
}
