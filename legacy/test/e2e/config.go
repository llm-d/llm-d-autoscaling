package e2e

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/testconfig"
)

// E2EConfig holds configuration for e2e tests loaded from environment variables.
// Common fields are inherited from testconfig.SharedConfig.
type E2EConfig struct {
	testconfig.SharedConfig

	// Feature gates
	// DeployWVA: env DEPLOY_WVA — require the WVA controller for this suite run.
	DeployWVA bool
	// ScaleToZeroEnabled: env SCALE_TO_ZERO_ENABLED — assume native HPA may use minReplicas=0
	// ("scale-to-zero" via HPAScaleToZero). Distinct from scale-from-zero (scale up from zero replicas).
	ScaleToZeroEnabled bool

	// Timeouts (seconds unless noted)
	PodReadyTimeout int // Wait for deployment/model pods ready
	ScaleUpTimeout  int // Long reconcile / scale-from-zero / job completion

	// Gomega Eventually timeouts and poll intervals (seconds)
	EventuallyShortSec      int // Quick checks (e.g. CRD list, delete verification)
	EventuallyMediumSec     int // Single-step medium waits (e.g. 60s)
	EventuallyStandardSec   int // Most status / reconcile waits (default 120s)
	EventuallyLongSec       int // Metrics / adapter stabilization (e.g. 180s)
	EventuallyExtendedSec   int // Long engine/HPA steps (e.g. 300s / 5m)
	PollIntervalSec         int // Default polling interval for Eventually
	PollIntervalQuickSec    int // Faster polling for short waits
	PollIntervalSlowSec     int // Slower polling for long-running conditions
	PollIntervalVerySlowSec int // e.g. job completion probes

}

// LoadConfigFromEnv reads e2e test configuration from environment variables.
func LoadConfigFromEnv() E2EConfig {
	cfg := E2EConfig{
		SharedConfig: testconfig.LoadSharedConfig(),

		DeployWVA:          testconfig.GetEnvBool("DEPLOY_WVA", true),
		ScaleToZeroEnabled: testconfig.GetEnvBool("SCALE_TO_ZERO_ENABLED", false),

		PodReadyTimeout: testconfig.GetEnvInt("POD_READY_TIMEOUT", 300), // 5 minutes
		ScaleUpTimeout:  testconfig.GetEnvInt("SCALE_UP_TIMEOUT", 600),  // 10 minutes

		EventuallyShortSec:      testconfig.GetEnvInt("E2E_EVENTUALLY_SHORT", 30),
		EventuallyMediumSec:     testconfig.GetEnvInt("E2E_EVENTUALLY_MEDIUM", 60),
		EventuallyStandardSec:   testconfig.GetEnvInt("E2E_EVENTUALLY_STANDARD", 120),
		EventuallyLongSec:       testconfig.GetEnvInt("E2E_EVENTUALLY_LONG", 180),
		EventuallyExtendedSec:   testconfig.GetEnvInt("E2E_EVENTUALLY_EXTENDED", 300),
		PollIntervalSec:         testconfig.GetEnvInt("E2E_EVENTUALLY_POLL", 5),
		PollIntervalQuickSec:    testconfig.GetEnvInt("E2E_EVENTUALLY_POLL_QUICK", 2),
		PollIntervalSlowSec:     testconfig.GetEnvInt("E2E_EVENTUALLY_POLL_SLOW", 10),
		PollIntervalVerySlowSec: testconfig.GetEnvInt("E2E_EVENTUALLY_POLL_VERY_SLOW", 15),
	}

	// OpenShift clusters typically don't have the HPAScaleToZero feature gate enabled, so native HPAs
	// cannot use minReplicas=0 ("scale-to-zero" on the HPA). Ignore SCALE_TO_ZERO_ENABLED there so e2e
	// does not assume that path (creation fails with: minReplicas must be >= 1).
	// Scale-from-zero (scaling workloads up from zero replicas) is separate; this block does not configure SCALER_BACKEND.
	if cfg.Environment == "openshift" && cfg.ScaleToZeroEnabled {
		cfg.ScaleToZeroEnabled = false
	}

	return cfg
}
