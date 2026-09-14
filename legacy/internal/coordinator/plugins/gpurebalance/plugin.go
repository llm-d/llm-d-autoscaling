package gpurebalance

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/go-logr/logr"
	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	AnnotationInferencePool = "llm-d.ai/epp-inference-pool"
	gpuQuotaResource        = "requests.nvidia.com/gpu"
	eppQueueMetric          = `sum(inference_extension_flow_control_queue_size{inference_pool=%q})`
	displayKindHPA          = "HorizontalPodAutoscaler"
	displayKindScaledObject = "ScaledObject"
)

// Plugin implements the Coordinator Plugin interface for GPU rebalancing.
type Plugin struct {
	client  client.Client
	promAPI promv1.API
}

// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch;patch;update

// New constructs the gpu-rebalance Plugin.
func New(c client.Client, promAPI promv1.API) *Plugin {
	return &Plugin{client: c, promAPI: promAPI}
}

// Name returns the unique plugin identifier.
func (p *Plugin) Name() string { return "gpu-rebalance" }

// scalerEntry pairs a managed scaling object with its inference-pool name.
type scalerEntry struct {
	obj          client.Object
	pool         string
	currentMax   int32
	effectiveMin int32
	displayKind  string
}

// Tick splits the GPU quota proportionally across managed HPAs or KEDA
// ScaledObjects based on each pool's EPP flow-control queue depth. Objects are
// grouped by namespace so each namespace is rebalanced independently against
// its own ResourceQuota. When all queues in a namespace are zero the quota is
// divided equally. MaxReplicas / maxReplicaCount is patched only when the
// computed target differs from the current value.
func (p *Plugin) Tick(ctx context.Context, selected []client.Object) error {
	log := ctrl.LoggerFrom(ctx).WithName("gpu-rebalance")

	// Group annotated scaling objects by namespace.
	byNamespace := make(map[string][]scalerEntry)
	for _, obj := range selected {
		entry, ok := scalerEntryFromObject(obj)
		if !ok {
			continue
		}
		if entry.pool == "" {
			continue
		}
		byNamespace[entry.obj.GetNamespace()] = append(byNamespace[entry.obj.GetNamespace()], entry)
	}
	if len(byNamespace) == 0 {
		return nil
	}

	for ns, entries := range byNamespace {
		if err := p.rebalanceNamespace(ctx, log, ns, entries); err != nil {
			return err
		}
	}
	return nil
}

func scalerEntryFromObject(obj client.Object) (scalerEntry, bool) {
	switch o := obj.(type) {
	case *autoscalingv2.HorizontalPodAutoscaler:
		return scalerEntry{
			obj:          o,
			pool:         o.Annotations[AnnotationInferencePool],
			currentMax:   o.Spec.MaxReplicas,
			effectiveMin: effectiveHPAMinReplicas(o.Spec.MinReplicas),
			displayKind:  displayKindHPA,
		}, true
	case *kedav1alpha1.ScaledObject:
		return scalerEntry{
			obj:          o,
			pool:         o.Annotations[AnnotationInferencePool],
			currentMax:   o.GetHPAMaxReplicas(),
			effectiveMin: *o.GetHPAMinReplicas(),
			displayKind:  displayKindScaledObject,
		}, true
	default:
		return scalerEntry{}, false
	}
}

// effectiveHPAMinReplicas preserves the plugin's existing minimum-one policy.
// HPA defaults minReplicas to 1 when omitted; explicit zero is also normalized
// to 1 because gpu-rebalance does not yet support scale-to-zero.
func effectiveHPAMinReplicas(configured *int32) int32 {
	if configured != nil && *configured > 1 {
		return *configured
	}
	return 1
}

// rebalanceNamespace applies proportional GPU quota allocation to all managed
// scaling objects in a single namespace.
func (p *Plugin) rebalanceNamespace(ctx context.Context, log logr.Logger, ns string, entries []scalerEntry) error {
	quota, err := p.namespaceGPUQuota(ctx, ns)
	if err != nil {
		return fmt.Errorf("reading GPU quota in namespace %s: %w", ns, err)
	}
	if quota <= 0 {
		log.V(1).Info("No GPU quota set, skipping", "namespace", ns)
		return nil
	}

	minimumTotal := int64(0)
	for _, entry := range entries {
		minimumTotal += int64(entry.effectiveMin)
	}
	if minimumTotal > quota {
		log.Info("Configured minimum replicas exceed GPU quota, skipping namespace",
			"namespace", ns, "minimumTotal", minimumTotal, "quota", quota)
		return nil
	}

	// Query EPP queue depth per pool (best-effort; default to 0 on error).
	queues := make([]float64, len(entries))
	for i, e := range entries {
		q, err := p.queryQueue(ctx, e.pool)
		if err != nil {
			log.V(1).Info("Queue query failed, using 0", "pool", e.pool, "err", err)
		} else {
			queues[i] = q
		}
	}

	// Weights: proportional to queue depth, equal when all queues are zero.
	totalQueue := 0.0
	for _, q := range queues {
		totalQueue += q
	}
	weights := make([]float64, len(entries))
	if totalQueue == 0 {
		for i := range weights {
			weights[i] = 1.0 / float64(len(entries))
		}
	} else {
		for i, q := range queues {
			weights[i] = q / totalQueue
		}
	}

	// TODO: scale-to-zero is not supported. When a pool's queue is 0 its weight is
	// 0% and the ideal target is 0 replicas, but the floor clamp below forces it to 1.
	// Supporting scale-to-zero requires: (a) the HPA has spec.minReplicas=0 (opt-in),
	// and (b) the Coordinator avoids setting maxReplicas=0 while in-flight requests are
	// still being drained (check active connection count or use a brief drain window
	// before zeroing). Until those conditions are met the floor is kept at 1 to prevent
	// accidental full scale-down.

	// Reserve every scaler's effective minimum, then distribute the remaining
	// quota proportionally. Any rounding remainder goes to the highest-weight pool.
	remaining := quota - minimumTotal
	targets := make([]int32, len(entries))
	allocated := int64(0)
	maxWeightIdx := 0
	for i := range entries {
		t := entries[i].effectiveMin + int32(math.Floor(float64(remaining)*weights[i]))
		targets[i] = t
		allocated += int64(t)
		if weights[i] > weights[maxWeightIdx] {
			maxWeightIdx = i
		}
	}
	if rem := quota - allocated; rem > 0 {
		targets[maxWeightIdx] += int32(rem)
	}

	// TODO: the current patch-on-every-tick approach causes wobble. Instantaneous
	// queue depth is noisy, so small fluctuations (e.g. q=270 vs q=286) move the
	// floor/ceil boundary each cycle, producing 1-replica flips every 15s. Each
	// flip triggers pod churn which perturbs the queue and drives the next flip.
	// Fix with two complementary guards:
	//   1. Minimum delta: skip the patch when abs(new - current) < N replicas so
	//      noise-driven micro-adjustments are ignored.
	//   2. Per-HPA cooldown: after patching an HPA, skip it for the next K ticks
	//      so the HPA and its pods have time to converge before the next reading.
	// An alternative to (2) is EWMA smoothing of the queue metric across ticks,
	// which dampens burst noise without adding a hard cooldown delay.

	for i, e := range entries {
		if targets[i] == e.currentMax {
			continue
		}
		log.Info("Setting max replica ceiling",
			"kind", e.displayKind, "name", e.obj.GetName(), "namespace", ns,
			"pool", e.pool,
			"from", e.currentMax, "to", targets[i],
			"queue", queues[i], "quota", quota,
		)
		if err := p.setMaxReplicas(ctx, e.obj, targets[i]); err != nil {
			return fmt.Errorf("patching %s %s: %w", e.displayKind, e.obj.GetName(), err)
		}
	}
	return nil
}

func (p *Plugin) namespaceGPUQuota(ctx context.Context, ns string) (int64, error) {
	// TODO: a namespace can have multiple ResourceQuotas; the effective GPU limit
	// is the minimum across all of them, not the first one found. Returning the
	// first match can overestimate the budget and allow over-scheduling.

	// TODO: ResourceQuotas can carry a spec.scopeSelector that restricts which
	// pods they apply to (e.g. only BestEffort or Terminating pods). This code
	// ignores scope selectors and treats any quota with a GPU field as the full
	// namespace budget, which may overstate the applicable limit.

	list := &corev1.ResourceQuotaList{}
	if err := p.client.List(ctx, list, client.InNamespace(ns)); err != nil {
		return 0, err
	}
	for i := range list.Items {
		if q, ok := list.Items[i].Spec.Hard[corev1.ResourceName(gpuQuotaResource)]; ok {
			return q.Value(), nil
		}
	}
	return 0, nil
}

func (p *Plugin) queryQueue(ctx context.Context, inferencePool string) (float64, error) {
	// TODO: the query filters only by inference_pool name with no namespace label.
	// If two namespaces each have a pool with the same name (e.g. "model-a"), the
	// sum includes both, inflating the queue reading for each namespace's allocation
	// decision. Fix: include a namespace label in the query if the EPP emits one,
	// or scope the metric series by passing the namespace as an additional matcher.

	query := fmt.Sprintf(eppQueueMetric, inferencePool)
	result, _, err := p.promAPI.Query(ctx, query, time.Now())
	if err != nil {
		return 0, err
	}
	vec, ok := result.(model.Vector)
	if !ok || len(vec) == 0 {
		return 0, nil
	}
	return float64(vec[0].Value), nil
}

// setMaxReplicas patches the scaler's max-replica ceiling to target under
// optimistic concurrency control. Each attempt re-reads the object and patches
// with MergeFromWithOptimisticLock, which pins resourceVersion on the patch so
// the API server rejects a stale write with a 409 Conflict rather than silently
// overwriting a concurrent change. On conflict the write is retried against the
// freshly read object; the Coordinator remains authoritative for the ceiling,
// so the recomputed target is re-applied rather than deferring to the other
// writer.
func (p *Plugin) setMaxReplicas(ctx context.Context, obj client.Object, target int32) error {
	log := ctrl.LoggerFrom(ctx).WithName("gpu-rebalance")
	key := client.ObjectKeyFromObject(obj)

	switch obj.(type) {
	case *autoscalingv2.HorizontalPodAutoscaler:
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur := &autoscalingv2.HorizontalPodAutoscaler{}
			if err := p.client.Get(ctx, key, cur); err != nil {
				return err
			}
			original := cur.DeepCopy()
			cur.Spec.MaxReplicas = target
			return p.patchWithConflictLog(ctx, log, cur, original, displayKindHPA, key)
		})
	case *kedav1alpha1.ScaledObject:
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur := &kedav1alpha1.ScaledObject{}
			if err := p.client.Get(ctx, key, cur); err != nil {
				return err
			}
			original := cur.DeepCopy()
			cur.Spec.MaxReplicaCount = ptr.To(target)
			return p.patchWithConflictLog(ctx, log, cur, original, displayKindScaledObject, key)
		})
	default:
		return fmt.Errorf("unsupported scaler type %T", obj)
	}
}

// patchWithConflictLog applies an optimistic-lock patch and, when the write is
// rejected with a 409 Conflict, logs the concurrent modification before
// returning the error so RetryOnConflict re-reads and retries.
func (p *Plugin) patchWithConflictLog(ctx context.Context, log logr.Logger, obj, original client.Object, kind string, key client.ObjectKey) error {
	err := p.client.Patch(ctx, obj, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
	if apierrors.IsConflict(err) {
		log.V(1).Info("Conflict patching max replica ceiling; re-reading and retrying",
			"kind", kind, "name", key.Name, "namespace", key.Namespace)
	}
	return err
}
