package fixtures

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// Load-generation helpers build Kubernetes Jobs used by test/benchmark (parallel
// curl workers, burst batches, optional guidellm). The e2e suite does not call
// these APIs; e2e uses bounded, deterministic jobs (for example saturation
// trigger flows) instead.
//
// This file lives under test/e2e/fixtures next to shared cluster helpers
// (EnsureModelService, HPA, etc.) that benchmarks already import. A later
// refactor may move load-only code and the embedded script into a dedicated
// package such as test/loadgen for clearer ownership. The script lives in this
// directory (not fixtures/scripts/) so //go:embed matches reliably for vet/gopls.
// hack/burst_load_generator.sh is a symlink to this file for docs and CLI use.

//go:embed burst_load_generator.sh
var burstLoadGeneratorScript string

// Burst load configuration constants
const (
	burstBatchSize          = 10    // Number of requests to send in parallel per batch
	burstBatchSleepDuration = "0.5" // Sleep duration between batches (seconds)
	burstCurlTimeoutSeconds = 180   // Timeout for each curl request (seconds)
)

// LoadConfig holds configuration for load generation
type LoadConfig struct {
	Strategy     string // "synthetic" or "sharegpt"
	RequestRate  int    // Requests per second
	NumPrompts   int    // Total number of requests
	InputTokens  int    // Average input tokens (for synthetic)
	OutputTokens int    // Average output tokens (for synthetic)
	ModelID      string // Model ID for requests
}

// CreateLoadJob creates a Kubernetes Job that generates load against the model service
func CreateLoadJob(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, name, targetServiceURL string,
	loadCfg LoadConfig,
) error {
	// Use pre-built guidellm image
	image := "ghcr.io/vllm-project/guidellm:latest"

	// Build load generator arguments
	args := buildLoadGeneratorArgs(targetServiceURL, loadCfg)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-load",
			Namespace: namespace,
			Labels: map[string]string{
				"app":           name + "-load",
				"test-resource": defaultTestResourceLabelValue,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":           name + "-load",
						"test-resource": defaultTestResourceLabelValue,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            "load-gen",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent, // Use cached image for faster subsequent tests
							Command:         []string{"guidellm"},    // guidellm is the entrypoint
							Args:            args,
							Env: []corev1.EnvVar{
								{
									Name:  "HF_HOME",
									Value: "/tmp",
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		},
	}

	_, createErr := k8sClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
	return createErr
}

// buildLoadGeneratorArgs builds the argument list for the guidellm benchmark command
// Format matches hack/benchmark-jobs-template.yaml
func buildLoadGeneratorArgs(targetURL string, cfg LoadConfig) []string {
	// guidellm benchmark command format
	args := []string{
		"benchmark",
		"--target", targetURL,
		"--rate-type", "constant",
		"--rate", strconv.Itoa(cfg.RequestRate),
		"--model", cfg.ModelID,
	}

	// Calculate max-seconds based on num-prompts and rate
	// For constant rate: max-seconds should be enough to send all prompts
	// Add buffer to ensure all requests are sent
	maxSeconds := (cfg.NumPrompts / cfg.RequestRate) + 10 // Add 10s buffer
	args = append(args, "--max-seconds", strconv.Itoa(maxSeconds))

	switch cfg.Strategy {
	case "synthetic":
		// guidellm uses --data format: prompt_tokens=X,output_tokens=Y
		args = append(args,
			"--data", fmt.Sprintf("prompt_tokens=%d,output_tokens=%d", cfg.InputTokens, cfg.OutputTokens),
		)
	case "sharegpt":
		args = append(args,
			"--dataset", "sharegpt",
			"--dataset-path", "/datasets/ShareGPT_V3_unfiltered_cleaned_split.json",
		)
	}

	// Add output path (optional but useful for debugging)
	args = append(args, "--output-path", "/tmp/benchmarks.json")

	return args
}

// CreateBurstLoadJob creates a Kubernetes Job that generates burst load using curl. Fails if the job already exists.
func CreateBurstLoadJob(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, name, targetServiceURL string,
	loadCfg LoadConfig,
) error {
	job := buildBurstLoadJob(namespace, name, targetServiceURL, loadCfg)
	_, err := k8sClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
	return err
}

// DeleteBurstLoadJob deletes the burst load Job. Idempotent; ignores NotFound.
func DeleteBurstLoadJob(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name string) error {
	jobName := name + "-load"
	err := k8sClient.BatchV1().Jobs(namespace).Delete(ctx, jobName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete Job %s: %w", jobName, err)
	}
	return nil
}

// EnsureBurstLoadJob creates or replaces the burst load Job (idempotent for test setup).
func EnsureBurstLoadJob(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, name, targetServiceURL string,
	loadCfg LoadConfig,
) error {
	jobName := name + "-load"
	_, err := k8sClient.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("check existing Job %s: %w", jobName, err)
		}
	} else {
		deleteErr := k8sClient.BatchV1().Jobs(namespace).Delete(ctx, jobName, metav1.DeleteOptions{})
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return fmt.Errorf("delete existing Job %s: %w", jobName, deleteErr)
		}
		waitErr := wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			_, err := k8sClient.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
			return errors.IsNotFound(err), nil
		})
		if waitErr != nil {
			return fmt.Errorf("timeout waiting for Job %s deletion: %w", jobName, waitErr)
		}
	}
	job := buildBurstLoadJob(namespace, name, targetServiceURL, loadCfg)
	_, err = k8sClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
	return err
}

func buildBurstLoadJob(namespace, name, targetServiceURL string, loadCfg LoadConfig) *batchv1.Job {
	backoffLimit := int32(0)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-load",
			Namespace: namespace,
			Labels: map[string]string{
				"app":           name + "-load",
				"test-resource": defaultTestResourceLabelValue,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":           name + "-load",
						"test-resource": defaultTestResourceLabelValue,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					// Override ndots to avoid musl libc DNS resolution failures on hosts with
					// many corporate search domains and ndots:5 (e.g. IBM workstations).
					// The Alpine-based curl image uses musl which mishandles ndots>=5.
					DNSConfig: &corev1.PodDNSConfig{
						Options: []corev1.PodDNSConfigOption{
							{Name: "ndots", Value: ptr.To("2")},
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "load-generator",
							Image:   "quay.io/curl/curl:8.11.1",
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{burstLoadGeneratorScript},
							Env: []corev1.EnvVar{
								{
									Name:  "TOTAL_REQUESTS",
									Value: strconv.Itoa(loadCfg.NumPrompts),
								},
								{
									Name:  "BATCH_SIZE",
									Value: strconv.Itoa(burstBatchSize),
								},
								{
									Name:  "CURL_TIMEOUT",
									Value: strconv.Itoa(burstCurlTimeoutSeconds),
								},
								{
									Name:  "MAX_TOKENS",
									Value: strconv.Itoa(loadCfg.OutputTokens),
								},
								{
									Name:  "BATCH_SLEEP",
									Value: burstBatchSleepDuration,
								},
								{
									Name:  "MODEL_ID",
									Value: loadCfg.ModelID,
								},
								{
									Name:  "TARGET_URL",
									Value: targetServiceURL,
								},
								{
									Name:  "MAX_RETRIES",
									Value: "24",
								},
								{
									Name:  "RETRY_DELAY",
									Value: "5",
								},
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("2Gi"),
									corev1.ResourceCPU:    resource.MustParse("2"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		},
	}
	return job
}

// Parallel load configuration constants
const (
	parallelBatchSize          = 10    // Number of requests to send in parallel per batch
	parallelBatchSleepDuration = "0.5" // Sleep duration between batches (seconds)
	parallelCurlTimeoutSeconds = 180   // Timeout for each curl request (seconds)
)

// createParallelLoadJob creates a single Kubernetes Job that generates load using curl
// This is used by CreateParallelLoadJobs to create multiple parallel workers
func createParallelLoadJob(name, namespace, targetURL, experimentLabel string, workerID int, loadCfg LoadConfig) *batchv1.Job {
	backoffLimit := int32(0)

	script := fmt.Sprintf(`#!/bin/sh
# =============================================================================
# Load Generator Configuration (injected from Go constants)
# =============================================================================
WORKER_ID=%d
TOTAL_REQUESTS=%d
BATCH_SIZE=%d
CURL_TIMEOUT=%d
MAX_TOKENS=%d
BATCH_SLEEP=%s
MODEL_ID="%s"
TARGET_URL="%s"
MAX_RETRIES=24
RETRY_DELAY=5

# =============================================================================
# Script Start
# =============================================================================
echo "Load generator worker $WORKER_ID starting..."
echo "Sending $TOTAL_REQUESTS requests to $TARGET_URL"

# Wait for service to be ready
echo "Waiting for service to be ready..."
CONNECTED=false
for i in $(seq 1 $MAX_RETRIES); do
  if curl -s -o /dev/null -w "%%{http_code}" "$TARGET_URL" 2>/dev/null | grep -qE "^(200|404|405)"; then
    echo "Connection test passed on attempt $i"
    CONNECTED=true
    break
  fi
  echo "Attempt $i failed, retrying in ${RETRY_DELAY}s..."
  sleep $RETRY_DELAY
done

if [ "$CONNECTED" != "true" ]; then
  echo "ERROR: Cannot connect to service after $MAX_RETRIES attempts"
  exit 1
fi

# Send requests aggressively in parallel batches (ignore individual curl failures)
SENT=0
while [ $SENT -lt $TOTAL_REQUESTS ]; do
  for i in $(seq 1 $BATCH_SIZE); do
    if [ $SENT -ge $TOTAL_REQUESTS ]; then break; fi
    (curl -s -o /dev/null --max-time $CURL_TIMEOUT -X POST "$TARGET_URL" \
      -H "Content-Type: application/json" \
      -d "{\"model\":\"$MODEL_ID\",\"prompt\":\"Write a detailed explanation of machine learning algorithms.\",\"max_tokens\":$MAX_TOKENS}" || true) &
    SENT=$((SENT + 1))
  done
  echo "Worker $WORKER_ID: sent $SENT / $TOTAL_REQUESTS requests..."
  sleep $BATCH_SLEEP
done

# Wait for all to complete at the end
wait || true

echo "Worker $WORKER_ID: completed all $TOTAL_REQUESTS requests"
exit 0
`, workerID, loadCfg.NumPrompts, parallelBatchSize, parallelCurlTimeoutSeconds, loadCfg.OutputTokens, parallelBatchSleepDuration, loadCfg.ModelID, targetURL)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"experiment":    experimentLabel,
				"worker":        strconv.Itoa(workerID),
				"test-resource": defaultTestResourceLabelValue,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"experiment":    experimentLabel,
						"worker":        strconv.Itoa(workerID),
						"test-resource": defaultTestResourceLabelValue,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "load-generator",
							Image:   "quay.io/curl/curl:8.11.1",
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{script},
							Env: []corev1.EnvVar{
								{
									Name:  "WORKER_ID",
									Value: strconv.Itoa(workerID),
								},
								{
									Name:  "TOTAL_REQUESTS",
									Value: strconv.Itoa(loadCfg.NumPrompts),
								},
								{
									Name:  "BATCH_SIZE",
									Value: strconv.Itoa(parallelBatchSize),
								},
								{
									Name:  "CURL_TIMEOUT",
									Value: strconv.Itoa(parallelCurlTimeoutSeconds),
								},
								{
									Name:  "MAX_TOKENS",
									Value: strconv.Itoa(loadCfg.OutputTokens),
								},
								{
									Name:  "BATCH_SLEEP",
									Value: parallelBatchSleepDuration,
								},
								{
									Name:  "MODEL_ID",
									Value: loadCfg.ModelID,
								},
								{
									Name:  "TARGET_URL",
									Value: targetURL,
								},
								{
									Name:  "MAX_RETRIES",
									Value: "24",
								},
								{
									Name:  "RETRY_DELAY",
									Value: "5",
								},
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("2Gi"),
									corev1.ResourceCPU:    resource.MustParse("2"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}
}

// CreateParallelLoadJobs creates multiple parallel load generation jobs. Fails if any job already exists.
func CreateParallelLoadJobs(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	baseName, namespace, targetURL string,
	numWorkers int,
	loadCfg LoadConfig,
) error {
	for i := 1; i <= numWorkers; i++ {
		jobName := fmt.Sprintf("%s-%d", baseName, i)
		job := createParallelLoadJob(jobName, namespace, targetURL, baseName, i, loadCfg)
		_, err := k8sClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create job %s: %w", jobName, err)
		}
	}
	return nil
}

// EnsureParallelLoadJobs deletes existing parallel load jobs and creates them (idempotent for test setup).
func EnsureParallelLoadJobs(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	baseName, namespace, targetURL string,
	numWorkers int,
	loadCfg LoadConfig,
) error {
	DeleteParallelLoadJobs(ctx, k8sClient, baseName, namespace, numWorkers)
	return CreateParallelLoadJobs(ctx, k8sClient, baseName, namespace, targetURL, numWorkers, loadCfg)
}

// DeleteParallelLoadJobs deletes all parallel load generation jobs
func DeleteParallelLoadJobs(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	baseName, namespace string,
	numWorkers int,
) {
	propagationPolicy := metav1.DeletePropagationBackground
	for i := 1; i <= numWorkers; i++ {
		jobName := fmt.Sprintf("%s-%d", baseName, i)
		err := k8sClient.BatchV1().Jobs(namespace).Delete(ctx, jobName, metav1.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		if err != nil && !errors.IsNotFound(err) {
			// Log warning but don't fail - cleanup is best effort
			_ = err // Explicitly ignore error for best-effort cleanup
		}
	}
}
