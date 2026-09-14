package locator

import (
	"fmt"

	lru "github.com/hashicorp/golang-lru/v2"
)

// defaultCacheSize is the size of the pod → top-level scale-target LRU.
// One entry is roughly 100 B of strings + a chainNode value; 4096 entries
// fit in well under a MB and cover typical clusters where the chain-node
// universe is a small multiple of variant count.
const defaultCacheSize = 4096

// podKey identifies a pod for cache purposes. Pods are uniquely named
// within a namespace, which is sufficient to key the immutable
// pod → top-level scale-target relation.
type podKey struct {
	Namespace, Name string
}

// cacheEntry holds both the scale target and pod labels for a cached pod.
type cacheEntry struct {
	target chainNode
	labels map[string]string
}

// resolutionCache memoizes pod → top-level scale-target resolution. The
// scale-target → managed scaler step is NOT cached; it always runs through
// the field index so annotation toggles and scaleTargetRef edits take
// effect on the next Locate call.
//
// Eviction is size-only LRU. No TTL, no watch-driven invalidation: the
// pod → top-level resource relation is immutable per Kubernetes ownerReference
// rules (controllers cannot be changed after creation), so cached entries
// are correct for the lifetime of the cached pod.
type resolutionCache struct {
	c *lru.Cache[podKey, cacheEntry]
}

func newResolutionCache(size int) (*resolutionCache, error) {
	if size <= 0 {
		return nil, fmt.Errorf("cache size must be > 0, got %d", size)
	}
	c, err := lru.New[podKey, cacheEntry](size)
	if err != nil {
		return nil, err
	}
	return &resolutionCache{c: c}, nil
}

// getTarget returns the cached top-level scale-target for the pod. The hit boolean
// is true even for negative entries (target == zero chainNode means the pod
// has no scaler-eligible ancestor).
func (r *resolutionCache) getTarget(k podKey) (chainNode, bool) {
	entry, ok := r.c.Get(k)
	return entry.target, ok
}

// getLabels returns the cached pod labels. Returns nil if not found.
func (r *resolutionCache) getLabels(k podKey) (map[string]string, bool) {
	entry, ok := r.c.Get(k)
	return entry.labels, ok
}

func (r *resolutionCache) add(k podKey, target chainNode, labels map[string]string) {
	r.c.Add(k, cacheEntry{
		target: target,
		labels: labels,
	})
}
