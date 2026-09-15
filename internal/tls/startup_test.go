package tls

import (
	"context"
	cryptotls "crypto/tls"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestResolveUsesHTTP2AndHTTP11(t *testing.T) {
	startup, err := Resolve(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	config := &cryptotls.Config{}
	for _, option := range startup.TLSOptions() {
		option(config)
	}

	if got, want := config.NextProtos, []string{"h2", "http/1.1"}; !equalStrings(got, want) {
		t.Fatalf("NextProtos = %v, want %v", got, want)
	}
}

func TestTLSOptionsReturnsCopy(t *testing.T) {
	startup, err := Resolve(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	options := startup.TLSOptions()
	options[0] = nil

	if fresh := startup.TLSOptions(); fresh[0] == nil {
		t.Fatal("TLSOptions returned the internal slice")
	}
}

func TestDefaultStartupHooksAreNoOps(t *testing.T) {
	startup, err := Resolve(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if err := startup.AddToScheme(runtime.NewScheme()); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	contextIn := context.Background()
	contextOut, err := startup.SetupWatcher(contextIn, nil)
	if err != nil {
		t.Fatalf("SetupWatcher() error = %v", err)
	}
	if contextOut != contextIn {
		t.Fatal("SetupWatcher() returned a different context")
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
