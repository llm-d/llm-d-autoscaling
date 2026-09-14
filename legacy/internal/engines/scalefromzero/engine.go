/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scalefromzero

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/controller-runtime/pkg/log"

	accel "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/actuator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/common"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/executor"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	poolutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/pool"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// Constants for condition
const (
	MetricsReasonAvailable  = "ScaleFromZero"
	MetricsMessageAvailable = "Scaled from zero due to pending requests"
	reasonDetails           = ": pending request - scale-up"
	targetEPPMetricName     = "inference_extension_flow_control_queue_size"
	targetEPPMetricLabel    = "target_model_name"
)

type Engine struct {
	client         client.Client
	executor       executor.Executor
	recorder       record.EventRecorder
	Datastore      datastore.Datastore
	DynamicClient  dynamic.Interface
	Actuator       *actuator.DirectActuator
	Mapper         meta.RESTMapper
	maxConcurrency int
	config         *config.Config // Unified configuration (injected from main.go)
}

// NewEngine creates a new instance of the scale-from-zero engine.
// cfg must be non-nil (validated in main.go before engine creation).
func NewEngine(client client.Client, recorder record.EventRecorder, mapper meta.RESTMapper, restConfig *rest.Config, ds datastore.Datastore, cfg *config.Config) (*Engine, error) {
	if cfg == nil {
		return nil, errors.New("config is nil in NewEngine - this should not happen")
	}

	maxConcurrency := cfg.ScaleFromZeroMaxConcurrency()
	if maxConcurrency <= 0 {
		return nil, fmt.Errorf("invalid scale-from-zero max concurrency: must be positive, got %d", maxConcurrency)
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	actuator, err := actuator.NewDirectActuator(restConfig)
	if err != nil {
		return nil, err
	}

	engine := Engine{
		client:         client,
		recorder:       recorder,
		Datastore:      ds,
		DynamicClient:  dynamicClient,
		Actuator:       actuator,
		Mapper:         mapper,
		maxConcurrency: maxConcurrency,
		config:         cfg,
	}

	// TODO: replace by an hybrid, polling and reactive executor when available
	engine.executor = executor.NewPollingExecutor(executor.PollingConfig{
		Config: executor.Config{
			OptimizeFunc: engine.optimize,
		},
		Interval:     100 * time.Millisecond, // frequent polling to quickly detect scale-from-zero opportunities
		RetryBackoff: 100 * time.Millisecond,
	})

	return &engine, nil
}

// StartOptimizeLoop starts the optimization loop for the scale-from-zero engine.
// It runs until the context is cancelled.
func (e *Engine) StartOptimizeLoop(ctx context.Context) {
	e.executor.Start(ctx)
}

// optimize performs the optimization logic.
func (e *Engine) optimize(ctx context.Context) error {
	logger := log.FromContext(ctx)

	// Get all inactive (replicas == 0) VAs
	inactiveVAs, scaleTargets, err := utils.InactiveVariantAutoscaling(ctx, e.client)
	if err != nil {
		return err
	}

	logger.V(logging.DEBUG).Info("Found inactive VariantAutoscaling resources", "count", len(inactiveVAs))

	var wg sync.WaitGroup
	sem := make(chan struct{}, e.maxConcurrency)
	errorCh := make(chan error, e.maxConcurrency)

	// Start error aggregation in a separate goroutine to prevent deadlock
	var aggregatedErrors []error
	var errorWg sync.WaitGroup
	errorWg.Add(1)
	go func() {
		defer errorWg.Done()
		for err := range errorCh {
			if err != nil {
				aggregatedErrors = append(aggregatedErrors, err)
			}
		}
	}()

variantLoop:
	for _, va := range inactiveVAs {
		// Check if context is cancelled, but don't return immediately
		select {
		case <-ctx.Done():
			logger.V(logging.DEBUG).Info("Context cancelled, stopping new work")
			break variantLoop
		default:
		}

		logger.V(logging.DEBUG).Info("Processing variant", "name", va.Name)
		wg.Add(1)

		// This call blocks if the channel is full (concurrency limit reached)
		sem <- struct{}{}
		go func(variant wvav1alpha1.VariantAutoscaling) {
			defer wg.Done()
			defer func() { <-sem }()

			err := e.processInactiveVariant(ctx, scaleTargets, variant, 1)
			if err != nil {
				logger.V(logging.DEBUG).Error(err, "Error Processing variant", "name", variant.Name)
				errorCh <- err
			} else {
				errorCh <- nil
			}
		}(va)
	}

	// Wait for all goroutines to complete, then close error channel
	wg.Wait()
	close(errorCh)

	// Wait for error aggregation to complete
	errorWg.Wait()

	// After all work is done, if the context was cancelled, return that error
	if err := ctx.Err(); err != nil {
		return err
	}

	if len(aggregatedErrors) > 0 {
		return errors.Join(aggregatedErrors...)
	}
	return nil
}

// ProcessInactiveVariant processes a single inactive VariantAutoscaling resource.
func (e *Engine) processInactiveVariant(ctx context.Context, scaleTargets map[string]scaletarget.ScaleTargetAccessor, va wvav1alpha1.VariantAutoscaling, targetWorkloadReplicas int) error {
	logger := log.FromContext(ctx)
	objAPI := va.GetScaleTargetAPI()
	objKind := va.GetScaleTargetKind()
	objName := va.GetScaleTargetName()

	// Parse Group, Version, Kind, Resource
	gvr, err := poolutil.GetResourceForKind(e.Mapper, objAPI, objKind)
	if err != nil {
		return err
	}

	unstructuredObj, err := e.DynamicClient.Resource(gvr).Namespace(va.Namespace).Get(ctx, objName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// Extract Labels for the pods created by the ScaleTarget object
	// Use ScaleTargetAccessor to handle both Deployment and LeaderWorkerSet uniformly
	key := utils.GetNamespacedKey(va.Namespace, objName)
	scaleTarget, found := scaleTargets[key]
	if !found {
		// Fetch on-demand if not in the cache
		scaleTarget, err = scaletarget.FetchScaleTarget(ctx, e.client, va.Name, objKind, objName, va.Namespace)
		if err != nil {
			return err
		}
	}
	podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
	if podTemplateSpec == nil {
		return errors.New("pod template spec is missing for target workload object")
	}
	labels := podTemplateSpec.Labels
	if labels == nil {
		return errors.New("labels are missing for target workload object")
	}

	// Check if inferencepool datastore is empty: this can happen during bootstrapping
	dsPoolList := e.Datastore.PoolList()
	if len(dsPoolList) == 0 {
		logger.Info("Inferencepool datastore is empty - skipping processing inactive variant", "value", va.Name)
		return nil
	}

	// Find target EPP for metrics collection in the same namespace as the VA
	pool, err := e.Datastore.PoolGetFromLabels(va.Namespace, labels)
	if err != nil {
		// Only skip on "not found" errors - return other errors to surface real datastore failures
		if errors.Is(err, datastore.ErrPoolNotSynced) {
			logger.V(logging.DEBUG).Info("Skipping variant, target EPP not found in datastore",
				"variant", va.Name,
				"namespace", va.Namespace,
				"modelID", va.Spec.ModelID)
			return nil
		}
		// Unexpected error - log and return to surface the issue
		logger.Error(err, "Unexpected error finding target EPP",
			"variant", va.Name,
			"namespace", va.Namespace,
			"modelID", va.Spec.ModelID)
		return err
	}

	// Use EPP source from registry
	namespacedPoolName := pool.Namespace + "/" + pool.Name
	eppSource := e.Datastore.PoolGetMetricsSource(namespacedPoolName)
	if eppSource == nil {
		// This is unexpected - pool exists but metrics source is missing
		err := fmt.Errorf("EPP metrics source not found in registry for pool %s", namespacedPoolName)
		logger.Error(err, "Datastore inconsistency detected",
			"variant", va.Name,
			"namespace", va.Namespace,
			"pool", namespacedPoolName)
		return err
	}

	// Execute the query with timing
	startTime := time.Now()
	results, err := eppSource.Refresh(ctx, source.RefreshSpec{})
	duration := time.Since(startTime).Seconds()
	metrics.ObserveMetricsCollectionDuration(duration, constants.QueryTypeQueueLength)

	if err != nil {
		reason := prometheus.CategorizePrometheusError(err)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeQueueLength, reason)
		return err
	}

	// Check for pending requests using EPP flowcontrol queue size metrics
	result := results["all_metrics"]
	pendingRequestExist := false
	for _, value := range result.Values {
		metricName := value.Labels["__name__"]
		if metricName == targetEPPMetricName && value.Value > 0 {
			if value.Labels[targetEPPMetricLabel] == va.Spec.ModelID {
				logger.Info(
					"Target workload has pending requests, scaling up from zero", "metricName", metricName,
					"metric", value.Labels, "value", value.Value)
				pendingRequestExist = true
				break
			}
		}
	}

	if !pendingRequestExist {
		// Scale-from-zero loop runs every 100ms; log at DEBUG to avoid flooding (10/sec per inactive VA).
		logger.V(logging.DEBUG).Info("Scale-from-zero: skipping VA, no pending requests in flow control queue",
			"va", va.Name,
			"namespace", va.Namespace,
			"modelID", va.Spec.ModelID)
		return nil
	}

	// 1.  Scale up from zero to one
	// TODO: Right now we are scaling all the VA for the same target model. We need to scale only the VA that has the lowest cost.
	err = e.Actuator.ScaleTargetObject(ctx, unstructuredObj, int32(targetWorkloadReplicas))
	if err != nil {
		logger.Error(err, "Error scaling up Target Workload", "variant", va.Name, "target VA model", va.Spec.ModelID)
		return err
	}
	logger.Info("Successfully scaled up Target Workload", "variant", va.Name, "target VA model", va.Spec.ModelID, "inferencepool", pool.EndpointPicker.ServiceName)

	// 2. Create or update VariantDecision
	va.Status.Actuation.Applied = false
	// Determine accelerator - try status first, then labels
	var accelerator string
	accelerator = va.Status.DesiredOptimizedAlloc.Accelerator
	if accelerator == "" {
		// Try to get from deployment/LWS nodeSelector/nodeAffinity, or VA labels
		key := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
		if scaleTarget, found := scaleTargets[key]; found {
			accelerator = accel.GetAcceleratorNameFromScaleTarget(&va, scaleTarget)
		} else {
			// Deployment/LWS not cached, fall back to VA label via nil deployment/LWS
			accelerator = accel.GetAcceleratorNameFromScaleTarget(&va, nil)
		}
	}

	decision, hasDecision := common.DecisionCache.Get(va.Name, va.Namespace)
	if !hasDecision {
		cost, err := strconv.ParseFloat(va.Spec.VariantCost, 64)
		if err != nil {
			return err
		}
		d := domain.VariantDecision{
			VariantName:        va.Name,
			Namespace:          va.Namespace,
			ModelID:            va.Spec.ModelID,
			Cost:               cost,
			TargetReplicas:     targetWorkloadReplicas, // Scale up to 1 replica
			CurrentReplicas:    targetWorkloadReplicas,
			DesiredReplicas:    targetWorkloadReplicas,
			LastRunTime:        metav1.Now(),
			SaturationBased:    false,
			SafetyOverride:     false,
			ModelBasedDecision: false,
			AcceleratorName:    accelerator,
			MetricsAvailable:   true,
			MetricsReason:      MetricsReasonAvailable,
			MetricsMessage:     MetricsMessageAvailable,
		}
		d.SetDecisionReason(domain.ActionScaleUp, domain.DecisionReasonScaleFromZero, string(domain.DecisionReasonScaleFromZero)+reasonDetails)
		common.DecisionCache.Set(va.Name, va.Namespace, d)
	} else {
		if decision.CurrentReplicas == 0 {
			decision.TargetReplicas = targetWorkloadReplicas
			decision.CurrentReplicas = targetWorkloadReplicas
			decision.DesiredReplicas = targetWorkloadReplicas
			decision.LastRunTime = metav1.Now()
			decision.SaturationBased = false
			decision.SafetyOverride = false
			decision.ModelBasedDecision = false
			decision.SetDecisionReason(domain.ActionScaleUp, domain.DecisionReasonScaleFromZero, string(domain.DecisionReasonScaleFromZero)+reasonDetails)
			decision.AcceleratorName = accelerator
			decision.MetricsAvailable = true
			decision.MetricsReason = MetricsReasonAvailable
			decision.MetricsMessage = MetricsMessageAvailable
			common.DecisionCache.Set(va.Name, va.Namespace, decision)
		} else {
			logger.Info("Target variant decision.CurrentReplicas is not zero", "value", decision.CurrentReplicas)
		}
	}

	// 3. Updates VA status.
	numReplicas := int32(targetWorkloadReplicas)
	va.Status.DesiredOptimizedAlloc = wvav1alpha1.OptimizedAlloc{
		NumReplicas: &numReplicas,
		LastRunTime: metav1.Now(),
		Accelerator: accelerator,
	}

	// Set condition based on decision characteristics
	wvav1alpha1.SetCondition(&va,
		wvav1alpha1.TypeOptimizationReady,
		metav1.ConditionTrue,
		"ScaleFromZeroMode",
		"scalefromzero decision: "+string(domain.DecisionReasonScaleFromZero)+reasonDetails)

	// Record event just before Actuation.Applied = true
	if hasDecision && targetWorkloadReplicas > 0 {
		e.recorder.Eventf(&va, corev1.EventTypeNormal, constants.K8SEventScaledUp, string(domain.DecisionReasonScaleFromZero)+reasonDetails)
	}
	va.Status.Actuation.Applied = true

	// Log scaling decision for E2E and operators (mirrors saturation engine "Applied ... via shared cache").
	logger.Info("Scale-from-zero decision written to cache",
		"va", va.Name,
		"namespace", va.Namespace,
		"targetReplicas", targetWorkloadReplicas,
		"reason", string(domain.DecisionReasonScaleFromZero)+reasonDetails)

	return nil
}
