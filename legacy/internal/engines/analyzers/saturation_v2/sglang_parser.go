package saturation_v2

import (
	"strconv"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// ParseEngineArgs parses a scale target's container args using the parser
// appropriate for the given inference engine, returning the shared
// EngineParams. It dispatches to ParseSGLangArgs for SGLang and ParseVLLMArgs
// otherwise (the default), so existing vLLM behavior is unchanged.
func ParseEngineArgs(engine inferenceengine.Engine, scaleTarget scaletarget.ScaleTargetAccessor) EngineParams {
	if engine == inferenceengine.EngineSGLang {
		return ParseSGLangArgs(scaleTarget)
	}
	return ParseVLLMArgs(scaleTarget)
}

// defaultSGLangEngineParams returns EngineParams with SGLang defaults.
// Flag defaults were taken from SGLang's server_args.py.
func defaultSGLangEngineParams() EngineParams {
	return EngineParams{
		Engine:               inferenceengine.EngineSGLang,
		GpuMemoryUtilization: 0.9, // --mem-fraction-static default
		BlockSize:            1,   // --page-size default
		KvCacheDtype:         "auto",
		TensorParallelSize:   1,
		// SGLang auto-derives --max-running-requests from available memory when
		// unset; 256 is a conservative placeholder that underestimates capacity
		// (safe: it biases toward scale-up rather than overload).
		MaxNumSeqs:            256,
		IsV1Engine:            true, // SGLang has a single (V1-style) engine
		ChunkedPrefillEnabled: true, // chunked prefill is on by default
	}
}

// ParseSGLangArgs scans a Deployment/LWS's containers for SGLang CLI arguments
// and returns the parsed parameters. It mirrors ParseVLLMArgs:
//   - --key=value and --key value argument formats
//   - hyphen/underscore normalization
//   - shell commands: ["/bin/sh", "-c", "python -m sglang.launch_server ..."]
//   - boolean flags: --disable-cuda-graph (no value)
func ParseSGLangArgs(scaleTarget scaletarget.ScaleTargetAccessor) EngineParams {
	params := defaultSGLangEngineParams()
	if scaleTarget == nil {
		resolveEffectiveMaxBatchedTokens(&params)
		return params
	}

	podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
	if podTemplateSpec == nil || len(podTemplateSpec.Spec.Containers) == 0 {
		resolveEffectiveMaxBatchedTokens(&params)
		return params
	}

	for _, container := range podTemplateSpec.Spec.Containers {
		// collectArgs and the --key/--key=value parsing loop are shared with the
		// vLLM parser; only the per-flag mapping (applySGLangParam) differs.
		allArgs := collectArgs(container.Command, container.Args)
		parseArgsWith(allArgs, &params, applySGLangParam)
	}

	resolveEffectiveMaxBatchedTokens(&params)
	return params
}

// applySGLangParam sets the corresponding EngineParams field from a
// normalized SGLang flag key and its string value. Parse errors are silently
// ignored and the default value is preserved (graceful degradation), matching
// the vLLM parser's behavior.
func applySGLangParam(key, value string, params *EngineParams) {
	switch key {
	case "mem_fraction_static":
		if v, err := strconv.ParseFloat(value, 64); err == nil {
			params.GpuMemoryUtilization = v
		}
	case "page_size":
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			params.BlockSize = v
		}
	case "kv_cache_dtype":
		params.KvCacheDtype = value
	case "tp_size", "tensor_parallel_size", "tp":
		if v, err := strconv.Atoi(value); err == nil {
			params.TensorParallelSize = v
		}
	case "max_running_requests":
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			params.MaxNumSeqs = v
		}
	case "max_total_tokens":
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			params.TotalKvTokensOverride = v
		}
	case "context_length":
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			params.MaxModelLen = v
		}
	case "max_prefill_tokens":
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			params.MaxNumBatchedTokens = v
		}
	case "chunked_prefill_size":
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			if v > 0 {
				params.MaxNumBatchedTokens = v
				params.ChunkedPrefillEnabled = true
			} else {
				// SGLang uses --chunked-prefill-size=-1 to disable chunked prefill.
				params.ChunkedPrefillEnabled = false
			}
		}
	case "disable_cuda_graph":
		params.EnforceEager = true
	}
}
