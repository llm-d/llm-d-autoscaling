package crd

import (
	"errors"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeDiscovery implements serverGroupsAndResourcesIface for testing.
type fakeDiscovery struct {
	apiLists []*metav1.APIResourceList
	err      error
}

func (f *fakeDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return nil, f.apiLists, f.err
}

func apiList(groupVersion string, kinds ...string) *metav1.APIResourceList {
	resources := make([]metav1.APIResource, len(kinds))
	for i, k := range kinds {
		resources[i] = metav1.APIResource{Kind: k}
	}
	return &metav1.APIResourceList{GroupVersion: groupVersion, APIResources: resources}
}

func TestCheckCRDInstalled(t *testing.T) {
	log := logr.Discard()

	t.Run("CRD present", func(t *testing.T) {
		disc := &fakeDiscovery{
			apiLists: []*metav1.APIResourceList{
				apiList("keda.sh/v1alpha1", "ScaledObject", "ScaledJob"),
			},
		}
		if !checkCRDInstalled(disc, "keda.sh/v1alpha1", "ScaledObject", log) {
			t.Error("want true when CRD is in discovery results")
		}
	})

	t.Run("CRD present — LeaderWorkerSet group", func(t *testing.T) {
		disc := &fakeDiscovery{
			apiLists: []*metav1.APIResourceList{
				apiList("leaderworkerset.x-k8s.io/v1", "LeaderWorkerSet"),
			},
		}
		if !checkCRDInstalled(disc, "leaderworkerset.x-k8s.io/v1", "LeaderWorkerSet", log) {
			t.Error("want true for LeaderWorkerSet in its API group")
		}
	})

	t.Run("CRD absent — wrong group", func(t *testing.T) {
		disc := &fakeDiscovery{
			apiLists: []*metav1.APIResourceList{
				apiList("apps/v1", "Deployment"),
			},
		}
		if checkCRDInstalled(disc, "keda.sh/v1alpha1", "ScaledObject", log) {
			t.Error("want false when group is not in discovery results")
		}
	})

	t.Run("CRD absent — right group, wrong kind", func(t *testing.T) {
		disc := &fakeDiscovery{
			apiLists: []*metav1.APIResourceList{
				apiList("keda.sh/v1alpha1", "ScaledJob"),
			},
		}
		if checkCRDInstalled(disc, "keda.sh/v1alpha1", "ScaledObject", log) {
			t.Error("want false when kind is not in discovery results")
		}
	})

	t.Run("partial error with results — continues checking", func(t *testing.T) {
		disc := &fakeDiscovery{
			apiLists: []*metav1.APIResourceList{
				apiList("keda.sh/v1alpha1", "ScaledObject"),
			},
			err: errors.New("some api group unavailable"),
		}
		if !checkCRDInstalled(disc, "keda.sh/v1alpha1", "ScaledObject", log) {
			t.Error("want true when CRD is present despite partial error")
		}
	})

	t.Run("total failure — nil apiLists", func(t *testing.T) {
		disc := &fakeDiscovery{
			apiLists: nil,
			err:      errors.New("discovery completely failed"),
		}
		if checkCRDInstalled(disc, "keda.sh/v1alpha1", "ScaledObject", log) {
			t.Error("want false when discovery returns no results at all")
		}
	})

	t.Run("detect total failure returns error", func(t *testing.T) {
		disc := &fakeDiscovery{
			apiLists: nil,
			err:      errors.New("discovery completely failed"),
		}
		installed, err := detectCRDInstalled(disc, "keda.sh/v1alpha1", "ScaledObject", log)
		if err == nil {
			t.Fatal("want error when discovery returns no results at all")
		}
		if installed {
			t.Error("want false when discovery cannot determine CRD availability")
		}
	})
}
