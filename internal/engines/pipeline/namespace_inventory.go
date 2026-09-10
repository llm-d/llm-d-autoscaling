package pipeline

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/discovery"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// DefaultSelectorKey is the reserved selector map key whose node selector
// applies to any namespace without an explicit entry. It cannot name a real
// namespace in the configuration.
const DefaultSelectorKey = "default"

// bucketResolver maps a workload namespace to the inventory bucket whose GPU
// pool it draws from. A bucket is either a named namespace (explicit selector),
// the shared DefaultSelectorKey fallback, "excluded" (bypasses the limiter
// entirely), or "denied" (no matching selector and no default → zero inventory).
type bucketResolver struct {
	named      sets.Set[string] // namespaces with an explicit selector (excludes DefaultSelectorKey)
	hasDefault bool
	exclude    sets.Set[string]
}

// resolve returns the bucket key for a namespace. excluded is true when the
// namespace bypasses this limiter. hasPool is false when the namespace has no
// bucket and no default applies (zero inventory: scaling is denied).
func (r bucketResolver) resolve(namespace string) (bucket string, excluded bool, hasPool bool) {
	if r.exclude.Has(namespace) {
		return "", true, false
	}
	if r.named.Has(namespace) {
		return namespace, false, true
	}
	if r.hasDefault {
		return DefaultSelectorKey, false, true
	}
	return "", false, false
}

// chargeBucket returns the bucket a namespace's existing GPU usage should be
// charged against, ignoring exclusion. Excluded namespaces bypass the cap, but
// their running replicas still occupy physical GPUs in the pool they draw from
// (named bucket if the namespace has an explicit selector, else the shared
// default), so their usage must debit that pool — otherwise the pool hands out
// GPUs that are already gone, diverging from the cluster-wide path that charges
// all usage. ok is false when no bucket applies (no matching selector and no
// default), in which case the usage cannot be attributed to any pool.
func (r bucketResolver) chargeBucket(namespace string) (bucket string, ok bool) {
	if r.named.Has(namespace) {
		return namespace, true
	}
	if r.hasDefault {
		return DefaultSelectorKey, true
	}
	return "", false
}

// NamespaceInventory tracks GPU capacity per (namespace bucket, accelerator
// type) by intersecting discovered cluster nodes with a per-namespace node
// label selector. It implements the Inventory interface; per-namespace usage
// accounting is driven by NamespaceLimiter via SetUsedByBucket (the type-keyed
// Inventory.SetUsed cannot express the namespace dimension).
//
// Namespaces map to node pools via node labels, the ground truth Kubernetes
// operators already maintain for node pinning (taints+tolerations,
// PodNodeSelector admission, managed node pools). Selectors are re-evaluated
// on every Refresh, so node additions/removals and label changes are picked up.
type NamespaceInventory struct {
	name      string
	discovery discovery.NodeDiscovery
	resolver  bucketResolver
	// selectors maps bucket name (named namespace or DefaultSelectorKey) to the
	// compiled node label selector used to gather that bucket's GPU pool.
	selectors map[string]labels.Selector

	mu sync.RWMutex
	// limitByBucketType maps bucket -> accelerator type -> total GPU capacity.
	limitByBucketType map[string]map[string]int
	// usedByBucketType maps bucket -> accelerator type -> GPUs in use, set by
	// SetUsedByBucket. Used to compute available capacity in CreateAllocator.
	usedByBucketType map[string]map[string]int
	// usedByNamespace maps workload namespace -> accelerator type -> GPUs in
	// use, set by SetUsedByNamespace on the V2 constraint path. Retained
	// alongside usedByBucketType because per-namespace pools must report each
	// namespace's own usage, which the bucket aggregate cannot recover once
	// several namespaces share the default bucket.
	usedByNamespace map[string]map[string]int
}

// NewNamespaceInventory creates a NamespaceInventory.
//
// exclude lists namespaces that bypass this limiter (no inventory constraint).
// selectors maps each governed namespace (or DefaultSelectorKey for the
// fallback) to a compiled node label selector. Namespaces neither excluded,
// named, nor covered by a default selector receive zero inventory.
func NewNamespaceInventory(name string, disc discovery.NodeDiscovery, exclude sets.Set[string], selectors map[string]labels.Selector) *NamespaceInventory {
	named := sets.New[string]()
	for ns := range selectors {
		if ns != DefaultSelectorKey {
			named.Insert(ns)
		}
	}
	if exclude == nil {
		exclude = sets.New[string]()
	}
	_, hasDefault := selectors[DefaultSelectorKey]
	return &NamespaceInventory{
		name:      name,
		discovery: disc,
		selectors: selectors,
		resolver: bucketResolver{
			named:      named,
			hasDefault: hasDefault,
			exclude:    exclude,
		},
		limitByBucketType: make(map[string]map[string]int),
		usedByBucketType:  make(map[string]map[string]int),
		usedByNamespace:   make(map[string]map[string]int),
	}
}

// Name returns the inventory identifier.
func (i *NamespaceInventory) Name() string {
	return i.name
}

// Refresh re-discovers cluster nodes and re-evaluates each bucket's node
// selector against the current node list, rebuilding per-bucket GPU limits.
// Accelerator model names are normalized to short names (e.g. "A100") to match
// VA label conventions, consistent with TypeInventory.
func (i *NamespaceInventory) Refresh(ctx context.Context) error {
	nodes, err := i.discovery.DiscoverNodes(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover nodes: %w", err)
	}

	// Pre-create every configured bucket so a named selector matching no nodes
	// yields an explicit empty pool (its namespaces are denied scale-up) rather
	// than silently inheriting another bucket's capacity.
	limitByBucketType := make(map[string]map[string]int, len(i.selectors))
	for bucket := range i.selectors {
		limitByBucketType[bucket] = make(map[string]int)
	}

	// Named bucket keys in deterministic order for unambiguous node assignment.
	namedBuckets := slices.Sorted(maps.Keys(i.selectors))
	namedBuckets = slices.DeleteFunc(namedBuckets, func(b string) bool {
		return b == DefaultSelectorKey
	})

	logger := ctrl.LoggerFrom(ctx)
	for _, node := range nodes {
		bucket, ambiguous, ok := i.assignNode(node, namedBuckets)
		if !ok {
			continue // node matches no selector — outside every namespace pool
		}
		if ambiguous {
			logger.V(logging.DEBUG).Info("node matches multiple namespace-inventory selectors; assigning to first by name to avoid double-counting GPUs",
				"node", node.Name, "bucket", bucket)
		}
		for model, info := range node.Accelerators {
			limitByBucketType[bucket][accelerator.NormalizeAcceleratorName(model)] += info.Count
		}
	}

	i.mu.Lock()
	i.limitByBucketType = limitByBucketType
	// Reset usage; the limiter re-supplies it each cycle via SetUsedByBucket
	// (V1) or SetUsedByNamespace (V2).
	i.usedByBucketType = make(map[string]map[string]int)
	i.usedByNamespace = make(map[string]map[string]int)
	i.mu.Unlock()
	return nil
}

// assignNode returns the single bucket a node's GPUs count toward. Named
// selectors take precedence over the default selector; when multiple named
// selectors match (an overlapping/misconfigured selector set), the
// lexicographically first wins and ambiguous is true, so a node's GPUs are
// never summed into more than one pool (which would let two namespaces both
// allocate the same physical GPUs). ok is false when no selector matches.
func (i *NamespaceInventory) assignNode(node discovery.NodeInfo, namedBuckets []string) (bucket string, ambiguous bool, ok bool) {
	set := labels.Set(node.Labels)
	matched := ""
	matchCount := 0
	for _, b := range namedBuckets {
		if i.selectors[b].Matches(set) {
			if matchCount == 0 {
				matched = b
			}
			matchCount++
		}
	}
	if matchCount > 0 {
		return matched, matchCount > 1, true
	}
	if sel, has := i.selectors[DefaultSelectorKey]; has && sel.Matches(set) {
		return DefaultSelectorKey, false, true
	}
	return "", false, false
}

// bucketAcceleratorTypes returns the accelerator types present in a bucket's
// pool. Used by NamespaceLimiter to resolve unknown accelerator names against
// the namespace's own pool rather than the cluster-wide aggregate.
func (i *NamespaceInventory) bucketAcceleratorTypes(bucket string) []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return slices.Collect(maps.Keys(i.limitByBucketType[bucket]))
}

// SetUsed satisfies the Inventory interface. NamespaceInventory requires
// per-namespace usage, which the type-keyed map cannot express, so this is a
// no-op: NamespaceLimiter supplies usage via SetUsedByBucket instead.
func (i *NamespaceInventory) SetUsed(_ map[string]int) {}

// SetUsedByBucket records GPU usage per (bucket, accelerator type). The keys
// are bucket names (as returned by the resolver), so the caller must remap
// workload namespaces onto their buckets before calling. Copies the input.
func (i *NamespaceInventory) SetUsedByBucket(usedByBucketType map[string]map[string]int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.usedByBucketType = copyNestedIntMap(usedByBucketType)
}

// SetUsedByNamespace implements NamespaceAwareInventory for the V2 constraint
// path. It keeps each namespace's own usage and additionally remaps usage onto
// inventory buckets via chargeBucket, so V2 charges usage exactly as the V1
// Limit path does: an excluded namespace bypasses the cap, but its running
// replicas still occupy physical GPUs in the pool they draw from and therefore
// still debit that pool. Copies the input.
func (i *NamespaceInventory) SetUsedByNamespace(usedByNS map[string]map[string]int) {
	perNamespace := make(map[string]map[string]int, len(usedByNS))
	byBucket := make(map[string]map[string]int)
	for ns, byType := range usedByNS {
		cp := make(map[string]int, len(byType))
		for t, n := range byType {
			cp[t] = n
		}
		perNamespace[ns] = cp

		bucket, ok := i.resolver.chargeBucket(ns)
		if !ok {
			// No selector match and no default: the usage belongs to no pool.
			continue
		}
		if byBucket[bucket] == nil {
			byBucket[bucket] = make(map[string]int)
		}
		for t, n := range byType {
			byBucket[bucket][t] += n
		}
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	i.usedByNamespace = perNamespace
	i.usedByBucketType = byBucket
}

// NamespaceResourcePools implements NamespaceAwareInventory, exposing each
// active namespace's GPU pool as a CLOSED allowlist so the V2 optimizer
// enforces the same partitioning the V1 allocator does:
//
//   - An excluded namespace is OMITTED, signalling "open" (bound only by the
//     cluster per-type constraint), matching the V1 allocator's pass-through.
//   - Every other active namespace is present. A namespace with no matching
//     selector and no default gets an empty inner map, a real deny-all, matching
//     V1's zero-inventory outcome.
//   - Each type in the namespace's pool is emitted with its finite capacity. A
//     type absent from the pool means the namespace's nodes hold none of it and
//     is denied, never falling through to the cluster aggregate.
//
// Physical inventory is always finite, so the unlimited sentinel (Limit < 0)
// that a quota provider may emit is never produced here: absent means deny, and
// every present pool carries a real GPU count.
//
// SetUsedByNamespace must be called first so the pools carry current usage.
func (i *NamespaceInventory) NamespaceResourcePools(activeNamespaces []string) map[string]map[string]ResourcePool {
	i.mu.RLock()
	defer i.mu.RUnlock()

	out := make(map[string]map[string]ResourcePool, len(activeNamespaces))
	// Namespaces without an explicit selector all draw from the single shared
	// default bucket, so they are collected and partitioned together below.
	var sharedDefault []string

	for _, ns := range activeNamespaces {
		bucket, excluded, hasPool := i.resolver.resolve(ns)
		switch {
		case excluded:
			continue
		case !hasPool:
			out[ns] = map[string]ResourcePool{}
		case bucket == DefaultSelectorKey:
			sharedDefault = append(sharedDefault, ns)
		default:
			// A named bucket is 1:1 with its namespace, so its pool maps across
			// exactly.
			perType := make(map[string]ResourcePool, len(i.limitByBucketType[bucket]))
			for t, limit := range i.limitByBucketType[bucket] {
				perType[t] = ResourcePool{Limit: limit, Used: i.usedByBucketType[bucket][t]}
			}
			out[ns] = perType
		}
	}

	if len(sharedDefault) > 0 {
		i.partitionDefaultLocked(sharedDefault, out)
	}
	return out
}

// partitionDefaultLocked splits the shared default bucket across the namespaces
// that fall through to it, writing their pools into out. Callers must hold i.mu.
//
// The default bucket is one physical node pool shared by every namespace without
// an explicit selector, whereas the V2 optimizer consumes a static budget per
// namespace. Handing each namespace the whole pool would let them all allocate it
// independently, and would inflate the cluster aggregate that callers derive by
// summing the per-namespace pools. So the pool's REMAINING capacity is split
// evenly and each namespace is reported as its own usage plus its share: the
// pool's Available is then exactly that share, and the shares sum to the bucket's
// remaining capacity, so the shared pool can never be over-allocated.
//
// This is where V2 necessarily diverges from V1: the V1 allocator decrements one
// live shared counter (first-come-first-served between namespaces), which a
// static per-namespace budget cannot express. A named selector gives a namespace
// its own bucket and avoids the split entirely.
func (i *NamespaceInventory) partitionDefaultLocked(namespaces []string, out map[string]map[string]ResourcePool) {
	// Sorted so the remainder is distributed deterministically across cycles.
	slices.Sort(namespaces)

	limits := i.limitByBucketType[DefaultSelectorKey]
	used := i.usedByBucketType[DefaultSelectorKey]
	for _, ns := range namespaces {
		out[ns] = make(map[string]ResourcePool, len(limits))
	}

	n := len(namespaces)
	for t, limit := range limits {
		remaining := limit - used[t]
		if remaining < 0 {
			remaining = 0
		}
		share, extra := remaining/n, remaining%n
		for idx, ns := range namespaces {
			s := share
			if idx < extra {
				s++
			}
			own := i.usedByNamespace[ns][t]
			out[ns][t] = ResourcePool{Limit: own + s, Used: own}
		}
	}
}

// CreateAllocator returns a namespaceTypeAllocator that allocates from
// per-(bucket, type) pools. Available = max(0, Limit - Used) per pool.
func (i *NamespaceInventory) CreateAllocator(_ context.Context) ResourceAllocator {
	i.mu.RLock()
	defer i.mu.RUnlock()

	remaining := make(map[string]map[string]int, len(i.limitByBucketType))
	total := 0
	for bucket, byType := range i.limitByBucketType {
		rem := make(map[string]int, len(byType))
		for t, limit := range byType {
			avail := limit - i.usedByBucketType[bucket][t]
			if avail < 0 {
				avail = 0
			}
			rem[t] = avail
			total += avail
		}
		remaining[bucket] = rem
	}

	return &namespaceTypeAllocator{
		resolver:          i.resolver,
		remainingByBucket: remaining,
		totalRemaining:    total,
	}
}

// TotalLimit returns total GPU capacity across all buckets and types.
func (i *NamespaceInventory) TotalLimit() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	total := 0
	for _, byType := range i.limitByBucketType {
		for _, limit := range byType {
			total += limit
		}
	}
	return total
}

// TotalUsed returns total GPUs in use across all buckets and types.
func (i *NamespaceInventory) TotalUsed() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	total := 0
	for _, byType := range i.usedByBucketType {
		for _, used := range byType {
			total += used
		}
	}
	return total
}

// TotalAvailable returns total available GPUs (Limit - Used, clamped) across
// all buckets and types.
func (i *NamespaceInventory) TotalAvailable() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	total := 0
	for bucket, byType := range i.limitByBucketType {
		for t, limit := range byType {
			avail := limit - i.usedByBucketType[bucket][t]
			if avail > 0 {
				total += avail
			}
		}
	}
	return total
}

// GetResourcePools returns cluster-wide per-type pools aggregated across all
// buckets. Aggregating to the type dimension (rather than exposing per-bucket
// keys) keeps DefaultLimiter-style accelerator resolution correct: a single
// pool still means a physically homogeneous cluster.
func (i *NamespaceInventory) GetResourcePools() map[string]ResourcePool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	pools := make(map[string]ResourcePool)
	for bucket, byType := range i.limitByBucketType {
		for t, limit := range byType {
			p := pools[t]
			p.Limit += limit
			p.Used += i.usedByBucketType[bucket][t]
			pools[t] = p
		}
	}
	return pools
}

// namespaceTypeAllocator implements ResourceAllocator with per-(bucket, type)
// tracking. Like typeAllocator it is NOT thread-safe and must be created per
// scaling-decision batch via NamespaceInventory.CreateAllocator.
type namespaceTypeAllocator struct {
	resolver          bucketResolver
	remainingByBucket map[string]map[string]int
	totalRemaining    int
}

// TryAllocate allocates GPUs from the pool of the decision's namespace bucket.
// Excluded namespaces are unconstrained (allocate all requested). Namespaces
// with no bucket and no default get zero. The accelerator type is taken from
// the decision; an unresolved type in a heterogeneous cluster yields zero
// (the limiter resolves it for homogeneous clusters before allocation).
func (a *namespaceTypeAllocator) TryAllocate(_ context.Context, decision *domain.VariantDecision, gpusRequested int) (int, error) {
	if gpusRequested <= 0 {
		return 0, nil
	}

	bucket, excluded, hasPool := a.resolver.resolve(decision.Namespace)
	if excluded {
		// Bypass the cap, but still debit the pool. Usage accounting only charges
		// pre-cycle CurrentReplicas, so an excluded namespace growing in the same
		// batch as a capped tenant on the shared default pool would otherwise let
		// both take the same GPUs (a one-cycle overcommit). Charging here keeps
		// the pool honest within the batch; only the availability check is
		// skipped, so the excluded namespace is never itself limited.
		if chargeTo, ok := a.resolver.chargeBucket(decision.Namespace); ok &&
			constants.IsAcceleratorResolved(decision.AcceleratorName) {
			if byType := a.remainingByBucket[chargeTo]; byType != nil {
				debit := min(gpusRequested, byType[decision.AcceleratorName])
				if debit > 0 {
					byType[decision.AcceleratorName] -= debit
					a.totalRemaining -= debit
				}
			}
		}
		return gpusRequested, nil
	}
	if !hasPool {
		// No selector match and no default → zero inventory for this namespace.
		return 0, nil
	}
	if !constants.IsAcceleratorResolved(decision.AcceleratorName) {
		// Cannot determine which type pool to debit. Resolution for
		// homogeneous clusters happens in the limiter before allocation.
		return 0, nil
	}

	avail := a.remainingByBucket[bucket][decision.AcceleratorName]
	if avail <= 0 {
		return 0, nil
	}
	allocated := gpusRequested
	if allocated > avail {
		allocated = avail
	}
	a.remainingByBucket[bucket][decision.AcceleratorName] -= allocated
	a.totalRemaining -= allocated
	return allocated, nil
}

// Remaining returns total remaining GPUs across all buckets and types.
func (a *namespaceTypeAllocator) Remaining() int {
	return a.totalRemaining
}

// Ensure NamespaceInventory implements Inventory and, for the V2 constraint
// path, NamespaceAwareInventory.
var (
	_ Inventory               = (*NamespaceInventory)(nil)
	_ NamespaceAwareInventory = (*NamespaceInventory)(nil)
)

// Ensure namespaceTypeAllocator implements ResourceAllocator.
var _ ResourceAllocator = (*namespaceTypeAllocator)(nil)
