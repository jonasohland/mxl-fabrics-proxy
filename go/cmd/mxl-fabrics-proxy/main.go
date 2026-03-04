package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/dpotapov/slogpfx"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/initiator"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/server"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/target"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/worker"
	"github.com/lmittmann/tint"
)

type CommandServe struct {
	DomainURL     []string `arg:"" help:"Domains that should be served by this initiator"`
	ListenAddress string   `short:"l" help:"Listen on this address for target requests" default:"127.0.0.1:2283"`
}

type CommandSubscribe struct {
	Subscription []string `arg:"" help:"URL of the local domain that the flows should be written to."`
	Provider     string   `short:"p" help:"Provider to use for the subscription (tcp,shm,verbs,efa)"`
}

type Options struct {
	LogLevel  string           `help:"Set the log level" enum:"debug,info,warn,error" default:"info"`
	Node      string           `help:"Local node address" default:"127.0.0.1"`
	Service   string           `help:"Local service address"`
	Serve     CommandServe     `cmd:"" help:"Serve flows in a domain to one or more subscribers"`
	Subscribe CommandSubscribe `cmd:"" help:"Subscribe to one or more flows from a server"`
}

type Context struct {
	Ctx     context.Context
	Wg      *sync.WaitGroup
	Node    string
	Service string
}

func (o *Options) ToContext(ctx context.Context, wg *sync.WaitGroup) *Context {
	return &Context{
		Ctx:     ctx,
		Wg:      wg,
		Node:    o.Node,
		Service: o.Service,
	}
}

func createLogger() *slog.Logger {
	return slog.New(
		slogpfx.NewHandler(
			tint.NewHandler(
				os.Stderr,
				&tint.Options{
					TimeFormat: time.RFC3339,
				},
			),
			&slogpfx.HandlerOptions{
				PrefixKeys: []string{"module"},
			},
		),
	)
}

func (c *CommandServe) Run(ctx *Context) error {
	server := server.NewServer(ctx.Wg, ctx.Ctx)

	domains := initiator.NewDomains()
	server.Mux(domains)

	subscriptions := initiator.NewSubscriptions(
		ctx.Ctx, ctx.Wg, domains,
		initiator.Config{Node: ctx.Node, Service: ctx.Service})

	server.Mux(subscriptions)

	for _, url := range c.DomainURL {
		if err := domains.Add(url); err != nil {
			return fmt.Errorf("add domain '%s': %w", url, err)
		}
	}

	if err := server.StartListening(c.ListenAddress, nil); err != nil {
		return err
	}

	return nil
}

func (c *CommandSubscribe) Run(ctx *Context) error {
	targets := target.NewTargets(ctx.Ctx, ctx.Wg, target.Config{Node: ctx.Node, Service: ctx.Service, Provider: c.Provider})

	for _, url := range c.Subscription {
		parts := strings.SplitN(url, "@", 2)
		if len(parts) != 2 {
			return errors.New("invalid subscription mapping format")
		}

		if err := targets.Create(parts[0], parts[1]); err != nil {
			return err
		}
	}

	return nil
}

func main() {
	var options Options
	kctx := kong.Parse(&options)

	slog.SetDefault(createLogger())

	versions, err := worker.GetVersions()
	if err != nil {
		slog.Error("failed to locate and run worker executable", "error", err)
		os.Exit(1)
	}

	wg := &sync.WaitGroup{}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.Info("starting", "proxy-version", versions.Proxy, "mxl-version", versions.MXL, "libfabric-version", versions.Libfabric)

	if err := kctx.Run(options.ToContext(ctx, wg)); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}

	<-ctx.Done()
	wg.Wait()
}
