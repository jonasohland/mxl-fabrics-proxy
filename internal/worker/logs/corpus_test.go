package logs

import (
	"bufio"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testdata/worker-output.txt is captured verbatim from a real mxl-replicator-worker
// (mxl 1.2.0-dev, libfabric 2.6) across a spread of failure modes: a target timing out on an
// idle source, a bad domain path, a malformed flow definition, a missing config key, an
// unparseable provider, and a run with FI_LOG_LEVEL=debug so that libfabric's own diagnostics
// go through the same logger.
//
// It is a fixture rather than a set of hand-written examples because the point is what the
// binary *does*, not what the documentation says it does — and on three counts those differ.
func corpus(t *testing.T) []string {
	t.Helper()

	file, err := os.Open("testdata/worker-output.txt")
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	require.NoError(t, scanner.Err())
	require.NotEmpty(t, lines)
	return lines
}

// Every captured line must parse. A line this parser does not recognise is emitted raw at warn
// (which is the right fallback), but a worker's ordinary output arriving that way would mean
// every info line from a healthy worker is logged as a warning.
func TestParseHandlesRealWorkerOutput(t *testing.T) {
	t.Parallel()

	for _, line := range corpus(t) {
		record, ok := Parse(line)
		require.True(t, ok, "did not recognise: %s", line)
		assert.Equal(t, 2026, record.Time.Year(), "line: %s", line)
		assert.NotEmpty(t, record.Message, "line: %s", line)
		assert.NotContains(t, record.Message, "] [", "a prefix token leaked into the message: %s", record.Message)
	}
}

// The logger name is present on some lines and absent on others *within one run*: a line
// logged before the mxl instance installs its named default logger has no name, one after has
// `[console]`. Both spellings are in the fixture and both must work, because requiring either
// would drop half of a failing worker's output.
func TestParseAcceptsBothLoggerNameShapes(t *testing.T) {
	t.Parallel()

	var withName, withoutName int
	for _, line := range corpus(t) {
		record, ok := Parse(line)
		require.True(t, ok)
		assert.NotContains(t, record.Message, "console")

		if strings.Contains(line, "[console]") {
			withName++
		} else {
			withoutName++
		}
	}

	assert.Positive(t, withName, "fixture must contain a named-logger line")
	assert.Positive(t, withoutName, "fixture must contain an unnamed-logger line")
}

// libfabric's diagnostics are routed through mxl's logger rather than written raw, and land in
// the source-location position as subsys:provider:line. Splitting on the first colon reads
// `libfabric` as the file and `core:core:372` as the line number, which it is not — so the
// whole token used to stay in the message.
func TestParseExtractsBothSourceLocationShapes(t *testing.T) {
	t.Parallel()

	locations := map[string]string{}
	for _, line := range corpus(t) {
		record, ok := Parse(line)
		require.True(t, ok)
		record.Attrs(func(a slog.Attr) bool {
			if a.Key == "location" {
				locations[a.Value.String()] = record.Message
			}
			return true
		})
	}

	require.Contains(t, locations, "RCTarget.cpp:32")
	require.Contains(t, locations, "libfabric:core:core:372")
	assert.Equal(t, "variable prefer_sysconfig=<not set>", locations["libfabric:core:core:372"])
	assert.NotContains(t, locations["RCTarget.cpp:32"], "[")
}

// A message ending in a bracketed path, and one containing several colons, both of which occur
// in real output and both of which a positional parser can mangle.
func TestParseKeepsAwkwardMessagesIntact(t *testing.T) {
	t.Parallel()

	messages := make([]string, 0, 16)
	for _, line := range corpus(t) {
		record, ok := Parse(line)
		require.True(t, ok)
		messages = append(messages, record.Message)
	}
	joined := strings.Join(messages, "\n")

	assert.Contains(t, joined, "Not a directory [/nonexistent/domain]")
	assert.Contains(t, joined, "fi_sockaddr_in://127.0.0.1:24501")
	assert.Contains(t, joined, "syntax error at line 1 near: not json")
	assert.Contains(t, joined, "fatal: missing required field: metrics_socket")
}

// Every `fatal:` line must arrive at error level. This is the line that says why a worker will
// not start, and losing its severity is how it gets missed.
func TestFatalLinesAreErrors(t *testing.T) {
	t.Parallel()

	fatals := 0
	for _, line := range corpus(t) {
		record, ok := Parse(line)
		require.True(t, ok)
		if strings.HasPrefix(record.Message, "fatal:") {
			fatals++
			assert.Equal(t, slog.LevelError, record.Level, "line: %s", line)
		}
	}
	assert.GreaterOrEqual(t, fatals, 4)
}

// Colour lands *inside* the level token, so without stripping the level is unrecognised, the
// token is not consumed, and every line is logged at info with escapes in its message. The
// launcher always gives the worker a pipe so this should never fire, but it costs little and a
// pty is one `script` away.
func TestParseStripsColour(t *testing.T) {
	t.Parallel()

	// Captured from `script -qec 'mxl-replicator-worker config.json' /dev/null`.
	line := "[2026-08-27 22:47:02.694] [\x1b[31m\x1b[1merror\x1b[m] fatal: missing required field: metrics_socket\r"

	record, ok := Parse(line)
	require.True(t, ok)
	assert.Equal(t, slog.LevelError, record.Level)
	assert.Equal(t, "fatal: missing required field: metrics_socket", record.Message)
}

// The fallback has to stay a fallback: a line with no timestamp is not recognised, and the
// pump emits it raw rather than dropping it. This covers a continuation line from a message
// that contained a newline, and anything a linked library writes to stdout on its own.
func TestParseLeavesUnrecognisedLinesToTheCaller(t *testing.T) {
	t.Parallel()

	for _, line := range []string{
		"    at some continuation of a previous message",
		"libfabric:1234:1700000000::core:core:ofi_register_provider():474<info> registering",
		"Segmentation fault",
	} {
		_, ok := Parse(line)
		assert.False(t, ok, "line: %s", line)
	}
}
