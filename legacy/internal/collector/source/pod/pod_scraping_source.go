// Package pod provides the Pod scraping metrics source implementation.
//
// This package implements the PodScrapingSource that scrapes metrics directly
// from pods via HTTP requests to their /metrics endpoints.
package pod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// PodScrapingSource implements MetricsSource for direct pod scraping.
type PodScrapingSource struct {
	config     PodScrapingSourceConfig
	k8sClient  client.Client
	httpClient *http.Client
	registry   *source.QueryList

	mu    sync.RWMutex // protects the cache and refresh operations
	cache *source.Cache
}

// NewPodScrapingSource creates a new PodScrapingSource for an InferencePool.
func NewPodScrapingSource(
	ctx context.Context,
	k8sClient client.Client,
	config PodScrapingSourceConfig,
) (*PodScrapingSource, error) {
	// Validate required fields
	if config.ServiceName == "" {
		return nil, errors.New("ServiceName is required")
	}
	if config.ServiceNamespace == "" {
		return nil, errors.New("ServiceNamespace is required")
	}

	// Set defaults
	if config.MetricsPath == "" {
		config.MetricsPath = "/metrics"
	}
	if config.MetricsScheme == "" {
		config.MetricsScheme = "http"
	}
	if config.ScrapeTimeout == 0 {
		config.ScrapeTimeout = 5 * time.Second
	}
	if config.MaxConcurrentScrapes == 0 {
		config.MaxConcurrentScrapes = 10
	}
	if config.DefaultTTL == 0 {
		config.DefaultTTL = 30 * time.Second
	}

	// Build HTTP client
	httpClient := &http.Client{
		Timeout: config.ScrapeTimeout,
	}

	podSource := &PodScrapingSource{
		config:     config,
		k8sClient:  k8sClient,
		httpClient: httpClient,
		registry:   source.NewQueryList(),
		cache:      source.NewCache(ctx, config.DefaultTTL, 1*time.Second),
	}

	// Register default query
	podSource.registry.MustRegister(source.QueryTemplate{
		Name:        "all_metrics",
		Type:        source.QueryTypeMetricName,
		Template:    "all_metrics",
		Params:      []string{},
		Description: "All metrics from pods scraped",
	})

	return podSource, nil
}

// QueryList returns the query registry for this source.
func (p *PodScrapingSource) QueryList() *source.QueryList {
	return p.registry
}

// Refresh executes queries and updates the cache.
// Called by engine/reconciler on-demand.
func (p *PodScrapingSource) Refresh(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	logger := ctrl.LoggerFrom(ctx)

	// Discover pods
	pods, err := p.discoverPods(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to discover pods: %w", err)
	}

	if len(pods) == 0 {
		logger.V(logging.DEBUG).Info("No ready pods found for scraping")
		return map[string]*source.MetricResult{
			"all_metrics": {
				QueryName:   "all_metrics",
				Values:      []source.MetricValue{},
				CollectedAt: time.Now(),
			},
		}, nil
	}

	// Scrape all pods concurrently
	results := p.scrapeAllPods(ctx, pods)

	// Aggregate results
	aggregated := p.aggregateResults(results)

	// Cache the aggregated result under the canonical "all_metrics" key.
	cacheKey := source.BuildCacheKey("all_metrics", nil)
	p.cache.Set(cacheKey, *aggregated, p.config.DefaultTTL)

	// Also cache per-metric slices keyed by __name__ so that
	// Get("vllm:queue_size", nil) returns only the relevant values.
	perMetric := splitByMetricName(aggregated)

	out := make(map[string]*source.MetricResult, len(perMetric)+1)
	out["all_metrics"] = aggregated
	for name, mr := range perMetric {
		out[name] = mr
		p.cache.Set(source.BuildCacheKey(name, nil), *mr, p.config.DefaultTTL)
	}

	logger.V(logging.DEBUG).Info("Scraped metrics from pods",
		"podCount", len(pods),
		"successCount", len(results),
		"metricCount", len(aggregated.Values),
		"distinctMetrics", len(perMetric))

	return out, nil
}

// Get retrieves cached metrics.
func (p *PodScrapingSource) Get(queryName string, params map[string]string) *source.CachedValue {
	p.mu.RLock()
	defer p.mu.RUnlock()

	cacheKey := source.BuildCacheKey(queryName, params)
	cached, ok := p.cache.Get(cacheKey)
	if !ok || cached.IsExpired() {
		return nil
	}

	return cached
}

// discoverPods finds all Ready pods for the service.
func (p *PodScrapingSource) discoverPods(ctx context.Context) ([]*corev1.Pod, error) {
	// Get Service
	svc := &corev1.Service{}
	svcKey := types.NamespacedName{
		Name:      p.config.ServiceName,
		Namespace: p.config.ServiceNamespace,
	}
	if err := p.k8sClient.Get(ctx, svcKey, svc); err != nil {
		return nil, fmt.Errorf("failed to get service %s: %w", svcKey, err)
	}

	// Check if service has a selector (headless services may not have one)
	if len(svc.Spec.Selector) == 0 {
		// Service has no selector - cannot discover pods via selector
		// Return empty list (no error) as this is a valid configuration
		return []*corev1.Pod{}, nil
	}

	// List pods using Service selector
	podList := &corev1.PodList{}
	selector := labels.SelectorFromSet(svc.Spec.Selector)
	if err := p.k8sClient.List(ctx, podList, &client.ListOptions{
		Namespace:     p.config.ServiceNamespace,
		LabelSelector: selector,
	}); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Filter to Ready pods only
	readyPods := []*corev1.Pod{}
	for i := range podList.Items {
		pod := &podList.Items[i]
		if isPodReady(pod) {
			readyPods = append(readyPods, pod)
		}
	}

	return readyPods, nil
}

// isPodReady checks if pod is in Ready condition.
func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// scrapeAllPods scrapes metrics from all pods concurrently.
func (p *PodScrapingSource) scrapeAllPods(ctx context.Context, pods []*corev1.Pod) map[string]*source.MetricResult {
	logger := ctrl.LoggerFrom(ctx)
	results := make(map[string]*source.MetricResult)
	var resultsMu sync.Mutex

	// Semaphore for concurrency control
	sem := make(chan struct{}, p.config.MaxConcurrentScrapes)
	var wg sync.WaitGroup

	for _, pod := range pods {
		wg.Add(1)
		go func(pod *corev1.Pod) {
			defer wg.Done()

			sem <- struct{}{}        // Acquire
			defer func() { <-sem }() // Release

			result, err := p.scrapePodMetrics(ctx, pod)
			if err != nil {
				errorType := "Failed to scrape pod"
				logger.V(logging.VERBOSE).Error(err, errorType,
					"pod", pod.Name)
				metrics.RecordError(constants.ComponentCollector, errorType)
				return
			}

			resultsMu.Lock()
			results[pod.Name] = result
			resultsMu.Unlock()
		}(pod)
	}

	wg.Wait()
	return results
}

// scrapePodMetrics scrapes metrics from a single pod.
func (p *PodScrapingSource) scrapePodMetrics(ctx context.Context, pod *corev1.Pod) (*source.MetricResult, error) {
	// Build URL: {scheme}://{podIP}:{port}{path}
	if pod.Status.PodIP == "" {
		return nil, fmt.Errorf("pod %s has no IP address", pod.Name)
	}

	url := fmt.Sprintf("%s://%s:%d%s",
		p.config.MetricsScheme,
		pod.Status.PodIP,
		p.config.MetricsPort,
		p.config.MetricsPath,
	)

	// Create request with timeout
	reqCtx, cancel := context.WithTimeout(ctx, p.config.ScrapeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Add authentication header if a bearer token is configured.
	token, useAuth := p.getAuthToken()
	if useAuth {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	// Execute request
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to scrape pod %s: %w", pod.Name, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pod %s returned status %d", pod.Name, resp.StatusCode)
	}

	// Parse Prometheus text format
	return p.parsePrometheusMetrics(resp.Body, pod.Name)
}

// getAuthToken returns the bearer token and whether authentication should be used.
// If BearerToken is configured, it is returned; otherwise authentication is skipped.
func (p *PodScrapingSource) getAuthToken() (string, bool) {
	if p.config.BearerToken != "" {
		return p.config.BearerToken, true
	}
	return "", false
}

// parsePrometheusMetrics parses Prometheus text format into MetricResult.
func (p *PodScrapingSource) parsePrometheusMetrics(reader io.Reader, podName string) (*source.MetricResult, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	metricFamilies, err := parser.TextToMetricFamilies(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to parse metrics: %w", err)
	}

	// Convert to source.MetricValue array
	values := []source.MetricValue{}
	now := time.Now()

	for name, family := range metricFamilies {
		for _, metric := range family.Metric {
			labels := make(map[string]string)
			labels["pod"] = podName // Add pod label

			// Extract metric labels
			for _, labelPair := range metric.Label {
				if labelPair.Name != nil && labelPair.Value != nil {
					labels[*labelPair.Name] = *labelPair.Value
				}
			}

			// Extract metric value
			var value float64
			switch {
			case metric.Gauge != nil:
				value = metric.Gauge.GetValue()
			case metric.Counter != nil:
				value = metric.Counter.GetValue()
			case metric.Histogram != nil:
				value = float64(metric.Histogram.GetSampleCount())
			case metric.Summary != nil:
				value = float64(metric.Summary.GetSampleCount())
			case metric.Untyped != nil:
				value = metric.Untyped.GetValue()
			}

			// Add metric name as label for identification
			labels["__name__"] = name

			values = append(values, source.MetricValue{
				Value:     value,
				Timestamp: now, // Use current time as scrape timestamp
				Labels:    labels,
			})
		}
	}

	return &source.MetricResult{
		QueryName:   "all_metrics",
		Values:      values,
		CollectedAt: now,
	}, nil
}

// splitByMetricName groups aggregated values by their "__name__" label,
// returning one MetricResult per distinct metric name.
// Values without a "__name__" label and the reserved name "all_metrics" are skipped.
func splitByMetricName(aggregated *source.MetricResult) map[string]*source.MetricResult {
	grouped := make(map[string][]source.MetricValue)
	for _, v := range aggregated.Values {
		name := v.Labels["__name__"]
		if name == "" || name == "all_metrics" {
			continue
		}
		grouped[name] = append(grouped[name], v)
	}

	out := make(map[string]*source.MetricResult, len(grouped))
	for name, vals := range grouped {
		out[name] = &source.MetricResult{
			QueryName:   name,
			Values:      vals,
			CollectedAt: aggregated.CollectedAt,
		}
	}
	return out
}

// aggregateResults combines metrics from all pods.
func (p *PodScrapingSource) aggregateResults(results map[string]*source.MetricResult) *source.MetricResult {
	allValues := []source.MetricValue{}
	var latestCollectedAt time.Time

	for _, result := range results {
		if result == nil {
			continue
		}

		// Add all values (already have pod label)
		allValues = append(allValues, result.Values...)

		if result.CollectedAt.After(latestCollectedAt) {
			latestCollectedAt = result.CollectedAt
		}
	}

	if latestCollectedAt.IsZero() {
		latestCollectedAt = time.Now()
	}

	return &source.MetricResult{
		QueryName:   "all_metrics",
		Values:      allValues,
		CollectedAt: latestCollectedAt,
	}
}
