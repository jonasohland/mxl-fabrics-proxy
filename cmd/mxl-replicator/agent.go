package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/jonasohland/mxl-replicator/internal/agent/ports"
)

// AgentOptions runs the per-node agent: one agent per node, supervising the worker processes
// for that node's sessions.
//
// What the agent no longer owns, compared to the legacy proxy: subscriptions. That state is
// now API state on the server, which is what collapses the agent's restart rate down to
// upgrades and removes the need for the companion reloader binary (§6.1).
type AgentOptions struct {
	Config []string `help:"YAML configuration file to load. Repeatable; later files win." type:"existingfile"`

	// Must be unique fleet-wide. Two agents claiming one node name is a real failure mode —
	// both receive the same assignments, both start workers, they fight over ports and
	// produce duplicate writes into the destination flow — so the server makes the liveness
	// lease exclusive and rejects the second claimant loudly (§7.1).
	Node string `help:"Node name. Must be unique fleet-wide." env:"MXL_REPLICATOR_NODE"`

	Server []string `help:"URL of the mxl-replicator server. Repeatable for an HA deployment." env:"MXL_REPLICATOR_SERVER"`
	Auth   AuthFlags `embed:""`

	// Kept byte-compatible with the legacy proxy's flag syntax and `domains:` YAML block,
	// so the mapping config carries over unchanged (§16).
	//
	// A destination domain must be a name that appears here. A raw path is never accepted
	// from the API — that invariant is what stops the API from being a remote
	// arbitrary-filesystem-write primitive on every node in the fleet (§7.2, §13).
	Domains map[string]string `help:"Map a domain name to a local path, e.g. -m cameras=/dev/shm/mxl0. Repeatable." short:"m" name:"map-domain"`

	// Optional auto-discovery of unconfigured domains. Discovered domains are reportable as
	// sources but are never usable as replication destinations, for the reason above.
	SearchPath []string `help:"Recursively search these paths for MXL domains. Discovered domains can be replication sources but never destinations." name:"search-path"`

	Listen string `help:"Address to serve agent metrics and health on." default:":2284"`

	PortRange ports.Range `help:"Range to allocate fabric ports from. The fabric connection is inbound to the destination node, so this range must be open there." default:"24000-24999"`

	WorkerBinary string `help:"Path to the data-plane worker binary." default:"mxl-fabrics-proxy-worker" env:"MXL_REPLICATOR_WORKER_BINARY"`
	// Fresh directory per worker *start* (not per logical worker): the worker does not
	// unlink a pre-existing metrics socket before binding, so a leftover file from a
	// SIGKILL is a fatal EADDRINUSE (WRS §6). Stale directories are swept at startup.
	WorkDir string `help:"Parent directory for per-worker-instance working directories. Keep the full path well under 108 bytes: it holds an AF_UNIX socket path." default:"/run/mxl-replicator" type:"path"`

	Idle IdleFlags `embed:""`

	// The worker's only environment knob (§12, WRS §7). Left empty it follows --log-level,
	// so raising the agent to debug also lights up the worker's transfer-loop logging.
	WorkerLogLevel string `help:"MXL_LOG_LEVEL for worker processes. Defaults to following --log-level." enum:",trace,debug,info,warning,error,critical,off" default:""`
}

// Validate is called by kong before Run.
func (c *AgentOptions) Validate() error {
	if c.Node == "" {
		return fmt.Errorf("--node is required: it is the node's fleet-wide unique name")
	}
	if len(c.Server) == 0 {
		return fmt.Errorf("--server is required: at least one server URL")
	}
	if _, err := c.Auth.Token(); err != nil {
		return err
	}

	if c.PortRange.IsZero() {
		return fmt.Errorf("--port-range is not set")
	}

	for name, path := range c.Domains {
		if name == "" {
			return fmt.Errorf("--map-domain: empty domain name for path %q", path)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("--map-domain %s: path %q must be absolute", name, path)
		}
	}

	if c.Idle.IdleTeardown < 0 {
		return fmt.Errorf("--idle-teardown must not be negative")
	}

	return nil
}

func (c *AgentOptions) Run(ctx context.Context, logger *slog.Logger) error {
	logger = logger.With("module", "agent")
	logger.Info("agent configuration resolved",
		"node", c.Node,
		"server", c.Server,
		"domains", len(c.Domains),
		"search_paths", len(c.SearchPath),
		"port_range", c.PortRange.String(),
		"worker_binary", c.WorkerBinary,
		"idle_timeout", c.Idle.IdleTimeout,
		"idle_teardown", c.Idle.IdleTeardown,
	)

	return errNotImplemented{role: "agent", milestone: "M5"}
}
