package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// BootstrapInitialConfigMaps performs an initial sync of known ConfigMaps before manager start.
// It ensures dynamic ConfigMap-backed settings are loaded before other reconcilers and engines run.
func (r *ConfigMapReconciler) BootstrapInitialConfigMaps(ctx context.Context) error {
	logger := log.FromContext(ctx)

	if r.Config == nil {
		err := errors.New("config is nil")
		errorType := "Config is nil in ConfigMapReconciler bootstrap"
		logger.Error(err, errorType)
		metrics.RecordError(constants.ComponentController, errorType)
		return err
	}

	systemNamespace := config.SystemNamespace()
	watchNamespace := r.Config.WatchNamespace()

	// Always bootstrap global ConfigMaps from system namespace
	targets := []struct {
		name      string
		namespace string
		isGlobal  bool
	}{
		{name: config.SaturationConfigMapName(), namespace: systemNamespace, isGlobal: true},
		{name: config.DefaultScaleToZeroConfigMapName, namespace: systemNamespace, isGlobal: true},
		{name: config.QMAnalyzerConfigMapName(), namespace: systemNamespace, isGlobal: true},
	}

	// Determine which namespaces to scan for namespace-local ConfigMaps
	var namespacesToScan []string

	if watchNamespace != "" {
		// Single-namespace mode: only watch the specified namespace
		if watchNamespace != systemNamespace {
			namespacesToScan = []string{watchNamespace}
			logger.Info("Initial ConfigMap bootstrap", "watchNamespace", watchNamespace)
		}
	} else {
		// All-namespaces mode: list and scan all namespaces
		namespaceList := &corev1.NamespaceList{}
		if err := r.List(ctx, namespaceList, &client.ListOptions{}); err != nil {
			errorType := "Failed to list namespaces during bootstrap"
			logger.Error(err, errorType)
			metrics.RecordError(constants.ComponentController, errorType)
			r.Config.MarkConfigMapsBootstrapFailed(err)
			return fmt.Errorf("failed to list namespaces: %w", err)
		}

		for _, ns := range namespaceList.Items {
			// Skip system namespace to avoid duplicate global config loading
			if ns.Name != systemNamespace {
				if ns.Annotations != nil {
					if value, ok := ns.Annotations[constants.NamespaceExcludeAnnotationKey]; ok {
						if value == constants.AnnotationValueTrue {
							continue // Skip excluded namespaces. Only for all-namespaces mode.
						}
					}

				}
				if ns.Labels != nil {
					if value, ok := ns.Labels[constants.NamespaceConfigEnabledLabelKey]; ok {
						if value == constants.AnnotationValueTrue {
							namespacesToScan = append(namespacesToScan, ns.Name)
						}
					}
				}
			}
		}
		logger.Info("Initial ConfigMap bootstrap", "namespaceCount", len(namespacesToScan))
	}

	// Add namespace-local ConfigMap targets
	for _, ns := range namespacesToScan {
		targets = append(targets,
			struct {
				name      string
				namespace string
				isGlobal  bool
			}{name: config.SaturationConfigMapName(), namespace: ns, isGlobal: false},
			struct {
				name      string
				namespace string
				isGlobal  bool
			}{name: config.DefaultScaleToZeroConfigMapName, namespace: ns, isGlobal: false},
			struct {
				name      string
				namespace string
				isGlobal  bool
			}{name: config.QMAnalyzerConfigMapName(), namespace: ns, isGlobal: false},
		)
	}

	// Bootstrap all target ConfigMaps
	for _, target := range targets {
		if err := r.bootstrapConfigMap(ctx, target.name, target.namespace, target.isGlobal); err != nil {
			r.Config.MarkConfigMapsBootstrapFailed(err)
			return err
		}
	}

	r.Config.MarkConfigMapsBootstrapComplete()
	logger.Info("Initial ConfigMap bootstrap completed", "targets", len(targets))
	return nil
}

func (r *ConfigMapReconciler) bootstrapConfigMap(ctx context.Context, name, namespace string, isGlobal bool) error {
	logger := log.FromContext(ctx)
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("Bootstrap ConfigMap not found, continuing with defaults", "name", name, "namespace", namespace)
			return nil
		}
		return fmt.Errorf("failed to bootstrap ConfigMap %s/%s: %w", namespace, name, err)
	}

	switch name {
	case config.SaturationConfigMapName():
		r.handleSaturationConfigMap(ctx, cm, namespace, isGlobal)
	case config.DefaultScaleToZeroConfigMapName:
		r.handleScaleToZeroConfigMap(ctx, cm, namespace, isGlobal)
	case config.QMAnalyzerConfigMapName():
		r.handleQMAnalyzerConfigMap(ctx, cm, namespace, isGlobal)
	default:
		logger.V(1).Info("Ignoring unrecognized bootstrap ConfigMap", "name", name, "namespace", namespace)
	}

	return nil
}
