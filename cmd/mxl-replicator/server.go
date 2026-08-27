package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"
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
		return fmt.Errorf("--lease-ttl (%s) must be greater than --heartbeat-interval (%s), or every agent expires between heartbeats", c.LeaseTTL, c.HeartbeatInterval)
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

	return nil
}

// SettlingWindow is the delay before the first reconcile (§7.3).
func (c *ServerOptions) SettlingWindow() time.Duration {
	return time.Duration(c.SettlingHeartbeats) * c.HeartbeatInterval
}

func (c *ServerOptions) Run(ctx context.Context, logger *slog.Logger) error {
	logger = logger.With("module", "server")
	logger.Info("server configuration resolved",
		"listen", c.Listen,
		"store", c.Store.Backend,
		"tls", c.TLS.Enabled(),
		"heartbeat", c.HeartbeatInterval,
		"lease_ttl", c.LeaseTTL,
		"settling_window", c.SettlingWindow(),
		"provider_order", c.ProviderOrder,
	)

	return errNotImplemented{role: "server", milestone: "M4"}
}
