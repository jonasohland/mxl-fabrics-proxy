package etcd

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// The bridge is tested on its own because nothing else reaches it: the etcd client only logs
// when something is wrong with a cluster, so a panic or a dropped field in here would first be
// seen during an outage, which is the worst possible time to discover that the diagnostics do
// not work.
func TestZapLoggerBridgesIntoSlog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))

	zl := newZapLogger(logger)

	zl.Info("connected", zap.String("endpoint", "http://etcd-0:2379"), zap.Int("attempt", 2))
	assert.Equal(t,
		"level=INFO msg=connected module=etcd attempt=2 endpoint=http://etcd-0:2379\n",
		buf.String(),
		"fields are sorted, so a line's shape does not change between occurrences")

	buf.Reset()
	zl.With(zap.String("cluster", "blue")).Warn("endpoint down", zap.String("endpoint", "e"))
	assert.Equal(t,
		"level=WARN msg=\"endpoint down\" module=etcd cluster=blue endpoint=e\n",
		buf.String(),
		"With fields survive alongside the call's own")

	buf.Reset()
	zl.Error("no leader")
	assert.Equal(t, "level=ERROR msg=\"no leader\" module=etcd\n", buf.String())

	// Nothing below the bridge decides the process should die.
	buf.Reset()
	zl.DPanic("would panic in development")
	assert.Contains(t, buf.String(), "level=ERROR")

	// The client's verbosity is the process's verbosity: clientv3 logs a good deal at debug and
	// info, and --log-level has to be able to turn it down.
	buf.Reset()
	zl.Debug("dial attempt")
	assert.Empty(t, buf.String())
	assert.False(t, zl.Core().Enabled(zapcore.DebugLevel))
	assert.True(t, zl.Core().Enabled(zapcore.WarnLevel))
}
