package controller_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRBACMarkersLeastPrivilege verifies kubebuilder:rbac markers follow least-privilege.
func TestRBACMarkersLeastPrivilege(t *testing.T) {
	rbacFile := "rbac.go"
	content, err := os.ReadFile(rbacFile)
	require.NoError(t, err, "Failed to read rbac.go")

	rbacContent := string(content)

	// Nodes should only have read verbs (get;list;watch), not write (update;patch)
	t.Run("nodes permissions are read-only", func(t *testing.T) {
		assert.NotContains(t, rbacContent, `resources=nodes,verbs=get;list;watch;update;patch`,
			"nodes should not have update;patch verbs (controller only lists nodes for GPU discovery)")
		assert.Contains(t, rbacContent, `resources=nodes,verbs=get;list;watch`,
			"nodes should have read-only verbs")
	})

	// nodes/status should only have read verbs
	t.Run("nodes/status permissions are read-only", func(t *testing.T) {
		assert.NotContains(t, rbacContent, `resources=nodes/status,verbs=get;list;update;patch;watch`,
			"nodes/status should not have update;patch verbs (unused write permissions)")
		assert.Contains(t, rbacContent, `resources=nodes/status,verbs=get;list;watch`,
			"nodes/status should have read-only verbs")
	})

	// ConfigMaps cluster-wide should only have read verbs (reconciler is read-only)
	t.Run("configmaps permissions are read-only at cluster scope", func(t *testing.T) {
		// The rbac.go file grants cluster-wide configmap permissions
		// configmap_reconciler.go has its own read-only marker, but rbac.go shouldn't grant update
		lines := strings.Split(rbacContent, "\n")
		for i, line := range lines {
			if strings.Contains(line, `resources=configmaps`) &&
				!strings.Contains(line, "configmaps/status") &&
				strings.Contains(line, "rbac.go") {
				// Check that this line or nearby context doesn't grant update verb
				context := strings.Join(lines[max(0, i-2):min(len(lines), i+3)], "\n")
				assert.NotContains(t, context, "update",
					"configmaps should not have update verb in cluster-wide RBAC marker")
			}
		}
	})
}
