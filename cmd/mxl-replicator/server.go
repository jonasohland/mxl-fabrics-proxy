package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server"
	"github.com/jonasohland/mxl-replicator/internal/server/negotiate"
	"github.com/jonasohland/mxl-replicator/internal/server/reconcile"
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
