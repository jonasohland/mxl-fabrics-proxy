package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/dpotapov/slogpfx"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/reload"
	"github.com/lmittmann/tint"
)

type Options struct {
	Config   []string      `help:"Configuration file or directory to watch (repeatable)" required:""`
	Server   string        `short:"s" help:"Address of the proxy to reload" default:"127.0.0.1:2283"`
	Interval time.Duration `help:"Interval at which the configuration is checked for changes" default:"1s"`
	Debounce time.Duration `help:"Time the configuration must stay unchanged before a reload is triggered" default:"2s"`
	Timeout  time.Duration `help:"Timeout for a single reload request" default:"30s"`
	LogLevel string        `help:"Set the log level" enum:"debug,info,warn,error" default:"info"`
}

func createLogger(logLevel string) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		level = slog.LevelInfo
	}

	return slog.New(
		slogpfx.NewHandler(
			tint.NewHandler(
				os.Stderr,
				&tint.Options{
					Level:      level,
					TimeFormat: time.RFC3339,
				},
			),
			&slogpfx.HandlerOptions{
				PrefixKeys: []string{"module"},
			},
		),
	)
}

func main() {
	var options Options
	kong.Parse(&options)

	slog.SetDefault(createLogger(options.LogLevel))

	watcher, err := reload.NewWatcher(reload.Options{
		Paths:    options.Config,
		Server:   options.Server,
		Interval: options.Interval,
		Debounce: options.Debounce,
		Timeout:  options.Timeout,
	})
	if err != nil {
		slog.Error("failed to create watcher", "error", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	watcher.Run(ctx)
}
