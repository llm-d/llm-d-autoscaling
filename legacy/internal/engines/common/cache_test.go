package common

import (
	"sync"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

func TestInternalDecisionCache(t *testing.T) {
	cache := &InternalDecisionCache{
		items: make(map[string]domain.VariantDecision),
	}

	// Test Set and Get
	decision := domain.VariantDecision{
		VariantName:     "test-variant",
		Namespace:       "test-ns",
		TargetReplicas:  5,
		AcceleratorName: "A100",
		Action:          domain.ActionScaleUp,
	}

	cache.Set("test-variant", "test-ns", decision)

	retrieved, ok := cache.Get("test-variant", "test-ns")
	if !ok {
		t.Error("Expected decision to be found in cache")
	}
	if retrieved.TargetReplicas != 5 {
		t.Errorf("Expected TargetReplicas 5, got %d", retrieved.TargetReplicas)
	}

	_, ok = cache.Get("non-existent", "test-ns")
	if ok {
		t.Error("Expected non-existent item to not be found")
	}

	// Test Concurrency
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.Set("test-variant", "test-ns", decision)
			cache.Get("test-variant", "test-ns")
		}()
	}
	wg.Wait()
}

// TestGlobalConfig removed - GlobalConfig has been removed in favor of unified Config
// from internal/config package. Config functionality is now tested in internal/config/loader_test.go

func TestDecisionToOptimizedAlloc(t *testing.T) {
	d := domain.VariantDecision{
		TargetReplicas:  3,
		AcceleratorName: "H100",
	}

	replicas, acc, _ := DecisionToOptimizedAlloc(d)

	if replicas == nil || *replicas != 3 {
		t.Errorf("Expected 3 replicas, got %v", replicas)
	}
	if acc != "H100" {
		t.Errorf("Expected H100 accelerator, got %s", acc)
	}
}
