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

package indexers

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
)

func TestIndexers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Indexers Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO()) //nolint:fatcontext // shared across BeforeSuite/AfterSuite

	var err error
	err = kedav1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	// Only the KEDA ScaledObject CRD is needed; HPAs are a built-in type and
	// VariantAutoscaling is no longer a CRD.
	var crdPaths []string
	if dir := getKEDACRDDir(); dir != "" {
		crdPaths = append(crdPaths, dir)
	}
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     crdPaths,
		ErrorIfCRDPathMissing: true,
	}

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

// getKEDACRDDir returns the path to the KEDA CRD bases bundled with the
// version of github.com/kedacore/keda/v2 that this module pins. Resolved at
// test time via `go list -m` so the path tracks whatever go.mod says,
// without hardcoding a version. Returns "" when resolution fails or the
// directory does not exist; callers should skip appending the path in
// that case.
func getKEDACRDDir() string {
	moduleDir, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/kedacore/keda/v2").Output()
	if err == nil && strings.TrimSpace(string(moduleDir)) != "" {
		dir := filepath.Join(strings.TrimSpace(string(moduleDir)), "config", "crd", "bases")
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}
	// Fall back to GOMODCACHE when the module is in the download cache but not
	// checked out locally (go list -m {{.Dir}} returns empty in that case).
	version, err := exec.Command("go", "list", "-m", "-f", "{{.Version}}", "github.com/kedacore/keda/v2").Output()
	if err != nil || strings.TrimSpace(string(version)) == "" {
		return ""
	}
	cache, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		return ""
	}
	dir := filepath.Join(strings.TrimSpace(string(cache)),
		"github.com", "kedacore", "keda", "v2@"+strings.TrimSpace(string(version)),
		"config", "crd", "bases")
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return dir
}

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
// ENVTEST-based tests depend on specific binaries, usually located in paths set by
// controller-runtime. When running tests directly (e.g., via an IDE) without using
// Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to
// setting the 'KUBEBUILDER_ASSETS' environment variable. To ensure the binaries are
// properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
