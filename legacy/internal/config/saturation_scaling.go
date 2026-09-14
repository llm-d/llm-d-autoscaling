package config

import "fmt"

// DefaultPriority is the default model priority multiplier.
// Higher priority → preferential GPU allocation in fair-share.
const DefaultPriority = 1.0

// SaturationScalingConfig holds saturation-based scaling thresholds for a model variant.
// Saturation scaling is enabled by default and uses these thresholds to determine when
// replicas are saturated and when to scale up.
type SaturationScalingConfig struct {
	// ModelID is the model identifier (only used in override entries)
	ModelID string `yaml:"model_id,omitempty"`

	// Namespace is the namespace for this override (only used in override entries)
	Namespace string `yaml:"namespace,omitempty"`

	// KvCacheThreshold: Replica is saturated if KV cache utilization >= this threshold (0.0-1.0)
	KvCacheThreshold float64 `yaml:"kvCacheThreshold"`

	// QueueLengthThreshold: Replica is saturated if queue length >= this threshold
	QueueLengthThreshold float64 `yaml:"queueLengthThreshold"`

	// KvSpareTrigger: Scale-up if average spare KV cache capacity < this value (0.0-1.0)
	KvSpareTrigger float64 `yaml:"kvSpareTrigger"`

	// QueueSpareTrigger: Scale-up if average spare queue capacity < this value
	QueueSpareTrigger float64 `yaml:"queueSpareTrigger"`

	// EnableLimiter: When true, includes the GPU limiter in the scaling pipeline
	// to constrain scaling decisions based on available cluster resources.
	// Default is false (limiter disabled).
	EnableLimiter bool `yaml:"enableLimiter,omitempty"`

	// EnableRescale: When true, the V2 GPU-constrained optimizer may run the
	// priority-weighted rescale pass — under contention it redistributes the whole
	// budget by priority x demand, reclaiming from lower-priority models so
	// higher-priority work can run. Default is false (additive fair-share only).
	//
	// This is a BUDGET-SCOPE flag, read only from the "default" entry: the value in
	// the global config governs the cluster budget, and a namespace-local config's
	// "default" governs that namespace's quota budget. It is ignored on per-model
	// override entries and has no effect unless a same-scope GPU budget exists
	// (see docs/plans/engine/rescale-alpha.md).
	EnableRescale bool `yaml:"enableRescale,omitempty"`

	// AnalyzerName selects which saturation analyzer to use.
	// "saturation" uses the V2 token-based analyzer.
	// Empty string (default) uses the V1 percentage-based analyzer.
	// To use the queueing model analyzer, deploy wva-queueing-model-config instead.
	AnalyzerName string `yaml:"analyzerName,omitempty"`

	// ScaleUpThreshold is the utilization threshold above which scale-up is triggered.
	// Applied by the engine post-step universally to every analyzer's result:
	//   requiredCapacity = max(0, totalDemand / ScaleUpThreshold − anticipatedSupply)
	// Default: 0.85 (85% utilization triggers scale-up)
	ScaleUpThreshold float64 `yaml:"scaleUpThreshold,omitempty"`

	// ScaleDownBoundary is the utilization boundary below which scale-down is safe.
	// Applied by the engine post-step universally to every analyzer's result:
	//   spareCapacity = max(0, totalSupply − totalDemand / ScaleDownBoundary)
	// Default: 0.70 (70% utilization allows scale-down)
	ScaleDownBoundary float64 `yaml:"scaleDownBoundary,omitempty"`

	// Priority is a multiplier for this model's scaling urgency.
	// Higher priority → preferential GPU allocation in fair-share.
	// Default: 1.0 (neutral).
	Priority float64 `yaml:"priority,omitempty"`

	// Analyzers configures the set of analyzers and their weights.
	// When empty and AnalyzerName is "saturation", defaults to
	// [{Name: "saturation", Score: 1.0, Enabled: true}].
	//
	// Deleting the analyzers section (and not setting analyzerName: saturation)
	// opts this entry out to the legacy V1 engine — see IsV2.
	Analyzers []AnalyzerScoreConfig `yaml:"analyzers,omitempty"`

	// ScaleToZero optionally enables/disables scaling to zero replicas for this
	// entry. When set, it overrides the separate wva-model-scale-to-zero-config
	// ConfigMap for the matching model/namespace; when nil, that ConfigMap (and
	// the WVA_SCALE_TO_ZERO env fallback) governs — see ResolveScaleToZeroEnabled.
	ScaleToZero *ScaleToZeroEnvelope `yaml:"scaleToZero,omitempty"`

	// Limiters is the sole source that selects the GPU limiter for the scaling
	// pipeline; it is applied live (no restart) and is honored only on the cluster
	// "default" entry (a budget-scope setting, like EnableRescale). A limiters list
	// selects a single mode — a quota entry wins over any gpu-inventory entry.
	// Each entry is either
	//   - {type: gpu-inventory}  — physical GPU limiter, no quota fields; or
	//   - {type: quota, ...}     — an inline quota entry (see QuotaLimiterConfig).
	// With no limiters declared, the physical-inventory limiter is used.
	// See Config.EffectiveLimiterMode / Config.EffectiveQuotaEntries.
	Limiters []QuotaLimiterConfig `yaml:"limiters,omitempty"`
}

// ScaleToZeroEnvelope is the inline scale-to-zero setting on a scaling entry.
// Enabled is a pointer so an absent field (nil) means "inherit from the separate
// scale-to-zero ConfigMap" rather than "disabled".
type ScaleToZeroEnvelope struct {
	Enabled *bool `yaml:"enabled,omitempty"`
}

// limiterTypeGPUInventory is the inline Limiters type that selects the physical
// GPU limiter. The other accepted types reuse the canonical LimiterType* values
// ("inventory" alias, "quota"); analyzer types are not restricted to a closed set
// so that externally-registered analyzers (see Engine.RegisterAnalyzer) work.
const limiterTypeGPUInventory = "gpu-inventory"

// AnalyzerScoreConfig configures an individual analyzer's weight in the
// composite scoring function. Per-analyzer threshold overrides are optional;
// when nil, the global top-level thresholds are used.
//
// Type and Parameters implement the ScalingPolicy plugin envelope (Phase 1 of
// the proposal in llm-d/llm-d-workload-variant-autoscaler#1245):
//
//	analyzers:
//	  - type: saturation
//	    parameters: { scaleUpThreshold: 0.95 }
//
// Well-known parameter keys (scaleUpThreshold, scaleDownBoundary, score,
// enabled) are folded into the typed fields below by Normalize(), so downstream
// consumers keep reading the typed fields. Type falls back to Name when unset,
// keeping the legacy `- name: saturation` form working unchanged.
type AnalyzerScoreConfig struct {
	Type              string         `yaml:"type,omitempty"`              // plugin type; falls back to Name
	Name              string         `yaml:"name"`                        //
	Enabled           *bool          `yaml:"enabled,omitempty"`           // default true
	Score             float64        `yaml:"score,omitempty"`             // default 1.0
	ScaleUpThreshold  *float64       `yaml:"scaleUpThreshold,omitempty"`  // overrides global
	ScaleDownBoundary *float64       `yaml:"scaleDownBoundary,omitempty"` // overrides global
	Parameters        map[string]any `yaml:"parameters,omitempty"`        // plugin params; folded by Normalize
}

// EffectiveType returns the analyzer plugin type: the explicit Type when set,
// otherwise the Name. Existing configs use `name:` as both identifier and type,
// so this keeps them working while allowing the new `type:` form.
func (a *AnalyzerScoreConfig) EffectiveType() string {
	if a.Type != "" {
		return a.Type
	}
	return a.Name
}

// Normalize folds well-known keys out of Parameters into the typed fields, so
// downstream consumers keep reading Score/Enabled/ScaleUpThreshold/
// ScaleDownBoundary. A typed field already set (from an explicit top-level key)
// wins over the same key in Parameters. Unknown parameter keys are tolerated.
// Returns an error if a known parameter has the wrong type.
func (a *AnalyzerScoreConfig) Normalize() error {
	if a.Parameters == nil {
		return nil
	}
	if v, ok := a.Parameters["scaleUpThreshold"]; ok && a.ScaleUpThreshold == nil {
		f, err := paramFloat("scaleUpThreshold", v)
		if err != nil {
			return err
		}
		a.ScaleUpThreshold = &f
	}
	if v, ok := a.Parameters["scaleDownBoundary"]; ok && a.ScaleDownBoundary == nil {
		f, err := paramFloat("scaleDownBoundary", v)
		if err != nil {
			return err
		}
		a.ScaleDownBoundary = &f
	}
	if v, ok := a.Parameters["score"]; ok && a.Score == 0 {
		f, err := paramFloat("score", v)
		if err != nil {
			return err
		}
		a.Score = f
	}
	if v, ok := a.Parameters["enabled"]; ok && a.Enabled == nil {
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("analyzer %q: parameter \"enabled\" must be a boolean, got %T", a.EffectiveType(), v)
		}
		a.Enabled = &b
	}
	return nil
}

// paramFloat coerces a YAML-decoded plugin parameter to float64. yaml.v3 decodes
// integers as int and decimals as float64 when the target is interface{}, so both
// are accepted.
func paramFloat(key string, v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	default:
		return 0, fmt.Errorf("parameter %q must be a number, got %T", key, v)
	}
}

// EffectiveScaleUpThreshold returns the per-analyzer threshold if set,
// otherwise falls back to the global value.
func (a *AnalyzerScoreConfig) EffectiveScaleUpThreshold(global float64) float64 {
	if a.ScaleUpThreshold != nil {
		return *a.ScaleUpThreshold
	}
	return global
}

// EffectiveScaleDownBoundary returns the per-analyzer boundary if set,
// otherwise falls back to the global value.
func (a *AnalyzerScoreConfig) EffectiveScaleDownBoundary(global float64) float64 {
	if a.ScaleDownBoundary != nil {
		return *a.ScaleDownBoundary
	}
	return global
}

// GetAnalyzerName implements the AnalyzerConfig interface.
// Returns "saturation" if Analyzers list is populated (new-style config),
// otherwise returns the raw AnalyzerName field (backward compat).
func (c *SaturationScalingConfig) GetAnalyzerName() string {
	if len(c.Analyzers) > 0 {
		return "saturation"
	}
	return c.AnalyzerName
}

// IsV2 returns true if this config selects the V2 token-based analyzer path.
// V2 is active when either the Analyzers list is populated (new-style) or
// AnalyzerName is "saturation" (old-style, backward compat).
func (c *SaturationScalingConfig) IsV2() bool {
	return len(c.Analyzers) > 0 || c.AnalyzerName == "saturation"
}

// V1 analyzer default thresholds, applied when fields are omitted from YAML config.
const (
	DefaultKvCacheThreshold     = 0.80
	DefaultQueueLengthThreshold = 5.0
	DefaultKvSpareTrigger       = 0.10
	DefaultQueueSpareTrigger    = 3.0
)

// V2 analyzer default thresholds, applied when fields are omitted from YAML config.
const (
	DefaultScaleUpThreshold  = 0.85
	DefaultScaleDownBoundary = 0.70
)

// Normalize folds each analyzer's Parameters into its typed fields. Call it at
// parse time, before ApplyDefaults() and Validate(), so defaulting and validation
// see the folded values.
func (c *SaturationScalingConfig) Normalize() error {
	for i := range c.Analyzers {
		if err := c.Analyzers[i].Normalize(); err != nil {
			return err
		}
	}
	return nil
}

// ApplyDefaults fills in zero-valued fields with their defaults.
// V1 thresholds (KvCacheThreshold, QueueLengthThreshold, KvSpareTrigger, QueueSpareTrigger)
// are always defaulted when zero. V2 thresholds (ScaleUpThreshold, ScaleDownBoundary) and
// the Analyzers list are defaulted only for V2 configs, so that a V1-style entry keeps its
// V2 thresholds zero — this is deliberate: entries are ApplyDefaults()'d individually at
// parse time, and defaulting V2 thresholds on a V1-style override would clobber a tuned
// global value during Merge(). The final resolved config is instead calibrated post-merge
// via ApplyV2ThresholdDefaults(). Must be called before Validate() to handle omitempty
// zero-values correctly.
func (c *SaturationScalingConfig) ApplyDefaults() {
	if c.Priority == 0 {
		c.Priority = DefaultPriority
	}
	// V1 defaults — apply unconditionally (V2 also inherits these for saturation checks)
	if c.KvCacheThreshold == 0 {
		c.KvCacheThreshold = DefaultKvCacheThreshold
	}
	if c.QueueLengthThreshold == 0 {
		c.QueueLengthThreshold = DefaultQueueLengthThreshold
	}
	if c.KvSpareTrigger == 0 {
		c.KvSpareTrigger = DefaultKvSpareTrigger
	}
	if c.QueueSpareTrigger == 0 {
		c.QueueSpareTrigger = DefaultQueueSpareTrigger
	}
	if c.IsV2() {
		if c.ScaleUpThreshold == 0 {
			c.ScaleUpThreshold = DefaultScaleUpThreshold
		}
		if c.ScaleDownBoundary == 0 {
			c.ScaleDownBoundary = DefaultScaleDownBoundary
		}
		// Default analyzers list when empty (backward compat for analyzerName: "saturation")
		if len(c.Analyzers) == 0 {
			enabled := true
			c.Analyzers = []AnalyzerScoreConfig{
				{Name: "saturation", Score: 1.0, Enabled: &enabled},
			}
		}
		// Apply per-entry defaults
		for i := range c.Analyzers {
			if c.Analyzers[i].Score == 0 {
				c.Analyzers[i].Score = 1.0
			}
			if c.Analyzers[i].Enabled == nil {
				enabled := true
				c.Analyzers[i].Enabled = &enabled
			}
		}
	}
}

// ApplyV2ThresholdDefaults fills zero-valued V2 thresholds (ScaleUpThreshold,
// ScaleDownBoundary) with their defaults regardless of IsV2(). Call this only on a
// FINAL resolved config (after base + override Merge), never on an individual stored
// entry: analyzer selection is global, so a V1-style per-model/namespace entry may run
// on the V2 path and must be calibrated — but defaulting these fields on the stored
// entry would clobber a tuned global threshold during Merge(). Inert on the V1 path,
// which never reads these fields.
func (c *SaturationScalingConfig) ApplyV2ThresholdDefaults() {
	if c.ScaleUpThreshold == 0 {
		c.ScaleUpThreshold = DefaultScaleUpThreshold
	}
	if c.ScaleDownBoundary == 0 {
		c.ScaleDownBoundary = DefaultScaleDownBoundary
	}
}

// Merge overlays non-zero fields from override onto c.
// This allows per-model overrides to specify only the fields they want to change,
// inheriting all other values from the base (typically the "default" config).
func (c *SaturationScalingConfig) Merge(override SaturationScalingConfig) {
	if override.KvCacheThreshold != 0 {
		c.KvCacheThreshold = override.KvCacheThreshold
	}
	if override.QueueLengthThreshold != 0 {
		c.QueueLengthThreshold = override.QueueLengthThreshold
	}
	if override.KvSpareTrigger != 0 {
		c.KvSpareTrigger = override.KvSpareTrigger
	}
	if override.QueueSpareTrigger != 0 {
		c.QueueSpareTrigger = override.QueueSpareTrigger
	}
	if override.AnalyzerName != "" {
		c.AnalyzerName = override.AnalyzerName
	}
	if override.ScaleUpThreshold != 0 {
		c.ScaleUpThreshold = override.ScaleUpThreshold
	}
	if override.ScaleDownBoundary != 0 {
		c.ScaleDownBoundary = override.ScaleDownBoundary
	}
	if override.EnableLimiter {
		c.EnableLimiter = override.EnableLimiter
	}
	if override.Priority != 0 {
		c.Priority = override.Priority
	}
	if len(override.Analyzers) > 0 {
		c.Analyzers = override.Analyzers
	}
	if override.ScaleToZero != nil {
		c.ScaleToZero = override.ScaleToZero
	}
	// Limiters is intentionally NOT merged: it is a cluster-"default"-scope setting
	// read only from the global default entry (see Config.EffectiveLimiterMode), so
	// a per-model override's limiters would never be consumed.
	if override.ModelID != "" {
		c.ModelID = override.ModelID
	}
	if override.Namespace != "" {
		c.Namespace = override.Namespace
	}
}

// Validate checks for invalid threshold values.
// Returns error with descriptive message if validation fails.
// Call ApplyDefaults() before Validate() to handle zero-valued omitempty fields.
func (c *SaturationScalingConfig) Validate() error {
	if c.KvCacheThreshold < 0 || c.KvCacheThreshold > 1 {
		return fmt.Errorf("kvCacheThreshold must be between 0 and 1, got %.2f", c.KvCacheThreshold)
	}
	if c.QueueLengthThreshold < 0 {
		return fmt.Errorf("queueLengthThreshold must be >= 0, got %.1f", c.QueueLengthThreshold)
	}
	if c.KvSpareTrigger < 0 || c.KvSpareTrigger > 1 {
		return fmt.Errorf("kvSpareTrigger must be between 0 and 1, got %.2f", c.KvSpareTrigger)
	}
	if c.QueueSpareTrigger < 0 {
		return fmt.Errorf("queueSpareTrigger must be >= 0, got %.1f", c.QueueSpareTrigger)
	}
	if c.Priority < 0 {
		return fmt.Errorf("priority must be >= 0, got %.2f", c.Priority)
	}

	// KV cache threshold should be greater than spare trigger (otherwise contradictory)
	if c.KvCacheThreshold < c.KvSpareTrigger {
		return fmt.Errorf("kvCacheThreshold (%.2f) should be >= kvSpareTrigger (%.2f)",
			c.KvCacheThreshold, c.KvSpareTrigger)
	}

	// V2 threshold range/consistency checks apply whenever the fields are set,
	// regardless of IsV2(): analyzer selection is global, so a V1-style per-model or
	// namespace entry's explicit scaleUpThreshold/scaleDownBoundary can still be
	// merged onto the V2 default and consumed on the V2 path. Zero means "unset"
	// (defaulted elsewhere), so it is skipped here.
	if c.ScaleUpThreshold != 0 && (c.ScaleUpThreshold < 0 || c.ScaleUpThreshold > 1) {
		return fmt.Errorf("scaleUpThreshold must be in (0, 1], got %.2f", c.ScaleUpThreshold)
	}
	if c.ScaleDownBoundary != 0 && (c.ScaleDownBoundary < 0 || c.ScaleDownBoundary > 1) {
		return fmt.Errorf("scaleDownBoundary must be in (0, 1], got %.2f", c.ScaleDownBoundary)
	}
	if c.ScaleUpThreshold != 0 && c.ScaleDownBoundary != 0 && c.ScaleUpThreshold <= c.ScaleDownBoundary {
		return fmt.Errorf("scaleUpThreshold (%.2f) must be > scaleDownBoundary (%.2f)", c.ScaleUpThreshold, c.ScaleDownBoundary)
	}

	// V2 analyzer per-analyzer overrides (only meaningful for a V2 config).
	if c.IsV2() {
		// A V2 config must have its thresholds set — ApplyDefaults fills them, so a
		// zero here means the caller skipped ApplyDefaults, which is invalid. (The
		// upper-bound and relational checks are handled by the non-zero global checks
		// above; V1-style entries are only range-checked there when non-zero.)
		if c.ScaleUpThreshold <= 0 {
			return fmt.Errorf("scaleUpThreshold must be in (0, 1], got %.2f", c.ScaleUpThreshold)
		}
		if c.ScaleDownBoundary <= 0 {
			return fmt.Errorf("scaleDownBoundary must be in (0, 1], got %.2f", c.ScaleDownBoundary)
		}

		// Per-analyzer threshold overrides. Analyzer types are intentionally NOT
		// restricted to a closed set: an unrecognized type is ignored at runtime
		// (no registered analyzer matches it), and rejecting it here would drop the
		// whole entry — breaking the Engine.RegisterAnalyzer extension path and
		// forward-compat with analyzers added in a newer release.
		for _, a := range c.Analyzers {
			if a.ScaleUpThreshold != nil {
				if *a.ScaleUpThreshold <= 0 || *a.ScaleUpThreshold > 1 {
					return fmt.Errorf("analyzer %q: scaleUpThreshold must be in (0, 1], got %.2f", a.EffectiveType(), *a.ScaleUpThreshold)
				}
			}
			if a.ScaleDownBoundary != nil {
				if *a.ScaleDownBoundary <= 0 || *a.ScaleDownBoundary > 1 {
					return fmt.Errorf("analyzer %q: scaleDownBoundary must be in (0, 1], got %.2f", a.EffectiveType(), *a.ScaleDownBoundary)
				}
			}
			up := a.EffectiveScaleUpThreshold(c.ScaleUpThreshold)
			down := a.EffectiveScaleDownBoundary(c.ScaleDownBoundary)
			if up <= down {
				return fmt.Errorf("analyzer %q: scaleUpThreshold (%.2f) must be > scaleDownBoundary (%.2f)", a.EffectiveType(), up, down)
			}
		}
	}

	if err := c.validateLimiters(); err != nil {
		return err
	}

	return nil
}

// validateLimiters checks the inline Limiters list. gpu-inventory/inventory
// entries must carry no quota fields; quota entries are validated end-to-end by
// the existing QuotaLimiterEntries.Validate() (name uniqueness, scope, per-type
// ranges), so the inline and file-based quota schemas stay identical.
func (c *SaturationScalingConfig) validateLimiters() error {
	if len(c.Limiters) == 0 {
		return nil
	}
	var quotaOnes []QuotaLimiterConfig
	for i, l := range c.Limiters {
		switch l.Type {
		case limiterTypeGPUInventory, string(LimiterTypeInventory):
			if l.Scope != "" || len(l.ClusterQuotas) > 0 || len(l.NamespaceQuotas) > 0 || len(l.Exclude) > 0 {
				return fmt.Errorf("limiters[%d] (type %q): must not set quota fields (scope/quotas/namespaceQuotas/exclude)", i, l.Type)
			}
		case string(LimiterTypeQuota):
			quotaOnes = append(quotaOnes, l)
		default:
			return fmt.Errorf("limiters[%d]: unknown limiter type %q (valid: %q, %q, %q)",
				i, l.Type, limiterTypeGPUInventory, LimiterTypeInventory, LimiterTypeQuota)
		}
	}
	if len(quotaOnes) > 0 {
		entries := QuotaLimiterEntries{Limiters: quotaOnes}
		if _, err := entries.Validate(); err != nil {
			return fmt.Errorf("limiters: %w", err)
		}
	}
	return nil
}
