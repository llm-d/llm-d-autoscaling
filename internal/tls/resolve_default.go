//go:build !distro

package tls

import (
	"context"

	"k8s.io/client-go/rest"
)

// Resolve returns the TLS startup configuration for the standard Kubernetes build.
func Resolve(_ context.Context, _ *rest.Config) (Startup, error) {
	return Startup{tlsOpts: defaultTLSOpts()}, nil
}
