// Command mxl-replicator drives MXL Fabrics replication for flows a host does not own.
//
// It ships both control-plane roles in one binary, selected by flags on one command:
//
//	mxl-replicator run               both roles — the default, and the single-host case
//	mxl-replicator run --agent       agent only: every ordinary fleet node
//	mxl-replicator run --server      server only: a control-plane-only node
//
// and the operator-facing verbs beside it, which talk to a running server's user API (§9.1):
//
//	mxl-replicator apply    -f studio-a.yaml [--dry-run] [--prune -n nab [-l show=x]]
//	mxl-replicator delete   -f studio-a.yaml
//	mxl-replicator delete   [-n nab] cam1-distribution
//	mxl-replicator label    domain studio-a:media/cameras role=cameras role-
//	mxl-replicator status   [-o json|yaml]
//	mxl-replicator get      nodes|flows|requests|paths|sessions|namespaces [filters]
//	mxl-replicator describe node|flow|request|path|session|namespace <name>
//
// Three read verbs with three jobs and no overlap: status summarises the fleet and names what is
// not active, get lists so you can find a name, describe explains one thing in full.
//
// Replication is requested by applying a manifest, not by editing a config file on every node:
// the intent is fleet-scoped and lives in the API (§2).
//
// The data plane is a separate binary, mxl-replicator-worker: one process = one flow, one
// direction, one peer, one role. This process never touches grain data.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
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
// Verbs sit at the top level, beside each other. There is no `ctl` grouping and no second binary:
// the set is small, a grouping noun for it would be ceremony, and a separate CLI would be a build
// target, an image entry and a packaging story for no gain.
//
// `run` is the daemon and is the default command, so the word itself is optional — but see
// [guardDefaultCommand] for the trap that creates.
//
// The two *roles* remain flags on `run` (--server / --agent) rather than subcommands of their
// own: they are modules of one binary, they share most of their operational surface, and a node
// running both is an ordinary deployment rather than a special case.
type CLI struct {
	Globals

	Run RunCmd `cmd:"" default:"withargs" help:"Run the server role, the agent role, or both."`

	Apply    ApplyCmd    `cmd:"" help:"Apply a manifest of replication requests."`
	Delete   DeleteCmd   `cmd:"" help:"Cancel replication requests, by manifest or by name."`
	Label    LabelCmd    `cmd:"" help:"Attach or remove labels on one node's domain."`
	Status   StatusCmd   `cmd:"" help:"Summarise the fleet and name anything that is not active."`
	Get      GetCmd      `cmd:"" help:"List nodes, flows, requests, paths, sessions or namespaces."`
	Describe DescribeCmd `cmd:"" help:"Show everything known about one node, flow, request, path, session or namespace."`
}

// commandNames are the verb names, for [guardDefaultCommand].
//
// **Read out of kong's model rather than written down.** A hand-maintained copy of [CLI]'s fields
// is a list that has to be edited in step with a struct that does not mention it, and the failure
// when it is not is silent and confusing: the new verb is not rejected as unknown, it falls through
// to `run` and is diagnosed as a positional argument that command does not want. `label` shipped
// missing from the list and did exactly that.
func commandNames(app *kong.Application) []string {
	var names []string
	for _, child := range app.Children {
		if child.Type == kong.CommandNode {
			names = append(names, child.Name)
		}
	}
	return names
}

// guardDefaultCommand rejects a mistyped verb instead of letting it fall through to `run`.
//
// `run` is kong's `default:"withargs"` command, which is what makes `mxl-replicator --agent`
// work. The cost is that `mxl-replicator aply -f x.yaml` is not an unknown command — it is `run`
// with a positional argument it does not want, and the error says so in terms of flags that have
// nothing to do with what was typed.
func guardDefaultCommand(commands, args []string) error {
	if len(args) == 0 {
		return nil
	}
	first := args[0]
	if strings.HasPrefix(first, "-") || slices.Contains(commands, first) {
		return nil
	}
	return fmt.Errorf("unknown command %q, expected one of %s", first, strings.Join(commands, ", "))
}

// isDaemon reports whether the selected command is the long-running one. kong's Command() is the
// selected path, so `run` is a prefix of it however the flags were spelled.
func isDaemon(command string) bool {
	return command == "run" || strings.HasPrefix(command, "run ")
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

	parser.FatalIfErrorf(guardDefaultCommand(commandNames(parser.Model), os.Args[1:]))

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

	// Only the daemon announces itself. The verbs are foreground commands whose output is the
	// answer, and a startup banner on stderr is noise in front of it.
	if isDaemon(kctx.Command()) {
		logger.Info("starting", "version", version.String(), "command", kctx.Command())
	}

	kctx.BindTo(ctx, (*context.Context)(nil))
	kctx.Bind(logger)

	if err := kctx.Run(); err != nil {
		// A daemon's failure is a log record — it is what an operator will find in the container's
		// logs. A verb's failure is a message to whoever just typed the command, and dressing it
		// as a structured log entry buries the sentence that says what to fix.
		if isDaemon(kctx.Command()) {
			logger.Error("exiting", "error", err)
		} else {
			fmt.Fprintln(os.Stderr, "mxl-replicator: "+err.Error())
		}
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
