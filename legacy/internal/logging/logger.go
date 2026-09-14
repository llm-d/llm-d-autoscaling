package logging

import (
	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	DEFAULT = 2
	VERBOSE = 3
	DEBUG   = 4
	TRACE   = 5
)

// InitLogging initializes the controller-runtime logger with zap backend.
func InitLogging(opts *zap.Options, logVerbosity *int) {
	// Unless --zap-log-level is explicitly set (via BindFlags), use -v
	// When BindFlags parses --zap-log-level, it sets opts.Level to a non-nil value.
	// If opts.Level is still nil, the user didn't set --zap-log-level, so we use -v.
	if opts.Level == nil {
		// See https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/log/zap#Options.Level
		lvl := -1 * (*logVerbosity)
		opts.Level = uberzap.NewAtomicLevelAt(zapcore.Level(int8(lvl)))
	}

	logger := zap.New(zap.UseFlagOptions(opts), zap.RawZapOpts(uberzap.AddCaller()))
	ctrl.SetLogger(logger)
}

// Sync flushes any buffered log entries.
func Sync() error {
	logger := ctrl.Log.WithName("logger-sync").GetSink()
	if syncer, ok := logger.(interface{ Sync() error }); ok {
		return syncer.Sync()
	}
	return nil
}
