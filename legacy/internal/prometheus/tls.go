package prometheus

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// IsHTTPS reports whether rawURL uses the https scheme.
// url.Parse normalizes the scheme to lowercase before comparison, so HTTPS:// is treated the same as https://.
func IsHTTPS(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == schemeHTTPS
}

// CreateTLSConfig creates a TLS configuration from getter-based Prometheus config.
// TLS is applied only for https:// endpoints. The configuration supports:
// - Server certificate validation via CA certificate
// - Mutual TLS authentication via client certificates
// - Insecure certificate verification (development/testing only)
func CreateTLSConfig(cfg *config.Config) (*tls.Config, error) {
	if cfg == nil {
		return nil, nil
	}

	insecureSkipVerify := cfg.PrometheusInsecureSkipVerify()
	serverName := cfg.PrometheusServerName()
	caCertPath := cfg.PrometheusCACertPath()
	clientCertPath := cfg.PrometheusClientCertPath()
	clientKeyPath := cfg.PrometheusClientKeyPath()

	config := &tls.Config{
		InsecureSkipVerify: insecureSkipVerify,
		ServerName:         serverName,
		MinVersion:         tls.VersionTLS12, // Enforce minimum TLS version - https://docs.redhat.com/en/documentation/openshift_container_platform/4.18/html/security_and_compliance/tls-security-profiles#:~:text=requires%20a%20minimum-,TLS%20version%20of%201.2,-.
		NextProtos:         []string{"h2", "http/1.1"},
	}

	if insecureSkipVerify {
		ctrl.Log.V(logging.VERBOSE).Info("TLS certificate verification is disabled, skipping certificate loading")
		return config, nil
	}

	// Load CA certificate if provided
	if caCertPath != "" {
		caCert, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate from %s: %w", caCertPath, err)
		}

		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate from %s", caCertPath)
		}
		config.RootCAs = caCertPool
		ctrl.Log.V(logging.VERBOSE).Info("CA certificate loaded successfully", "path", caCertPath)
	}

	// Load client certificate and key if provided
	if clientCertPath != "" && clientKeyPath != "" {
		cert, err := tls.LoadX509KeyPair(clientCertPath, clientKeyPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate from %s and key from %s: %w",
				clientCertPath, clientKeyPath, err)
		}
		config.Certificates = []tls.Certificate{cert}
		ctrl.Log.V(logging.VERBOSE).Info("Client certificate loaded successfully",
			"cert_path", clientCertPath, "key_path", clientKeyPath)
	}

	return config, nil
}

// ValidateTLSConfig validates the Prometheus transport configuration.
// Ensures the URL scheme is supported, and that certificate files exist when
// HTTPS verification is enabled.
func ValidateTLSConfig(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("config is nil")
	}

	baseURL := cfg.PrometheusBaseURL()
	insecureSkipVerify := cfg.PrometheusInsecureSkipVerify()
	allowHTTP := cfg.PrometheusAllowHTTP()
	caCertPath := cfg.PrometheusCACertPath()
	clientCertPath := cfg.PrometheusClientCertPath()
	clientKeyPath := cfg.PrometheusClientKeyPath()

	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("invalid Prometheus URL %q: %w", baseURL, err)
	}

	// Reject credentials embedded in the URL for any scheme: they leak through
	// process listings, config dumps, redirect Referer headers, and any log that
	// doesn't redact the URL — even over TLS. Use PROMETHEUS_BEARER_TOKEN or
	// PROMETHEUS_TOKEN_PATH (over https) instead.
	if u.User != nil {
		return fmt.Errorf("refusing to use Prometheus URL with embedded credentials %q; use PROMETHEUS_BEARER_TOKEN or PROMETHEUS_TOKEN_PATH instead", u.Redacted())
	}

	switch u.Scheme {
	case schemeHTTPS:
		// Continue with the HTTPS-specific validation below.
	case schemeHTTP:
		if !allowHTTP {
			return fmt.Errorf("plain HTTP Prometheus URL %q is not allowed; set PROMETHEUS_ALLOW_HTTP=true to permit http:// endpoints", baseURL)
		}
		if cfg.PrometheusBearerToken() != "" || cfg.PrometheusTokenPath() != "" {
			return fmt.Errorf("refusing to send bearer token authentication over plain HTTP Prometheus URL %q", baseURL)
		}
		if insecureSkipVerify || caCertPath != "" || clientCertPath != "" || clientKeyPath != "" || cfg.PrometheusServerName() != "" {
			return fmt.Errorf("TLS-related settings are not supported with plain HTTP Prometheus URL %q; remove them or use an https:// URL", baseURL)
		}
		ctrl.Log.Info("Plain HTTP Prometheus endpoint allowed by configuration", "address", u.Redacted())
		return nil
	default:
		return fmt.Errorf("unsupported Prometheus URL scheme %q in %q; expected http or https", u.Scheme, baseURL)
	}

	// If InsecureSkipVerify is true, we don't need to validate certificate files
	// since we're intentionally skipping certificate verification
	if insecureSkipVerify {
		ctrl.Log.V(logging.VERBOSE).Info("TLS certificate verification is disabled - this is not recommended for production")
		return nil
	}

	// Check if certificate files exist (only when not skipping verification)
	if caCertPath != "" {
		if _, err := os.Stat(caCertPath); os.IsNotExist(err) {
			return fmt.Errorf("CA certificate file not found: %s", caCertPath)
		}
	}

	if clientCertPath != "" {
		if _, err := os.Stat(clientCertPath); os.IsNotExist(err) {
			return fmt.Errorf("client certificate file not found: %s", clientCertPath)
		}
	}

	if clientKeyPath != "" {
		if _, err := os.Stat(clientKeyPath); os.IsNotExist(err) {
			return fmt.Errorf("client key file not found: %s", clientKeyPath)
		}
	}

	return nil
}
