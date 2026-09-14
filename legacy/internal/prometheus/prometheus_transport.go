package prometheus

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/prometheus/client_golang/api"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// CreatePrometheusTransport creates a custom transport for the Prometheus client.
// TLS configuration is only applied for https:// endpoints; plain http://
// endpoints (allowed only when PROMETHEUS_ALLOW_HTTP is set, see ValidateTLSConfig)
// use the default transport unmodified.
func CreatePrometheusTransport(cfg *config.Config) (http.RoundTripper, error) {
	// Clone the default transport to get all the good defaults
	transport := http.DefaultTransport.(*http.Transport).Clone()

	if IsHTTPS(cfg.PrometheusBaseURL()) {
		tlsConfig, err := CreateTLSConfig(cfg)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = tlsConfig
		ctrl.Log.V(logging.VERBOSE).Info("TLS configuration applied to Prometheus HTTPS transport")
	}

	return transport, nil
}

// CreatePrometheusClientConfig creates a complete Prometheus client configuration.
// Supports both direct bearer tokens and token files for flexible authentication.
func CreatePrometheusClientConfig(cfg *config.Config) (*api.Config, error) {
	clientConfig := &api.Config{
		Address: cfg.PrometheusBaseURL(),
	}

	// Create the custom transport; TLS is applied only for https:// endpoints.
	transport, err := CreatePrometheusTransport(cfg)
	if err != nil {
		return nil, err
	}

	// Add bearer token authentication if provided
	bearerToken := cfg.PrometheusBearerToken()

	// If no direct bearer token but token path is provided, read from file
	if bearerToken == "" && cfg.PrometheusTokenPath() != "" {
		tokenBytes, err := os.ReadFile(cfg.PrometheusTokenPath())
		if err != nil {
			return nil, fmt.Errorf("failed to read bearer token from %s: %w", cfg.PrometheusTokenPath(), err)
		}
		bearerToken = strings.TrimSpace(string(tokenBytes))
		ctrl.Log.V(logging.VERBOSE).Info("Bearer token loaded from file", "path", cfg.PrometheusTokenPath())
	}

	if bearerToken != "" {
		// Create a custom round tripper that adds the bearer token
		transport = &bearerTokenRoundTripper{
			base:  transport,
			token: bearerToken,
		}
	}

	clientConfig.RoundTripper = transport

	return clientConfig, nil
}

// bearerTokenRoundTripper adds bearer token authentication to HTTPS requests.
// The header is only set for https:// requests to prevent credential leakage
// if a redirect leads to a plain http:// endpoint.
type bearerTokenRoundTripper struct {
	base  http.RoundTripper
	token string
}

// RoundTrip adds the Authorization header for HTTPS requests only.
func (b *bearerTokenRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == schemeHTTPS {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(req)
}
