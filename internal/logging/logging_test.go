package logging

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLevel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{" info ", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"err", slog.LevelError},
	} {
		got, err := ParseLevel(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}

	_, err := ParseLevel("verbose")
	assert.Error(t, err)
}

// The worker level names must stay parseable by the agent's log translator, which expects
// trace|debug|info|warning|error out of the worker's stdout (WRS §7).
func TestWorkerLogLevel(t *testing.T) {
	assert.Equal(t, "debug", WorkerLogLevel(slog.LevelDebug))
	assert.Equal(t, "info", WorkerLogLevel(slog.LevelInfo))
	assert.Equal(t, "warning", WorkerLogLevel(slog.LevelWarn))
	assert.Equal(t, "error", WorkerLogLevel(slog.LevelError))

	// Levels between the named ones round down to the enclosing level rather than falling
	// through to "error".
	assert.Equal(t, "info", WorkerLogLevel(slog.LevelDebug+1))
}

func TestNewEmitsThroughEveryFormat(t *testing.T) {
	for _, format := range Formats() {
		buf := &bytes.Buffer{}

		logger, err := New(Options{Level: slog.LevelInfo, Format: Format(format), Output: buf})
		require.NoError(t, err, format)

		logger.Info("hello", "module", "test", "key", "value")
		assert.Contains(t, buf.String(), "hello", format)
		assert.Contains(t, buf.String(), "value", format)
	}
}

func TestNewRespectsLevel(t *testing.T) {
	buf := &bytes.Buffer{}

	logger, err := New(Options{Level: slog.LevelWarn, Format: FormatText, Output: buf})
	require.NoError(t, err)

	logger.Info("suppressed")
	logger.Warn("emitted")

	assert.NotContains(t, buf.String(), "suppressed")
	assert.Contains(t, buf.String(), "emitted")
}

func TestNewRejectsBadInput(t *testing.T) {
	_, err := New(Options{Format: FormatText})
	assert.Error(t, err, "a nil writer must not silently discard logs")

	_, err = New(Options{Format: "logfmt", Output: &bytes.Buffer{}})
	assert.Error(t, err)
}
