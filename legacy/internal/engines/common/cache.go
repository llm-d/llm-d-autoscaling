package common

import (
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// InternalDecisionCache holds the latest saturation decisions for VAs.
// This is used to pass decisions from the Engine to the Controller without API server interaction.
type InternalDecisionCache struct {
	sync.RWMutex
	items map[string]domain.VariantDecision
}

// Key format: namespace/name
func cacheKey(name, namespace string) string {
	return namespace + "/" + name
}

func (c *InternalDecisionCache) Set(name, namespace string, d domain.VariantDecision) {
	c.Lock()
	defer c.Unlock()
	key := cacheKey(name, namespace)
	c.items[key] = d
}

func (c *InternalDecisionCache) Get(name, namespace string) (domain.VariantDecision, bool) {
	c.RLock()
	defer c.RUnlock()
	key := cacheKey(name, namespace)
	val, ok := c.items[key]
	return val, ok
}

// Global cache instance
var DecisionCache = &InternalDecisionCache{
	items: make(map[string]domain.VariantDecision),
}

// DecisionTrigger is a channel to trigger reconciliation for VAs.
// Buffered to prevent blocking the engine loop.
var DecisionTrigger = make(chan event.GenericEvent, 1000)

// DecisionToOptimizedAlloc converts a VariantDecision to OptimizedAlloc status fields.
func DecisionToOptimizedAlloc(d domain.VariantDecision) (*int32, string, metav1.Time) {
	// If LastRunTime is adding to VariantDecision, use it, else Now
	// For now we assume the consumer sets LastRunTime or uses Now
	numReplicas := int32(d.TargetReplicas)
	return &numReplicas, d.AcceleratorName, metav1.NewTime(time.Now())
}

// GlobalConfig and Config singleton have been removed in favor of unified Config
// from internal/config package. All components now receive Config via dependency injection.
