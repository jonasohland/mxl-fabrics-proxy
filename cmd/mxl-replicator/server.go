package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server"
	"github.com/jonasohland/mxl-replicator/internal/server/negotiate"
	"github.com/jonasohland/mxl-replicator/internal/server/reconcile"
	"github.com/jonasohland/mxl-replicator/ui"
)

// ServerOptions runs the central connection management server.
//
// Agents register with it, report the flows they observe, and poll it for assignments.
// There is no agent-to-agent traffic on the control plane at all.
type ServerOptions struct {
	Config []string `help:"YAML configuration file to load. Repeatable; later files win." type:"existingfile"`

	Listen string `help:"Address to serve the user API (/v1) and the agent API (/agent/v1) on." default:":2283" env:"MXL_REPLICATOR_LISTEN"`

	Store StoreFlags `embed:"" prefix:"store-"`
	Auth  AuthFlags  `embed:""`
	TLS   TLSFlags   `embed:"" prefix:"tls-"`

	// Off by default, because the assets are the one part of this server that something else may
	// already be serving: the supported deployments are this binary, or a proxy fronting both the
	// app and the API on one origin, and turning it on unasked would put a second copy behind the
	// second shape (`ui.md` §6).
	UI bool `help:"Serve the bundled web UI at /. Requires a binary built with the UI assets (make ui)." env:"MXL_REPLICATOR_UI"`

	HeartbeatInterval time.Duration `help:"Interval at which agents are expected to renew their liveness lease." default:"5s"`
	LeaseTTL          time.Duration `help:"Liveness lease TTL. Should be a small multiple of --heartbeat-interval." default:"15s"`

	// The settling window is expressed as a multiple of the heartbeat rather than as an
	// absolute duration so it scales with the configured heartbeat instead of being an
	// unrelated magic number (§7.3).
	SettlingHeartbeats int `help:"Wait this many heartbeat intervals before the first reconcile, so a server restart or leader change adopts running sessions instead of re-establishing them. Ends early once every leased agent has reported." default:"3"`

	// §10.4: an explicit provider is honoured or the request fails, and is never silently
	// substituted. This is only the order used when a request does not pin one.
	ProviderOrder []string `help:"Provider preference order used when a request does not pin a provider." default:"efa,verbs,tcp,shm"`

	MaxLongPollWait time.Duration `help:"Upper bound on the assignment long-poll hold time (§9.2). Must stay below any intermediate proxy's idle timeout." default:"30s"`

	EventRingSize int `help:"Event-log entries kept per path, request and node (§12.1). Bounded by count, never by age: an age bound expires the overnight failure exactly when someone arrives to read it." default:"50"`
	LogTailBytes  int `help:"Largest worker log tail this server will store per path (§12.2). Anything over it is truncated at the head, keeping the fatal line." default:"8192"`

	// The one part of the event log whose volume is set by the fleet rather than by the control
	// plane, and therefore the one part with a switch (§12.1). Entries are batched per reconcile
	// pass, so a churning node costs one entry per kind per pass rather than one per flow.
	InventoryEvents bool `help:"Record flows and domains appearing and disappearing on each node's event log (§12.1). Off with --no-server-inventory-events, for a fleet whose producers churn constantly." default:"true" negatable:""`

	// The session-level worker settings. They live here rather than on the agent because the
	// library performs no negotiation of its own and both ends of a session must be handed
	// identical values — a value two agents could disagree about is a bug, not a configuration
	// choice (§5.5, §10.3).
	Idle IdleFlags `embed:""`

	ConnectTimeout time.Duration `help:"How long an initiator waits for its target's endpoint before giving up. 0 waits indefinitely." default:"60s"`

	// Must match on both ends or the target reports garbage latency with no error at all
	// (WRS §5.3). Session-level for exactly that reason.
	NoNetworkLatencyMeasurement bool `help:"Disable the worker's network latency measurement. Applied to both ends of every session."`

	DegradedRestarts int `help:"Worker restarts in the classification window before a session is reported DEGRADED." default:"3"`
	FailedRestarts   int `help:"Worker restarts in the classification window before a session is reported FAILED." default:"10"`
}

// Validate is called by kong before Run.
func (c *ServerOptions) Validate() error {
	if err := c.Store.Validate(); err != nil {
		return err
	}
	if err := c.TLS.Validate(); err != nil {
		return err
	}
	if _, err := c.Auth.Token(); err != nil {
		return err
	}

	// Refused at startup rather than serving an empty `/`. Whether a binary carries the app is
	// decided by whether the node build ran before the go one, which is not something an operator
	// staring at a blank page can see — so the flag either has assets behind it or the process
	// does not start.
	if _, err := c.uiAssets(); err != nil {
		return err
	}

	if c.LeaseTTL <= c.HeartbeatInterval {
		return fmt.Errorf("--lease-ttl (%s) must be greater than --heartbeat-interval (%s)", c.LeaseTTL, c.HeartbeatInterval)
	}
	if c.SettlingHeartbeats < 0 {
		return fmt.Errorf("--settling-heartbeats must not be negative")
	}
	if len(c.ProviderOrder) == 0 {
		return fmt.Errorf("--provider-order must not be empty")
	}
	for _, provider := range c.ProviderOrder {
		switch provider {
		case "efa", "verbs", "tcp", "shm":
		default:
			return fmt.Errorf("--provider-order: unknown provider %q", provider)
		}
	}

	if c.Idle.IdleTimeout < 0 || c.Idle.IdleTeardown < 0 || c.ConnectTimeout < 0 {
		return fmt.Errorf("timeouts must not be negative")
	}
	if c.DegradedRestarts <= 0 {
		return fmt.Errorf("--degraded-restarts must be positive")
	}
	if c.FailedRestarts < c.DegradedRestarts {
		return fmt.Errorf("--failed-restarts (%d) must be at least --degraded-restarts (%d)", c.FailedRestarts, c.DegradedRestarts)
	}

	return nil
}

// providerOrder converts the configured order into API values. The flag is validated above, so
// every entry is a provider this project knows.
func (c *ServerOptions) providerOrder() []api.Provider {
	out := make([]api.Provider, 0, len(c.ProviderOrder))
	for _, provider := range c.ProviderOrder {
		out = append(out, api.Provider(provider))
	}
	return out
}

// uiAssets returns the embedded app when --server-ui asked for it, nil when it did not, and an
// error when it was asked for and this binary has none.
func (c *ServerOptions) uiAssets() (fs.FS, error) {
	if !c.UI {
		return nil, nil
	}
	assets, ok := ui.Assets()
	if !ok {
		return nil, fmt.Errorf("--server-ui: this binary was built without the web UI; run `make ui` and build again")
	}
	return assets, nil
}

// SettlingWindow is the delay before the first reconcile (§7.3).
func (c *ServerOptions) SettlingWindow() time.Duration {
	return time.Duration(c.SettlingHeartbeats) * c.HeartbeatInterval
}

func (c *ServerOptions) Run(ctx context.Context, logger *slog.Logger) error {
	logger = logger.With("module", "server")

	instance, err := c.build(ctx, logger)
	if err != nil {
		return err
	}
	defer instance.close()

	return instance.server.Run(ctx)
}

// instance is a constructed server and the resources it borrows.
type instance struct {
	server *server.Server
	close  func()
}

// build opens the store, elects a leader candidate and constructs the server.
func (c *ServerOptions) build(ctx context.Context, logger *slog.Logger) (*instance, error) {
	token, err := c.Auth.Token()
	if err != nil {
		return nil, err
	}

	// Validate has already rejected --server-ui on a binary without assets; this is the same call
	// for its value, so that build stands on its own when it is used outside the CLI.
	assets, err := c.uiAssets()
	if err != nil {
		return nil, err
	}

	replica := server.ReplicaName()

	backing, elector, closeStore, err := c.Store.Open(ctx, replica, c.LeaseTTL, logger)
	if err != nil {
		return nil, err
	}

	logger.Info("server configuration resolved",
		"listen", c.Listen,
		"store", c.Store.Backend,
		"tls", c.TLS.Enabled(),
		"auth", token != "",
		"ui", assets != nil,
		"replica", replica,
		"heartbeat", c.HeartbeatInterval,
		"lease_ttl", c.LeaseTTL,
		"settling_window", c.SettlingWindow(),
		"provider_order", c.ProviderOrder,
		"idle_timeout", c.Idle.IdleTimeout,
		"idle_teardown", c.Idle.IdleTeardown,
	)

	srv, err := server.New(server.Config{
		Store:              backing,
		Elector:            elector,
		Logger:             logger,
		Listen:             c.Listen,
		TLSCert:            c.TLS.Cert,
		TLSKey:             c.TLS.Key,
		Token:              token,
		HeartbeatInterval:  c.HeartbeatInterval,
		LeaseTTL:           c.LeaseTTL,
		SettlingHeartbeats: c.SettlingHeartbeats,
		MaxLongPollWait:    c.MaxLongPollWait,
		EventRingSize:      c.EventRingSize,
		LogTailBytes:       c.LogTailBytes,
		UI:                 assets,

		NoInventoryEvents: !c.InventoryEvents,
		Reconcile: reconcile.Config{
			Negotiate:                   negotiate.Config{Order: c.providerOrder()},
			IdleTimeout:                 c.Idle.IdleTimeout,
			IdleTeardown:                c.Idle.IdleTeardown,
			ConnectTimeout:              c.ConnectTimeout,
			NoNetworkLatencyMeasurement: c.NoNetworkLatencyMeasurement,
			DegradedRestarts:            c.DegradedRestarts,
			FailedRestarts:              c.FailedRestarts,
		},
	})
	if err != nil {
		closeStore()
		return nil, err
	}

	return &instance{server: srv, close: closeStore}, nil
}
