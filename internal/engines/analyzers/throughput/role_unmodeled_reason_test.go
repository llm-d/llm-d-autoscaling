package throughput

import (
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

// TestRoleUnmodeledReasonMatchesPipeline pins the duplicated roleUnmodeledReason
// literal against pipeline.ReasonRoleUnmodeled. The literal is duplicated rather
// than imported because this package cannot import internal/engines/pipeline in
// production code without reopening the same test-binary import cycle
// TestFallbackKSatMatchesConfigDefault already guards against (pipeline imports
// internal/config, and internal/config's own in-package tests import this
// package). A test file may import pipeline freely: it compiles only into this
// package's own test binary, which internal/config's tests never link.
func TestRoleUnmodeledReasonMatchesPipeline(t *testing.T) {
	if roleUnmodeledReason != pipeline.ReasonRoleUnmodeled {
		t.Fatalf("roleUnmodeledReason = %q, want %q (pipeline.ReasonRoleUnmodeled); the duplicated literal has drifted",
			roleUnmodeledReason, pipeline.ReasonRoleUnmodeled)
	}
}
