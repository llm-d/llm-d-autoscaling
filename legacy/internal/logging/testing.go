package logging

import (
	"context"

	"github.com/go-logr/logr"
	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// NewTestLogger creates a new Zap logger using the dev mode.
func NewTestLogger() logr.Logger {
	return zap.New(
		zap.UseDevMode(true),
		zap.Level(uberzap.NewAtomicLevelAt(zapcore.Level(-1*TRACE))),
		zap.RawZapOpts(uberzap.AddCaller()),
	)
}

// NewTestLoggerIntoContext creates a new Zap logger using the dev mode and inserts it into the given context.
func NewTestLoggerIntoContext(ctx context.Context) context.Context {
	return log.IntoContext(ctx, NewTestLogger())
}
