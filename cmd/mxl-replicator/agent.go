package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/agent"
	"github.com/jonasohland/mxl-replicator/internal/agent/inventory"
	"github.com/jonasohland/mxl-replicator/internal/agent/ports"
	"github.com/jonasohland/mxl-replicator/internal/agent/probe"
	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/client"
	"github.com/jonasohland/mxl-replicator/internal/config"
	"github.com/jonasohland/mxl-replicator/internal/logging"
	"github.com/jonasohland/mxl-replicator/internal/metrics"
	"github.com/jonasohland/mxl-replicator/internal/worker/exec"
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

	Server []string  `help:"URL of the mxl-replicator server. Repeatable for an HA deployment." env:"MXL_REPLICATOR_SERVER"`
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

	// What this node can be reached on (§10.1). Repeatable, and *added* to any `fabrics:` block
	// in a configuration file rather than replacing it, so a file describing the hardware can be
	// extended on the command line. Anything with more than a couple of attachments wants the
	// file.
	//
	// sep:"none" because the value's own separator is a comma: kong would otherwise split
	// `provider=verbs,fabric=ib-a` into two flag values, each of which is half an attachment.
	Fabric []string `help:"Declare a fabric attachment, e.g. --agent-fabric provider=verbs,fabric=ib-a,interface=ib0. Repeatable. Selectors: address, interface, device, or none when the node has exactly one of that provider." sep:"none"`

	Listen string `help:"Address to serve agent metrics and health on." default:":2284"`

	PortRange ports.Range `help:"Range to allocate fabric ports from. The fabric connection is inbound to the destination node, so this range must be open there." default:"24000-24999"`

	// Agent-local observation policy, and a different knob from the server's two-tier idle
	// policy: this decides when a flow is *reported* as no longer producing, not what the fleet
	// does about it (§11.1).
	FlowIdleAfter time.Duration `help:"Report a flow as no longer producing after its head index has stood still for this long. Coarse on purpose: a raw head index must never reach inventory." default:"3s"`

	WorkerBinary string `help:"Path to the data-plane worker binary." default:"mxl-fabrics-proxy-worker" env:"MXL_REPLICATOR_WORKER_BINARY"`
	// Fresh directory per worker *start* (not per logical worker): the worker does not
	// unlink a pre-existing metrics socket before binding, so a leftover file from a
	// SIGKILL is a fatal EADDRINUSE (WRS §6). Stale directories are swept at startup.
	WorkDir string `help:"Parent directory for per-worker-instance working directories. Keep the full path well under 108 bytes: it holds an AF_UNIX socket path." default:"/run/mxl-replicator" type:"path"`

	// The worker's only environment knob (§12, WRS §7). Left empty it follows --log-level,
	// so raising the agent to debug also lights up the worker's transfer-loop logging.
	WorkerLogLevel string `help:"MXL_LOG_LEVEL for worker processes. Defaults to following --log-level." enum:",trace,debug,info,warning,error,critical,off" default:""`

	// fabrics is the merged attachment list — the configuration files' `fabrics:` blocks plus
	// --agent-fabric — resolved at parse time.
	fabrics []probe.Attachment

	// fabricsDefaulted records that nothing was configured and shm was assumed, so Run can say
	// so. See resolve.
	fabricsDefaulted bool
}

// resolve loads the configuration files and folds them into the flags.
//
// The rule is that **a flag wins over a file for a scalar, and adds to it for a collection**.
// Domain mappings merge with the flag winning per name; fabric attachments and search paths
// accumulate. It is the shape that makes a file describing a host's hardware extensible on the
// command line without a second way to spell "replace everything".
func (c *AgentOptions) resolve() error {
	loaded, err := config.LoadAgent(c.Config...)
	if err != nil {
		return err
	}
	if err := loaded.Validate(); err != nil {
		return err
	}

	if c.Node == "" {
		c.Node = loaded.Node
	}
	if len(c.Server) == 0 {
		c.Server = loaded.Server
	}
	c.SearchPath = append(slices.Clone(loaded.SearchPaths), c.SearchPath...)

	domains := maps.Clone(loaded.Domains)
	if domains == nil {
		domains = map[string]string{}
	}
	maps.Copy(domains, c.Domains)
	c.Domains = domains

	c.fabrics = slices.Clone(loaded.Fabrics)
	for _, raw := range c.Fabric {
		attachment, err := config.ParseFabric(raw)
		if err != nil {
			return err
		}
		c.fabrics = append(c.fabrics, attachment)
	}

	if len(c.fabrics) == 0 {
		// **Plan decision — no attachments configured means shm.** A node with none can do
		// nothing, so the alternative is refusing to start; but that would make
		// `mxl-replicator run` with no arguments fail, and that invocation is the single-host and
		// development case §2.2 exists to serve.
		//
		// shm is the right assumption rather than a placeholder: it is structurally same-node-only
		// (§10.1), it needs no address and no operator-assigned label because its label is derived
		// from the node name, and same-host replication between two domains is exactly what the
		// legacy loopback.yml scenario does. A node that is meant to reach other hosts declares
		// what it can reach them on, and says so.
		c.fabrics = []probe.Attachment{{Provider: api.ProviderSHM}}
		c.fabricsDefaulted = true
	}

	return nil
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
	if c.FlowIdleAfter <= 0 {
		return fmt.Errorf("--flow-idle-after must be positive")
	}

	for name, path := range c.Domains {
		if name == "" {
			return fmt.Errorf("--map-domain: empty domain name for path %q", path)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("--map-domain %s: path %q must be absolute", name, path)
		}
	}

	return nil
}

func (c *AgentOptions) Run(ctx context.Context, logger *slog.Logger) error {
	logger = logger.With("module", "agent")

	built, err := c.build(ctx, logger)
	if err != nil {
		return err
	}

	logger.Info("agent configuration resolved",
		"node", c.Node,
		"server", c.Server,
		"domains", len(c.Domains),
		"search_paths", len(c.SearchPath),
		"fabrics", len(c.fabrics),
		"port_range", c.PortRange.String(),
		"worker_binary", c.WorkerBinary,
		"work_dir", c.WorkDir)

	if c.fabricsDefaulted {
		logger.Warn("no fabric attachments configured; assuming shm, which only ever pairs with this node",
			"hint", "declare --agent-fabric or a fabrics: block to replicate to other hosts")
	}

	serving := make(chan error, 1)
	go func() { serving <- c.serve(ctx, logger, built) }()

	err = built.Run(ctx)

	if serveErr := <-serving; serveErr != nil && err == nil {
		err = serveErr
	}
	return err
}

// build assembles the agent from its parts.
func (c *AgentOptions) build(ctx context.Context, logger *slog.Logger) (*agent.Agent, error) {
	token, err := c.Auth.Token()
	if err != nil {
		return nil, err
	}

	launcher, err := exec.NewLauncher(exec.Options{
		Binary:   c.WorkerBinary,
		WorkRoot: c.WorkDir,
		LogLevel: c.workerLogLevel(ctx, logger),
		Logger:   logger.With("module", "worker"),
	})
	if err != nil {
		return nil, err
	}

	// Anything under the work root at startup is the debris of an agent that is gone: this one
	// holds no persistent state and adopts nothing (§6.1). Leaving it costs disk and makes the
	// directory unreadable when something does need looking at.
	if err := exec.Sweep(c.WorkDir, logger); err != nil {
		logger.Warn("could not sweep stale worker directories", "error", err)
	}

	allocator, err := ports.NewAllocator(c.PortRange)
	if err != nil {
		return nil, err
	}

	apiClient, err := client.New(client.Options{
		Servers: c.Server,
		Token:   token,
		Logger:  logger.With("module", "client"),
	})
	if err != nil {
		return nil, err
	}

	// The inventory wakes the report loop when a flow appears or its liveness changes. The agent
	// does not exist yet, and cannot: it needs the inventory. The closure is safe because nothing
	// calls it until Agent.Run starts the inventory's own goroutines.
	var built *agent.Agent
	inv, err := inventory.New(inventory.Options{
		Domains:     domainList(c.Domains),
		SearchPaths: c.SearchPath,
		IdleAfter:   c.FlowIdleAfter,
		Logger:      logger.With("module", "inventory"),
		OnChange:    func() { built.Notify() },
	})
	if err != nil {
		return nil, err
	}

	built, err = agent.New(agent.Config{
		Node:      c.Node,
		Client:    apiClient,
		Launcher:  launcher,
		Inventory: inv,
		Ports:     allocator,
		Probe:     c.prober(logger),
		Logger:    logger,
	})
	if err != nil {
		return nil, err
	}
	return built, nil
}

// prober returns the capability probe: what libfabric offers on this host, joined against what
// the operator declared (§10.2, §10.5).
//
// Run at startup and again on every re-registration, never on a heartbeat. The `-v` probe doubles
// as the load probe — it proves the binary exists and its shared libraries resolve before
// anything is assigned to this node — and its versions are not decoration: `target_info` is
// produced by one node's mxl-fabrics and consumed by another's, so a node pair straddling an mxl
// version boundary is a compatibility concern neither agent can detect alone.
func (c *AgentOptions) prober(logger *slog.Logger) func(context.Context) (api.Capabilities, error) {
	return func(ctx context.Context) (api.Capabilities, error) {
		versions, err := exec.ProbeVersions(ctx, c.WorkerBinary)
		if err != nil {
			return api.Capabilities{}, err
		}

		interfaces, err := exec.ProbeInterfaces(ctx, c.WorkerBinary)
		if err != nil {
			return api.Capabilities{}, err
		}

		joined := probe.Join(c.fabrics, interfaces, probe.Options{
			Node:   c.Node,
			Logger: logger.With("module", "probe"),
		})
		if len(joined.Attachments) == 0 {
			// Not fatal: registering with nothing is a node that will fail every negotiation with
			// no_shared_fabric, on the *other* node, which is a long way from the mistake. Say so
			// here, where the candidate list from the join is still in the log above.
			logger.Error("no configured fabric attachment matches what libfabric reports on this node; nothing can be replicated to or from it",
				"configured", len(c.fabrics), "reported", len(interfaces))
		}

		return api.Capabilities{
			Fabrics:   joined.Attachments,
			Versions:  versions,
			SchedPrio: agent.SchedPrioAvailable(),
		}, nil
	}
}

// workerLogLevel resolves MXL_LOG_LEVEL for worker processes (§12).
//
// The worker's only environment knob, and the legacy supervisor never plumbed it through — which
// left every spdlog::debug call in the transfer loops compiled in but permanently silent. Left
// unset it follows the agent's own level, which is read back off the logger rather than threaded
// through as a second copy of the same setting.
func (c *AgentOptions) workerLogLevel(ctx context.Context, logger *slog.Logger) slog.Level {
	if c.WorkerLogLevel != "" {
		if level, err := logging.ParseLevel(c.WorkerLogLevel); err == nil {
			return level
		}
		// spdlog understands names slog does not (trace, critical, off). Map the extremes onto
		// the nearest slog level; WorkerLogLevel renders them back out.
		switch c.WorkerLogLevel {
		case "trace":
			return slog.LevelDebug
		case "critical", "off":
			return slog.LevelError
		}
	}

	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn} {
		if logger.Enabled(ctx, level) {
			return level
		}
	}
	return slog.LevelError
}

// serve runs the agent's own HTTP surface: health, and this node's metrics.
//
// The registry is the agent's own rather than the process default, because a combined instance
// runs a server on a second listener with a second registry and the two expose different things
// (§4.7).
func (c *AgentOptions) serve(ctx context.Context, logger *slog.Logger, built *agent.Agent) error {
	registry := metrics.New()
	registry.MustRegister(built.Collector())

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler(registry, logger.With("module", "metrics")))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		// Deliberately not a replication health check. The legacy proxy had the right instinct:
		// a peer being unreachable is no reason to restart this process and drop every other
		// flow (§11).
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})

	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", c.Listen, err)
	}

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logger.Info("serving agent health and metrics", "address", listener.Addr().String())
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func domainList(domains map[string]string) []inventory.Domain {
	out := make([]inventory.Domain, 0, len(domains))
	for name, path := range domains {
		out = append(out, inventory.Domain{Name: name, Path: path})
	}
	slices.SortFunc(out, func(a, b inventory.Domain) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		default:
			return 0
		}
	})
	return out
}
