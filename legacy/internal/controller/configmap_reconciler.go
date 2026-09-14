/*
Copyright 2025.

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

package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// ConfigMapReconciler reconciles ConfigMaps to update the unified configuration.
// Its sole responsibility is to keep the Config object synchronized with ConfigMap changes.
type ConfigMapReconciler struct {
	client.Reader
	Scheme    *runtime.Scheme
	Config    *config.Config
	Datastore datastore.Datastore
	Recorder  record.EventRecorder

	// ThroughputRegistered is the throughput-analyzer registration decision
	// frozen at startup (cmd/main.go). Analyzer registration cannot change
	// without a controller restart; the reconciler compares live config against
	// this to warn when a runtime edit would be silently inert.
	ThroughputRegistered bool
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps/status,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles ConfigMap changes and updates the unified configuration.
func (r *ConfigMapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the ConfigMap
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, req.NamespacedName, cm); err != nil {
		if apierrors.IsNotFound(err) {
			// ConfigMap was deleted - handle cleanup
			logger.Info("ConfigMap deleted", "name", req.Name, "namespace", req.Namespace)
			r.handleConfigMapDeletion(ctx, req.Name, req.Namespace)
			return ctrl.Result{}, nil
		}
		errorType := "Failed to get ConfigMap"
		logger.Error(err, errorType, "name", req.Name, "namespace", req.Namespace)
		metrics.RecordError(constants.ComponentController, errorType)
		return ctrl.Result{}, err
	}

	name := cm.GetName()
	namespace := cm.GetNamespace()
	systemNamespace := config.SystemNamespace()

	// Determine if this is a global or namespace-local ConfigMap
	isGlobal := namespace == systemNamespace
	isNamespaceLocal := !isGlobal && r.shouldWatchNamespaceLocalConfigMap(ctx, namespace)

	// Only process if it's global or namespace-local (tracked)
	if !isGlobal && !isNamespaceLocal {
		logger.V(1).Info("Ignoring ConfigMap from untracked namespace", "name", name, "namespace", namespace)
		return ctrl.Result{}, nil
	}

	// Route to appropriate handler based on ConfigMap name
	switch name {
	case config.SaturationConfigMapName():
		r.handleSaturationConfigMap(ctx, cm, namespace, isGlobal)
	case config.DefaultScaleToZeroConfigMapName:
		r.handleScaleToZeroConfigMap(ctx, cm, namespace, isGlobal)
	case config.QMAnalyzerConfigMapName():
		r.handleQMAnalyzerConfigMap(ctx, cm, namespace, isGlobal)
	default:
		logger.V(1).Info("Ignoring unrecognized ConfigMap", "name", name, "namespace", namespace)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConfigMapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		WithEventFilter(ConfigMapPredicate(r.Datastore, r.Config)).
		Complete(r)
}

// handleConfigMapDeletion handles ConfigMap deletion events.
func (r *ConfigMapReconciler) handleConfigMapDeletion(ctx context.Context, name, namespace string) {
	logger := log.FromContext(ctx)
	systemNamespace := config.SystemNamespace()

	// Only handle namespace-local ConfigMap deletions (not global)
	if namespace == systemNamespace {
		return
	}

	// Check if this namespace should be tracked
	if !r.shouldWatchNamespaceLocalConfigMap(ctx, namespace) {
		return
	}

	// Remove namespace-local config on deletion
	switch name {
	case config.SaturationConfigMapName():
		r.Config.RemoveNamespaceConfig(namespace)
		logger.Info("Removed namespace-local saturation config on ConfigMap deletion", "namespace", namespace)
	case config.DefaultScaleToZeroConfigMapName:
		r.Config.RemoveNamespaceConfig(namespace)
		logger.Info("Removed namespace-local scale-to-zero config on ConfigMap deletion", "namespace", namespace)
	case config.QMAnalyzerConfigMapName():
		r.Config.RemoveNamespaceConfig(namespace)
		logger.Info("Removed namespace-local queueing model config on ConfigMap deletion", "namespace", namespace)
	}
}

// shouldWatchNamespaceLocalConfigMap returns true if a namespace-local ConfigMap should be watched.
// In single-namespace mode (--watch-namespace set), it watches all ConfigMaps in the watched namespace.
// In multi-namespace mode, it checks exclusion first (highest priority), then VA-based tracking (automatic), then opt-in label (explicit).
func (r *ConfigMapReconciler) shouldWatchNamespaceLocalConfigMap(ctx context.Context, namespace string) bool {
	// In single-namespace mode, watch all ConfigMaps in the watched namespace
	// Explicit CLI flag overrides annotation/label-based filtering
	if r.Config != nil {
		watchNamespace := r.Config.WatchNamespace()
		if watchNamespace != "" && namespace == watchNamespace {
			return true
		}
	}

	// Multi-namespace mode: Check exclusion first (highest priority - overrides everything)
	if isNamespaceExcluded(ctx, r.Reader, namespace) {
		return false
	}

	// Check VA-based tracking (automatic)
	if r.Datastore != nil && r.Datastore.IsNamespaceTracked(namespace) {
		return true
	}

	// Check label-based opt-in (explicit)
	return isNamespaceConfigEnabled(ctx, r.Reader, namespace)
}

// handleSaturationConfigMap handles updates to the saturation scaling ConfigMap.
// Supports both global and namespace-local ConfigMaps.
func (r *ConfigMapReconciler) handleSaturationConfigMap(ctx context.Context, cm *corev1.ConfigMap, namespace string, isGlobal bool) {
	logger := log.FromContext(ctx)

	// Parse saturation scaling config entries
	configs, count := parseSaturationConfig(cm.Data, logger)

	// Update global or namespace-local config
	if isGlobal {
		r.Config.UpdateSaturationConfig(configs)
		logger.Info("Updated global saturation config from ConfigMap", "entries", count)
		r.warnIfThroughputRegistrationDiverged(logger, cm)
	} else {
		r.Config.UpdateSaturationConfigForNamespace(namespace, configs)
		logger.Info("Updated namespace-local saturation config from ConfigMap", "namespace", namespace, "entries", count)
	}
}

// warnIfThroughputRegistrationDiverged compares the live global config's
// throughput-analyzer enablement against the registration decision frozen at
// controller startup. Analyzer registration cannot change without a restart,
// so a divergence means this ConfigMap edit is silently inert; it emits a
// Warning event on cm plus a log line telling the operator to restart.
//
// It runs only after the initial ConfigMap bootstrap has completed. The frozen
// decision (ThroughputRegistered) is captured just after bootstrap returns
// (cmd/main.go); during the synchronous bootstrap pass it is still its zero
// value, so comparing against it would emit a spurious "restart required"
// warning on every healthy startup. The manager — and therefore all runtime
// reconciles — starts only after the decision is frozen, so gating on bootstrap
// completion leaves genuine runtime edits fully covered.
//
// Namespace-local config does not participate: startup registration reads only
// the global saturation config, so a namespace-local edit can never cause this
// kind of drift.
func (r *ConfigMapReconciler) warnIfThroughputRegistrationDiverged(logger logr.Logger, cm *corev1.ConfigMap) {
	if !r.Config.ConfigMapsBootstrapComplete() {
		// Bootstrap pass: the registration decision has not been frozen yet.
		return
	}

	want := r.Config.ThroughputAnalyzerEnabled()
	if want == r.ThroughputRegistered {
		return
	}

	logger.Info("Throughput analyzer registration diverged from live config; controller restart required to apply",
		"wantEnabled", want, "registered", r.ThroughputRegistered, "configMap", cm.Name)
	if r.Recorder != nil {
		msg := fmt.Sprintf(
			"Throughput analyzer enablement in config (%t) differs from the registration "+
				"frozen at controller startup (%t); analyzer registration cannot change at "+
				"runtime. Restart the wva-controller-manager to apply.",
			want, r.ThroughputRegistered)
		r.Recorder.Event(cm, corev1.EventTypeWarning, constants.K8SEventThroughputAnalyzerRestartRequired, msg)
	}
}

// handleScaleToZeroConfigMap handles updates to the scale-to-zero ConfigMap.
// Supports both global and namespace-local ConfigMaps.
func (r *ConfigMapReconciler) handleScaleToZeroConfigMap(ctx context.Context, cm *corev1.ConfigMap, namespace string, isGlobal bool) {
	logger := log.FromContext(ctx)

	// Parse scale-to-zero config
	scaleToZeroConfig := config.ParseScaleToZeroConfigMap(cm.Data)

	// Log parsed config for debugging
	logger.Info("Processing scale-to-zero ConfigMap",
		"name", cm.GetName(),
		"namespace", namespace,
		"isGlobal", isGlobal,
		"configKeys", len(cm.Data),
		"parsedModelCount", len(scaleToZeroConfig))

	// Update global or namespace-local config
	if isGlobal {
		r.Config.UpdateScaleToZeroConfig(scaleToZeroConfig)
		logger.Info("Updated global scale-to-zero config from ConfigMap", "modelCount", len(scaleToZeroConfig))
	} else {
		r.Config.UpdateScaleToZeroConfigForNamespace(namespace, scaleToZeroConfig)
		logger.Info("Updated namespace-local scale-to-zero config from ConfigMap", "namespace", namespace, "modelCount", len(scaleToZeroConfig))
	}
}

// handleQMAnalyzerConfigMap handles updates to the queueing model ConfigMap.
// Supports both global and namespace-local ConfigMaps.
func (r *ConfigMapReconciler) handleQMAnalyzerConfigMap(ctx context.Context, cm *corev1.ConfigMap, namespace string, isGlobal bool) {
	logger := log.FromContext(ctx)

	// Parse queue model based scaling config entries
	configs, count := parseQMAnalyzerConfig(cm.Data, logger)

	// Update global or namespace-local config
	if isGlobal {
		r.Config.UpdateQMAnalyzerConfig(configs)
		logger.Info("Updated global queueing model config from ConfigMap", "entries", count)
	} else {
		r.Config.UpdateQMAnalyzerConfigForNamespace(namespace, configs)
		logger.Info("Updated namespace-local queueing model config from ConfigMap", "namespace", namespace, "entries", count)
	}
}
