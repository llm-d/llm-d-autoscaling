// Package inferenceengine identifies which LLM inference engine (vLLM, SGLang)
// a scale target runs. WVA's metric collection and deployment-argument parsing
// are engine-specific, so the engine must be known before queries are built.
//
// Detection is conservative: a variant is treated as SGLang only when a strong
// signal is present in its pod template. Everything else — including pods with no
// recognizable signal — defaults to vLLM, preserving WVA's historical behavior.
package inferenceengine

import (
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// Engine identifies an LLM inference engine.
type Engine string

const (
	// EngineVLLM is the vLLM inference engine. It is the default when no other
	// engine is detected, so existing deployments are unaffected.
	EngineVLLM Engine = "vllm"

	// EngineSGLang is the SGLang inference engine.
	EngineSGLang Engine = "sglang"
)

// String returns the engine identifier (e.g. "vllm").
func (e Engine) String() string {
	return string(e)
}

// Detect inspects a scale target's leader pod template (container images,
// commands, and args) and returns the inference engine it runs. It defaults to
// EngineVLLM when the scale target is nil, has no pod template, or carries no
// SGLang signal.
func Detect(st scaletarget.ScaleTargetAccessor) Engine {
	if st == nil {
		return EngineVLLM
	}
	return detectFromPodTemplate(st.GetLeaderPodTemplateSpec())
}

// detectFromPodTemplate returns the engine implied by a pod template, defaulting
// to EngineVLLM.
func detectFromPodTemplate(tmpl *corev1.PodTemplateSpec) Engine {
	if tmpl == nil {
		return EngineVLLM
	}
	for i := range tmpl.Spec.Containers {
		if isSGLangContainer(&tmpl.Spec.Containers[i]) {
			return EngineSGLang
		}
	}
	return EngineVLLM
}

// isSGLangContainer reports whether a container runs an SGLang server, based on
// its image reference or launch command/args.
func isSGLangContainer(c *corev1.Container) bool {
	if strings.Contains(strings.ToLower(c.Image), "sglang") {
		return true
	}

	// Join command + args into a single lowercase string so we catch both the
	// argv form (["python", "-m", "sglang.launch_server", ...]) and the shell
	// form (["/bin/sh", "-c", "python -m sglang.launch_server ..."]).
	cmd := strings.ToLower(strings.Join(slices.Concat(c.Command, c.Args), " "))

	// Match only the documented launch forms. A bare "-m sglang" prefix is
	// intentionally not matched: it is broader than the contract (it would also
	// match "-m sglang_bench" and similar), and the two forms below already cover
	// every supported SGLang launch.
	return strings.Contains(cmd, "sglang.launch_server") ||
		strings.Contains(cmd, "sglang serve")
}

// Present returns the deterministically-ordered set of distinct engines detected
// across a collection of scale targets. vLLM is ordered first when present. When
// the input is empty, it returns just EngineVLLM so callers always query at least
// the default engine.
func Present(scaleTargets map[string]scaletarget.ScaleTargetAccessor) []Engine {
	seen := make(map[Engine]bool, 2)
	for _, st := range scaleTargets {
		seen[Detect(st)] = true
	}
	if len(seen) == 0 {
		return []Engine{EngineVLLM}
	}

	// Deterministic order: vLLM first, then SGLang.
	engines := make([]Engine, 0, len(seen))
	for _, e := range []Engine{EngineVLLM, EngineSGLang} {
		if seen[e] {
			engines = append(engines, e)
		}
	}
	return engines
}
