package config

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
)

// TestThroughputAnalyzerName_MatchesCanonical guards the throughput analyzer
// name that config.go duplicates as a literal (throughputAnalyzerName) against
// the canonical throughput.AnalyzerName. The literal is duplicated to keep
// internal/config from depending on the analyzers layer; this test restores the
// coupling at test time so a future rename of throughput.AnalyzerName fails here
// instead of silently making ThroughputAnalyzerEnabled always return false.
func TestThroughputAnalyzerName_MatchesCanonical(t *testing.T) {
	if throughputAnalyzerName != throughput.AnalyzerName {
		t.Fatalf("throughputAnalyzerName = %q, want %q (canonical throughput.AnalyzerName); the duplicated literal has drifted",
			throughputAnalyzerName, throughput.AnalyzerName)
	}
}

// TestConfig_ThreadSafeUpdates tests that concurrent reads and writes to DynamicConfig
// are thread-safe and don't cause race conditions or data corruption.
func TestConfig_ThreadSafeUpdates(t *testing.T) {
	cfg := NewTestConfig()

	const (
		numReaders = 10
		numWriters = 5
		iterations = 100
	)

	var (
		readErrors  int64
		writeErrors int64
		wg          sync.WaitGroup
	)

	// Spawn reader goroutines that continuously read from DynamicConfig
	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				// Read various dynamic config values
				interval := cfg.OptimizationInterval()
				if interval <= 0 {
					atomic.AddInt64(&readErrors, 1)
					t.Logf("Reader %d: Invalid interval at iteration %d: %v", readerID, j, interval)
					continue
				}

				satConfig := cfg.SaturationConfig()
				if satConfig == nil {
					atomic.AddInt64(&readErrors, 1)
					t.Logf("Reader %d: Nil saturation config at iteration %d", readerID, j)
					continue
				}

				scaleToZeroConfig := cfg.ScaleToZeroConfig()
				if scaleToZeroConfig == nil {
					atomic.AddInt64(&readErrors, 1)
					t.Logf("Reader %d: Nil scale-to-zero config at iteration %d", readerID, j)
					continue
				}

				// Small sleep to increase chance of concurrent access
				time.Sleep(time.Microsecond)
			}
		}(i)
	}

	// Spawn writer goroutines that continuously update DynamicConfig
	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func(_ int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {

				// Update saturation config
				newSatConfig := make(map[string]SaturationScalingConfig)
				newSatConfig["test-accelerator"] = SaturationScalingConfig{
					KvCacheThreshold:     0.8,
					QueueLengthThreshold: 5,
					KvSpareTrigger:       0.1,
					QueueSpareTrigger:    3,
				}
				cfg.UpdateSaturationConfig(newSatConfig)

				// Update scale-to-zero config
				newScaleToZeroConfig := make(ScaleToZeroConfigData)
				enabled := true
				newScaleToZeroConfig["model1"] = ModelScaleToZeroConfig{
					ModelID:           "model1",
					EnableScaleToZero: &enabled,
					RetentionPeriod:   "5m",
				}
				cfg.UpdateScaleToZeroConfig(newScaleToZeroConfig)

				// Small sleep to increase chance of concurrent access
				time.Sleep(time.Microsecond)
			}
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Verify no errors occurred
	assert.Equal(t, int64(0), readErrors, "No read errors should occur in thread-safe access")
	assert.Equal(t, int64(0), writeErrors, "No write errors should occur in thread-safe updates")

	// Verify final state is consistent
	finalInterval := cfg.OptimizationInterval()
	assert.Greater(t, finalInterval, time.Duration(0), "Final interval should be positive")

	finalSatConfig := cfg.SaturationConfig()
	assert.NotNil(t, finalSatConfig, "Final saturation config should not be nil")

	finalScaleToZeroConfig := cfg.ScaleToZeroConfig()
	assert.NotNil(t, finalScaleToZeroConfig, "Final scale-to-zero config should not be nil")
}

// TestConfig_ThreadSafeConcurrentReads tests that multiple concurrent reads don't block each other.
func TestConfig_ThreadSafeConcurrentReads(t *testing.T) {
	cfg := NewTestConfig()

	const numReaders = 50
	var wg sync.WaitGroup
	start := make(chan struct{})

	// Spawn many reader goroutines
	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // Wait for signal to start
			// All readers should be able to read concurrently (RWMutex allows multiple readers)
			interval := cfg.OptimizationInterval()
			satConfig := cfg.SaturationConfig()
			scaleToZeroConfig := cfg.ScaleToZeroConfig()
			_ = interval
			_ = satConfig
			_ = scaleToZeroConfig
		}()
	}

	// Start all readers at once
	close(start)

	// Measure time - should be fast since RWMutex allows concurrent reads
	startTime := time.Now()
	wg.Wait()
	duration := time.Since(startTime)

	// Concurrent reads should complete quickly (much faster than sequential)
	// If reads were blocking each other, this would take much longer
	assert.Less(t, duration, 100*time.Millisecond, "Concurrent reads should complete quickly")
}

// TestDetectImmutableParameterChanges tests that attempts to change immutable parameters
// are correctly detected.
func TestDetectImmutableParameterChanges(t *testing.T) {
	// Create initial config with Prometheus URL
	initialConfig := NewTestConfig()
	initialConfig.setPrometheusBaseURLForTesting("https://prometheus-initial:9090")

	tests := []struct {
		name        string
		configMap   map[string]string
		expectError bool
		errorMsg    string
	}{
		{
			name: "No immutable parameter change",
			configMap: map[string]string{
				"GLOBAL_OPT_INTERVAL": "120s",
			},
			expectError: false,
		},
		{
			name: "Attempt to change PROMETHEUS_BASE_URL",
			configMap: map[string]string{
				"PROMETHEUS_BASE_URL": "https://prometheus-new:9090",
			},
			expectError: true,
			errorMsg:    "PROMETHEUS_BASE_URL",
		},
		{
			name: "Attempt to change METRICS_BIND_ADDRESS",
			configMap: map[string]string{
				"METRICS_BIND_ADDRESS": ":9443",
			},
			expectError: true,
			errorMsg:    "METRICS_BIND_ADDRESS",
		},
		{
			name: "Attempt to change HEALTH_PROBE_BIND_ADDRESS",
			configMap: map[string]string{
				"HEALTH_PROBE_BIND_ADDRESS": ":8082",
			},
			expectError: true,
			errorMsg:    "HEALTH_PROBE_BIND_ADDRESS",
		},
		{
			name: "Attempt to change LEADER_ELECTION_ID",
			configMap: map[string]string{
				"LEADER_ELECTION_ID": "new-election-id",
			},
			expectError: true,
			errorMsg:    "LEADER_ELECTION_ID",
		},
		{
			name: "Multiple immutable parameter changes",
			configMap: map[string]string{
				"PROMETHEUS_BASE_URL":  "https://prometheus-new:9090",
				"METRICS_BIND_ADDRESS": ":9443",
			},
			expectError: true,
			errorMsg:    "PROMETHEUS_BASE_URL",
		},
		{
			name: "Mixed mutable and immutable changes",
			configMap: map[string]string{
				"GLOBAL_OPT_INTERVAL": "120s",
				"PROMETHEUS_BASE_URL": "https://prometheus-new:9090",
			},
			expectError: true,
			errorMsg:    "PROMETHEUS_BASE_URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changes, err := DetectImmutableParameterChanges(initialConfig, tt.configMap)
			if tt.expectError {
				require.Error(t, err, "Should detect immutable parameter change")
				require.NotEmpty(t, changes, "Should return list of changed immutable parameters")
				// Verify the expected key is in the changes list
				found := false
				for _, change := range changes {
					if change.Key == tt.errorMsg {
						found = true
						break
					}
				}
				assert.True(t, found, "Expected key %q should be in the changes list", tt.errorMsg)
				// Verify all detected changes are in the ConfigMap
				for _, change := range changes {
					assert.Contains(t, tt.configMap, change.Key, "Detected change should be in ConfigMap")
				}
			} else {
				require.NoError(t, err, "Should not detect immutable parameter change")
				assert.Empty(t, changes, "Should return empty list when no immutable changes")
			}
		})
	}
}

// TestDetectImmutableParameterChanges_NoInitialConfig tests detection when initial config
// doesn't have certain fields set.
func TestDetectImmutableParameterChanges_NoInitialConfig(t *testing.T) {
	initialConfig := NewTestConfig()
	// Don't set Prometheus config initially

	// Attempting to set PROMETHEUS_BASE_URL when it wasn't set initially
	// This is actually allowed during initial load, but not during runtime updates
	// For runtime updates, we should detect it as a change attempt
	configMap := map[string]string{
		"PROMETHEUS_BASE_URL": "https://prometheus-new:9090",
	}

	changes, err := DetectImmutableParameterChanges(initialConfig, configMap)
	// If Prometheus wasn't set initially, setting it via ConfigMap is a change attempt
	// (though this would typically be caught at startup, not runtime)
	require.Error(t, err, "Should detect change attempt even if initial value was not set")
	assert.NotEmpty(t, changes, "Should return list of changed immutable parameters")
}

// TestDetectImmutableParameterChanges_EmptyConfigMap tests that empty ConfigMap doesn't trigger errors.
func TestDetectImmutableParameterChanges_EmptyConfigMap(t *testing.T) {
	initialConfig := NewTestConfig()
	initialConfig.setPrometheusBaseURLForTesting("https://prometheus:9090")

	configMap := map[string]string{}

	changes, err := DetectImmutableParameterChanges(initialConfig, configMap)
	require.NoError(t, err, "Empty ConfigMap should not trigger errors")
	assert.Empty(t, changes, "Should return empty list for empty ConfigMap")
}

// TestDetectImmutableParameterChanges_OnlyMutable tests that only mutable parameters don't trigger errors.
func TestDetectImmutableParameterChanges_OnlyMutable(t *testing.T) {
	initialConfig := NewTestConfig()
	initialConfig.setPrometheusBaseURLForTesting("https://prometheus:9090")

	// Only mutable parameters
	configMap := map[string]string{
		"GLOBAL_OPT_INTERVAL": "120s",
	}

	changes, err := DetectImmutableParameterChanges(initialConfig, configMap)
	require.NoError(t, err, "Mutable parameters should not trigger errors")
	assert.Empty(t, changes, "Should return empty list for mutable parameters only")
}

// TestConfig_NamespaceAwareResolutionPrecedence tests that namespace-local config
// takes precedence over global config.
func TestConfig_NamespaceAwareResolutionPrecedence(t *testing.T) {
	cfg := NewTestConfig()

	// Set up global saturation config
	globalSatConfig := map[string]SaturationScalingConfig{
		"default": {
			KvCacheThreshold:     0.80,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.10,
			QueueSpareTrigger:    3,
		},
	}
	cfg.UpdateSaturationConfig(globalSatConfig)

	// Set up global scale-to-zero config
	globalScaleToZeroConfig := ScaleToZeroConfigData{
		"model1": {
			ModelID:           "model1",
			EnableScaleToZero: boolPtr(true),
			RetentionPeriod:   "10m",
		},
	}
	cfg.UpdateScaleToZeroConfig(globalScaleToZeroConfig)

	namespace := "test-namespace"

	// Test 1: No namespace-local config, should return global
	t.Run("No namespace-local config returns global", func(t *testing.T) {
		satConfig := cfg.SaturationConfigForNamespace(namespace)
		assert.Equal(t, 1, len(satConfig), "Should return global config")
		assert.Equal(t, 0.80, satConfig["default"].KvCacheThreshold, "Should use global value")

		scaleToZeroConfig := cfg.ScaleToZeroConfigForNamespace(namespace)
		assert.Equal(t, 1, len(scaleToZeroConfig), "Should return global config")
		assert.Equal(t, "model1", scaleToZeroConfig["model1"].ModelID, "Should use global value")
	})

	// Test 2: Namespace-local config takes precedence
	t.Run("Namespace-local config takes precedence", func(t *testing.T) {
		// Set namespace-local saturation config
		nsSatConfig := map[string]SaturationScalingConfig{
			"default": {
				KvCacheThreshold:     0.70, // Different from global (0.80)
				QueueLengthThreshold: 3,    // Different from global (5)
				KvSpareTrigger:       0.20, // Different from global (0.10)
				QueueSpareTrigger:    5,    // Different from global (3)
			},
		}
		cfg.UpdateSaturationConfigForNamespace(namespace, nsSatConfig)

		// Set namespace-local scale-to-zero config
		nsScaleToZeroConfig := ScaleToZeroConfigData{
			"model1": {
				ModelID:           "model1",
				EnableScaleToZero: boolPtr(false), // Different from global (true)
				RetentionPeriod:   "5m",           // Different from global (10m)
			},
		}
		cfg.UpdateScaleToZeroConfigForNamespace(namespace, nsScaleToZeroConfig)

		// Verify namespace-local config is returned
		satConfig := cfg.SaturationConfigForNamespace(namespace)
		assert.Equal(t, 1, len(satConfig), "Should return namespace-local config")
		assert.Equal(t, 0.70, satConfig["default"].KvCacheThreshold, "Should use namespace-local value")
		assert.Equal(t, float64(3), satConfig["default"].QueueLengthThreshold, "Should use namespace-local value")

		scaleToZeroConfig := cfg.ScaleToZeroConfigForNamespace(namespace)
		assert.Equal(t, 1, len(scaleToZeroConfig), "Should return namespace-local config")
		assert.Equal(t, false, *scaleToZeroConfig["model1"].EnableScaleToZero, "Should use namespace-local value")
		assert.Equal(t, "5m", scaleToZeroConfig["model1"].RetentionPeriod, "Should use namespace-local value")

		// Verify global config is unchanged
		globalSatConfig := cfg.SaturationConfigForNamespace("")
		assert.Equal(t, 0.80, globalSatConfig["default"].KvCacheThreshold, "Global config should be unchanged")
	})

	// Test 3: Empty namespace returns global
	t.Run("Empty namespace returns global", func(t *testing.T) {
		satConfig := cfg.SaturationConfigForNamespace("")
		assert.Equal(t, 0.80, satConfig["default"].KvCacheThreshold, "Empty namespace should return global")
	})
}

// TestConfig_NamespaceConfigDeletion tests that removing namespace-local config
// falls back to global config.
func TestConfig_NamespaceConfigDeletion(t *testing.T) {
	cfg := NewTestConfig()

	// Set up global saturation config
	globalSatConfig := map[string]SaturationScalingConfig{
		"default": {
			KvCacheThreshold:     0.80,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.10,
			QueueSpareTrigger:    3,
		},
	}
	cfg.UpdateSaturationConfig(globalSatConfig)

	// Set up global scale-to-zero config
	globalScaleToZeroConfig := ScaleToZeroConfigData{
		"model1": {
			ModelID:           "model1",
			EnableScaleToZero: boolPtr(true),
			RetentionPeriod:   "10m",
		},
	}
	cfg.UpdateScaleToZeroConfig(globalScaleToZeroConfig)

	namespace := "test-namespace"

	// Set namespace-local config
	nsSatConfig := map[string]SaturationScalingConfig{
		"default": {
			KvCacheThreshold:     0.70,
			QueueLengthThreshold: 3,
			KvSpareTrigger:       0.20,
			QueueSpareTrigger:    5,
		},
	}
	cfg.UpdateSaturationConfigForNamespace(namespace, nsSatConfig)

	nsScaleToZeroConfig := ScaleToZeroConfigData{
		"model1": {
			ModelID:           "model1",
			EnableScaleToZero: boolPtr(false),
			RetentionPeriod:   "5m",
		},
	}
	cfg.UpdateScaleToZeroConfigForNamespace(namespace, nsScaleToZeroConfig)

	// Verify namespace-local config is active
	satConfig := cfg.SaturationConfigForNamespace(namespace)
	assert.Equal(t, 0.70, satConfig["default"].KvCacheThreshold, "Should use namespace-local value")

	// Remove namespace-local config (simulating ConfigMap deletion)
	cfg.RemoveNamespaceConfig(namespace)

	// Verify fallback to global config
	satConfig = cfg.SaturationConfigForNamespace(namespace)
	assert.Equal(t, 0.80, satConfig["default"].KvCacheThreshold, "Should fall back to global value after deletion")

	scaleToZeroConfig := cfg.ScaleToZeroConfigForNamespace(namespace)
	assert.Equal(t, true, *scaleToZeroConfig["model1"].EnableScaleToZero, "Should fall back to global value after deletion")
	assert.Equal(t, "10m", scaleToZeroConfig["model1"].RetentionPeriod, "Should fall back to global value after deletion")
}

// TestConfig_MultipleNamespaces tests that different namespaces can have different configs.
func TestConfig_MultipleNamespaces(t *testing.T) {
	cfg := NewTestConfig()

	// Set up global config
	globalSatConfig := map[string]SaturationScalingConfig{
		"default": {
			KvCacheThreshold:     0.80,
			QueueLengthThreshold: 5,
		},
	}
	cfg.UpdateSaturationConfig(globalSatConfig)

	namespace1 := "namespace1"
	namespace2 := "namespace2"

	// Set namespace1 config
	ns1SatConfig := map[string]SaturationScalingConfig{
		"default": {
			KvCacheThreshold:     0.70,
			QueueLengthThreshold: 3,
		},
	}
	cfg.UpdateSaturationConfigForNamespace(namespace1, ns1SatConfig)

	// Set namespace2 config
	ns2SatConfig := map[string]SaturationScalingConfig{
		"default": {
			KvCacheThreshold:     0.90,
			QueueLengthThreshold: 7,
		},
	}
	cfg.UpdateSaturationConfigForNamespace(namespace2, ns2SatConfig)

	// Verify each namespace has its own config
	satConfig1 := cfg.SaturationConfigForNamespace(namespace1)
	assert.Equal(t, 0.70, satConfig1["default"].KvCacheThreshold, "Namespace1 should have its own config")

	satConfig2 := cfg.SaturationConfigForNamespace(namespace2)
	assert.Equal(t, 0.90, satConfig2["default"].KvCacheThreshold, "Namespace2 should have its own config")

	// Verify global config is unchanged
	globalSatConfig2 := cfg.SaturationConfigForNamespace("")
	assert.Equal(t, 0.80, globalSatConfig2["default"].KvCacheThreshold, "Global config should be unchanged")
}

func TestQMAnalyzerConfig_GlobalGetSet(t *testing.T) {
	cfg := NewTestConfig()

	// Initially empty
	qmCfg := cfg.QMAnalyzerConfig()
	if len(qmCfg) != 0 {
		t.Fatalf("expected empty queueing model config, got %d entries", len(qmCfg))
	}

	// Set global config
	cfg.UpdateQMAnalyzerConfig(QMAnalyzerConfigPerModel{
		"default": domain.QueueingModelScalingConfig{SLOMultiplier: 3.0},
	})

	qmCfg = cfg.QMAnalyzerConfig()
	if len(qmCfg) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(qmCfg))
	}
	if qmCfg["default"].SLOMultiplier != 3.0 {
		t.Errorf("SLOMultiplier = %f, want 3.0", qmCfg["default"].SLOMultiplier)
	}
}

func TestQMAnalyzerConfig_NamespaceOverride(t *testing.T) {
	cfg := NewTestConfig()

	cfg.UpdateQMAnalyzerConfig(QMAnalyzerConfigPerModel{
		"default": domain.QueueingModelScalingConfig{SLOMultiplier: 3.0},
	})

	cfg.UpdateQMAnalyzerConfigForNamespace("prod", QMAnalyzerConfigPerModel{
		"default": domain.QueueingModelScalingConfig{SLOMultiplier: 5.0},
	})

	global := cfg.QMAnalyzerConfig()
	if global["default"].SLOMultiplier != 3.0 {
		t.Errorf("global SLOMultiplier = %f, want 3.0", global["default"].SLOMultiplier)
	}

	nsCfg := cfg.QMAnalyzerConfigForNamespace("prod")
	if nsCfg["default"].SLOMultiplier != 5.0 {
		t.Errorf("namespace SLOMultiplier = %f, want 5.0", nsCfg["default"].SLOMultiplier)
	}

	otherCfg := cfg.QMAnalyzerConfigForNamespace("staging")
	if otherCfg["default"].SLOMultiplier != 3.0 {
		t.Errorf("fallback SLOMultiplier = %f, want 3.0", otherCfg["default"].SLOMultiplier)
	}
}

func TestQMAnalyzerConfig_ReturnsCopy(t *testing.T) {
	cfg := NewTestConfig()
	cfg.UpdateQMAnalyzerConfig(QMAnalyzerConfigPerModel{
		"default": domain.QueueingModelScalingConfig{SLOMultiplier: 3.0},
	})

	copy1 := cfg.QMAnalyzerConfig()
	copy1["default"] = domain.QueueingModelScalingConfig{SLOMultiplier: 99.0}

	copy2 := cfg.QMAnalyzerConfig()
	if copy2["default"].SLOMultiplier != 3.0 {
		t.Errorf("stored config was mutated: SLOMultiplier = %f, want 3.0", copy2["default"].SLOMultiplier)
	}
}

// Helper function to create bool pointer
func boolPtr(b bool) *bool {
	return &b
}

// TestConfig_ThroughputAnalyzerEnabled covers the predicate shared by
// cmd/main.go's startup registration gate and the ConfigMapReconciler's
// runtime-divergence check (migrated from cmd/main_test.go when the
// predicate moved to (*Config).ThroughputAnalyzerEnabled).
func TestConfig_ThroughputAnalyzerEnabled(t *testing.T) {
	tests := []struct {
		name     string
		analyzer []AnalyzerScoreConfig
		want     bool
	}{
		{
			name:     "absent — no saturation config entries",
			analyzer: nil,
			want:     false,
		},
		{
			name: "absent — other analyzers present, throughput missing",
			analyzer: []AnalyzerScoreConfig{
				{Name: "saturation"},
			},
			want: false,
		},
		{
			name: "enabled — explicit Enabled:true",
			analyzer: []AnalyzerScoreConfig{
				{Name: throughputAnalyzerName, Enabled: boolPtr(true)},
			},
			want: true,
		},
		{
			name: "enabled — present with Enabled nil (defaults true)",
			analyzer: []AnalyzerScoreConfig{
				{Name: throughputAnalyzerName},
			},
			want: true,
		},
		{
			name: "disabled — explicit Enabled:false",
			analyzer: []AnalyzerScoreConfig{
				{Name: throughputAnalyzerName, Enabled: boolPtr(false)},
			},
			want: false,
		},
		{
			name: "enabled — matched via Type override, Name differs (EffectiveType, not Name)",
			analyzer: []AnalyzerScoreConfig{
				{Name: "my-throughput-analyzer", Type: throughputAnalyzerName},
			},
			want: true,
		},
		{
			name: "absent — Name is throughput but Type overrides to something else",
			analyzer: []AnalyzerScoreConfig{
				{Name: throughputAnalyzerName, Type: "saturation"},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewTestConfig()
			cfg.UpdateSaturationConfig(map[string]SaturationScalingConfig{
				"default": {Analyzers: tt.analyzer},
			})

			got := cfg.ThroughputAnalyzerEnabled()
			if got != tt.want {
				t.Errorf("ThroughputAnalyzerEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}
