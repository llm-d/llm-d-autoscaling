package prometheus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FormatPrometheusDuration converts a Go time.Duration to Prometheus duration format.
// Prometheus uses formats like "5m", "1h", "30s", "1d".
func FormatPrometheusDuration(d time.Duration) string {
	// Handle days (Prometheus supports 'd' suffix)
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		days := d / (24 * time.Hour)
		return fmt.Sprintf("%dd", days)
	}

	// Handle hours
	if d >= time.Hour && d%time.Hour == 0 {
		hours := d / time.Hour
		return fmt.Sprintf("%dh", hours)
	}

	// Handle minutes
	if d >= time.Minute && d%time.Minute == 0 {
		minutes := d / time.Minute
		return fmt.Sprintf("%dm", minutes)
	}

	// Handle seconds
	if d >= time.Second && d%time.Second == 0 {
		seconds := d / time.Second
		return fmt.Sprintf("%ds", seconds)
	}

	// Default to seconds (round down)
	seconds := d / time.Second
	if seconds > 0 {
		return fmt.Sprintf("%ds", seconds)
	}

	// Very short durations, use milliseconds (Prometheus doesn't support ms, use minimum 1s)
	return "1s"
}

// CategorizePrometheusError maps a Prometheus error to a bounded set of error categories
// suitable for use as a metric label. This prevents high cardinality in metrics.
func CategorizePrometheusError(err error) string {
	if err == nil {
		return ""
	}

	// Check for context errors first
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}

	// Check error message content
	errMsg := strings.ToLower(err.Error())

	switch {
	case strings.Contains(errMsg, "connection refused"):
		return "connection_refused"
	case strings.Contains(errMsg, "no such host"):
		return "dns_error"
	case strings.Contains(errMsg, "timeout"):
		return "timeout"
	case strings.Contains(errMsg, "parse error"):
		return "parse_error"
	case strings.Contains(errMsg, "bad_data"):
		return "bad_data"
	case strings.Contains(errMsg, "execution"):
		return "execution_error"
	case strings.Contains(errMsg, "query processing"):
		return "query_processing"
	default:
		return "unknown"
	}
}
