package config

import (
	"testing"
)

func TestQMAnalyzerConfigMapName_Default(t *testing.T) {
	t.Setenv("QUEUEING_MODEL_CONFIG_MAP_NAME", "")
	if got := QMAnalyzerConfigMapName(); got != "wva-queueing-model-config" {
		t.Errorf("QMAnalyzerConfigMapName() = %q, want %q", got, "wva-queueing-model-config")
	}
}

func TestQMAnalyzerConfigMapName_EnvOverride(t *testing.T) {
	t.Setenv("QUEUEING_MODEL_CONFIG_MAP_NAME", "custom-qm-config")
	if got := QMAnalyzerConfigMapName(); got != "custom-qm-config" {
		t.Errorf("QMAnalyzerConfigMapName() = %q, want %q", got, "custom-qm-config")
	}
}
