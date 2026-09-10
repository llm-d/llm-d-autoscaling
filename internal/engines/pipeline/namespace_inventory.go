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
	// Reset usage; the limiter re-supplies it via SetUsedByBucket each cycle.
	i.usedByBucketType = make(map[string]map[string]int)
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

// Ensure NamespaceInventory implements Inventory.
var _ Inventory = (*NamespaceInventory)(nil)

// Ensure namespaceTypeAllocator implements ResourceAllocator.
var _ ResourceAllocator = (*namespaceTypeAllocator)(nil)
