// Package pod provides the Pod scraping metrics source implementation.
//
// This file contains configuration types and defaults for PodScrapingSource.
package pod

import "time"

// PodScrapingSourceConfig contains configuration for pod scraping.
type PodScrapingSourceConfig struct {
	// Service identification (required)
	ServiceName      string
	ServiceNamespace string

	// Metrics endpoint (provided by client/engine)
	MetricsPort   int32  // provided by client
	MetricsPath   string // provided by client, default: "/metrics"
	MetricsScheme string // provided by client, default: "http"

	// Authentication
	BearerToken string

	// Scraping behavior
	ScrapeTimeout        time.Duration // default: 5s per pod
	MaxConcurrentScrapes int           // default: 10

	// Cache configuration
	DefaultTTL time.Duration // default: 30s
}
