package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// NodeSelector mirrors metav1.LabelSelector but carries yaml tags.
//
// The ConfigMaps are decoded with gopkg.in/yaml.v3, which ignores the json tags
// that metav1 types rely on and lowercases field names instead. Embedding
// metav1.LabelSelector directly therefore parses `matchLabels:` as nothing: the
// selector comes out empty, compiles to labels.Everything(), and every namespace
// silently matches every node. Declaring the shape here with explicit yaml tags
// (json tags too, so the type also survives a json/sigs.k8s.io-yaml decode)
// keeps the configured selector intact.
type NodeSelector struct {
	// MatchLabels is a map of {key,value} pairs, ANDed together.
	MatchLabels map[string]string `yaml:"matchLabels,omitempty" json:"matchLabels,omitempty"`

	// MatchExpressions is a list of label selector requirements, ANDed together.
	MatchExpressions []NodeSelectorRequirement `yaml:"matchExpressions,omitempty" json:"matchExpressions,omitempty"`
}

// NodeSelectorRequirement is one label selector requirement: a key, an
// operator, and (for In/NotIn) the values it is matched against.
type NodeSelectorRequirement struct {
	// Key is the label key the requirement applies to.
	Key string `yaml:"key" json:"key"`

	// Operator is one of In, NotIn, Exists, or DoesNotExist.
	Operator string `yaml:"operator" json:"operator"`

	// Values is the set of label values. Required for In/NotIn, and must be
	// empty for Exists/DoesNotExist.
	Values []string `yaml:"values,omitempty" json:"values,omitempty"`
}

// IsEmpty reports whether the selector constrains nothing. An empty selector
// compiles to labels.Everything() without error, so callers must reject it
// explicitly rather than relying on compilation to fail.
func (s NodeSelector) IsEmpty() bool {
	return len(s.MatchLabels) == 0 && len(s.MatchExpressions) == 0
}

// toLabelSelector converts to the Kubernetes type so the standard compiler and
// its operator validation can be reused.
func (s NodeSelector) toLabelSelector() *metav1.LabelSelector {
	out := &metav1.LabelSelector{}
	if s.MatchLabels != nil {
		out.MatchLabels = maps.Clone(s.MatchLabels)
	}
	for _, r := range s.MatchExpressions {
		out.MatchExpressions = append(out.MatchExpressions, metav1.LabelSelectorRequirement{
			Key:      r.Key,
			Operator: metav1.LabelSelectorOperator(r.Operator),
			Values:   slices.Clone(r.Values),
		})
	}
	return out
}

// Compile converts the selector into a labels.Selector. An empty selector is
// rejected: it would match every node and hand one namespace the whole cluster.
func (s NodeSelector) Compile() (labels.Selector, error) {
	if s.IsEmpty() {
		return nil, errors.New("selector is empty (matches every node); specify matchLabels or matchExpressions")
	}
	return metav1.LabelSelectorAsSelector(s.toLabelSelector())
}

// DeepCopy returns a copy that shares no maps or slices with the receiver.
func (s NodeSelector) DeepCopy() NodeSelector {
	out := NodeSelector{}
	if s.MatchLabels != nil {
		out.MatchLabels = maps.Clone(s.MatchLabels)
	}
	if s.MatchExpressions != nil {
		out.MatchExpressions = make([]NodeSelectorRequirement, len(s.MatchExpressions))
		for i, r := range s.MatchExpressions {
			r.Values = slices.Clone(r.Values)
			out.MatchExpressions[i] = r
		}
	}
	return out
}

// NamespaceInventoryConfig is the resolved configuration for the
// namespace-scoped physical GPU limiter. It is derived from the inline
// `limiters:` entry of type namespace-inventory and owns its own validation and
// deep copy, so the quota schema's rules never silently apply to it.
type NamespaceInventoryConfig struct {
	// Name identifies the limiter in logs and DecisionStep traces.
	Name string

	// Exclude lists namespaces that bypass the cap entirely. Their usage is
	// still charged against the pool they draw from, so a shared pool cannot be
	// overcommitted.
	Exclude []string

	// Selectors maps a namespace (or the reserved NamespaceInventoryDefaultKey)
	// to the node label selector whose matching nodes form its GPU pool.
	Selectors map[string]NodeSelector
}

// NamespaceInventoryDefaultKey is the reserved selectors key whose pool serves
// every namespace without an explicit entry. It cannot name a real namespace;
// list the literal "default" namespace in Exclude if it must bypass the limiter.
const NamespaceInventoryDefaultKey = "default"

// Validate checks the resolved configuration. Selectors must be present and
// none may match every node, since an empty selector compiles successfully and
// would give one namespace the entire cluster.
func (c NamespaceInventoryConfig) Validate() error {
	if len(c.Selectors) == 0 {
		return errors.New("requires at least one entry in selectors")
	}
	for _, ns := range slices.Sorted(maps.Keys(c.Selectors)) {
		if _, err := c.Selectors[ns].Compile(); err != nil {
			return fmt.Errorf("invalid node selector for namespace %q: %w", ns, err)
		}
	}
	// A namespace that is both excluded and named would be uncapped by the
	// resolver yet charged to its own bucket, which reads as a config mistake
	// rather than an intent worth honoring.
	for _, ns := range slices.Sorted(maps.Keys(c.Selectors)) {
		if ns != NamespaceInventoryDefaultKey && slices.Contains(c.Exclude, ns) {
			return fmt.Errorf("namespace %q is both excluded and given a selector; remove it from one", ns)
		}
	}
	return nil
}

// DeepCopy returns a copy that shares no maps or slices with the receiver, so
// callers cannot mutate configuration held behind the Config mutex.
func (c NamespaceInventoryConfig) DeepCopy() NamespaceInventoryConfig {
	out := NamespaceInventoryConfig{Name: c.Name}
	if c.Exclude != nil {
		out.Exclude = slices.Clone(c.Exclude)
	}
	if c.Selectors != nil {
		out.Selectors = make(map[string]NodeSelector, len(c.Selectors))
		for ns, sel := range c.Selectors {
			out.Selectors[ns] = sel.DeepCopy()
		}
	}
	return out
}
