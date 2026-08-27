// Package logs translates the worker's log lines into slog records (WRS §7).
//
// The worker never configures a logger. The mxl library installs spdlog's default colour
// logger, named `console`, and the lines that come out have spdlog's default pattern plus
// mxl's own source-location prefixes:
//
//	[2026-08-27 14:03:11.842] [console] [info] connected
//	[2026-08-27 14:03:11.900] [console] [debug] [Flow.cpp:88] opened
//
// Everything goes to stdout, including the `fatal:` line that explains why a worker is about
// to exit, and libfabric's own diagnostics are routed into the same stream. So the stream is
// not uniformly spdlog, and that is the reason [Parse] reports whether it recognised a line
// rather than swallowing what it cannot read.
package logs

import (
	"log/slog"
	"strings"
	"time"
)

// timeLayout is spdlog's default timestamp format.
const timeLayout = "2006-01-02 15:04:05.000"

// loggerName is what mxl calls its logger. It carries no information, so it is dropped.
const loggerName = "console"

// Parse turns one line of worker output into an slog record.
//
// ok is false for a line this package does not recognise — a libfabric diagnostic, a line
// wrapped by something in between, anything at all. **The caller must still emit those.** The
// legacy translator dropped every line it could not parse, which is an efficient way to lose
// the one message that explains why a worker will not start.
//
// The record's time is the worker's own timestamp, so a line reads with the time the worker
// logged it rather than the time the agent got round to it.
func Parse(line string) (slog.Record, bool) {
	rest := strings.TrimRight(line, "\r\n")

	stamp, after, ok := bracketed(rest)
	if !ok {
		return slog.Record{}, false
	}
	at, err := time.ParseInLocation(timeLayout, stamp, time.Local)
	if err != nil {
		// Not a spdlog line. Anything could be here — this is also the stream libfabric writes
		// its own diagnostics to (WRS §2).
		return slog.Record{}, false
	}
	rest = after

	if name, after, ok := bracketed(rest); ok && name == loggerName {
		rest = after
	}

	level := slog.LevelInfo
	if name, after, ok := bracketed(rest); ok {
		if parsed, known := Level(name); known {
			level, rest = parsed, after
		}
	}

	// mxl prefixes some lines with a source location. Only consume a bracket that looks like
	// one: unlike the legacy parser, which consumed every leading bracket and therefore ate
	// the first token of any message that happened to start with '[' (WRS §7).
	location := ""
	if token, after, ok := bracketed(rest); ok && isSourceLocation(token) {
		location, rest = token, after
	}

	record := slog.NewRecord(at, level, strings.TrimLeft(rest, " "), 0)
	if location != "" {
		record.AddAttrs(slog.String("location", location))
	}
	return record, true
}

// Level maps a spdlog level name onto an slog level.
//
// This is the inverse of logging.WorkerLogLevel, which renders the agent's own level into
// MXL_LOG_LEVEL for the worker's environment. The two are one contract read in two directions
// and a test pins them together: a level the agent can ask for and then not recognise coming
// back would silently misreport every line the worker logs at it.
//
// spdlog's `critical` has no slog equivalent above error, so it maps to error.
func Level(name string) (slog.Level, bool) {
	switch name {
	case "trace", "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "err", "error", "critical":
		return slog.LevelError, true
	default:
		return 0, false
	}
}

// bracketed consumes a leading `[token]`, returning the token and what follows.
func bracketed(s string) (token, rest string, ok bool) {
	s = strings.TrimLeft(s, " ")
	if !strings.HasPrefix(s, "[") {
		return "", s, false
	}
	end := strings.IndexByte(s, ']')
	if end < 0 {
		return "", s, false
	}
	return s[1:end], s[end+1:], true
}

// isSourceLocation reports whether a token looks like `Flow.cpp:88`.
func isSourceLocation(token string) bool {
	file, line, found := strings.Cut(token, ":")
	if !found || file == "" || line == "" || strings.ContainsAny(file, " \t") {
		return false
	}
	for _, r := range line {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
