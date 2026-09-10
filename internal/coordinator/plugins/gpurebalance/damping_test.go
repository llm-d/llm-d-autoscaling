package gpurebalance

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testKey = "ns-a/ScaledObject/pool-a-so"

// TestShouldPatch_RaisesImmediately pins the asymmetry: widening a ceiling is
// reversible and cannot evict anything, so it is never delayed. Delaying it
// would make a genuine demand spike late, which is the failure mode the damping
// is not supposed to introduce.
func TestShouldPatch_RaisesImmediately(t *testing.T) {
	s := newStabilizer()
	assert.True(t, s.shouldPatch(testKey, 4, 6), "an increase must be applied on the first tick")
	assert.True(t, s.shouldPatch(testKey, 6, 9), "consecutive increases must not be throttled")
}

// TestShouldPatch_LowerTargetMustPersist covers the core of #1427: a single
// noisy reading that computes a lower ceiling must not reach the cluster,
// because a lowered ceiling is enforced immediately and evicts pods.
func TestShouldPatch_LowerTargetMustPersist(t *testing.T) {
	s := newStabilizer()

	for tick := 1; tick < downscaleStabilizationTicks; tick++ {
		assert.False(t, s.shouldPatch(testKey, 8, 6),
			"tick %d: a lower ceiling must be held until it has persisted", tick)
	}

	assert.True(t, s.shouldPatch(testKey, 8, 6),
		"the lower ceiling must be applied once it has held for downscaleStabilizationTicks")
}

// TestShouldPatch_RecoveryResetsStreak is the noise case from the issue: the
// queue dips for a tick or two, then recovers. The transient dip must not
// accumulate toward a downgrade, otherwise repeated unrelated blips eventually
// add up to an eviction.
func TestShouldPatch_RecoveryResetsStreak(t *testing.T) {
	s := newStabilizer()

	require.False(t, s.shouldPatch(testKey, 8, 6), "first low reading is held")
	require.False(t, s.shouldPatch(testKey, 8, 6), "second low reading is held")

	// Queue recovers and the computed ceiling agrees with reality again.
	assert.False(t, s.shouldPatch(testKey, 8, 8), "agreement is not a patch")

	// The earlier dip must be forgotten, so a fresh dip starts counting from zero.
	for tick := 1; tick < downscaleStabilizationTicks; tick++ {
		assert.False(t, s.shouldPatch(testKey, 8, 6),
			"tick %d: streak should have reset after recovery", tick)
	}
	assert.True(t, s.shouldPatch(testKey, 8, 6), "and only then apply")
}

// TestShouldPatch_ResetsAfterApplying guards the churn case: once a downgrade is
// applied, the counter has to start over. If it did not, a scaler drifting
// steadily downward would patch on every subsequent tick.
func TestShouldPatch_ResetsAfterApplying(t *testing.T) {
	s := newStabilizer()

	for tick := 1; tick < downscaleStabilizationTicks; tick++ {
		require.False(t, s.shouldPatch(testKey, 8, 6))
	}
	require.True(t, s.shouldPatch(testKey, 8, 6), "applies once persisted")

	// Next tick computes a lower ceiling again; it must serve a fresh window.
	assert.False(t, s.shouldPatch(testKey, 6, 5),
		"a further downgrade must serve its own stabilization window, not patch immediately")
}

// TestShouldPatch_TracksScalersIndependently ensures one noisy pool cannot
// consume another pool's stabilization window — they are separate scalers with
// separate keys.
func TestShouldPatch_TracksScalersIndependently(t *testing.T) {
	s := newStabilizer()
	const other = "ns-a/ScaledObject/pool-b-so"

	for tick := 1; tick < downscaleStabilizationTicks; tick++ {
		require.False(t, s.shouldPatch(testKey, 8, 6))
	}

	assert.False(t, s.shouldPatch(other, 8, 6),
		"a different scaler starts its own window")
	assert.True(t, s.shouldPatch(testKey, 8, 6),
		"the original scaler's window is unaffected by the other")
}

// TestRetain_PrunesDepartedScalers keeps the state map bounded to the live set.
// Without this, every ScaledObject ever observed would be retained for the
// lifetime of the process.
func TestRetain_PrunesDepartedScalers(t *testing.T) {
	s := newStabilizer()
	const departed = "ns-a/ScaledObject/deleted-so"

	require.False(t, s.shouldPatch(testKey, 8, 6))
	require.False(t, s.shouldPatch(departed, 8, 6))
	require.Len(t, s.downTicks, 2)

	s.retain(map[string]struct{}{testKey: {}})

	assert.Contains(t, s.downTicks, testKey, "a live scaler keeps its streak")
	assert.NotContains(t, s.downTicks, departed, "a departed scaler is pruned")
}

// TestScalerKey_DistinguishesKindAndNamespace guards the key derivation: an HPA
// and a ScaledObject may share a name in one namespace, and the same name may
// exist in two namespaces. Collapsing either would let one scaler's readings
// damp another's.
func TestScalerKey_DistinguishesKindAndNamespace(t *testing.T) {
	obj := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "shared"},
	}

	hpaKey := scalerKey("ns-a", displayKindHPA, obj)
	soKey := scalerKey("ns-a", displayKindScaledObject, obj)
	otherNS := scalerKey("ns-b", displayKindHPA, obj)

	assert.NotEqual(t, hpaKey, soKey, "kind must be part of the key")
	assert.NotEqual(t, hpaKey, otherNS, "namespace must be part of the key")
}
