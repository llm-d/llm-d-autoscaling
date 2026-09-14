package coordinator

import (
	"testing"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// hpaWith builds a minimal HPA with the given annotations and metrics.
func hpaWith(ann map[string]string, metrics []autoscalingv2.MetricSpec, owners ...metav1.OwnerReference) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "h",
			Namespace:       "ns",
			Annotations:     ann,
			OwnerReferences: owners,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			Metrics: metrics,
		},
	}
}

func managedAnn() map[string]string {
	return map[string]string{annotations.Managed: "true"}
}

func wvaExternalMetric() autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ExternalMetricSourceType,
		External: &autoscalingv2.ExternalMetricSource{
			Metric: autoscalingv2.MetricIdentifier{Name: constants.WVADesiredReplicas},
		},
	}
}

func otherExternalMetric() autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ExternalMetricSourceType,
		External: &autoscalingv2.ExternalMetricSource{
			Metric: autoscalingv2.MetricIdentifier{Name: "queue_depth"},
		},
	}
}

func kedaOwnerRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "keda.sh/v1alpha1",
		Kind:       constants.ScaledObjectKind,
		Name:       "my-so",
		UID:        "uid-1",
		Controller: ptr.To(true),
	}
}

func TestIsHPAUnderControl(t *testing.T) {
	tests := []struct {
		name string
		hpa  *autoscalingv2.HorizontalPodAutoscaler
		want bool
	}{
		{
			name: "nil hpa is excluded",
			hpa:  nil,
			want: false,
		},
		{
			name: "missing managed annotation is excluded",
			hpa:  hpaWith(nil, nil),
			want: false,
		},
		{
			name: "managed=false annotation is excluded",
			hpa:  hpaWith(map[string]string{annotations.Managed: "false"}, nil),
			want: false,
		},
		{
			name: "managed and no wva_desired_replicas metric is included",
			hpa:  hpaWith(managedAnn(), nil),
			want: true,
		},
		{
			name: "managed with unrelated external metric is included",
			hpa:  hpaWith(managedAnn(), []autoscalingv2.MetricSpec{otherExternalMetric()}),
			want: true,
		},
		{
			name: "managed with wva_desired_replicas external metric is excluded",
			hpa:  hpaWith(managedAnn(), []autoscalingv2.MetricSpec{wvaExternalMetric()}),
			want: false,
		},
		{
			name: "managed but owned by KEDA ScaledObject is excluded",
			hpa:  hpaWith(managedAnn(), nil, kedaOwnerRef()),
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHPAUnderControl(tc.hpa); got != tc.want {
				t.Fatalf("IsHPAUnderControl=%v, want %v", got, tc.want)
			}
		})
	}
}

func soWith(ann map[string]string, triggers []kedav1alpha1.ScaleTriggers) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "s",
			Namespace:   "ns",
			Annotations: ann,
		},
		Spec: kedav1alpha1.ScaledObjectSpec{
			Triggers: triggers,
		},
	}
}

func promTriggerWVA() kedav1alpha1.ScaleTriggers {
	return kedav1alpha1.ScaleTriggers{
		Type: "prometheus",
		Metadata: map[string]string{
			"query": "wva_desired_replicas{variant_name=\"v\"}",
		},
	}
}

func promTriggerOther() kedav1alpha1.ScaleTriggers {
	return kedav1alpha1.ScaleTriggers{
		Type: "prometheus",
		Metadata: map[string]string{
			"query": "vllm:num_requests_waiting",
		},
	}
}

func cpuTrigger() kedav1alpha1.ScaleTriggers {
	return kedav1alpha1.ScaleTriggers{Type: "cpu", Metadata: map[string]string{"value": "70"}}
}

func TestIsScaledObjectUnderControl(t *testing.T) {
	tests := []struct {
		name string
		so   *kedav1alpha1.ScaledObject
		want bool
	}{
		{
			name: "nil so is excluded",
			so:   nil,
			want: false,
		},
		{
			name: "missing managed annotation is excluded",
			so:   soWith(nil, nil),
			want: false,
		},
		{
			name: "managed and no triggers is included",
			so:   soWith(managedAnn(), nil),
			want: true,
		},
		{
			name: "managed with non-prometheus trigger is included",
			so:   soWith(managedAnn(), []kedav1alpha1.ScaleTriggers{cpuTrigger()}),
			want: true,
		},
		{
			name: "managed with non-WVA prometheus trigger is included",
			so:   soWith(managedAnn(), []kedav1alpha1.ScaleTriggers{promTriggerOther()}),
			want: true,
		},
		{
			name: "managed with wva_desired_replicas prometheus trigger is excluded",
			so:   soWith(managedAnn(), []kedav1alpha1.ScaleTriggers{promTriggerWVA()}),
			want: false,
		},
		{
			name: "managed with mixed triggers including WVA is excluded",
			so:   soWith(managedAnn(), []kedav1alpha1.ScaleTriggers{cpuTrigger(), promTriggerWVA()}),
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsScaledObjectUnderControl(tc.so); got != tc.want {
				t.Fatalf("IsScaledObjectUnderControl=%v, want %v", got, tc.want)
			}
		})
	}
}
