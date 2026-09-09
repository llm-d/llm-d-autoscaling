package pipeline

// tracerScope is the OTel instrumentation scope shared by the optimizer,
// limiter, and enforcer stages of the scaling pipeline.
const tracerScope = "llm-d-wva/internal/engines/pipeline"
