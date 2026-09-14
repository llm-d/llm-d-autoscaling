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
	"os"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

type configMapReconcilerTestSetup struct {
	ctx             context.Context
	cfg             *config.Config
	ds              datastore.Datastore
	reconciler      *ConfigMapReconciler
	systemNamespace string
	testNamespace   string
}

func setupConfigMapReconcilerTest(systemNamespace, testNamespace string) configMapReconcilerTestSetup {
	ctx := context.Background()
	logging.NewTestLogger()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: systemNamespace,
		},
	}
	Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

	if testNamespace != "" {
		testNS := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: testNamespace,
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, testNS))).NotTo(HaveOccurred())
	}

	ds := datastore.NewDatastore(config.NewTestConfig())

	cfg, err := newTestConfigWithPrometheus("https://prometheus:9090")
	Expect(err).NotTo(HaveOccurred())

	recorder := record.NewFakeRecorder(100)
	reconciler := &ConfigMapReconciler{
		Reader:    k8sClient,
		Scheme:    runtime.NewScheme(),
		Config:    cfg,
		Datastore: ds,
		Recorder:  recorder,
	}

	return configMapReconcilerTestSetup{
		ctx:             ctx,
		cfg:             cfg,
		ds:              ds,
		reconciler:      reconciler,
		systemNamespace: systemNamespace,
		testNamespace:   testNamespace,
	}
}

func newTestConfigWithPrometheus(prometheusURL string) (*config.Config, error) {
	// Set environment variable for Prometheus URL
	_ = os.Setenv("PROMETHEUS_BASE_URL", prometheusURL)
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	cfg, err := config.Load(nil, "")
	if err != nil {
		return nil, fmt.Errorf("failed to create test config: %w", err)
	}
	return cfg, nil
}

// failingK8sClient wraps a real client but fails on specific operations
type failingK8sClient struct {
	client.Reader
	failGet   bool
	failList  bool
	getError  error
	listError error
}

func (f *failingK8sClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if f.failGet && f.getError != nil {
		return f.getError
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}

func (f *failingK8sClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if f.failList && f.listError != nil {
		return f.listError
	}
	return f.Reader.List(ctx, list, opts...)
}
