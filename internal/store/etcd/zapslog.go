package etcd

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// newZapLogger adapts the process logger so the etcd client logs through it.
//
// clientv3 has no slog seam. Handed no logger it builds its own, writing JSON to stderr at info
// level, which would interleave a second differently-shaped log format into the operator's
// output — and handing it [zap.NewNop] instead throws away exactly the diagnostics that matter
// when the control plane cannot reach its store: endpoint failures, reconnects, TLS and auth
// errors. So it is bridged.
//
// The bridge is also what puts the client's level under --log-level, since [slogCore.Enabled]
// asks the slog handler rather than carrying a level of its own.
func newZapLogger(logger *slog.Logger) *zap.Logger {
	return zap.New(&slogCore{logger: logger.With("module", "etcd")})
}

// slogCore is a [zapcore.Core] that writes to an [slog.Logger].
type slogCore struct {
	logger *slog.Logger

	// fields accumulated by With, which zap uses to build sub-loggers.
	fields []zapcore.Field
}

var _ zapcore.Core = (*slogCore)(nil)

func (c *slogCore) Enabled(level zapcore.Level) bool {
	return c.logger.Enabled(context.Background(), slogLevel(level))
}

func (c *slogCore) With(fields []zapcore.Field) zapcore.Core {
	return &slogCore{logger: c.logger, fields: slices.Concat(c.fields, fields)}
}

func (c *slogCore) Check(entry zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return ce.AddCore(entry, c)
	}
	return ce
}

func (c *slogCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range c.fields {
		f.AddTo(enc)
	}
	for _, f := range fields {
		f.AddTo(enc)
	}

	attrs := make([]slog.Attr, 0, len(enc.Fields))
	for key, value := range enc.Fields {
		attrs = append(attrs, slog.Any(key, value))
	}
	// Map iteration order is random, and a log line whose fields shuffle between occurrences is
	// hard to read and impossible to diff.
	slices.SortFunc(attrs, func(a, b slog.Attr) int { return strings.Compare(a.Key, b.Key) })

	c.logger.LogAttrs(context.Background(), slogLevel(entry.Level), entry.Message, attrs...)
	return nil
}

// Sync is a no-op: the slog handler owns its writer and its flushing.
func (c *slogCore) Sync() error { return nil }

// slogLevel maps zap's levels onto slog's. zap's three fatal-ish levels all become Error —
// nothing below this bridge gets to decide the process should die.
func slogLevel(level zapcore.Level) slog.Level {
	switch level {
	case zapcore.DebugLevel:
		return slog.LevelDebug
	case zapcore.InfoLevel:
		return slog.LevelInfo
	case zapcore.WarnLevel:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}
