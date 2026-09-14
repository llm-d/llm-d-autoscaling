package queueingmodel

import (
	"sync"
	"testing"
	"time"
)

func TestNewParameterStore(t *testing.T) {
	store := NewParameterStore()
	if store == nil {
		t.Fatal("NewParameterStore returned nil")
	}
	if store.params == nil {
		t.Fatal("params map not initialized")
	}
	if len(store.params) != 0 {
		t.Fatal("params map should be empty")
	}
}

func TestParameterStore_GetNonExistent(t *testing.T) {
	store := NewParameterStore()
	got := store.Get("ns", "variant")
	if got != nil {
		t.Fatalf("expected nil for non-existent variant, got %+v", got)
	}
}

func TestParameterStore_SetAndGet(t *testing.T) {
	store := NewParameterStore()
	now := time.Now()
	params := &LearnedParameters{
		Alpha:       1.5,
		Beta:        2.5,
		Gamma:       3.5,
		NIS:         0.42,
		Covariance:  [][]float64{{1, 0}, {0, 1}},
		LastUpdated: now,
	}

	store.Set("ns1", "variantA", params)
	got := store.Get("ns1", "variantA")

	if got == nil {
		t.Fatal("expected non-nil parameters")
	}
	if got.Alpha != 1.5 {
		t.Errorf("Alpha = %v, want 1.5", got.Alpha)
	}
	if got.Beta != 2.5 {
		t.Errorf("Beta = %v, want 2.5", got.Beta)
	}
	if got.Gamma != 3.5 {
		t.Errorf("Gamma = %v, want 3.5", got.Gamma)
	}
	if got.NIS != 0.42 {
		t.Errorf("NIS = %v, want 0.42", got.NIS)
	}
	if !got.LastUpdated.Equal(now) {
		t.Errorf("LastUpdated = %v, want %v", got.LastUpdated, now)
	}
}

func TestParameterStore_SetOverrides(t *testing.T) {
	store := NewParameterStore()
	store.Set("ns", "v1", &LearnedParameters{Alpha: 1.0})
	store.Set("ns", "v1", &LearnedParameters{Alpha: 2.0})

	got := store.Get("ns", "v1")
	if got.Alpha != 2.0 {
		t.Errorf("Alpha = %v, want 2.0 after override", got.Alpha)
	}
}

func TestParameterStore_DifferentNamespaces(t *testing.T) {
	store := NewParameterStore()
	store.Set("ns1", "v1", &LearnedParameters{Alpha: 1.0})
	store.Set("ns2", "v1", &LearnedParameters{Alpha: 2.0})

	got1 := store.Get("ns1", "v1")
	got2 := store.Get("ns2", "v1")

	if got1.Alpha != 1.0 {
		t.Errorf("ns1/v1 Alpha = %v, want 1.0", got1.Alpha)
	}
	if got2.Alpha != 2.0 {
		t.Errorf("ns2/v1 Alpha = %v, want 2.0", got2.Alpha)
	}
}

func TestParameterStore_DifferentVariants(t *testing.T) {
	store := NewParameterStore()
	store.Set("ns", "v1", &LearnedParameters{Alpha: 1.0})
	store.Set("ns", "v2", &LearnedParameters{Alpha: 2.0})

	got1 := store.Get("ns", "v1")
	got2 := store.Get("ns", "v2")

	if got1.Alpha != 1.0 {
		t.Errorf("ns/v1 Alpha = %v, want 1.0", got1.Alpha)
	}
	if got2.Alpha != 2.0 {
		t.Errorf("ns/v2 Alpha = %v, want 2.0", got2.Alpha)
	}
}

func TestParameterStore_ConcurrentAccess(t *testing.T) {
	store := NewParameterStore()
	var wg sync.WaitGroup

	// Concurrent writers
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store.Set("ns", "v1", &LearnedParameters{Alpha: float32(i)})
		}(i)
	}

	// Concurrent readers
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = store.Get("ns", "v1")
		}()
	}

	wg.Wait()

	// Should have a valid value after all goroutines complete
	got := store.Get("ns", "v1")
	if got == nil {
		t.Fatal("expected non-nil parameters after concurrent writes")
	}
}
