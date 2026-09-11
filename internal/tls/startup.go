package tls

import (
	"context"
	cryptotls "crypto/tls"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Startup contains the TLS configuration and manager startup hooks selected for
// the current build.
type Startup struct {
	tlsOpts      []func(*cryptotls.Config)
	addToScheme  func(*runtime.Scheme) error
	setupWatcher func(context.Context, ctrl.Manager) (context.Context, error)
}

// TLSOptions returns a copy of the TLS options used by the server endpoints.
func (s Startup) TLSOptions() []func(*cryptotls.Config) {
	return append([]func(*cryptotls.Config){}, s.tlsOpts...)
}

// AddToScheme registers any APIs required by the selected TLS implementation.
func (s Startup) AddToScheme(scheme *runtime.Scheme) error {
	if s.addToScheme == nil {
		return nil
	}
	return s.addToScheme(scheme)
}

// SetupWatcher installs any TLS configuration watcher and returns the context
// that should be passed to the manager.
func (s Startup) SetupWatcher(ctx context.Context, mgr ctrl.Manager) (context.Context, error) {
	if s.setupWatcher == nil {
		return ctx, nil
	}
	return s.setupWatcher(ctx, mgr)
}

func defaultTLSOpts() []func(*cryptotls.Config) {
	return []func(*cryptotls.Config){
		func(config *cryptotls.Config) {
			config.NextProtos = []string{"h2", "http/1.1"}
		},
	}
}
