// Package logs translates the worker's log lines into slog records (WRS §7).
//
// The worker never configures a logger. The mxl library installs spdlog's default colour
// logger, named `console`, and everything goes to stdout — including the `fatal:` line that
// explains why a worker is about to exit.
//
// The shape varies more than that description suggests, and the variation was measured against
// a real worker (mxl 1.2.0-dev, libfabric 2.6) rather than assumed:
//
//	[2026-08-27 22:47:02.625] [error] fatal: unknown error: failed to create flow writer
//	[2026-08-27 22:47:01.623] [console] [error] [flow.cpp:244] Failed to create flow : ...
//	[2026-08-27 22:45:04.409] [info] [RCTarget.cpp:32] Setting up RC target with source ...
//	[2026-08-27 22:45:22.285] [info] [libfabric:core:core:372] variable prefer_sysconfig=<not set>
//
// Three things to take from that, all of which this parser is built around:
//
//   - **The logger name is optional, and it varies within a single run.** The first two lines
//     above came out of the same process. A line logged before the mxl instance installs its
//     named default logger carries no name; one logged after carries `[console]`. Requiring
//     either spelling would drop half of a failing worker's output.
//   - **libfabric's diagnostics come through this logger, not raw.** They are formatted as
//     `[libfabric:<subsys>:<provider>:<line>]` in the source-location position, which is why
//     [isSourceLocation] splits on the last colon rather than the first.
//   - **Not everything is guaranteed to be spdlog.** Nothing promises that a library on the
//     link line will not write to stdout directly, so [Parse] reports whether it recognised a
//     line rather than swallowing what it cannot read.
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
	rest := stripANSI(strings.TrimRight(line, "\r\n"))

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

// isSourceLocation reports whether a token looks like a source location: anything without
// spaces, ending in a colon and a line number.
//
// Two shapes occur, and the split is on the **last** colon because of the second:
//
//	[RCTarget.cpp:32]            mxl's own sources
//	[libfabric:core:core:372]    libfabric's, routed through mxl's logger as subsys:prov:line
//
// Splitting on the first colon reads `libfabric` as the file and `core:core:372` as the line
// number, which is not a line number, so the whole token stays in the message.
//
// The rule is deliberately general rather than fitted to those two, because a third shape is
// more likely than not. It is also inherently ambiguous — a message genuinely beginning with
// `[10.0.1.7:24011]` is indistinguishable from a location — and that ambiguity is why the
// failure is arranged to be benign: the token becomes a `location` attribute instead of
// leading text. Nothing is dropped either way.
func isSourceLocation(token string) bool {
	if token == "" || strings.ContainsAny(token, " \t") {
		return false
	}
	colon := strings.LastIndexByte(token, ':')
	if colon <= 0 || colon == len(token)-1 {
		return false
	}
	for _, r := range token[colon+1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// stripANSI removes CSI escape sequences.
//
// The worker's sink suppresses colour when stdout is not a TTY, and the launcher always gives
// it a pipe, so this should never fire in production. It is here because when colour *is*
// emitted it lands **inside the level token** — `[<esc>[31m<esc>[1merror<esc>[m]` — so the
// level would not be recognised, the token would not be consumed, and every line would be
// logged at info with escape codes in its message. Cheap insurance against a stray `script`,
// a pty, or a future spdlog that decides differently.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != 0x1b {
			out.WriteByte(s[i])
			i++
			continue
		}
		i++
		if i < len(s) && s[i] == '[' {
			i++
			// Parameter and intermediate bytes, then a final byte in 0x40-0x7e.
			for i < len(s) && s[i] >= 0x20 && s[i] <= 0x3f {
				i++
			}
			if i < len(s) {
				i++
			}
		}
	}
	return out.String()
}
