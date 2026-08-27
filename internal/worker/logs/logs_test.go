package logs

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/logging"
)

func TestParse(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		line     string
		level    slog.Level
		message  string
		location string
	}{
		"plain": {
			line:    `[2026-08-27 14:03:11.842] [console] [info] connected`,
			level:   slog.LevelInfo,
			message: "connected",
		},
		"with source location": {
			line:     `[2026-08-27 14:03:11.900] [console] [debug] [Flow.cpp:88] opened flow`,
			level:    slog.LevelDebug,
			message:  "opened flow",
			location: "Flow.cpp:88",
		},
		"warning maps to warn": {
			line:    `[2026-08-27 14:03:11.900] [console] [warning] maxMessageSize is not set`,
			level:   slog.LevelWarn,
			message: "maxMessageSize is not set",
		},
		"trace maps to debug": {
			line:    `[2026-08-27 14:03:11.900] [console] [trace] tick`,
			level:   slog.LevelDebug,
			message: "tick",
		},
		"fatal is an error line": {
			line:    `[2026-08-27 14:03:11.900] [console] [error] fatal: timed out waiting for a grain`,
			level:   slog.LevelError,
			message: "fatal: timed out waiting for a grain",
		},
		"no logger name": {
			line:    `[2026-08-27 14:03:11.842] [info] connected`,
			level:   slog.LevelInfo,
			message: "connected",
		},
		"no level defaults to info": {
			line:    `[2026-08-27 14:03:11.842] [console] connected`,
			level:   slog.LevelInfo,
			message: "connected",
		},
		// The legacy parser consumed every leading bracket, so this message lost its first
		// token and gained a bogus source location (WRS §7).
		"message starting with a bracket": {
			line:    `[2026-08-27 14:03:11.842] [console] [info] [peer] disconnected`,
			level:   slog.LevelInfo,
			message: "[peer] disconnected",
		},
		"empty message": {
			line:    `[2026-08-27 14:03:11.842] [console] [info] `,
			level:   slog.LevelInfo,
			message: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			record, ok := Parse(tc.line)
			require.True(t, ok)
			assert.Equal(t, tc.level, record.Level)
			assert.Equal(t, tc.message, record.Message)
			assert.Equal(t, 2026, record.Time.Year())

			location := ""
			record.Attrs(func(a slog.Attr) bool {
				if a.Key == "location" {
					location = a.Value.String()
				}
				return true
			})
			assert.Equal(t, tc.location, location)
		})
	}
}

// Everything that is not a spdlog line must be reported as such rather than swallowed: this is
// also the stream libfabric writes its own diagnostics to, and the legacy translator dropped
// every line it could not parse.
func TestParseRejectsNonWorkerLines(t *testing.T) {
	t.Parallel()

	for name, line := range map[string]string{
		"libfabric diagnostic": "libfabric:1234:1700000000::core:core:ofi_register_provider():474<info> registering provider: tcp",
		"bare text":            "starting up",
		"empty":                "",
		"unterminated bracket": "[2026-08-27 14:03:11.842 console info connected",
		"bad timestamp":        "[not a time] [console] [info] connected",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, ok := Parse(line)
			assert.False(t, ok)
		})
	}
}

func TestParseKeepsTheWorkersTimestamp(t *testing.T) {
	t.Parallel()

	record, ok := Parse(`[2026-08-27 14:03:11.842] [console] [info] connected`)
	require.True(t, ok)

	want := time.Date(2026, 8, 27, 14, 3, 11, 842_000_000, time.Local)
	assert.True(t, record.Time.Equal(want), "got %s, want %s", record.Time, want)
}

func TestParseToleratesLineEndings(t *testing.T) {
	t.Parallel()

	record, ok := Parse("[2026-08-27 14:03:11.842] [console] [info] connected\r\n")
	require.True(t, ok)
	assert.Equal(t, "connected", record.Message)
}

// The agent asks for a level in the worker's vocabulary and then has to recognise it coming
// back. A name it can set but not parse would silently misreport every line logged at it.
func TestLevelIsTheInverseOfWorkerLogLevel(t *testing.T) {
	t.Parallel()

	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		name := logging.WorkerLogLevel(level)
		parsed, known := Level(name)
		require.True(t, known, "worker level %q is not one this package parses", name)
		assert.Equal(t, level, parsed)
	}
}
