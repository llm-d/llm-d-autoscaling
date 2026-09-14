package saturation_v2

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// CapacityRecord holds cached capacity knowledge for a specific variant.
// This allows the analyzer to make capacity estimates for variants that
// currently have zero replicas, either from their own prior data or from
// a compatible variant via FindCompatible.
type CapacityRecord struct {
	AcceleratorName       string
	GpuCount              int
	NumGpuBlocks          int64
	BlockSize             int64
	TotalKvCapacityTokens int64
	EffectiveCapacity     int64
	EngineParams          *EngineParams // parsed deployment params for k2 derivation
	LearnedFrom           string        // "live", "deployment", "annotation"
	LearnedAt             time.Time
}

// CapacityKnowledgeStore is a thread-safe in-memory cache of capacity
// records keyed by "namespace|modelID|variantName". It enables the analyzer
// to estimate capacity for zero-replica variants and newly created
// deployments before any live metrics are available.
//
// For cross-variant estimation, use FindCompatible which searches for records
// from other variants with matching hardware and engine parameters.
type CapacityKnowledgeStore struct {
	mu      sync.RWMutex
	records map[string]*CapacityRecord
}

// NewCapacityKnowledgeStore creates an empty capacity store.
func NewCapacityKnowledgeStore() *CapacityKnowledgeStore {
	return &CapacityKnowledgeStore{
		records: make(map[string]*CapacityRecord),
	}
}

// storeKey builds the map key for a given namespace, model, and variant.
// The pipe delimiter is safe because Kubernetes resource names follow DNS
// naming rules and cannot contain the "|" character.
func storeKey(namespace, modelID, variantName string) string {
	return fmt.Sprintf("%s|%s|%s", namespace, modelID, variantName)
}

// Update stores or overwrites a capacity record for a specific variant.
// Live data is always authoritative and should always be written via Update.
func (s *CapacityKnowledgeStore) Update(namespace, modelID, variantName string, record CapacityRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record.LearnedAt = time.Now()
	s.records[storeKey(namespace, modelID, variantName)] = &record
}

// Get returns the stored capacity record for a specific variant, or nil
// if none exists. For cross-variant lookup, use FindCompatible instead.
func (s *CapacityKnowledgeStore) Get(namespace, modelID, variantName string) *CapacityRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.records[storeKey(namespace, modelID, variantName)]
}

// IsStale returns true if the record for the given variant is older than
// CapacityStalenessTimeout, or if no record exists.
func (s *CapacityKnowledgeStore) IsStale(namespace, modelID, variantName string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[storeKey(namespace, modelID, variantName)]
	if !ok {
		return true
	}
	return time.Since(rec.LearnedAt) > CapacityStalenessTimeout
}

// LoadFromScaleTarget parses the engine args from a scale target (dispatching by
// detected engine via ParseEngineArgs) and stores an estimated capacity record for
// the variant. It does NOT overwrite an existing "live" record — scale
// target-derived data is a fallback only.
func (s *CapacityKnowledgeStore) LoadFromScaleTarget(namespace, modelID, variantName, accelerator string, gpuCount int, scaleTarget scaletarget.ScaleTargetAccessor) {
	if scaleTarget == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := storeKey(namespace, modelID, variantName)

	// Don't overwrite live data
	if existing, ok := s.records[key]; ok && existing.LearnedFrom == learnedFromLive {
		return
	}

	params := ParseEngineArgs(inferenceengine.Detect(scaleTarget), scaleTarget)
	record := &CapacityRecord{
		AcceleratorName: accelerator,
		GpuCount:        gpuCount,
		EngineParams:    &params,
		LearnedFrom:     "deployment", // same for deployment and LWS
		LearnedAt:       time.Now(),
	}

	// If num_gpu_blocks_override is set (vLLM), we can estimate k1.
	if params.NumGpuBlocksOverride > 0 {
		record.NumGpuBlocks = params.NumGpuBlocksOverride
		record.BlockSize = params.BlockSize
		record.TotalKvCapacityTokens = params.NumGpuBlocksOverride * params.BlockSize
	} else if params.TotalKvTokensOverride > 0 {
		// SGLang exposes total KV token capacity directly via --max-total-tokens.
		record.TotalKvCapacityTokens = params.TotalKvTokensOverride
	}

	// Provide a conservative capacity estimate so that brand-new variants
	// with no live data or compatible siblings can still be considered for
	// scale-up. EffectiveMaxBatchedTokens (the per-step token budget) is a
	// safe lower bound — real capacity is almost always much higher.
	if record.EffectiveCapacity <= 0 && params.EffectiveMaxBatchedTokens > 0 {
		record.EffectiveCapacity = params.EffectiveMaxBatchedTokens
	}

	s.records[key] = record
}

// EvictStale removes capacity records that have not been updated within the
// given timeout. This prevents unbounded memory growth from deleted or
// long-unused variants. Use a long timeout (e.g. EvictionTimeout = 24h)
// since historical capacity data is valuable for zero-replica estimation.
func (s *CapacityKnowledgeStore) EvictStale(timeout time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	evicted := 0
	for key, rec := range s.records {
		if time.Since(rec.LearnedAt) > timeout {
			delete(s.records, key)
			evicted++
		}
	}
	return evicted
}

// FindCompatible searches across all namespaces for a capacity record from
// another variant with matching configuration: same model, accelerator type,
// GPU count, and compatible engine parameters (as defined by IsCapacityCompatible).
// Capacity is a property of hardware + engine config, not namespace, so
// cross-namespace matching is intentional.
//
// Returns the best match (preferring "live" records over "deployment"/"lws" records),
// or nil if no compatible record exists.
func (s *CapacityKnowledgeStore) FindCompatible(modelID, accelerator string, gpuCount int, params *EngineParams) *CapacityRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best *CapacityRecord
	for key, rec := range s.records {
		// Parse key: "namespace|modelID|variantName"
		parts := strings.SplitN(key, "|", 3)
		if len(parts) < 3 || parts[1] != modelID {
			continue
		}

		// Must match accelerator type and GPU count
		if rec.AcceleratorName != accelerator || rec.GpuCount != gpuCount {
			continue
		}

		// Must have compatible engine parameters
		if rec.EngineParams == nil || !rec.EngineParams.IsCapacityCompatible(params) {
			continue
		}

		// Must have useful capacity data
		if rec.EffectiveCapacity <= 0 && rec.TotalKvCapacityTokens <= 0 {
			continue
		}

		// Prefer live data over deployment/lws-derived
		if best == nil || (best.LearnedFrom != "live" && rec.LearnedFrom == learnedFromLive) {
			best = rec
		}
	}

	return best
}
