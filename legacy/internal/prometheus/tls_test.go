package prometheus

import (
	"crypto/tls"
	"net/http"
	"os"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const envPrometheusAllowHTTP = "PROMETHEUS_ALLOW_HTTP"

func init() {
	// Initialize logger for tests
	logging.NewTestLogger()
}

func testConfigFromEnv(t *testing.T, env map[string]string) *config.Config {
	t.Helper()

	keys := []string{
		"PROMETHEUS_BASE_URL",
		"PROMETHEUS_BEARER_TOKEN",
		"PROMETHEUS_TOKEN_PATH",
		"PROMETHEUS_TLS_INSECURE_SKIP_VERIFY",
		envPrometheusAllowHTTP,
		"PROMETHEUS_CA_CERT_PATH",
		"PROMETHEUS_CLIENT_CERT_PATH",
		"PROMETHEUS_CLIENT_KEY_PATH",
		"PROMETHEUS_SERVER_NAME",
	}

	originalValues := make(map[string]string, len(keys))
	originalSet := make(map[string]bool, len(keys))
	for _, key := range keys {
		value, ok := os.LookupEnv(key)
		if ok {
			originalValues[key] = value
			originalSet[key] = true
		}
		_ = os.Unsetenv(key)
	}

	for key, value := range env {
		require.NoError(t, os.Setenv(key, value))
	}

	t.Cleanup(func() {
		for _, key := range keys {
			if originalSet[key] {
				_ = os.Setenv(key, originalValues[key])
			} else {
				_ = os.Unsetenv(key)
			}
		}
	})

	cfg, err := config.Load(nil, "")
	require.NoError(t, err)
	return cfg
}

func TestIsHTTPS(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   bool
	}{
		{"https scheme", "https://prometheus:9090", true},
		{"HTTPS uppercase scheme", "HTTPS://prometheus:9090", true},
		{"http scheme", "http://prometheus:9090", false},
		{"empty string", "", false},
		{"invalid url", "://bad", false},
		{"no scheme", "prometheus:9090", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsHTTPS(tt.rawURL))
		})
	}
}

func TestCreateTLSConfig(t *testing.T) {
	tests := []struct {
		name        string
		promConfig  *config.Config
		expectError bool
	}{
		{
			name:        "nil config",
			promConfig:  nil,
			expectError: false,
		},
		{
			name: "TLS with insecure skip verify",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":                 "https://prometheus:9090",
				"PROMETHEUS_TLS_INSECURE_SKIP_VERIFY": "true",
			}),
			expectError: false,
		},
		{
			name: "TLS with server name",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":    "https://prometheus:9090",
				"PROMETHEUS_SERVER_NAME": "prometheus.example.com",
			}),
			expectError: false,
		},
		{
			name: "insecure skip verify with invalid CA cert path should not error",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":                 "https://prometheus:9090",
				"PROMETHEUS_TLS_INSECURE_SKIP_VERIFY": "true",
				"PROMETHEUS_CA_CERT_PATH":             "/nonexistent/path/ca.crt",
			}),
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tlsCfg, err := CreateTLSConfig(tt.promConfig)
			if tt.expectError {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			if tt.promConfig != nil {
				assert.NotNil(t, tlsCfg)
			} else {
				assert.Nil(t, tlsCfg)
			}
		})
	}
}

func TestCreateTLSConfig_InsecureSkipVerifySkipsCertLoading(t *testing.T) {
	invalidCertFile, err := os.CreateTemp(t.TempDir(), "invalid-cert-*.crt")
	require.NoError(t, err)
	_, err = invalidCertFile.WriteString("# CA certificate not provided - using system CA bundle")
	require.NoError(t, err)
	require.NoError(t, invalidCertFile.Close())

	cfg := testConfigFromEnv(t, map[string]string{
		"PROMETHEUS_BASE_URL":                 "https://prometheus:9090",
		"PROMETHEUS_TLS_INSECURE_SKIP_VERIFY": "true",
		"PROMETHEUS_CA_CERT_PATH":             invalidCertFile.Name(),
	})

	tlsCfg, err := CreateTLSConfig(cfg)
	assert.NoError(t, err)
	assert.NotNil(t, tlsCfg)
	assert.True(t, tlsCfg.InsecureSkipVerify)
	assert.Nil(t, tlsCfg.RootCAs, "RootCAs should be nil when insecureSkipVerify is true")
}

func TestValidateTLSConfig(t *testing.T) {
	tests := []struct {
		name        string
		promConfig  *config.Config
		expectError bool
	}{
		{
			name:        "nil config - should fail",
			promConfig:  nil,
			expectError: true,
		},
		{
			name: "HTTP URL without opt-in - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL": "http://prometheus:9090",
			}),
			expectError: true,
		},
		{
			name: "HTTP URL with opt-in - should pass",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":  "http://prometheus:9090",
				envPrometheusAllowHTTP: "true",
			}),
			expectError: false,
		},
		{
			name: "HTTP URL with opt-in and CA cert - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":     "http://prometheus:9090",
				envPrometheusAllowHTTP:    "true",
				"PROMETHEUS_CA_CERT_PATH": "/tmp/ca.crt",
			}),
			expectError: true,
		},
		{
			name: "HTTP URL with opt-in and bearer token - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":     "http://prometheus:9090",
				envPrometheusAllowHTTP:    "true",
				"PROMETHEUS_BEARER_TOKEN": "secret",
			}),
			expectError: true,
		},
		{
			name: "HTTP URL with opt-in and token path - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":   "http://prometheus:9090",
				envPrometheusAllowHTTP:  "true",
				"PROMETHEUS_TOKEN_PATH": "/var/run/secrets/token",
			}),
			expectError: true,
		},
		{
			name: "HTTP URL with opt-in and insecure skip verify - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":                 "http://prometheus:9090",
				envPrometheusAllowHTTP:                "true",
				"PROMETHEUS_TLS_INSECURE_SKIP_VERIFY": "true",
			}),
			expectError: true,
		},
		{
			name: "HTTP URL with opt-in and client cert path - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":         "http://prometheus:9090",
				envPrometheusAllowHTTP:        "true",
				"PROMETHEUS_CLIENT_CERT_PATH": "/tmp/client.crt",
			}),
			expectError: true,
		},
		{
			name: "HTTP URL with opt-in and client key path - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":        "http://prometheus:9090",
				envPrometheusAllowHTTP:       "true",
				"PROMETHEUS_CLIENT_KEY_PATH": "/tmp/client.key",
			}),
			expectError: true,
		},
		{
			name: "HTTP URL with embedded credentials - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":  "http://user:pass@prometheus:9090",
				envPrometheusAllowHTTP: "true",
			}),
			expectError: true,
		},
		{
			name: "HTTP URL with opt-in and server name - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":    "http://prometheus:9090",
				envPrometheusAllowHTTP:   "true",
				"PROMETHEUS_SERVER_NAME": "prometheus.example.com",
			}),
			expectError: true,
		},
		{
			name: "unsupported URL scheme - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL": "ftp://prometheus:9090",
			}),
			expectError: true,
		},
		{
			name: "HTTPS URL with embedded credentials - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL": "https://user:pass@prometheus:9090",
			}),
			expectError: true,
		},
		{
			name: "HTTPS URL with missing CA cert - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":     "https://prometheus:9090",
				"PROMETHEUS_CA_CERT_PATH": t.TempDir() + "/missing-ca.crt",
			}),
			expectError: true,
		},
		{
			name: "HTTPS URL with missing client cert - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":         "https://prometheus:9090",
				"PROMETHEUS_CLIENT_CERT_PATH": t.TempDir() + "/missing-client.crt",
			}),
			expectError: true,
		},
		{
			name: "HTTPS URL with missing client key - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":        "https://prometheus:9090",
				"PROMETHEUS_CLIENT_KEY_PATH": t.TempDir() + "/missing-client.key",
			}),
			expectError: true,
		},
		{
			name: "TLS with insecure skip verify",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":                 "https://prometheus:9090",
				"PROMETHEUS_TLS_INSECURE_SKIP_VERIFY": "true",
			}),
			expectError: false,
		},
		{
			name: "TLS with server name",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":    "https://prometheus:9090",
				"PROMETHEUS_SERVER_NAME": "prometheus.example.com",
			}),
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTLSConfig(tt.promConfig)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCreatePrometheusTransport(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		// expectCustomTLS is true when CreateTLSConfig should have been called
		// (i.e. the URL is https://). CreateTLSConfig always sets MinVersion to
		// tls.VersionTLS12, so we use that as a sentinel to distinguish our
		// custom TLS config from any TLS config the cloned default transport
		// may carry for h2 support (which has MinVersion == 0).
		expectCustomTLS bool
	}{
		{
			name: "http with opt-in - custom TLS config should not be applied",
			env: map[string]string{
				"PROMETHEUS_BASE_URL":  "http://prometheus:9090",
				envPrometheusAllowHTTP: "true",
			},
			expectCustomTLS: false,
		},
		{
			name: "https - custom TLS config should be applied",
			env: map[string]string{
				"PROMETHEUS_BASE_URL": "https://prometheus:9090",
			},
			expectCustomTLS: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfigFromEnv(t, tt.env)
			rt, err := CreatePrometheusTransport(cfg)
			require.NoError(t, err)
			require.NotNil(t, rt)

			transport, ok := rt.(*http.Transport)
			require.True(t, ok, "expected *http.Transport")

			if tt.expectCustomTLS {
				// CreateTLSConfig always enforces MinVersion: tls.VersionTLS12.
				require.NotNil(t, transport.TLSClientConfig)
				assert.Equal(t, uint16(tls.VersionTLS12), transport.TLSClientConfig.MinVersion)
			} else if transport.TLSClientConfig != nil {
				// TLSClientConfig may be non-nil (cloned from DefaultTransport for h2
				// support), but our custom MinVersion must NOT be set.
				assert.NotEqual(t, uint16(tls.VersionTLS12), transport.TLSClientConfig.MinVersion)
			}
		})
	}
}
