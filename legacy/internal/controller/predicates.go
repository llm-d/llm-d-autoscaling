package controller

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ConfigMapPredicate returns a predicate that filters ConfigMap events to only the target ConfigMaps.
// It matches the enqueue function logic - allows either configmap name if namespace matches.
// This predicate is used to filter only the target configmaps.
//
// For namespace-local ConfigMap support:
// - Global ConfigMaps: well-known names in controller namespace
// - Namespace-local ConfigMaps: well-known names in watched or tracked namespaces
//
// Filtering behavior:
//   - Single-namespace mode (--watch-namespace set): Always allow ConfigMaps from the watched namespace
//   - Multi-namespace mode: Only allow ConfigMaps from tracked namespaces (namespaces with VAs)
//
// ds is the datastore used to check if a namespace is tracked (fast, in-memory check).
// cfg is the configuration used to check if single-namespace mode is enabled.
// Opt-in labels and exclusion are handled in the handler to avoid expensive API calls in the predicate.
func ConfigMapPredicate(ds datastore.Datastore, cfg *config.Config) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		name := obj.GetName()
		namespace := obj.GetNamespace()
		systemNamespace := config.SystemNamespace()

		// Well-known ConfigMap names
		wellKnownNames := map[string]bool{
			config.ConfigMapName():                 true,
			config.SaturationConfigMapName():       true,
			config.DefaultScaleToZeroConfigMapName: true,
			config.QMAnalyzerConfigMapName():       true,
		}

		// Check if this is a well-known ConfigMap name
		if !wellKnownNames[name] {
			return false
		}

		// Global ConfigMaps: must be in controller namespace
		if namespace == systemNamespace {
			return true
		}

		// Single-namespace mode: watch all ConfigMaps in the watched namespace
		// Explicit CLI flag overrides tracking-based filtering
		if cfg != nil {
			watchNamespace := cfg.WatchNamespace()
			if watchNamespace != "" && namespace == watchNamespace {
				return true
			}
		}

		// Multi-namespace mode: only allow in tracked namespaces (namespaces with VAs)
		// This prevents cluster-wide watching and cache sync timeouts.
		// Opt-in labels and exclusion are still checked in the handler for accuracy.
		if ds != nil {
			return ds.IsNamespaceTracked(namespace)
		}

		// If no datastore provided, fall back to allowing all (backwards compatible)
		// This should not happen in production, but provides safety during setup.
		return true
	})
}

// AnnotatedScalerPredicate passes events only for objects bearing llm-d.ai/managed: "true".
// For Update events, either old or new object must be managed so that annotation removal
// reaches handleAnnotatedScalerEvent and triggers the untrack path.
func AnnotatedScalerPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return annotations.IsManaged(e.Object) },
		DeleteFunc: func(e event.DeleteEvent) bool { return annotations.IsManaged(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return annotations.IsManaged(e.ObjectOld) || annotations.IsManaged(e.ObjectNew)
		},
		GenericFunc: func(e event.GenericEvent) bool { return annotations.IsManaged(e.Object) },
	}
}
