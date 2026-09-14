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

package utils

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
	gink "github.com/onsi/ginkgo/v2"
	gom "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	promAPI "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	clusterName         = "kind-wva-gpu-cluster"
	prometheusHelmChart = "https://prometheus-community.github.io/helm-charts"
	monitoringNamespace = "workload-variant-autoscaler-monitoring"

	certmanagerVersion = "v1.16.3"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"
)

func warnError(err error) {
	_, _ = fmt.Fprintf(gink.GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(gink.GinkgoWriter, "chdir dir: %s\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(gink.GinkgoWriter, "running: %s\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s failed with error: (%v) %s", command, err, string(output))
	}

	return string(output), nil
}

// InstallPrometheusOperator installs the prometheus Operator to be used to export the enabled metrics.
// Includes TLS certificate generation and configuration for HTTPS support.
func InstallPrometheusOperator() error {
	cmd := exec.Command("kubectl", "create", "ns", monitoringNamespace)
	if _, err := Run(cmd); err != nil {
		return err
	}

	// Wait for namespace to be created and active
	cmd = exec.Command("kubectl", "wait", "--for=jsonpath={.status.phase}=Active", "namespace/"+monitoringNamespace, "--timeout=30s")
	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("failed to wait for namespace to be ready: %w", err)
	}

	// Generate TLS certificates for Prometheus
	if err := generateTLSCertificates(); err != nil {
		return fmt.Errorf("failed to generate TLS certificates: %w", err)
	}

	// Create Kubernetes secret for TLS certificates
	if err := createTLSCertificateSecret(); err != nil {
		return fmt.Errorf("failed to create TLS certificate secret: %w", err)
	}

	cmd = exec.Command("helm", "repo", "add", "prometheus-community", prometheusHelmChart)
	if _, err := Run(cmd); err != nil {
		return err
	}

	cmd = exec.Command("helm", "repo", "update")
	if _, err := Run(cmd); err != nil {
		return err
	}

	// Install Prometheus with TLS configuration
	cmd = exec.Command("helm", "upgrade", "-i", "kube-prometheus-stack", "prometheus-community/kube-prometheus-stack",
		"-n", monitoringNamespace,
		"-f", "deploy/prometheus-operator/prometheus-tls-values.yaml")
	if _, err := Run(cmd); err != nil {
		return err
	}
	return nil
}

// UninstallPrometheusOperator uninstalls the prometheus
func UninstallPrometheusOperator() {
	cmd := exec.Command("helm", "uninstall", "kube-prometheus-stack", "-n", monitoringNamespace)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}

	cmd = exec.Command("kubectl", "delete", "ns", monitoringNamespace)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// IsPrometheusCRDsInstalled checks if any Prometheus CRDs are installed
// by verifying the existence of key CRDs related to Prometheus.
func IsPrometheusCRDsInstalled() bool {
	// List of common Prometheus CRDs
	prometheusCRDs := []string{
		"prometheuses.monitoring.coreos.com",
		"prometheusrules.monitoring.coreos.com",
		"prometheusagents.monitoring.coreos.com",
	}

	cmd := exec.Command("kubectl", "get", "crds", "-o", "custom-columns=NAME:.metadata.name")
	output, err := Run(cmd)
	if err != nil {
		return false
	}
	crdList := GetNonEmptyLines(output)
	for _, crd := range prometheusCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// generateTLSCertificates generates self-signed TLS certificates for Prometheus
func generateTLSCertificates() error {
	// Create TLS certificates directory
	cmd := exec.Command("mkdir", "-p", "hack/tls-certs")
	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("failed to create TLS certs directory: %w", err)
	}

	// Check if certificate already exists and is valid
	certFile := "hack/tls-certs/prometheus-cert.pem"
	keyFile := "hack/tls-certs/prometheus-key.pem"

	// Check if certificate is still valid (not expired)
	cmd = exec.Command("openssl", "x509", "-checkend", "86400", "-noout", "-in", certFile)
	if err := cmd.Run(); err == nil {
		// Certificate exists and is valid
		return nil
	}

	// Generate self-signed certificate
	cmd = exec.Command("openssl", "req", "-x509", "-newkey", "rsa:4096",
		"-keyout", keyFile,
		"-out", certFile,
		"-days", "3650",
		"-nodes",
		"-subj", "/CN=prometheus",
		"-addext", "subjectAltName=DNS:kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local,DNS:kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc,DNS:prometheus,DNS:localhost,IP:127.0.0.1")

	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("failed to generate TLS certificate: %w", err)
	}
	return nil
}

// createTLSCertificateSecret creates a Kubernetes secret for TLS certificates
func createTLSCertificateSecret() error { //nolint:unparam // error return kept for interface consistency
	certFile := "hack/tls-certs/prometheus-cert.pem"
	keyFile := "hack/tls-certs/prometheus-key.pem"

	cmd := exec.Command("kubectl", "create", "secret", "tls", "prometheus-tls",
		"--cert="+certFile,
		"--key="+keyFile,
		"-n", monitoringNamespace)

	if _, err := Run(cmd); err != nil {
		// Secret might already exist, which is fine
		fmt.Println("TLS secret already exists or creation failed (this is usually OK)")
	}

	return nil
}

// UninstallCertManager uninstalls the cert manager
func UninstallCertManager() {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	_, _ = Run(cmd)
}

// InstallCertManager installs the cert manager bundle.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "apply", "-f", url)
	if _, err := Run(cmd); err != nil {
		return err
	}
	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)

	_, err := Run(cmd)
	return err
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster
func LoadImageToKindClusterWithName(name string, maxGPUs int) error {
	cluster, err := CheckIfClusterExistsOrCreate(maxGPUs, "mix")
	if err != nil {
		return err
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	cmd := exec.Command("kind", kindOptions...)
	_, err = Run(cmd)
	return err
}

func CheckIfClusterExistsOrCreate(maxGPUs int, gpuType string) (string, error) {
	// Check if the kind cluster exists
	existsCmd := exec.Command("kind", "get", "clusters")
	output, err := Run(existsCmd)
	if err != nil {
		return "", err
	}
	clusterExists := false
	clusters := GetNonEmptyLines(output)
	for _, c := range clusters {
		if c == clusterName {
			clusterExists = true
			break
		}
	}

	// Create the kind cluster if it doesn't exist
	expectedVersion := os.Getenv("K8S_EXPECTED_VERSION")
	if !clusterExists {
		scriptCmd := exec.Command("bash", "deploy/kind-emulator/setup.sh", "-g", strconv.Itoa(maxGPUs), "-t", gpuType, "K8S_VERSION="+expectedVersion)
		if _, err := Run(scriptCmd); err != nil {
			return "", fmt.Errorf("failed to create kind cluster: %v", err)
		}
	} else {
		checkKubernetesVersion(expectedVersion)
	}

	return clusterName, nil
}

// checkKubernetesVersion verifies that the cluster is running the expected Kubernetes version
func checkKubernetesVersion(expectedVersion string) {
	gink.By("checking Kubernetes cluster version")

	expectedVersionClean := strings.TrimPrefix(expectedVersion, "v")

	cmd := exec.Command("kubectl", "version")
	output, err := Run(cmd)
	if err != nil {
		gink.Fail(fmt.Sprintf("Failed to get Kubernetes version: %s\n", expectedVersion))
	}

	// Extract server version
	lines := strings.Split(output, "\n")
	var serverVersion string
	for _, line := range lines {
		if strings.HasPrefix(line, "Server Version: v") {
			serverVersion = strings.TrimPrefix(line, "Server Version: v")
			break
		}
	}

	// Parse expected version (e.g., "1.32.0" -> major=1, minor=32)
	expectedParts := strings.Split(expectedVersionClean, ".")

	expectedMajor, err := strconv.Atoi(expectedParts[0])
	if err != nil {
		gink.Fail(fmt.Sprintf("failed to parse expected major version: %v", err))
	}

	expectedMinor, err := strconv.Atoi(expectedParts[1])
	if err != nil {
		gink.Fail(fmt.Sprintf("failed to parse expected minor version: %v", err))
	}

	// Parse actual server version (e.g., "1.32.0" -> major=1, minor=32)
	serverParts := strings.Split(serverVersion, ".")

	serverMajor, err := strconv.Atoi(serverParts[0])
	if err != nil {
		gink.Fail(fmt.Sprintf("failed to parse server major version: %v", err))
	}

	serverMinor, err := strconv.Atoi(serverParts[1])
	if err != nil {
		gink.Fail(fmt.Sprintf("failed to parse server minor version: %v", err))
	}

	// Check if actual version is >= expected version
	if serverMajor < expectedMajor || (serverMajor == expectedMajor && serverMinor < expectedMinor) {
		gink.Fail(fmt.Sprintf("Kubernetes version v%s is below required minimum %s\n", serverVersion, expectedVersion))
	}
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, err
	}

	// Handle test package paths (e2e suite, benchmark suite, etc.)
	m := regexp.MustCompile(`/test/[^/]+$`)
	wd = m.ReplaceAllString(wd, "")
	return wd, nil
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %s to be uncomment", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		_, err := out.WriteString(strings.TrimPrefix(scanner.Text(), prefix))
		if err != nil {
			return err
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err := out.WriteString("\n"); err != nil {
			return err
		}
	}

	_, err = out.Write(content[idx+len(target):])
	if err != nil {
		return err
	}
	// false positive
	// nolint:gosec
	return os.WriteFile(filename, out.Bytes(), 0644)
}

// isPortAvailable checks if the specified port is available
func isPortAvailable(port int) (bool, error) {
	// Try to bind to the port to check if it's available
	listener, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
	if err != nil {
		return false, err // Port is already in use
	}
	if err := listener.Close(); err != nil {
		gom.Expect(err).NotTo(gom.HaveOccurred(), "Failed to close listener")
	}
	return true, nil // Port is available
}

// StartPortForwarding sets up port forwarding to a Service on the specified port
func startPortForwarding(service *corev1.Service, namespace string, localPort, servicePort int) *exec.Cmd {
	// Check if the port is already in use
	if available, err := isPortAvailable(localPort); !available {
		gink.Fail(fmt.Sprintf("Port %d is already in use. Cannot start port forwarding for service: %s. Error: %v", localPort, service.Name, err))
	}

	portForwardCmd := exec.Command("kubectl", "port-forward",
		"service/"+service.Name,
		fmt.Sprintf("%d:%d", localPort, servicePort), "-n", namespace)
	err := portForwardCmd.Start()
	gom.Expect(err).NotTo(gom.HaveOccurred(), "Port-forward command should start successfully for service: "+service.Name)

	// Check if the port-forward process is still running
	gom.Eventually(func() error {
		if portForwardCmd.ProcessState != nil && portForwardCmd.ProcessState.Exited() {
			return fmt.Errorf("port-forward process exited unexpectedly with code: %d", portForwardCmd.ProcessState.ExitCode())
		}
		return nil
	}, 10*time.Second, 1*time.Second).Should(gom.Succeed(), fmt.Sprintf("Port-forward to localPort %d should keep running for service: %s with servicePort %d", localPort, service.Name, servicePort))

	return portForwardCmd
}

// CreateLoadGeneratorJob creates and launches a Kubernetes Job for load generation using GuideLLM with the specified parameters
func CreateLoadGeneratorJob(namespace, targetURL, modelName string, rate, maxSeconds, inputTokens, outputTokens int, k8sClient *kubernetes.Clientset, ctx context.Context) (*batchv1.Job, error) {

	// Always use a standard python image and install guidellm at runtime
	// using python:3.11 and installing cpu-only torch (~200MB) to be lightweight and fast
	image := "registry.access.redhat.com/ubi9/python-311:9.7"

	cmd := fmt.Sprintf("echo 'Starting installation...' && pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu && pip install --no-cache-dir guidellm && echo 'Installation complete, starting benchmark...' && guidellm benchmark --target %s --rate-type constant --rate %d --max-seconds %d --model %s --data prompt_tokens=%d,output_tokens=%d --output-path /tmp/benchmarks.json",
		targetURL, rate, maxSeconds, modelName, inputTokens, outputTokens)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("guidellm-job-%d", rand.Intn(1000)),
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "guidellm-e2e-container",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Env: []corev1.EnvVar{
								{
									Name:  "HF_HOME",
									Value: "/tmp",
								},
							},
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{cmd},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
			BackoffLimit: ptr.To(int32(4)),
		},
	}

	// Create the Job
	_, err := k8sClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})

	if err != nil {
		return nil, fmt.Errorf("failed to create load generator Job: %v", err)
	}

	return job, nil
}

// WaitForJobPodRunning waits for the job's pod to be in Running phase with container started.
// This ensures the load generator has actually started (past the pip install phase).
func WaitForJobPodRunning(ctx context.Context, job *batchv1.Job, k8sClient *kubernetes.Clientset, timeout time.Duration, w io.Writer) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := k8sClient.CoreV1().Pods(job.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "job-name=" + job.Name,
		})
		if err != nil {
			_, _ = fmt.Fprintf(w, "Warning: failed to list pods for job %s: %v\n", job.Name, err)
			return false, nil
		}

		if len(pods.Items) == 0 {
			_, _ = fmt.Fprintf(w, "Waiting for job pod to be created...\n")
			return false, nil
		}

		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning {
				// Check if container is actually running (not just initializing)
				for _, cs := range pod.Status.ContainerStatuses {
					if cs.State.Running != nil {
						_, _ = fmt.Fprintf(w, "Load generator pod %s is running (started at %v)\n",
							pod.Name, cs.State.Running.StartedAt.Time)
						return true, nil
					}
				}
				_, _ = fmt.Fprintf(w, "Pod %s is Running but container not started yet...\n", pod.Name)
			} else {
				_, _ = fmt.Fprintf(w, "Pod %s phase: %s\n", pod.Name, pod.Status.Phase)
			}
		}

		return false, nil
	})
}

// WaitForLoadGeneratorReady waits for the load generator to be actively sending requests.
// It waits for the pod to be running and then an additional stabilization period
// to allow pip install and guidellm startup to complete.
func WaitForLoadGeneratorReady(ctx context.Context, job *batchv1.Job, k8sClient *kubernetes.Clientset, w io.Writer) error {
	// First wait for pod to be running
	if err := WaitForJobPodRunning(ctx, job, k8sClient, 5*time.Minute, w); err != nil {
		return fmt.Errorf("timeout waiting for load generator pod to start: %w", err)
	}

	// Wait additional time for pip install and guidellm startup
	// pip install torch + guidellm typically takes 2-3 minutes
	_, _ = fmt.Fprintf(w, "Waiting 3 minutes for pip install and guidellm startup...\n")
	time.Sleep(3 * time.Minute)

	_, _ = fmt.Fprintf(w, "Load generator should now be actively sending requests\n")
	return nil
}

// CalculateExpectedArrivalRate calculates the expected arrival rate (req/min) based on GuideLLM rate (req/sec), ITL/TTFT latencies, and the number of output tokens.
//
// With constant rate type, GuideLLM maintains a number of concurrent in-flight requests
// approximately equal to the target rate. The actual throughput is then determined by:
//
//	Throughput (req/s) = Concurrency / Request_Duration (s)
//
// Where:
//
//	Concurrency = target_rate (e.g., 5 concurrent requests for rate=5)
//	Request_Duration = TTFT + (ITL * output_tokens)
func CalculateExpectedArrivalRate(loadRateReqPerSec, ttftMs, itlMs, outputTokens int) float64 {
	// Calculate request duration in seconds
	requestDurationSec := float64(ttftMs+(itlMs*outputTokens)) / 1000.0

	// With constant rate type, concurrency is approximately equal to target rate
	concurrency := float64(loadRateReqPerSec)

	// Throughput = concurrency / request duration
	actualRatePerSec := concurrency / requestDurationSec

	// Convert to req/min
	return actualRatePerSec * 60.0
}

// StopJob deletes a Kubernetes Job and ensures it is removed from the cluster along with its Pods.
func StopJob(namespace string, job *batchv1.Job, k8sClient *kubernetes.Clientset, ctx context.Context) error {
	// Delete the Job with Foreground propagation to ensure pods are cleaned up first
	propagationPolicy := metav1.DeletePropagationForeground
	if err := k8sClient.BatchV1().Jobs(namespace).Delete(ctx, job.Name, metav1.DeleteOptions{
		PropagationPolicy: &propagationPolicy,
	}); err != nil {
		// If already deleted, consider it successful
		if client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete job: %w", err)
		}
	}

	// Wait for the Job to be fully deleted (including all pods)
	if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := k8sClient.BatchV1().Jobs(namespace).Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			// Job not found means it's deleted
			if client.IgnoreNotFound(err) == nil {
				return true, nil
			}
			// Other errors, keep waiting
			return false, nil
		}
		// Job still exists, keep waiting
		return false, nil
	}); err != nil {
		return fmt.Errorf("timed out waiting for job deletion: %w", err)
	}

	return nil
}

// StopCmd attempts to gracefully stop the provided command, handling early exits and timeouts.
func StopCmd(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("command or process is nil")
	}

	// Check if process has already exited
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		fmt.Printf("Warning: Process (PID %d) has already exited with code %d\n",
			cmd.Process.Pid, cmd.ProcessState.ExitCode())
		return nil
	}

	// Try graceful shutdown with SIGINT
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		// If we can't signal, the process might have already exited
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			fmt.Printf("Warning: Process (PID %d) exited before signal could be sent (exit code: %d)\n",
				cmd.Process.Pid, cmd.ProcessState.ExitCode())
			return nil
		}
		return fmt.Errorf("failed to send interrupt signal: %w", err)
	}

	// Wait for graceful shutdown with timeout
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		// Process exited, check if it was due to early termination
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				fmt.Printf("Warning: Process (PID %d) exited early with code %d\n",
					cmd.Process.Pid, exitErr.ExitCode())
				// Don't treat early exit as an error for cleanup purposes
				return nil
			}
			return fmt.Errorf("process exited with error: %w", err)
		}
		return nil
	case <-time.After(5 * time.Second):
		// Timeout - force kill
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill process: %w", err)
		}
		// Wait for the kill to complete
		<-done
		return nil
	}
}

// SetUpPortForward sets up port forwarding to a Service on the specified port
func SetUpPortForward(k8sClient *kubernetes.Clientset, ctx context.Context, serviceName, namespace string, localPort, servicePort int) *exec.Cmd {
	service, err := k8sClient.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	gom.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to fetch service")
	portForwardCmd := startPortForwarding(service, namespace, localPort, servicePort)
	return portForwardCmd
}

// VerifyPortForwardReadiness checks if the port forwarding is ready by sending HTTP requests to the specified local port
func VerifyPortForwardReadiness(ctx context.Context, localPort int, request string) error {
	var client *http.Client
	tr := &http.Transport{}
	// Skip TLS verification for HTTPS endpoints (e.g. Prometheus uses self-signed certs in tests).
	if strings.HasPrefix(request, "https://") {
		tr = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		client = &http.Client{Transport: tr, Timeout: 5 * time.Second}
		resp, err := client.Get(request)
		if err != nil {
			return false, nil // Retrying
		}
		defer func() {
			err := resp.Body.Close()
			gom.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to close response body")
		}()
		// Retry on 4xx and 5xx errors
		if resp.StatusCode >= 500 {
			fmt.Printf("Debug: Error - Returned status code: %d, retrying...\n", resp.StatusCode)
			return false, nil // Retry on client and server errors
		}

		return true, nil // Success
	})
	return err
}

// ValidateAppLabelUniqueness checks if the appLabel is already in use by other resources and fails if it's not unique
func ValidateAppLabelUniqueness(namespace, appLabel string, k8sClient *kubernetes.Clientset, crClient client.Client) {
	// Create a context with timeout to prevent hanging tests
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Check if any pods exist with the specified app label
	podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + appLabel,
	})
	if err != nil {
		gink.Fail(fmt.Sprintf("Failed to check existing pods for label uniqueness: %v", err))
	}

	// Check if any deployments exist with the specified app label
	deploymentList, err := k8sClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + appLabel,
	})
	if err != nil {
		gink.Fail(fmt.Sprintf("Failed to check existing deployments for label uniqueness: %v", err))
	}

	// Check if any services exist with the specified app label
	serviceList, err := k8sClient.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + appLabel,
	})
	if err != nil {
		gink.Fail(fmt.Sprintf("Failed to check existing services for label uniqueness: %v", err))
	}

	// Check if any ServiceMonitors exist with the specified app label
	serviceMonitorList := &promoperator.ServiceMonitorList{}
	err = crClient.List(ctx, serviceMonitorList, client.InNamespace(namespace), client.MatchingLabels{"app": appLabel})
	if err != nil {
		gink.Fail(fmt.Sprintf("Failed to check existing ServiceMonitors for label uniqueness: %v", err))
	}

	// Collects conflicting resources to show in error logs
	var conflicting []string

	if len(podList.Items) > 0 {
		for _, pod := range podList.Items {
			conflicting = append(conflicting, "Pod: "+pod.Name)
		}
	}

	if len(deploymentList.Items) > 0 {
		for _, deployment := range deploymentList.Items {
			conflicting = append(conflicting, "Deployment: "+deployment.Name)
		}
	}

	if len(serviceList.Items) > 0 {
		for _, service := range serviceList.Items {
			conflicting = append(conflicting, "Service: "+service.Name)
		}
	}

	if len(serviceMonitorList.Items) > 0 {
		for _, serviceMonitor := range serviceMonitorList.Items {
			conflicting = append(conflicting, "ServiceMonitor: "+serviceMonitor.Name)
		}
	}

	// Fails if any conflicts are found
	if len(conflicting) > 0 {
		gink.Fail(fmt.Sprintf("App label '%s' is not unique in namespace '%s'. Make sure to delete conflicting resources: %s",
			appLabel, namespace, strings.Join(conflicting, ", ")))
	}
}

// creates a VariantAutoscaling resource with owner reference to deployment
func CreateVariantAutoscalingResource(namespace, resourceName, scaleTargetRefName, modelId, acc string, variantCost float64) *variant.VariantAutoscaling {
	return &variant.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: namespace,
			Labels: map[string]string{
				"inference.optimization/acceleratorName": acc,
			},
		},
		Spec: variant.VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       scaleTargetRefName,
			},
			ModelID: modelId,
			VariantAutoscalingConfigSpec: variant.VariantAutoscalingConfigSpec{
				VariantCost: fmt.Sprintf("%.1f", variantCost),
			},
		},
	}
}

// PrometheusQueryResult represents the response from Prometheus API
type PrometheusQueryResult struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// PrometheusServiceInfo contains information about the Prometheus service endpoint
type PrometheusServiceInfo struct {
	ServiceName string
	Namespace   string
	Port        int
	Scheme      string // "http" or "https"
}

// PrometheusClient wraps the official Prometheus client
type PrometheusClient struct {
	client promv1.API
}

// API returns the underlying Prometheus v1 API for advanced queries (e.g., QueryRange).
func (p *PrometheusClient) API() promv1.API {
	return p.client
}

type authRoundTripper struct {
	token string
	rt    http.RoundTripper
}

func (a *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+a.token)
	return a.rt.RoundTrip(req)
}

// GetPrometheusServiceInfoFromConfigMap reads the Prometheus service info from the WVA controller configmap.
// It reads the wva-manager-config configmap and extracts the service name, namespace, and port
// from PROMETHEUS_BASE_URL.
// Example URL: "https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local:9090"
func GetPrometheusServiceInfoFromConfigMap(ctx context.Context, k8sClient *kubernetes.Clientset, wvaNamespace string) (*PrometheusServiceInfo, error) {
	configMapName := "wva-manager-config"

	cm, err := k8sClient.CoreV1().ConfigMaps(wvaNamespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get configmap %s/%s: %w", wvaNamespace, configMapName, err)
	}

	configYAML, ok := cm.Data["config.yaml"]
	if !ok {
		return nil, fmt.Errorf("config.yaml not found in configmap %s/%s", wvaNamespace, configMapName)
	}

	// Parse PROMETHEUS_BASE_URL from the YAML content
	// Format: PROMETHEUS_BASE_URL: "https://service-name.namespace.svc.cluster.local:port"
	re := regexp.MustCompile(`PROMETHEUS_BASE_URL:\s*"(https?://)?([^:/"]+)\.([^:/"]+)\.svc\.cluster\.local:(\d+)"`)
	matches := re.FindStringSubmatch(configYAML)
	if len(matches) < 5 {
		return nil, fmt.Errorf("PROMETHEUS_BASE_URL not found or invalid format in configmap %s/%s config.yaml", wvaNamespace, configMapName)
	}

	scheme := "http"
	if matches[1] == "https://" {
		scheme = "https"
	}

	port, err := strconv.Atoi(matches[4])
	if err != nil {
		return nil, fmt.Errorf("invalid port in PROMETHEUS_BASE_URL: %w", err)
	}

	return &PrometheusServiceInfo{
		ServiceName: matches[2],
		Namespace:   matches[3],
		Port:        port,
		Scheme:      scheme,
	}, nil
}

// NewPrometheusClient creates a new Prometheus client for e2e tests
func NewPrometheusClient(baseURL string, insecureSkipVerify bool) (*PrometheusClient, error) {
	config := promAPI.Config{
		Address: baseURL,
	}

	roundTripper := promAPI.DefaultRoundTripper
	if rt, ok := roundTripper.(*http.Transport); ok {
		if insecureSkipVerify {
			rt.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
	}

	if token := os.Getenv("PROMETHEUS_TOKEN"); token != "" {
		roundTripper = &authRoundTripper{
			token: token,
			rt:    roundTripper,
		}
	}
	config.RoundTripper = roundTripper

	client, err := promAPI.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create prometheus client: %w", err)
	}

	return &PrometheusClient{
		client: promv1.NewAPI(client),
	}, nil
}

// QueryWithRetry queries Prometheus API with retries and returns the metric value
func (p *PrometheusClient) QueryWithRetry(ctx context.Context, query string) (float64, error) {
	var result float64

	// Define the backoff strategy
	backoff := wait.Backoff{
		Duration: 100 * time.Millisecond, // Initial delay
		Factor:   2.0,                    // Exponential factor
		Jitter:   0.25,                   // 25% jitter
		Steps:    5,                      // Max 5 attempts
		Cap:      5 * time.Second,        // Max delay cap
	}

	// Use wait.ExponentialBackoff for retries
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		value, queryErr := p.executeQuery(ctx, query)
		if queryErr == nil {
			result = value
			return true, nil // Success, stop retrying
		}

		// Check if this is a permanent error (don't retry)
		if isPermanentPrometheusError(queryErr) {
			return false, queryErr // Stop retrying, return error
		}

		return false, nil // Continue retrying
	})

	if err != nil {
		return 0, err
	}

	return result, nil
}

// executeQuery performs a single query attempt using the official Prometheus API
func (p *PrometheusClient) executeQuery(ctx context.Context, query string) (float64, error) {
	result, warnings, err := p.client.Query(ctx, query, time.Now())
	if err != nil {
		return 0, fmt.Errorf("prometheus query failed: %w", err)
	}

	// Log any warnings from Prometheus
	if len(warnings) > 0 {
		fmt.Printf("Debug: Prometheus warnings: %v\n", warnings)
	}

	return extractValueFromResult(result)
}

// extractValueFromResult extracts float64 value from Prometheus query result
func extractValueFromResult(result model.Value) (float64, error) {
	switch v := result.(type) {
	case model.Vector:
		if len(v) == 0 {
			return 0, errors.New("no data returned from prometheus query")
		}
		return float64(v[0].Value), nil
	case *model.Scalar:
		return float64(v.Value), nil
	default:
		return 0, fmt.Errorf("unexpected result type: %T", result)
	}
}

// isPermanentPrometheusError determines if a Prometheus error should not be retried
func isPermanentPrometheusError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()

	// Permanent errors that shouldn't be retried
	permanentErrors := []string{
		"bad_data",          // Invalid query syntax
		"invalid parameter", // Bad parameters
		"parse error",       // Query parsing failed
		"unauthorized",      // Auth issues
		"forbidden",         // Permission issues
	}

	for _, permErr := range permanentErrors {
		if strings.Contains(strings.ToLower(errStr), permErr) {
			return true
		}
	}

	return false
}

// GetWVAReplicaMetrics queries Prometheus for metrics emitted by the WVA autoscaler
func GetWVAReplicaMetrics(variantName, namespace, acceleratorType string) (currentReplicas, desiredReplicas, desiredRatio float64, err error) {

	client, err := NewPrometheusClient("https://localhost:9090", true)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to create prometheus client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	labels := fmt.Sprintf(`variant_name="%s",exported_namespace="%s",accelerator_type="%s"`, variantName, namespace, acceleratorType)

	// Query both metrics with retries
	currentQuery := fmt.Sprintf(`%s{%s}`, constants.WVACurrentReplicas, labels)
	currentReplicas, err = client.QueryWithRetry(ctx, currentQuery)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to query current replicas: %w", err)
	}

	desiredQuery := fmt.Sprintf(`%s{%s}`, constants.WVADesiredReplicas, labels)
	desiredReplicas, err = client.QueryWithRetry(ctx, desiredQuery)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to query desired replicas: %w", err)
	}

	desiredRatioQuery := fmt.Sprintf(`%s{%s}`, constants.WVADesiredRatio, labels)
	desiredRatio, err = client.QueryWithRetry(ctx, desiredRatioQuery)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to query desired ratio: %w", err)
	}

	return currentReplicas, desiredReplicas, desiredRatio, nil
}

// DefaultPrometheusURL is the default Prometheus endpoint for E2E tests.
// Environment variables:
//   - PROMETHEUS_URL: Override the Prometheus endpoint (default: https://localhost:9090)
//   - PROMETHEUS_SKIP_TLS_VERIFY: Set to "false" to enable TLS cert verification (default: true)
const DefaultPrometheusURL = "https://localhost:9090"

// DefaultPrometheusQueryTimeout is the default timeout for Prometheus queries.
// This can be adjusted for environments with slower network or larger result sets.
const DefaultPrometheusQueryTimeout = 30 * time.Second

// namespaceRegex validates Kubernetes namespace names (RFC 1123 DNS label)
var namespaceRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// validateNamespace checks if a namespace name is valid to prevent PromQL injection.
func validateNamespace(namespace string) error {
	if namespace == "" {
		return errors.New("namespace cannot be empty")
	}
	if len(namespace) > 63 {
		return fmt.Errorf("namespace too long: %d characters (max 63)", len(namespace))
	}
	if !namespaceRegex.MatchString(namespace) {
		return errors.New("invalid namespace name: must be lowercase alphanumeric with optional dashes")
	}
	return nil
}

// GetQueueMetrics queries Prometheus for vLLM queue length metrics for pods in a namespace.
// Returns a map of pod name to queue length, and total queue length across all pods.
func GetQueueMetrics(namespace string) (podQueues map[string]float64, totalQueue float64, err error) {
	// Validate namespace to prevent PromQL injection
	if err := validateNamespace(namespace); err != nil {
		return nil, 0, fmt.Errorf("invalid namespace: %w", err)
	}

	prometheusURL := os.Getenv("PROMETHEUS_URL")
	if prometheusURL == "" {
		prometheusURL = DefaultPrometheusURL
	}

	// Allow TLS verification to be configured via environment variable.
	// WARNING: insecureSkipVerify=true disables TLS certificate verification and is intended
	// only for E2E tests with self-signed certificates. Do not use in production.
	// Set PROMETHEUS_SKIP_TLS_VERIFY=false to enable certificate verification.
	insecureSkipVerify := true // Default for E2E tests with self-signed certs
	if skipVerify := os.Getenv("PROMETHEUS_SKIP_TLS_VERIFY"); skipVerify != "" {
		insecureSkipVerify = strings.EqualFold(skipVerify, "true")
	}

	client, err := NewPrometheusClient(prometheusURL, insecureSkipVerify)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create prometheus client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), DefaultPrometheusQueryTimeout)
	defer cancel()

	// Query queue length per pod
	// Note: PromQL doesn't support parameterized queries. This is safe because:
	// 1. constants.VLLMNumRequestsWaiting is a compile-time constant we control
	// 2. namespace is validated above against RFC 1123 DNS label format (alphanumeric + dashes only)
	query := fmt.Sprintf(`%s{namespace="%s"}`, constants.VLLMNumRequestsWaiting, namespace)

	result, warnings, err := client.client.Query(ctx, query, time.Now())
	if err != nil {
		return nil, 0, fmt.Errorf("failed to query queue metrics: %w", err)
	}
	if len(warnings) > 0 {
		fmt.Printf("Prometheus warnings: %v\n", warnings)
	}

	podQueues = make(map[string]float64)

	if result.Type() == model.ValVector {
		vector, ok := result.(model.Vector)
		if !ok {
			return nil, 0, fmt.Errorf("unexpected Prometheus result type: %T", result)
		}
		for _, sample := range vector {
			podName := string(sample.Metric["pod"])
			queueLen := float64(sample.Value)
			podQueues[podName] = queueLen
			totalQueue += queueLen
		}
	}

	return podQueues, totalQueue, nil
}

// setEnvIfNotSet sets an environment variable only if it's not already set.
// This allows CI to override defaults by setting env vars before running tests.
func setEnvIfNotSet(key, value string) {
	if os.Getenv(key) == "" {
		gom.Expect(os.Setenv(key, value)).To(gom.Succeed())
	}
}

// setupEnvironment sets up necessary environment variables for the E2E tests
func SetupTestEnvironment(image string, numNodes, gpusPerNode int, gpuTypes string) {
	// Set default environment variables for Kind cluster creation
	// Note: These use setEnvIfNotSet to allow CI workflow to override with different values
	gom.Expect(os.Setenv("IMG", image)).To(gom.Succeed())
	gom.Expect(os.Setenv("CLUSTER_NAME", clusterName)).To(gom.Succeed())
	setEnvIfNotSet("CLUSTER_NODES", strconv.Itoa(numNodes))
	setEnvIfNotSet("CLUSTER_GPUS", strconv.Itoa(gpusPerNode))
	setEnvIfNotSet("CLUSTER_GPU_TYPE", gpuTypes)                                     // Use CLUSTER_GPU_TYPE to match Makefile
	gom.Expect(os.Setenv("WVA_IMAGE_PULL_POLICY", "IfNotPresent")).To(gom.Succeed()) // The image is built locally by the tests
	gom.Expect(os.Setenv("CREATE_CLUSTER", "true")).To(gom.Succeed())                // Always create a new cluster for E2E tests

	// EPP is deployed by ./deploy/install-epp.sh (via deploy-e2e-infra Makefile target); install.sh deploys WVA only.
	// Tests apply their own VariantAutoscaling / ScaledObject / model workloads.
	gom.Expect(os.Setenv("DEPLOY_WVA", "true")).To(gom.Succeed())
	gom.Expect(os.Setenv("DEPLOY_PROMETHEUS", "true")).To(gom.Succeed())
	gom.Expect(os.Setenv("WVA_RECONCILE_INTERVAL", "30s")).To(gom.Succeed())

	// Tests deploy their own workloads — skip any model server pre-deploy.
}
