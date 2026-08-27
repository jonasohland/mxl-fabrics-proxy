// Command mxl-replicator drives MXL Fabrics replication for flows a host does not own.
//
// It ships both control-plane roles in one binary, selected by flags on one command:
//
//	mxl-replicator run               both roles — the default, and the single-host case
//	mxl-replicator run --agent       agent only: every ordinary fleet node
//	mxl-replicator run --server      server only: a control-plane-only node
//
// The data plane is a separate binary, mxl-fabrics-proxy-worker, which keeps its name: one
// process = one flow, one direction, one peer, one role. This process never touches grain
// data.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/jonasohland/mxl-replicator/internal/logging"
	"github.com/jonasohland/mxl-replicator/internal/version"
)

// Globals are the flags every role shares.
type Globals struct {
	LogLevel  string           `help:"Log level." enum:"debug,info,warn,error" default:"info" env:"MXL_REPLICATOR_LOG_LEVEL"`
	LogFormat string           `help:"Log output format." enum:"pretty,text,json" default:"pretty" env:"MXL_REPLICATOR_LOG_FORMAT"`
	Version   kong.VersionFlag `help:"Print version information and exit." short:"V"`
}

// CLI is the root command.
//
// There is one command. The two roles are selected by flags on it (--server / --agent)
// rather than being subcommands of their own: they are modules of one binary, they share
// most of their operational surface, and a node running both is an ordinary deployment
// rather than a special case. `run` is the default command, so the word itself is optional.
type CLI struct {
	Globals

	Run RunCmd `cmd:"" default:"withargs" help:"Run the server role, the agent role, or both."`
}

func main() {
	var cli CLI

	parser := kong.Must(&cli,
		kong.Name("mxl-replicator"),
		kong.Description("Replicate MXL flows between hosts over MXL Fabrics."),
		// Short usage, not full: these roles carry enough flags that dumping the whole list
		// buries the actual error.
		kong.ShortUsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true}),
		kong.Vars{"version": version.String()},
	)

	kctx, err := parser.Parse(os.Args[1:])
	parser.FatalIfErrorf(err)

	logger, err := logging.New(logging.Options{
		Level:  mustParseLevel(cli.LogLevel),
		Format: logging.Format(cli.LogFormat),
		Output: os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mxl-replicator:", err)
		os.Exit(1)
	}
	slog.SetDefault(logger)

	// SIGINT/SIGTERM cancel the context; every role shuts down from that one signal. The
	// worker gets the same treatment from the agent: SIGTERM with a 5s grace period.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info("starting", "version", version.String(), "command", kctx.Command())

	kctx.BindTo(ctx, (*context.Context)(nil))
	kctx.Bind(logger)

	if err := kctx.Run(); err != nil {
		logger.Error("exiting", "error", err)
		os.Exit(1)
	}
}

// mustParseLevel cannot fail: kong has already constrained the flag to the enum.
func mustParseLevel(name string) slog.Level {
	level, err := logging.ParseLevel(name)
	if err != nil {
		panic(err)
	}
	return level
}
