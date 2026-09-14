package crd

import (
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// serverGroupsAndResourcesIface is the minimal discovery surface needed by checkCRDInstalled.
type serverGroupsAndResourcesIface interface {
	ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error)
}

// CheckCRDInstalled reports whether a CRD with the given groupVersion and kind is
// registered in the cluster. It is called once at startup; dynamic CRD installation
// after the controller starts is not yet handled.
func CheckCRDInstalled(restConfig *rest.Config, groupVersion, kind string, logger logr.Logger) bool {
	installed, err := DetectCRDInstalled(restConfig, groupVersion, kind, logger)
	if err != nil {
		logger.Error(err, "failed to discover API resources",
			"groupVersion", groupVersion, "kind", kind)
	}
	return installed
}

// DetectCRDInstalled reports whether a CRD with the given groupVersion and kind
// is registered in the cluster, returning an error when discovery could not
// determine API resources at all.
func DetectCRDInstalled(restConfig *rest.Config, groupVersion, kind string, logger logr.Logger) (bool, error) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return false, err
	}
	return detectCRDInstalled(discoveryClient, groupVersion, kind, logger)
}

func checkCRDInstalled(disc serverGroupsAndResourcesIface, groupVersion, kind string, logger logr.Logger) bool {
	installed, err := detectCRDInstalled(disc, groupVersion, kind, logger)
	if err != nil {
		logger.Error(err, "failed to discover API resources",
			"groupVersion", groupVersion, "kind", kind)
	}
	return installed
}

func detectCRDInstalled(disc serverGroupsAndResourcesIface, groupVersion, kind string, logger logr.Logger) (bool, error) {
	_, apiLists, err := disc.ServerGroupsAndResources()
	if err != nil {
		// Partial errors are common (e.g. unavailable API services); continue if we got results.
		if apiLists == nil {
			return false, err
		}
		logger.V(1).Info("partial error discovering API resources (this is usually fine)", "error", err)
	}

	for _, apiList := range apiLists {
		if apiList.GroupVersion == groupVersion {
			for _, resource := range apiList.APIResources {
				if resource.Kind == kind {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// CheckKEDACRD reports whether the KEDA ScaledObject CRD is installed.
// TODO: checked once at startup; handle KEDA installed after controller starts.
func CheckKEDACRD(restConfig *rest.Config, logger logr.Logger) bool {
	return CheckCRDInstalled(restConfig, "keda.sh/v1alpha1", "ScaledObject", logger)
}

// CheckLeaderWorkerSetCRD reports whether the LeaderWorkerSet CRD is installed.
// TODO: checked once at startup; handle LWS installed after controller starts.
func CheckLeaderWorkerSetCRD(restConfig *rest.Config, logger logr.Logger) bool {
	return CheckCRDInstalled(restConfig, "leaderworkerset.x-k8s.io/v1", "LeaderWorkerSet", logger)
}
