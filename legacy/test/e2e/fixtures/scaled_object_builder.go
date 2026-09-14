package fixtures

import (
	"context"
	"fmt"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
)

const (
	scaledObjectSuffix = "-so"

	kindLeaderWorkerSet = "LeaderWorkerSet"
	kindDeployment      = "Deployment"
	apiVersionLWS       = "leaderworkerset.x-k8s.io/v1"
	apiVersionAppsV1    = "apps/v1"
)

// ScaledObjectOption configures a KEDA ScaledObject before it is applied.
type ScaledObjectOption func(*kedav1alpha1.ScaledObject)

// WithScaledObjectScaleTargetKind sets the Kind and APIVersion on the ScaledObject's ScaleTargetRef.
func WithScaledObjectScaleTargetKind(kind string) ScaledObjectOption {
	return func(so *kedav1alpha1.ScaledObject) {
		if so.Spec.ScaleTargetRef == nil {
			return
		}
		so.Spec.ScaleTargetRef.Kind = kind
		switch kind {
		case kindLeaderWorkerSet:
			so.Spec.ScaleTargetRef.APIVersion = apiVersionLWS
		case kindDeployment:
			so.Spec.ScaleTargetRef.APIVersion = apiVersionAppsV1
		default:
			// Keep existing APIVersion for unknown kinds
		}
	}
}

// WithScaledObjectScaleDownStabilizationWindow sets the HPA scale-down stabilization window via
// KEDA's Advanced config. The default (300 s) is too long for e2e tests; pass a shorter value
// (e.g. 30 s) to make scale-down assertions complete within a reasonable test timeout.
func WithScaledObjectScaleDownStabilizationWindow(seconds int32) ScaledObjectOption {
	return func(so *kedav1alpha1.ScaledObject) {
		if so.Spec.Advanced == nil {
			so.Spec.Advanced = &kedav1alpha1.AdvancedConfig{}
		}
		if so.Spec.Advanced.HorizontalPodAutoscalerConfig == nil {
			so.Spec.Advanced.HorizontalPodAutoscalerConfig = &kedav1alpha1.HorizontalPodAutoscalerConfig{}
		}
		if so.Spec.Advanced.HorizontalPodAutoscalerConfig.Behavior == nil {
			so.Spec.Advanced.HorizontalPodAutoscalerConfig.Behavior = &autoscalingv2.HorizontalPodAutoscalerBehavior{}
		}
		so.Spec.Advanced.HorizontalPodAutoscalerConfig.Behavior.ScaleDown = &autoscalingv2.HPAScalingRules{
			StabilizationWindowSeconds: ptr.To(seconds),
		}
	}
}

// WithScaledObjectWVAAnnotations adds the WVA annotation-based discovery annotations to the
// ScaledObject. The ScaledObject then serves as both the WVA discovery source and the KEDA scaler.
func WithScaledObjectWVAAnnotations(modelID, cost string) ScaledObjectOption {
	return func(so *kedav1alpha1.ScaledObject) {
		if so.Annotations == nil {
			so.Annotations = make(map[string]string)
		}
		so.Annotations[annotations.Managed] = "true"
		so.Annotations[annotations.ModelID] = modelID
		so.Annotations[annotations.VariantCost] = cost
	}
}

// CreateScaledObject creates a KEDA ScaledObject for WVA. Fails if it already exists.
func CreateScaledObject(
	ctx context.Context,
	crClient client.Client,
	namespace, name, scaleTargetName, variantName string,
	minReplicas, maxReplicas int32,
	monitoringNamespace string,
	opts ...ScaledObjectOption,
) error {
	return crClient.Create(ctx, buildScaledObject(namespace, name, scaleTargetName, variantName, minReplicas, maxReplicas, monitoringNamespace, opts...))
}

// scaledObjectRef returns a minimal typed object for ScaledObject identity (Get/Delete).
// name is the base name; the ScaledObject resource name is name + scaledObjectSuffix.
func scaledObjectRef(namespace, name string) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name + scaledObjectSuffix,
		},
	}
}

// DeleteScaledObject deletes the ScaledObject. Idempotent; ignores NotFound.
func DeleteScaledObject(ctx context.Context, crClient client.Client, namespace, name string) error {
	err := crClient.Delete(ctx, scaledObjectRef(namespace, name))
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete ScaledObject %s: %w", name+scaledObjectSuffix, err)
	}
	return nil
}

// EnsureScaledObject creates or replaces the ScaledObject (idempotent for test setup).
func EnsureScaledObject(
	ctx context.Context,
	crClient client.Client,
	namespace, name, scaleTargetName, variantName string,
	minReplicas, maxReplicas int32,
	monitoringNamespace string,
	opts ...ScaledObjectOption,
) error {
	obj := buildScaledObject(namespace, name, scaleTargetName, variantName, minReplicas, maxReplicas, monitoringNamespace, opts...)
	existing := scaledObjectRef(namespace, name)
	key := client.ObjectKey{Namespace: namespace, Name: obj.GetName()}
	err := crClient.Get(ctx, key, existing)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("check existing ScaledObject %s: %w", obj.GetName(), err)
		}
	} else {
		deleteErr := crClient.Delete(ctx, existing)
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return fmt.Errorf("delete existing ScaledObject %s: %w", obj.GetName(), deleteErr)
		}
		waitErr := wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			check := scaledObjectRef(namespace, name)
			getErr := crClient.Get(ctx, key, check)
			return errors.IsNotFound(getErr), nil
		})
		if waitErr != nil {
			return fmt.Errorf("timeout waiting for ScaledObject %s deletion: %w", obj.GetName(), waitErr)
		}
	}
	return crClient.Create(ctx, obj)
}

func buildScaledObject(namespace, name, scaleTargetName, variantName string, minReplicas, maxReplicas int32, monitoringNamespace string, opts ...ScaledObjectOption) *kedav1alpha1.ScaledObject {
	objName := name + scaledObjectSuffix
	prometheusURL := "https://kube-prometheus-stack-prometheus." + monitoringNamespace + ".svc.cluster.local:9090"
	// Prometheus renames the metric's namespace label to exported_namespace when the scrape
	// target's namespace (workload-variant-autoscaler-system) differs from the label value
	// (the workload namespace, e.g. llm-d-sim). Use exported_namespace to match what
	// Prometheus actually stores.
	query := fmt.Sprintf("wva_desired_replicas{variant_name=%q,exported_namespace=%q}", variantName, namespace)

	spec := kedav1alpha1.ScaledObjectSpec{
		ScaleTargetRef: &kedav1alpha1.ScaleTarget{
			APIVersion: apiVersionAppsV1,
			Kind:       kindDeployment,
			Name:       scaleTargetName,
		},
		PollingInterval: ptr.To(int32(5)),
		CooldownPeriod:  ptr.To(int32(30)),
		MinReplicaCount: ptr.To(minReplicas),
		MaxReplicaCount: ptr.To(maxReplicas),
		Triggers: []kedav1alpha1.ScaleTriggers{
			{
				Type: "prometheus",
				Name: "wva-desired-replicas",
				Metadata: map[string]string{
					"serverAddress":       prometheusURL,
					"query":               query,
					"threshold":           "1",
					"activationThreshold": "0",
					"metricType":          "Value", // desired replicas is an absolute gauge; use value directly, not per-pod average
					"unsafeSsl":           "true",
				},
			},
		},
	}
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objName,
			Namespace: namespace,
			Labels:    map[string]string{"test-resource": defaultTestResourceLabelValue},
		},
		Spec: spec,
	}
	for _, opt := range opts {
		opt(so)
	}
	return so
}
