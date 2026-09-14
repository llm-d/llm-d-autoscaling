package inferenceengine

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

func deploymentWith(container corev1.Container) scaletarget.ScaleTargetAccessor {
	return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{container},
				},
			},
		},
	})
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name      string
		container corev1.Container
		want      Engine
	}{
		{
			name:      "vLLM image",
			container: corev1.Container{Name: "server", Image: "vllm/vllm-openai:latest", Command: []string{"vllm", "serve", "m"}},
			want:      EngineVLLM,
		},
		{
			name:      "SGLang image",
			container: corev1.Container{Name: "server", Image: "lmsysorg/sglang:v0.5.9-cu130-runtime"},
			want:      EngineSGLang,
		},
		{
			name:      "SGLang launch_server in args (argv form)",
			container: corev1.Container{Name: "server", Image: "registry.example.com/serving:1.0", Command: []string{"python", "-m", "sglang.launch_server", "--model-path", "m"}},
			want:      EngineSGLang,
		},
		{
			name:      "SGLang launch_server in shell command",
			container: corev1.Container{Name: "server", Command: []string{"/bin/sh", "-c", "python3 -m sglang.launch_server --model-path m --tp 2"}},
			want:      EngineSGLang,
		},
		{
			name:      "SGLang serve subcommand",
			container: corev1.Container{Name: "server", Image: "registry.example.com/serving:1.0", Command: []string{"sglang", "serve", "m"}},
			want:      EngineSGLang,
		},
		{
			name:      "no signal defaults to vLLM",
			container: corev1.Container{Name: "server", Image: "registry.example.com/serving:1.0", Command: []string{"serve"}},
			want:      EngineVLLM,
		},
		{
			// Guard against the dropped over-broad "-m sglang" substring: a module
			// whose name merely starts with "sglang" (e.g. a benchmark harness) must
			// NOT be detected as an SGLang server.
			name:      "-m sglang_bench is not an SGLang server (false-positive guard)",
			container: corev1.Container{Name: "server", Image: "registry.example.com/serving:1.0", Command: []string{"python", "-m", "sglang_bench", "--model", "m"}},
			want:      EngineVLLM,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Detect(deploymentWith(tt.container)); got != tt.want {
				t.Errorf("Detect() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectNilAndEmpty(t *testing.T) {
	if got := Detect(nil); got != EngineVLLM {
		t.Errorf("Detect(nil) = %q, want %q", got, EngineVLLM)
	}
	empty := scaletarget.NewDeploymentAccessor(&appsv1.Deployment{})
	if got := Detect(empty); got != EngineVLLM {
		t.Errorf("Detect(empty) = %q, want %q", got, EngineVLLM)
	}
}

func TestPresent(t *testing.T) {
	vllm := deploymentWith(corev1.Container{Name: "s", Image: "vllm/vllm-openai"})
	sglang := deploymentWith(corev1.Container{Name: "s", Image: "lmsysorg/sglang"})

	tests := []struct {
		name    string
		targets map[string]scaletarget.ScaleTargetAccessor
		want    []Engine
	}{
		{
			name:    "empty defaults to vLLM",
			targets: nil,
			want:    []Engine{EngineVLLM},
		},
		{
			name:    "vLLM only",
			targets: map[string]scaletarget.ScaleTargetAccessor{"a": vllm},
			want:    []Engine{EngineVLLM},
		},
		{
			name:    "SGLang only",
			targets: map[string]scaletarget.ScaleTargetAccessor{"a": sglang},
			want:    []Engine{EngineSGLang},
		},
		{
			name:    "mixed is vLLM-first deterministic",
			targets: map[string]scaletarget.ScaleTargetAccessor{"a": sglang, "b": vllm},
			want:    []Engine{EngineVLLM, EngineSGLang},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Present(tt.targets)
			if len(got) != len(tt.want) {
				t.Fatalf("Present() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Present()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
