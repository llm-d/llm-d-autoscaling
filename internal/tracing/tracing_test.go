package tracing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestConfigFromEnvDefaults(t *testing.T) {
	// No OTEL_* variables set: tracing must be off and the service name must
	// fall back to the project default.
	for _, key := range []string{
		"OTEL_TRACES_EXPORTER", "OTEL_SERVICE_NAME", "OTEL_SERVICE_VERSION",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_INSECURE", "OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG",
		"POD_NAMESPACE", "POD_NAME",
	} {
		t.Setenv(key, "")
	}

	cfg := ConfigFromEnv()

	assert.Equal(t, ExporterNone, cfg.Exporter)
	assert.False(t, cfg.Enabled(), "tracing must be off by default")
	assert.Equal(t, DefaultServiceName, cfg.ServiceName)
}

func TestConfigFromEnvReadsStandardVariables(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "OTLP")
	t.Setenv("OTEL_SERVICE_NAME", "wva-test")
	t.Setenv("OTEL_SERVICE_VERSION", "v1.2.3")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "collector:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_TRACES_SAMPLER", "traceidratio")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.25")
	t.Setenv("POD_NAMESPACE", "wva-system")
	t.Setenv("POD_NAME", "wva-controller-0")

	cfg := ConfigFromEnv()

	// The exporter name is case-insensitive.
	assert.Equal(t, ExporterOTLP, cfg.Exporter)
	assert.True(t, cfg.Enabled())
	assert.Equal(t, "wva-test", cfg.ServiceName)
	assert.Equal(t, "v1.2.3", cfg.ServiceVersion)
	assert.Equal(t, "collector:4317", cfg.Endpoint)
	assert.True(t, cfg.Insecure)
	assert.Equal(t, "traceidratio", cfg.Sampler)
	assert.Equal(t, "0.25", cfg.SamplerArg)
	assert.Equal(t, "wva-system", cfg.Namespace)
	assert.Equal(t, "wva-controller-0", cfg.InstanceID)
}

func TestConfigFromEnvPrefersTracesEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "generic:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "traces:4317")

	assert.Equal(t, "traces:4317", ConfigFromEnv().Endpoint)
}

func TestConfigEnabled(t *testing.T) {
	cases := map[string]bool{
		"":        false,
		"none":    false,
		"NONE":    false,
		"otlp":    true,
		"console": true,
	}
	for exporter, want := range cases {
		assert.Equal(t, want, Config{Exporter: exporter}.Enabled(), "exporter %q", exporter)
	}
}

func TestInitDisabledInstallsNoProvider(t *testing.T) {
	// Capture whatever provider is installed, and assert Init leaves it alone.
	before := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(before) })

	otel.SetTracerProvider(noop.NewTracerProvider())

	shutdown, err := Init(context.Background(), Config{Exporter: ExporterNone})
	require.NoError(t, err)
	require.NotNil(t, shutdown)

	_, span := Tracer().Start(context.Background(), SpanReconcile)
	defer span.End()
	assert.False(t, span.SpanContext().IsValid(),
		"a disabled provider must produce non-recording spans")
	assert.False(t, span.IsRecording())

	// The no-op shutdown must succeed and be safe to call.
	assert.NoError(t, shutdown(context.Background()))
}

func TestInitConsoleExporterInstallsProviderAndShutsDown(t *testing.T) {
	before := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(before) })

	shutdown, err := Init(context.Background(), Config{
		Exporter:       ExporterConsole,
		ServiceName:    "wva-test",
		ServiceVersion: "v0.0.1",
		Namespace:      "wva-system",
		InstanceID:     "wva-controller-0",
	})
	require.NoError(t, err)
	require.NotNil(t, shutdown)

	_, span := Tracer().Start(context.Background(), SpanReconcile)
	assert.True(t, span.SpanContext().IsValid(), "an enabled provider must produce recording spans")
	span.End()

	require.NoError(t, shutdown(context.Background()))
	// Shutting down twice must not panic or error; the manager may race with
	// an error path that also flushes.
	assert.NoError(t, shutdown(context.Background()))
}

func TestInitRejectsUnknownExporter(t *testing.T) {
	before := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(before) })

	shutdown, err := Init(context.Background(), Config{Exporter: "kafka"})
	require.Error(t, err)
	assert.Nil(t, shutdown)
	assert.Contains(t, err.Error(), "kafka")
}

func TestNewSampler(t *testing.T) {
	cases := []struct {
		sampler string
		arg     string
		want    string
	}{
		{"", "", sdktrace.ParentBased(sdktrace.AlwaysSample()).Description()},
		{"always_on", "", sdktrace.AlwaysSample().Description()},
		{"always_off", "", sdktrace.NeverSample().Description()},
		{"traceidratio", "0.5", sdktrace.TraceIDRatioBased(0.5).Description()},
		{"parentbased_always_off", "", sdktrace.ParentBased(sdktrace.NeverSample()).Description()},
		{"parentbased_traceidratio", "0.1", sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1)).Description()},
		// An unparsable ratio must not panic; it falls back to 1.0.
		{"traceidratio", "not-a-number", sdktrace.TraceIDRatioBased(1.0).Description()},
		// An unknown sampler name falls back to the OpenTelemetry default.
		{"lottery", "", sdktrace.ParentBased(sdktrace.AlwaysSample()).Description()},
	}

	for _, tc := range cases {
		got := newSampler(Config{Sampler: tc.sampler, SamplerArg: tc.arg})
		assert.Equal(t, tc.want, got.Description(), "sampler=%q arg=%q", tc.sampler, tc.arg)
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	assert.Equal(t, "http://collector:4317", normalizeEndpoint("collector:4317"))
	assert.Equal(t, "https://collector:4317", normalizeEndpoint("https://collector:4317"))
	assert.Equal(t, "http://collector:4317", normalizeEndpoint("http://collector:4317"))
}

func TestNewResourceCarriesIdentity(t *testing.T) {
	res, err := newResource(context.Background(), Config{
		ServiceName:    "wva-test",
		ServiceVersion: "v9.9.9",
		Namespace:      "wva-system",
		InstanceID:     "wva-controller-0",
	})
	require.NoError(t, err)

	attrs := map[string]string{}
	for _, kv := range res.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}

	assert.Equal(t, "wva-test", attrs["service.name"])
	assert.Equal(t, "v9.9.9", attrs["service.version"])
	assert.Equal(t, "wva-system", attrs["k8s.namespace.name"])
	assert.Equal(t, "wva-controller-0", attrs["service.instance.id"])
	assert.Equal(t, "wva-controller-0", attrs["k8s.pod.name"])
}

func TestNewResourceOmitsEmptyOptionalAttributes(t *testing.T) {
	res, err := newResource(context.Background(), Config{ServiceName: "wva-test"})
	require.NoError(t, err)

	for _, kv := range res.Attributes() {
		switch string(kv.Key) {
		case "service.version", "k8s.namespace.name", "service.instance.id", "k8s.pod.name":
			t.Errorf("attribute %q must be omitted when unset", kv.Key)
		}
	}
}

func TestRecordError(t *testing.T) {
	// RecordError on a nil error must leave the span unset; the no-op span is
	// enough to prove it does not panic.
	_, span := noop.NewTracerProvider().Tracer("test").Start(context.Background(), "x")
	defer span.End()
	RecordError(span, nil)
}

func TestTracerIsUsableBeforeInit(t *testing.T) {
	// Tracer resolves the global provider lazily, so calling it before Init
	// must not panic and must yield a usable (no-op) tracer.
	tracer := Tracer()
	require.NotNil(t, tracer)
	_, span := tracer.Start(context.Background(), "x")
	span.End()
}
