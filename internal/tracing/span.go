package tracing

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// instrumentationName is the default OTel instrumentation scope: the short
// component name, matching the service.name this project reports.
//
// Packages that emit spans override it with their own scope constant, formed as
// "llm-d-wva/<package path>". This mirrors llm-d-router's convention
// ("llm-d-router/pkg/..."): a short component name rather than the full Go
// module path, followed by the emitting package's path within the repo.
const instrumentationName = "llm-d-wva"

// Span names for the stages of one optimization cycle. SpanReconcile is the
// root; the rest are its children, in pipeline order.
//
// Names are bare snake_case verbs, not dotted namespaces: the instrumentation
// scope already identifies the component that emitted the span, so repeating it
// in the name only makes the name longer. This matches llm-d-router's
// convention so both projects group the same way in a trace backend.
const (
	SpanReconcile = "reconcile"
	SpanCollect   = "collect"
	SpanAnalyze   = "analyze"
	SpanOptimize  = "optimize"
	SpanLimit     = "limit"
	SpanEnforce   = "enforce"
	SpanActuate   = "actuate"

	// SpanPrometheusQuery is a child of SpanCollect: one PromQL query.
	SpanPrometheusQuery = "prometheus_query"
)

// Attribute keys for the decision context carried on pipeline spans.
//
// Unlike span names, attribute keys are dotted and namespaced, per the
// OpenTelemetry attribute naming convention (compare http.method, k8s.pod.name).
//
// Every value written under these keys is either an integer, a boolean, or a
// string drawn from a bounded set the controller itself defines (analyzer and
// optimizer names, accelerator types, decision reasons, Kubernetes object
// names). Free-form, user-controlled text is never attached to a span.
const (
	AttrVariantAutoscaling = "wva.variant_autoscaling"
	AttrNamespace          = "wva.namespace"
	AttrModelID            = "wva.model_id"
	AttrAcceleratorType    = "wva.accelerator_type"
	AttrCurrentReplicas    = "wva.current_replicas"
	AttrDesiredReplicas    = "wva.desired_replicas"
	AttrAnalyzer           = "wva.analyzer"
	AttrAnalyzerCount      = "wva.analyzer_count"
	AttrOptimizer          = "wva.optimizer"
	AttrLimiter            = "wva.limiter"
	AttrMode               = "wva.mode"
	AttrModelCount         = "wva.model_count"
	AttrVariantCount       = "wva.variant_count"
	AttrDecisionCount      = "wva.decision_count"
	AttrWasLimited         = "wva.was_limited"
	AttrLimitedCount       = "wva.limited_decision_count"
	AttrLimitedBy          = "wva.limited_by"
	AttrDecisionReason     = "wva.decision_reason"
	AttrReplicaMetrics     = "wva.replica_metrics_count"
	AttrQueryType          = "wva.query_type"
	AttrQuery              = "wva.query"
	AttrScaleToZero        = "wva.scale_to_zero_enabled"
)

// Tracer returns a tracer for the given instrumentation scope, defaulting to
// instrumentationName. Emitting packages pass their own scope constant:
//
//	ctx, span := tracing.Tracer(tracerScope).Start(ctx, tracing.SpanOptimize)
//	defer span.End()
//
// The global provider is resolved on every call, so Tracer is safe to use
// before Init runs, and after it when tracing is disabled: the provider is then
// the OpenTelemetry no-op implementation and the spans it returns record
// nothing.
func Tracer(scope ...string) trace.Tracer {
	name := instrumentationName
	if len(scope) > 0 && scope[0] != "" {
		name = scope[0]
	}
	return otel.Tracer(name)
}

// RecordError marks span as failed and attaches err. It is a no-op when err
// is nil, so it can be deferred over a named error return.
func RecordError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// ModelAttrs returns the attributes identifying a model scope.
func ModelAttrs(modelID, namespace string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 2)
	if modelID != "" {
		attrs = append(attrs, attribute.String(AttrModelID, modelID))
	}
	if namespace != "" {
		attrs = append(attrs, attribute.String(AttrNamespace, namespace))
	}
	return attrs
}

// VariantAttrs returns the attributes identifying a single VariantAutoscaling
// and its replica counts.
func VariantAttrs(name, namespace string, current, desired int) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 4)
	if name != "" {
		attrs = append(attrs, attribute.String(AttrVariantAutoscaling, name))
	}
	if namespace != "" {
		attrs = append(attrs, attribute.String(AttrNamespace, namespace))
	}
	attrs = append(attrs,
		attribute.Int(AttrCurrentReplicas, current),
		attribute.Int(AttrDesiredReplicas, desired),
	)
	return attrs
}
