package scalefromzero

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	appsV1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
	utiltest "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	poolreconciler "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	vav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
	unittestutil "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// stubSource is a MetricsSource that returns a canned Refresh result, so tests
// can drive the engine through collection outcomes that are impractical to
// reproduce against a live scraper.
type stubSource struct {
	results map[string]*source.MetricResult
}

func (s *stubSource) QueryList() *source.QueryList { return nil }

func (s *stubSource) Refresh(context.Context, source.RefreshSpec) (map[string]*source.MetricResult, error) {
	return s.results, nil
}

func (s *stubSource) Get(string, map[string]string) *source.CachedValue { return nil }

// stubSourceDatastore serves a fixed metrics source for every pool while
// delegating all other datastore behaviour to the embedded implementation.
type stubSourceDatastore struct {
	datastore.Datastore
	src source.MetricsSource
}

func (d stubSourceDatastore) PoolGetMetricsSource(string) source.MetricsSource { return d.src }

// TestPendingRequestsUnknownIsNotIdle asserts that a variant is only treated as
// idle when the EPP scrape actually succeeded. A failed collection reports an
// empty metric set, which is byte-identical to an idle pool; concluding "no
// pending requests" from it leaves a variant with queued work stuck at zero
// replicas and logs nothing above DEBUG.
func TestPendingRequestsUnknownIsNotIdle(t *testing.T) {
	tests := []struct {
		name    string
		results map[string]*source.MetricResult
		wantErr bool
	}{
		{
			name: "collection failed: error must surface, not read as idle",
			results: map[string]*source.MetricResult{
				"all_metrics": {
					QueryName: "all_metrics",
					Values:    []source.MetricValue{},
					Error:     errors.New("failed to scrape all 3 ready pod(s)"),
				},
			},
			wantErr: true,
		},
		{
			name:    "result absent entirely: queue depth is unknown",
			results: map[string]*source.MetricResult{},
			wantErr: true,
		},
		{
			name: "collection succeeded and pool is genuinely idle",
			results: map[string]*source.MetricResult{
				"all_metrics": {
					QueryName: "all_metrics",
					Values:    []source.MetricValue{},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gvk := schema.GroupVersionKind{
				Group:   v1.GroupVersion.Group,
				Version: v1.GroupVersion.Version,
				Kind:    "InferencePool",
			}
			pool := utiltest.MakeInferencePool("pool1").
				Namespace(namespace).
				Selector(selector_v1).
				TargetPorts(8080).
				EndpointPickerRef("epp-pool1-svc").ObjRef()
			pool.SetGroupVersionKind(gvk)

			// Variants are discovered from annotated HPAs targeting each Deployment.
			hpa := managedHPA(namespace, resourceName, deploymentName, modelId)
			dp := unittestutil.MakeDeployment(deploymentName, namespace, 0, selector_v1)
			svc := unittestutil.MakeService("epp-pool1-svc", namespace)

			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = v1alpha2.Install(scheme)
			_ = v1.Install(scheme)
			_ = vav1alpha1.AddToScheme(scheme)
			_ = appsV1.AddToScheme(scheme)
			_ = corev1.AddToScheme(scheme)

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects([]client.Object{pool, dp, hpa, svc}...).
				Build()
			fakeDynamicClient := dynamicfake.NewSimpleDynamicClient(scheme, dp)

			namespacedName := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}
			gknn := common.GKNN{
				NamespacedName: namespacedName,
				GroupKind: schema.GroupKind{
					Group: pool.GroupVersionKind().Group,
					Kind:  pool.GroupVersionKind().Kind,
				},
			}
			ctx := context.Background()

			ds := datastore.NewDatastore(nil)
			reconciler := &poolreconciler.InferencePoolReconciler{Client: fakeClient, Datastore: ds, PoolGKNN: gknn}
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName})
			require.NoError(t, err)

			engine := &Engine{
				client:         fakeClient,
				recorder:       record.NewFakeRecorder(100),
				Datastore:      stubSourceDatastore{Datastore: ds, src: &stubSource{results: tt.results}},
				DynamicClient:  fakeDynamicClient,
				Mapper:         testrestmapper.TestOnlyStaticRESTMapper(scheme, schema.GroupVersion{Group: "apps", Version: "v1"}),
				maxConcurrency: 30,
			}

			err = engine.optimize(ctx)
			if tt.wantErr {
				require.Error(t, err, "a failed or missing collection must not be reported as an idle pool")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
