package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// TestPruneLastGoodAnalysis locks the two guards the prune relies on (empty
// active set is a no-op; a nil map does not panic) plus the core contract:
// departed model keys are evicted while active ones keep their inner entries.
func TestPruneLastGoodAnalysis(t *testing.T) {
	keyA := utils.GetNamespacedKey("ns", "model-a")
	keyB := utils.GetNamespacedKey("ns", "model-b")
	ts := time.Now()

	newState := func() map[string]map[string]time.Time {
		return map[string]map[string]time.Time{
			keyA: {"saturation": ts},
			keyB: {"saturation": ts},
		}
	}

	t.Run("evicts departed keeps active", func(t *testing.T) {
		e := &Engine{lastGoodAnalysis: newState()}
		e.pruneLastGoodAnalysis(map[string]bool{keyA: true})

		require.Contains(t, e.lastGoodAnalysis, keyA, "active model must be kept")
		assert.NotContains(t, e.lastGoodAnalysis, keyB, "departed model must be evicted")
		assert.Equal(t, ts, e.lastGoodAnalysis[keyA]["saturation"],
			"surviving model's inner analyzer entries must be left intact")
	})

	t.Run("empty active set is a no-op", func(t *testing.T) {
		e := &Engine{lastGoodAnalysis: newState()}
		e.pruneLastGoodAnalysis(map[string]bool{})

		assert.Contains(t, e.lastGoodAnalysis, keyA,
			"a zero-model cycle must not wipe accumulated state")
		assert.Contains(t, e.lastGoodAnalysis, keyB,
			"a zero-model cycle must not wipe accumulated state")
	})

	t.Run("nil map does not panic", func(t *testing.T) {
		e := &Engine{}
		assert.NotPanics(t, func() {
			e.pruneLastGoodAnalysis(map[string]bool{keyA: true})
		})
	})
}
