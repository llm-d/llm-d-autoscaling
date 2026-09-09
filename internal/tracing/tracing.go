// Package tracing provides OpenTelemetry tracing for the WVA optimization
// pipeline: bootstrap of the global tracer provider, and helpers for starting
// the spans that make up one optimization cycle.
//
// Tracing is opt-in and off by default. When it is off no tracer provider is
// installed, so the global provider stays the OpenTelemetry no-op
// implementation: spans are non-recording singletons, no exporter runs, and no
// telemetry leaves the process.
//
// Configuration uses the standard OTEL_* environment variables so WVA behaves
// like any other OpenTelemetry application. See docs/user-guide/monitoring.md.
package tracing

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// DefaultServiceName is the service.name reported when OTEL_SERVICE_NAME is unset.
const DefaultServiceName = "llm-d-wva"

// Exporter kinds accepted by OTEL_TRACES_EXPORTER.
const (
	// ExporterNone disables tracing. It is the default.
	ExporterNone = "none"
	// ExporterOTLP exports spans over OTLP gRPC.
	ExporterOTLP = "otlp"
	// ExporterConsole writes spans to stdout, for local debugging.
	ExporterConsole = "console"
)

// shutdownTimeout bounds the flush of pending spans at process exit so a
// slow or unreachable collector cannot block shutdown indefinitely.
const shutdownTimeout = 5 * time.Second

// Config describes how the tracer provider is built. Use ConfigFromEnv to
// populate it from the standard OpenTelemetry environment variables.
type Config struct {
	// Exporter selects the span exporter: ExporterNone, ExporterOTLP, or
	// ExporterConsole. ExporterNone disables tracing entirely.
	Exporter string

	// ServiceName is reported as the service.name resource attribute.
	ServiceName string

	// ServiceVersion is reported as the service.version resource attribute.
	// Empty means the attribute is omitted.
	ServiceVersion string

	// Endpoint is the OTLP gRPC endpoint (host:port). Empty lets the exporter
	// fall back to its own environment-variable defaults.
	Endpoint string

	// Insecure disables transport security for the OTLP exporter. Production
	// deployments should terminate TLS; see the hardening follow-up noted in
	// the package documentation.
	Insecure bool

	// Sampler and SamplerArg mirror OTEL_TRACES_SAMPLER and
	// OTEL_TRACES_SAMPLER_ARG.
	Sampler    string
	SamplerArg string

	// Namespace and InstanceID identify this controller instance in the
	// resource attributes.
	Namespace  string
	InstanceID string
}

// Enabled reports whether the configuration turns tracing on.
func (c Config) Enabled() bool {
	return c.Exporter != "" && !strings.EqualFold(c.Exporter, ExporterNone)
}

// ConfigFromEnv builds a Config from the standard OpenTelemetry environment
// variables:
//
//	OTEL_TRACES_EXPORTER            none (default) | otlp | console
//	OTEL_SERVICE_NAME               defaults to DefaultServiceName
//	OTEL_SERVICE_VERSION            optional
//	OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, OTEL_EXPORTER_OTLP_ENDPOINT
//	OTEL_EXPORTER_OTLP_INSECURE     true | false
//	OTEL_TRACES_SAMPLER, OTEL_TRACES_SAMPLER_ARG
//
// The controller instance is taken from POD_NAME (or the hostname) and the
// namespace from POD_NAMESPACE.
func ConfigFromEnv() Config {
	cfg := Config{
		Exporter:       strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER"))),
		ServiceName:    os.Getenv("OTEL_SERVICE_NAME"),
		ServiceVersion: os.Getenv("OTEL_SERVICE_VERSION"),
		Endpoint:       os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		Sampler:        strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER"))),
		SamplerArg:     os.Getenv("OTEL_TRACES_SAMPLER_ARG"),
		Namespace:      os.Getenv("POD_NAMESPACE"),
		InstanceID:     os.Getenv("POD_NAME"),
	}

	if cfg.Exporter == "" {
		cfg.Exporter = ExporterNone
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = DefaultServiceName
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if insecure, err := strconv.ParseBool(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE")); err == nil {
		cfg.Insecure = insecure
	}
	if cfg.InstanceID == "" {
		// In a Pod the hostname is the Pod name, so this keeps the attribute
		// useful without requiring a downward-API entry in the manifest.
		if hostname, err := os.Hostname(); err == nil {
			cfg.InstanceID = hostname
		}
	}
	return cfg
}

// ShutdownFunc flushes and releases the tracer provider. It is safe to call
// even when tracing is disabled, in which case it does nothing.
type ShutdownFunc func(context.Context) error

// Init installs the global tracer provider and W3C trace-context propagator
// according to cfg, and returns a function that shuts them down.
//
// When cfg disables tracing, Init installs nothing and returns a no-op
// shutdown, leaving the OpenTelemetry no-op provider in place.
func Init(ctx context.Context, cfg Config) (ShutdownFunc, error) {
	if !cfg.Enabled() {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := newExporter(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating span exporter: %w", err)
	}

	res, err := newResource(ctx, cfg)
	if err != nil {
		// The exporter owns a connection; release it rather than leaking it
		// on a failure path that never installs the provider.
		_ = exporter.Shutdown(ctx)
		return nil, fmt.Errorf("building tracing resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(newSampler(cfg)),
	)

	otel.SetTracerProvider(provider)
	// W3C trace context and baggage, so a trace started upstream (or in
	// llm-d-router) continues through WVA rather than starting a new one.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func(shutdownCtx context.Context) error {
		// Bound the flush: shutdown must not hang on an unreachable collector.
		shutdownCtx, cancel := context.WithTimeout(shutdownCtx, shutdownTimeout)
		defer cancel()
		return provider.Shutdown(shutdownCtx)
	}, nil
}

// newExporter constructs the span exporter selected by cfg.Exporter.
func newExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	switch strings.ToLower(cfg.Exporter) {
	case ExporterConsole, "stdout":
		return stdouttrace.New(stdouttrace.WithPrettyPrint())
	case ExporterOTLP:
		opts := []otlptracegrpc.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracegrpc.WithEndpointURL(normalizeEndpoint(cfg.Endpoint)))
		}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		return otlptracegrpc.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unsupported OTEL_TRACES_EXPORTER %q (want %q, %q, or %q)",
			cfg.Exporter, ExporterNone, ExporterOTLP, ExporterConsole)
	}
}

// normalizeEndpoint gives a bare host:port endpoint a scheme, since
// WithEndpointURL requires a URL while OTEL_EXPORTER_OTLP_ENDPOINT is
// commonly set to host:port.
func normalizeEndpoint(endpoint string) string {
	if strings.Contains(endpoint, "://") {
		return endpoint
	}
	return "http://" + endpoint
}

// newResource describes this process to the trace backend.
func newResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(cfg.ServiceName),
	}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.Namespace != "" {
		attrs = append(attrs, semconv.K8SNamespaceName(cfg.Namespace))
	}
	if cfg.InstanceID != "" {
		attrs = append(attrs,
			semconv.ServiceInstanceID(cfg.InstanceID),
			semconv.K8SPodName(cfg.InstanceID),
		)
	}

	// Merge over the default resource so the SDK's telemetry.sdk.* attributes
	// and any OTEL_RESOURCE_ATTRIBUTES entries are preserved.
	return resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(attrs...),
	)
}

// newSampler maps OTEL_TRACES_SAMPLER to an SDK sampler. Unrecognized values
// fall back to parentbased_always_on, which is the OpenTelemetry default.
func newSampler(cfg Config) sdktrace.Sampler {
	ratio := func(def float64) float64 {
		if cfg.SamplerArg == "" {
			return def
		}
		parsed, err := strconv.ParseFloat(cfg.SamplerArg, 64)
		if err != nil {
			return def
		}
		return parsed
	}

	switch cfg.Sampler {
	case "always_off":
		return sdktrace.NeverSample()
	case "always_on":
		return sdktrace.AlwaysSample()
	case "traceidratio":
		return sdktrace.TraceIDRatioBased(ratio(1.0))
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample())
	case "parentbased_traceidratio":
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio(1.0)))
	default:
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	}
}
