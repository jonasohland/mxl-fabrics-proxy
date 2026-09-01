package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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

	// **Areas**, and between them the whole of this project's authority over this node's
	// filesystem (§10.6, §13). `read` is where domains are discovered from; `write` is where
	// replication may create them. Neither implies the other, both default false, so a node with
	// no area at all offers no sources and accepts no destinations.
	//
	// *This supersedes `--search-path` and `--output-root`.* One noun with two independent grants
	// is what "read these as a pair" was asking for, and it is what makes a domain's fleet-wide
	// name `<area>/<elements>` — one identity grammar for every domain, whichever direction this
	// project uses it in.
	//
	// *There was also a `-m`/`--map-domain` flag, kept byte-compatible with the legacy proxy's.*
	// It did two jobs at once — granting authority to read a directory, and giving that directory
	// a fleet-wide name — and those separate: the grant is an area's `read` bit, the naming is the
	// area's own name plus what the filesystem already decided (§10.6, §16).
	//
	// sep:"none" because a directory may contain a comma, and kong would otherwise split one
	// area into two halves of nothing.
	Area []string `help:"Declare an area: a directory MXL domains live in, with its grants. e.g. --agent-area media=/dev/shm/mxl:r or fast=/dev/shm/mxl/replicated:rw. Repeatable." sep:"none"`

	// areas is the merged list — the configuration files' `areas:` blocks plus --agent-area —
	// resolved at parse time.
	areas []api.Area

	// What this node can be reached on (§10.1). Repeatable, and *added* to any `fabrics:` block
	// in a configuration file rather than replacing it, so a file describing the hardware can be
	// extended on the command line. Anything with more than a couple of attachments wants the
	// file.
	//
	// sep:"none" because the value's own separator is a comma: kong would otherwise split
	// `provider=verbs,fabric=ib-a` into two flag values, each of which is half an attachment.
	Fabric []string `help:"Declare a fabric attachment, e.g. --agent-fabric provider=verbs,fabric=ib-a,interface=ib0. Repeatable. Naming selectors: address, interface, device, or none when the node has exactly one of that provider; narrowed by network=10.1.0.0/16 and ip_version=4|6, which combine with a name and with each other." sep:"none"`

	// The other answer to "this node configured no attachments", and a better one than shm
	// wherever a fleet is flat enough for the question not to have an operator (§10.1).
	//
	// It is deliberately *only* a replacement for that fallback: an attachment the operator wrote
	// is never joined by a detected one, because the two would then be competing descriptions of
	// the same hardware and the second one is the guess.
	//
	// **The flag names the label because the label is the part with a consequence.** Two nodes
	// pair iff they share one (§10.1), so `default` pairs a node with every other node that also
	// detected and with nothing else — which is why this is a flag and not the behaviour. An
	// operator turning it on is asserting that their fleet is one flat network, a claim they are
	// entitled to make and the server cannot check.
	DetectDefaultFabric bool `help:"When no fabric attachment is configured, detect one from what libfabric reports rather than assuming shm, and label it \"default\". Nodes pair only with other nodes using the same label, so this is for a flat network where every node is reachable from every other."`

	Listen string `help:"Address to serve agent metrics and health on." default:":2284"`

	PortRange ports.Range `help:"Range to allocate fabric ports from. The fabric connection is inbound to the destination node, so this range must be open there." default:"24000-24999"`

	// Agent-local observation policy, and a different knob from the server's two-tier idle
	// policy: this decides when a flow is *reported* as no longer producing, not what the fleet
	// does about it (§11.1).
	FlowIdleAfter time.Duration `help:"Report a flow as no longer producing after its head index has stood still for this long. Coarse on purpose: a raw head index must never reach inventory." default:"3s"`

	// Rate control on worker *starts* (§6.3). Agent-local rather than session-level, unlike the
	// idle and connect timeouts: it describes what this host can absorb while workers are coming
	// up, there is nothing for two nodes to disagree about, and the server could not know the
	// answer for a node it has never run on (§5.5, §6.2).
	//
	// The burst is the number that matters — how many workers may go into setup at the same
	// instant — and the rate bounds the tail of a bulk re-establishment. Zero on the rate means no
	// limit, which is the direction the sentinel has to point: a zero meaning "start nothing"
	// would be a typo that silently stops every flow on the node.
	//
	// The defaults are conservative and a node with headroom should raise them, the burst first:
	// two at once and one every two seconds re-establishes fifty workers in a minute and a half
	// (§6.1, §6.3).
	StartRate  float64 `help:"Workers this node may start per second. 0 means no limit." default:"0.5"`
	StartBurst int     `help:"How many workers may start at once before --agent-start-rate applies." default:"2"`

	WorkerBinary string `help:"Path to the data-plane worker binary." default:"mxl-replicator-worker" env:"MXL_REPLICATOR_WORKER_BINARY"`
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
// Fabric attachments and areas accumulate. It is the shape that makes a file describing a host's
// hardware extensible on the command line without a second way to spell "replace everything".
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
	c.areas = slices.Clone(loaded.Areas)
	for _, raw := range c.Area {
		area, err := config.ParseArea(raw)
		if err != nil {
			return err
		}
		c.areas = append(c.areas, area)
	}

	c.fabrics = slices.Clone(loaded.Fabrics)
	for _, raw := range c.Fabric {
		attachment, err := config.ParseFabric(raw)
		if err != nil {
			return err
		}
		c.fabrics = append(c.fabrics, attachment)
	}

	if len(c.fabrics) == 0 && !c.DetectDefaultFabric {
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
		//
		// --agent-detect-default-fabric is the other answer to the same question, for a node that would
		// rather guess at connectivity than assume it has none: it is checked here rather than
		// resolved here because the guess needs the probe, and the probe belongs to the
		// registration path (§10.5). Left to that path, the two fallbacks stay mutually
		// exclusive by construction.
		c.fabrics = []probe.Attachment{{Provider: api.ProviderSHM}}
		c.fabricsDefaulted = true
	}

	return nil
}

// Validate is called by kong before Run.
func (c *AgentOptions) Validate() error {
	if c.Node == "" {
		return fmt.Errorf("--node is required")
	}
	if len(c.Server) == 0 {
		return fmt.Errorf("--server is required")
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

	// Zero is a mode — no rate control — so only a negative rate is a mistake. A burst below one
	// is refused outright rather than clamped: it is the one setting in this block that would stop
	// the node's media, since a bucket that can never hold a token admits no worker ever, and
	// discovering that at parse time is the difference between a failed start and a node that
	// comes up looking healthy with every session in `starting` (§6.3).
	if c.StartRate < 0 {
		return fmt.Errorf("--start-rate cannot be negative; 0 means no limit")
	}
	if c.StartBurst < 1 {
		return fmt.Errorf("--start-burst must be at least 1; use --start-rate 0 to turn rate control off")
	}

	// The merged picture, and the only place that has one. **The one rule left is that no two
	// areas share a path** (§10.6): areas may nest, and the innermost containing one names a
	// directory, so the only arrangement that rule cannot decide is two areas on one directory.
	//
	// The same function the inventory builds its resolver with, so the rule an operator is held
	// to at parse time and the rule that decides where a target worker writes cannot drift apart.
	if err := inventory.ValidateAreas(c.areas); err != nil {
		return err
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
		"areas", len(c.areas),
		"fabrics", len(c.fabrics),
		"detect_default_fabric", c.DetectDefaultFabric,
		"port_range", c.PortRange.String(),
		"start_rate", c.StartRate,
		"start_burst", c.StartBurst,
		"worker_binary", c.WorkerBinary,
		"work_dir", c.WorkDir)

	if c.StartRate <= 0 {
		// Worth a line: it is not the default, and the symptom it takes the guard off — a node
		// running out of pinned memory or registrations when a bulk re-establishment starts every
		// worker at once — presents as workers failing rather than as a missing setting (§6.3).
		logger.Warn("worker starts are not rate limited on this node",
			"hint", "set --agent-start-rate if a bulk re-establishment exhausts this host")
	}

	if c.fabricsDefaulted {
		logger.Warn("no fabric attachments configured; assuming shm, which only ever pairs with this node",
			"hint", "declare --agent-fabric or a fabrics: block to replicate to other hosts")
	}

	// Inert rather than wrong, so it is warned about rather than refused at parse time. A
	// fallback that is not taken is not a configuration error — and the arrangement that produces
	// this is layered configuration, a base deployment setting the flag and a node adding the
	// `fabrics:` block it turns out to need, where refusing would break the node that got it
	// right.
	if c.DetectDefaultFabric && len(c.fabrics) > 0 {
		logger.Warn("--agent-detect-default-fabric has no effect: this node configured its fabric attachments explicitly",
			"attachments", len(c.fabrics))
	}

	// Bound before the agent starts, and synchronously. The bind is the one startup step that
	// fails for a reason outside this process — the port is already taken — and doing it inside
	// the serve goroutine deferred that error until the goroutine's result was read, which is
	// after Agent.Run returns, which is at shutdown. The agent ran on looking healthy with no
	// metrics endpoint at all, and reported the failure when SIGINT finally let it be read.
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", c.Listen, err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	serving := make(chan error, 1)
	go func() {
		// Serving stopping early is the same class of failure as never starting: the node is
		// unobservable. End the agent with it rather than leave it running unscrapable.
		defer cancel()
		serving <- c.serve(ctx, logger, built, listener)
	}()

	err = built.Run(ctx)

	// Before the receive, not after: Agent.Run can return for reasons of its own, and serve is
	// blocked in Serve until its context ends.
	cancel()

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
		Areas:     c.areas,
		IdleAfter: c.FlowIdleAfter,
		Logger:    logger.With("module", "inventory"),
		OnChange:  func() { built.Notify() },
	})
	if err != nil {
		return nil, err
	}

	built, err = agent.New(agent.Config{
		Node:       c.Node,
		Client:     apiClient,
		Launcher:   launcher,
		Inventory:  inv,
		Ports:      allocator,
		Probe:      c.prober(logger),
		Logger:     logger,
		StartRate:  c.StartRate,
		StartBurst: c.StartBurst,
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
// Areas are deliberately *not* assembled here. They are static agent configuration rather than
// something libfabric reports, the agent already holds them through its inventory, and it is the
// agent that adds them to the registration this returns (§10.2, §10.6).
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

		joined := probe.Join(c.detectFabrics(interfaces, logger), interfaces, probe.Options{
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

// detectFabrics returns the attachments to join against the probe: the configured ones, or the one
// --agent-detect-default-fabric detected out of the probe itself (§10.1).
//
// It runs here rather than at parse time because it is the only place the probe's output exists,
// and it re-runs on every re-registration for the same reason capabilities do (§10.2): a node
// that gained an interface should advertise it, and re-detecting is how it does. That the answer
// can change across a re-registration is the flag's real cost — a node whose tcp address moved
// re-detects onto the new one and every session through it re-establishes, where an operator who
// named the address would have seen the attachment dropped and the reason logged. Explicit
// configuration is still the better answer for anything that has one.
//
// The detection is logged whether or not it succeeded, at info: it is a decision made on the
// operator's behalf, and it is the kind that presents a long way from its cause when it goes
// wrong — as `no_shared_fabric` on some other node.
func (c *AgentOptions) detectFabrics(interfaces []exec.Interface, logger *slog.Logger) []probe.Attachment {
	if len(c.fabrics) > 0 || !c.DetectDefaultFabric {
		return c.fabrics
	}

	detected, skipped := probe.Detect(interfaces, api.DefaultProviderOrder)
	if detected.Provider == "" {
		logger.Error("--agent-detect-default-fabric found nothing usable in what libfabric reports on this node",
			"skipped", skipped,
			"hint", "declare --agent-fabric or a fabrics: block")
		return nil
	}

	logger.Info("fabric attachment detected",
		"provider", detected.Provider,
		"address", detected.Address,
		"fabric", detected.Fabric,
		"skipped", skipped)
	return []probe.Attachment{detected}
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

// serve runs the agent's own HTTP surface: health, and this node's metrics, on an
// already-bound listener. Binding is the caller's, so a port collision is a startup error rather
// than something discovered at shutdown — see Run.
//
// The registry is the agent's own rather than the process default, because a combined instance
// runs a server on a second listener with a second registry and the two expose different things
// (§4.7).
func (c *AgentOptions) serve(ctx context.Context, logger *slog.Logger, built *agent.Agent, listener net.Listener) error {
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
