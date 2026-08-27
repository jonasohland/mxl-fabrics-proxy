// Package logging constructs the process logger and translates its level for the worker.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/dpotapov/slogpfx"
	"github.com/lmittmann/tint"
)

// Format selects the log encoding.
type Format string

const (
	// FormatPretty is human-oriented colourised output, with the "module" attribute lifted
	// into a prefix. Carried over from the legacy proxy.
	FormatPretty Format = "pretty"
	// FormatText is slog's key=value encoding.
	FormatText Format = "text"
	// FormatJSON is slog's JSON encoding, for log shippers.
	FormatJSON Format = "json"
)

// Formats lists every valid Format, in the order they should appear in help text.
func Formats() []string {
	return []string{string(FormatPretty), string(FormatText), string(FormatJSON)}
}

// Levels lists every valid level name, in the order they should appear in help text.
func Levels() []string {
	return []string{"debug", "info", "warn", "error"}
}

// Options configures New.
type Options struct {
	Level  slog.Level
	Format Format
	// Output defaults to os.Stderr when nil. The worker logs to stdout (WRS §7) and the
	// agent re-emits those lines through this logger, so keeping the two streams separate
	// is deliberate.
	Output io.Writer
}

// New builds a logger. It returns an error only for an unknown Format; an unknown level is
// rejected earlier, by ParseLevel.
func New(opts Options) (*slog.Logger, error) {
	if opts.Output == nil {
		return nil, fmt.Errorf("logging: no output writer")
	}

	switch opts.Format {
	case FormatPretty:
		return slog.New(
			slogpfx.NewHandler(
				tint.NewHandler(opts.Output, &tint.Options{
					Level:      opts.Level,
					TimeFormat: time.RFC3339,
				}),
				&slogpfx.HandlerOptions{PrefixKeys: []string{"module"}},
			),
		), nil
	case FormatText:
		return slog.New(slog.NewTextHandler(opts.Output, &slog.HandlerOptions{Level: opts.Level})), nil
	case FormatJSON:
		return slog.New(slog.NewJSONHandler(opts.Output, &slog.HandlerOptions{Level: opts.Level})), nil
	default:
		return nil, fmt.Errorf("logging: unknown format %q", opts.Format)
	}
}

// ParseLevel maps a level name onto an slog.Level.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: unknown level %q", name)
	}
}

// WorkerLogLevel renders an slog.Level as a value for the worker's MXL_LOG_LEVEL
// environment variable (§12).
//
// MXL_LOG_LEVEL is the worker's only environment knob: the mxl library calls
// spdlog::cfg::load_env_levels on it (WRS §7). The legacy supervisor never plumbed it
// through, which left the spdlog::debug calls in the transfer loops compiled in but
// permanently silent — one of the concrete things this rewrite fixes.
//
// The names returned here are the ones the agent's log translator expects to parse back out
// of the worker's stdout (WRS §7: trace|debug|info|warning|error), so the two must not
// drift apart.
func WorkerLogLevel(level slog.Level) string {
	switch {
	case level <= slog.LevelDebug:
		return "debug"
	case level <= slog.LevelInfo:
		return "info"
	case level <= slog.LevelWarn:
		return "warning"
	default:
		return "error"
	}
}
