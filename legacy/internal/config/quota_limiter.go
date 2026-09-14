package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// QuotaLimiterReservedNamespaceKey is the reserved key in the namespace-scoped
// quotas map that matches any namespace not explicitly listed. A real namespace
// named "default" cannot be configured directly via this key — list it in
// `exclude` or use a strict allowlist instead.
const QuotaLimiterReservedNamespaceKey = "default"

// QuotaUnlimited is the per-type quota value that disables the cap for that
// accelerator type in the containing namespace. Follows the Kubernetes
// convention of -1 = no limit (e.g., `terminationGracePeriodSeconds: -1`).
const QuotaUnlimited = -1

// MaxQuotaValue is the largest finite quota a single entry may declare. It is
// far above any realistic GPU count (a million accelerators) and exists only to
// keep the aggregate sum (summed across namespaces in aggregateNamespacePools)
// well clear of int64 overflow — an overflowed sum could wrap negative and be
// misread as the QuotaUnlimited (-1) sentinel.
const MaxQuotaValue = 1 << 20 // 1,048,576

// QuotaScope identifies whether a quota limiter applies cluster-wide or
// per-namespace. Both scopes can coexist; each acts independently.
type QuotaScope string

const (
	// QuotaScopeCluster caps total GPUs of a given accelerator type across
	// all namespaces.
	QuotaScopeCluster QuotaScope = "cluster"

	// QuotaScopeNamespace caps GPUs of a given accelerator type within a
	// specific namespace, with optional fall-through via the reserved
	// `default` key.
	QuotaScopeNamespace QuotaScope = "namespace"
)

// QuotaLimiterConfig is one entry in the quota-limiter ConfigMap. Each
// entry produces an independent limiter — cluster and namespace scopes
// are declared as separate entries so operators can enable them
// independently.
//
// The active scope is chosen by the Scope field: when Scope ==
// QuotaScopeCluster, only ClusterQuotas is consulted; when Scope ==
// QuotaScopeNamespace, only NamespaceQuotas and Exclude are.
type QuotaLimiterConfig struct {
	// Name identifies the limiter in logs, metrics, and DecisionStep traces.
	// Must be non-empty and unique across all entries.
	Name string `yaml:"name" json:"name"`

	// Type must be the literal string "quota" — leaves room for future
	// limiter types in the same config schema (e.g., reservation, priority).
	Type string `yaml:"type" json:"type"`

	// Scope selects which quota map below is consulted.
	Scope QuotaScope `yaml:"scope" json:"scope"`

	// ClusterQuotas applies when Scope == QuotaScopeCluster. Keys are
	// accelerator type names (e.g., "H100"); values are the cluster-wide
	// cap in GPUs (or QuotaUnlimited for no cap).
	ClusterQuotas map[string]int `yaml:"quotas,omitempty" json:"quotas,omitempty"`

	// NamespaceQuotas applies when Scope == QuotaScopeNamespace. Top-level
	// keys are namespace names (with the reserved key `default` matching
	// any namespace not explicitly listed); inner keys are accelerator
	// type names; inner values are caps (or QuotaUnlimited).
	//
	// Looked up via QuotaForNamespace; missing or zero entries mean "no
	// allocation".
	NamespaceQuotas map[string]map[string]int `yaml:"namespaceQuotas,omitempty" json:"namespaceQuotas,omitempty"`

	// Exclude lists namespaces that bypass this limiter entirely (no
	// constraint applied). Only meaningful when Scope == QuotaScopeNamespace.
	// Useful for system namespaces or privileged tenants.
	Exclude []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`
}

// IsExcluded reports whether the given namespace bypasses this limiter.
// Always false for cluster-scoped entries.
func (q QuotaLimiterConfig) IsExcluded(namespace string) bool {
	if q.Scope != QuotaScopeNamespace {
		return false
	}
	return slices.Contains(q.Exclude, namespace)
}

// QuotaForNamespace returns the per-type quota map for the given namespace,
// applying the lookup rules from the design:
//
//  1. Excluded namespaces return (nil, true) — caller should skip enforcement.
//  2. Exact match in NamespaceQuotas wins.
//  3. Otherwise, fall through to the reserved `default` key. The returned map
//     represents the cap *per* unlisted namespace (matching the Kubernetes
//     LimitRange default semantic), not a shared pool — the limiter tracks
//     usage per concrete namespace name, so each unlisted namespace consumes
//     its own budget at the default level.
//  4. Otherwise, return an empty map (treated as zero quota by callers).
//
// The boolean second return value indicates whether the namespace is
// excluded (true) — the per-type map is nil in that case and callers
// must skip enforcement rather than treating it as zero.
//
// The returned per-type map is a fresh copy, so callers may read or mutate it
// without aliasing the config (and, critically, without two unlisted namespaces
// sharing one `default` map). Always returns (nil, false) for cluster-scoped
// entries.
func (q QuotaLimiterConfig) QuotaForNamespace(namespace string) (map[string]int, bool) {
	if q.Scope != QuotaScopeNamespace {
		return nil, false
	}
	if q.IsExcluded(namespace) {
		return nil, true
	}
	if quotas, ok := q.NamespaceQuotas[namespace]; ok {
		return maps.Clone(quotas), false
	}
	if quotas, ok := q.NamespaceQuotas[QuotaLimiterReservedNamespaceKey]; ok {
		return maps.Clone(quotas), false
	}
	return map[string]int{}, false
}

// clone returns a deep copy of q: the ClusterQuotas / NamespaceQuotas maps and
// the Exclude slice are duplicated so the result shares no mutable state with
// the original. Used by Config.EffectiveQuotaEntries to hand out entries that callers
// cannot use to mutate the config-owned snapshot.
func (q QuotaLimiterConfig) clone() QuotaLimiterConfig {
	out := q // copies scalar fields and (to be replaced) map/slice headers
	if q.ClusterQuotas != nil {
		out.ClusterQuotas = maps.Clone(q.ClusterQuotas)
	}
	if q.NamespaceQuotas != nil {
		out.NamespaceQuotas = make(map[string]map[string]int, len(q.NamespaceQuotas))
		for ns, perType := range q.NamespaceQuotas {
			out.NamespaceQuotas[ns] = maps.Clone(perType)
		}
	}
	if q.Exclude != nil {
		out.Exclude = slices.Clone(q.Exclude)
	}
	return out
}

// QuotaLimiterEntries is the top-level ConfigMap shape: one or more
// QuotaLimiterConfig entries under a single `limiters` key. Allows
// multiple limiters (e.g., cluster + namespace) to coexist in one
// ConfigMap data entry.
type QuotaLimiterEntries struct {
	Limiters []QuotaLimiterConfig `yaml:"limiters" json:"limiters"`
}

// Validate checks an entries block for structural correctness. It
// returns nil only when every entry parses cleanly. When multiple entries
// (or multiple fields within an entry) are broken, all errors are
// accumulated and joined with errors.Join — an operator fixing a malformed
// ConfigMap sees every problem in one pass instead of one-per-rebuild.
// Error messages name the failing entry by index and field so the bad row
// is easy to locate.
//
// Validation rules (all from issue #1002):
//   - Name is non-empty and unique across entries.
//   - Type == "quota" (other limiter types may be added later).
//   - Scope is one of the two QuotaScope values.
//   - Per-type quota values are >= QuotaUnlimited (only -1 is allowed as negative).
//   - Accelerator type names are non-empty.
//   - For namespace scope: namespace names are non-empty; warn (not error)
//     if a namespace appears in both Exclude and NamespaceQuotas — the
//     entry is still valid and Exclude wins.
//
// Returned warnings (non-fatal) are surfaced via a second return value
// so callers can log them without rejecting the config.
func (e *QuotaLimiterEntries) Validate() (warnings []string, err error) {
	if e == nil {
		return nil, errors.New("quota limiter entries are nil")
	}
	var errs []error
	seenNames := make(map[string]int, len(e.Limiters))
	for i, entry := range e.Limiters {
		if entry.Name == "" {
			errs = append(errs, fmt.Errorf("entry[%d]: name must not be empty", i))
			// Without a name we cannot meaningfully validate the rest of this
			// entry's contents (error messages would lack the entry identifier).
			continue
		}
		if prev, ok := seenNames[entry.Name]; ok {
			errs = append(errs, fmt.Errorf("entry[%d]: duplicate limiter name %q (first seen at entry[%d])", i, entry.Name, prev))
			continue
		}
		seenNames[entry.Name] = i

		if entry.Type != "quota" {
			errs = append(errs, fmt.Errorf("entry[%d] (%q): type must be \"quota\", got %q", i, entry.Name, entry.Type))
			// Keep going: scope/quotas validation is still useful diagnostically.
		}

		switch entry.Scope {
		case QuotaScopeCluster:
			if len(entry.NamespaceQuotas) > 0 {
				errs = append(errs, fmt.Errorf("entry[%d] (%q): namespaceQuotas is invalid for cluster scope; use the quotas map", i, entry.Name))
			}
			if len(entry.Exclude) > 0 {
				errs = append(errs, fmt.Errorf("entry[%d] (%q): exclude is invalid for cluster scope", i, entry.Name))
			}
			if vErr := validateTypeQuotaMap(entry.ClusterQuotas, fmt.Sprintf("entry[%d] (%q).quotas", i, entry.Name)); vErr != nil {
				errs = append(errs, vErr)
			}

		case QuotaScopeNamespace:
			if len(entry.ClusterQuotas) > 0 {
				errs = append(errs, fmt.Errorf("entry[%d] (%q): quotas is invalid for namespace scope; use namespaceQuotas", i, entry.Name))
			}
			for _, ns := range entry.Exclude {
				if strings.TrimSpace(ns) == "" {
					errs = append(errs, fmt.Errorf("entry[%d] (%q): exclude contains an empty namespace name", i, entry.Name))
				}
			}
			for ns, perType := range entry.NamespaceQuotas {
				if strings.TrimSpace(ns) == "" {
					errs = append(errs, fmt.Errorf("entry[%d] (%q): namespaceQuotas contains an empty namespace key", i, entry.Name))
					continue
				}
				if entry.IsExcluded(ns) {
					warnings = append(warnings,
						fmt.Sprintf("entry[%d] (%q): namespace %q is in both exclude and namespaceQuotas; the quota entry will be ignored (exclude wins)", i, entry.Name, ns))
				}
				if vErr := validateTypeQuotaMap(perType, fmt.Sprintf("entry[%d] (%q).namespaceQuotas[%q]", i, entry.Name, ns)); vErr != nil {
					errs = append(errs, vErr)
				}
			}

		default:
			errs = append(errs, fmt.Errorf("entry[%d] (%q): scope must be %q or %q, got %q",
				i, entry.Name, QuotaScopeCluster, QuotaScopeNamespace, entry.Scope))
		}
	}
	return warnings, errors.Join(errs...)
}

// validateTypeQuotaMap checks per-accelerator-type entries: type name
// non-empty, value >= QuotaUnlimited. Empty maps are allowed (the
// limiter simply has no caps for that scope). Errors within a single map
// are accumulated with errors.Join so an operator with multiple bad rows
// sees them all at once.
func validateTypeQuotaMap(quotas map[string]int, location string) error {
	var errs []error
	for accType, value := range quotas {
		if strings.TrimSpace(accType) == "" {
			errs = append(errs, fmt.Errorf("%s: accelerator type name must not be empty", location))
			continue
		}
		if value < QuotaUnlimited {
			errs = append(errs, fmt.Errorf("%s[%q]: quota value %d is invalid; must be >= %d (only -1 is allowed as negative — means unlimited)",
				location, accType, value, QuotaUnlimited))
		}
		if value > MaxQuotaValue {
			errs = append(errs, fmt.Errorf("%s[%q]: quota value %d exceeds the maximum of %d",
				location, accType, value, MaxQuotaValue))
		}
	}
	return errors.Join(errs...)
}
