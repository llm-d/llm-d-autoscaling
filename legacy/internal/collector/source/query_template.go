package source

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

var (
	// Pre-compiled regex patterns for PromQL value escaping.
	backslashPattern = regexp.MustCompile(`\\`)
	quotePattern     = regexp.MustCompile(`"`)
)

// Common parameter names used across queries.
const (
	ParamNamespace = "namespace"
	ParamModelID   = "modelID"
	ParamPodFilter = "podFilter" // Optional regex filter for pod names
)

// QueryType distinguishes between simple metric names and full PromQL expressions.
type QueryType string

const (
	// QueryTypeMetricName is a simple metric name (e.g., "vllm:kv_cache_usage_perc").
	// Used for backends that don't support PromQL (pod-scrape, EPP).
	QueryTypeMetricName QueryType = "metric"
	// QueryTypePromQL is a full PromQL expression with optional template parameters.
	// Only supported by the Prometheus backend.
	QueryTypePromQL QueryType = "promql"
)

// QueryTemplate defines a registered query with its metadata and template.
type QueryTemplate struct {
	// Name is the unique identifier for this query (e.g., "kv_cache_usage").
	Name string
	// Type indicates whether this is a metric name or PromQL expression.
	Type QueryType
	// Template is the query string with {{.ParamName}} placeholders.
	// For QueryTypeMetricName: just the metric name (e.g., "vllm:kv_cache_usage_perc")
	// For QueryTypePromQL: full PromQL (e.g., "max by (pod) ({{.metric}}{namespace=\"{{.namespace}}\"})")
	Template string
	// Params lists the parameter names required by this template (e.g., ["namespace", "modelID"]).
	Params []string
	// Description documents what this query returns.
	Description string
}

// QueryList stores and manages query templates for a metrics source.
// Each source (PrometheusSource, EPPSource, etc.) has its own QueryList.
type QueryList struct {
	mu      sync.RWMutex
	queries map[string]QueryTemplate
}

// NewQueryList creates a new query registry.
func NewQueryList() *QueryList {
	return &QueryList{
		queries: make(map[string]QueryTemplate),
	}
}

// Register adds a query template to this registry.
func (r *QueryList) Register(query QueryTemplate) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if query.Name == "" {
		return errors.New("query name is required")
	}
	if query.Template == "" {
		return fmt.Errorf("query template is required for %q", query.Name)
	}

	if _, exists := r.queries[query.Name]; exists {
		return fmt.Errorf("query %q already registered", query.Name)
	}
	r.queries[query.Name] = query
	return nil
}

// MustRegister is like Register but panics on error.
// Use for init-time registration where errors are programming mistakes.
func (r *QueryList) MustRegister(query QueryTemplate) {
	if err := r.Register(query); err != nil {
		panic(fmt.Sprintf("failed to register query: %v", err))
	}
}

// Get retrieves a registered query by name.
func (r *QueryList) Get(name string) *QueryTemplate {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if q, ok := r.queries[name]; ok {
		return &q
	}
	return nil
}

// Build constructs the final query string by substituting parameters.
// Uses simple {{.paramName}} placeholder replacement.
func (r *QueryList) Build(name string, params map[string]string) (string, error) {
	r.mu.RLock()
	query, ok := r.queries[name]
	r.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("query %q not found", name)
	}

	// Validate all required parameters are provided
	for _, param := range query.Params {
		if _, ok := params[param]; !ok {
			return "", fmt.Errorf("missing required parameter %q for query %q", param, name)
		}
	}

	// Substitute parameters in template
	result := query.Template
	for key, value := range params {
		placeholder := "{{." + key + "}}"
		result = strings.ReplaceAll(result, placeholder, value)
	}

	return result, nil
}

// List returns all registered query names.
func (r *QueryList) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.queries))
	for name := range r.queries {
		names = append(names, name)
	}
	return names
}

// --- Helpers ---

// EscapePromQLValue escapes a value for safe use in PromQL label matchers.
// It escapes backslashes and double quotes to prevent injection when values
// are substituted into query templates.
//
// Kubernetes naming (e.g. namespace) only allows DNS labels, so namespace
// values cannot contain " or \. Other parameters are not restricted: e.g.
// VariantAutoscaling.Spec.ModelID is user-controlled and has no pattern
// validation, so escaping is required for modelID and any future
// user- or config-driven parameters.
func EscapePromQLValue(value string) string {
	// Escape backslashes first (must be done before escaping quotes)
	value = backslashPattern.ReplaceAllString(value, `\\`)
	// Escape double quotes
	value = quotePattern.ReplaceAllString(value, `\"`)
	return value
}
