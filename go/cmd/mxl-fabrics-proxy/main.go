package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/dpotapov/slogpfx"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/config"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/initiator"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/metrics"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/server"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/target"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/worker"
	"github.com/lmittmann/tint"
)

type Options struct {
	LogLevel      string            `help:"Set the log level" enum:"debug,info,warn,error" default:"info"`
	Node          string            `help:"Local node address" default:"127.0.0.1"`
	Service       string            `help:"Local service address"`
	Listen        string            `short:"l" help:"Listen on this address for requests" default:"127.0.0.1:2283"`
	EFAUseWait    bool              `help:"Use wait completion with EFA provider"`
	Config        []string          `help:"Configuration file to load"`
	Domain        []string          `help:"" short:"d"`
	DomainMapping map[string]string `help:"" short:"m" long:"map-domain"`
	Subscribe     []string          `help:"" short:"s"`
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

func launch(ctx context.Context, wg *sync.WaitGroup, options *Options) error {
	metrics := metrics.NewMetrics()
	domains := initiator.NewDomains()
	subs := initiator.NewSubscriptions(ctx, wg, domains, metrics)
	targets := target.NewTargets(ctx, metrics)

	cfg := &config.Config{}

	for _, domain := range options.Domain {
		if err := cfg.AddDomainFromString(domain); err != nil {
			return err
		}
	}

	for name, domain := range options.DomainMapping {
		if err := cfg.AddDomainMappingFromString(name, domain); err != nil {
			return err
		}
	}

	for _, sub := range options.Subscribe {
		if err := cfg.AddRequestFromString(sub); err != nil {
			return err
		}
	}

	runner, err := config.NewRunner(cfg, options.Config, domains, subs, targets)
	if err != nil {
		return err
	}

	server := server.NewServer(wg, ctx)

	server.Mux(subs)
	server.Mux(domains)
	server.Mux(metrics)
	server.Mux(runner)

	if err := server.StartListening(options.Listen, nil); err != nil {
		return err
	}

	return nil
}

func main() {
	var options Options
	kong.Parse(&options)

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
	if err := launch(ctx, wg, &options); err != nil {
		slog.Error("launch failed", "error", err)
		cancel()
		wg.Wait()
		os.Exit(1)
	}

	<-ctx.Done()
	cancel()
	wg.Wait()
}
