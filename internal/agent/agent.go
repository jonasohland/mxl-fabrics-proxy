package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/agent/inventory"
	"github.com/jonasohland/mxl-replicator/internal/agent/ports"
	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/client"
	"github.com/jonasohland/mxl-replicator/internal/version"
	"github.com/jonasohland/mxl-replicator/internal/worker"
)

// Defaults for the knobs an operator has no reason to set.
//
// Note which knobs are *absent* here: the worker idle timeout and the long-idle teardown
// threshold are session-level and live on the server, because both ends of a session must be
// handed identical values and a value two nodes could disagree about is a bug rather than a
// configuration choice (§5.5, §10.3, M4).
const (
	// DefaultPollWait is how long the agent asks the server to hold an assignment poll open. The
	// server caps it; both must stay below any intermediate proxy's idle timeout (§9.2).
	DefaultPollWait = 25 * time.Second

	// DefaultReportInterval is the backstop for reporting. Worker state changes wake the report
	// loop immediately — that is the establishment path — so this only bounds how long an
	// inventory change can go unreported.
	DefaultReportInterval = 500 * time.Millisecond

	// DefaultRestartWindow is the span over which worker restarts are counted, which is what the
	// server classifies DEGRADED and FAILED from (§15.1). Never from an exit code.
	DefaultRestartWindow = 5 * time.Minute

	// DefaultBackoffMin and DefaultBackoffMax bound the restart backoff (§11.1 mechanism 3).
	//
	// It replaces the legacy flat 3 s, and it is the catch-all for everything the other two idle
	// mechanisms do not anticipate: if a worker dies repeatedly for any reason, the cycle
	// stretches toward minutes rather than sitting at a flat few seconds, which is what keeps
	// §8.3's write-volume sizing true and the logs readable.
	DefaultBackoffMin = time.Second
	DefaultBackoffMax = 2 * time.Minute

	// DefaultBackoffReset is how long a worker must live before its next death starts the backoff
	// over. Time to death is the signal: dying in under a second every attempt is a permanent
	// error, dying after minutes of healthy transfer is transient (§15.1).
	DefaultBackoffReset = time.Minute

	// DefaultTargetInfoTimeout bounds the wait for a target's blob. The worker writes it as soon
	// as its fabric endpoint is bound, before the receive loop starts (WRS §5.1), so a target
	// that has not produced one in this long is not coming up.
	DefaultTargetInfoTimeout = 30 * time.Second

	// DefaultStopGrace bounds how long the agent waits for a worker to go away before it stops
	// waiting. The launcher still gets rid of the process; this only bounds patience.
	DefaultStopGrace = 10 * time.Second

	// DefaultRegisterBackoffMax bounds retries of a registration that keeps being refused —
	// most importantly a node name another instance is holding (§7.1).
	DefaultRegisterBackoffMax = 30 * time.Second

	// DefaultStartRate and DefaultStartBurst pace worker starts (§6.3).
	//
	// Not applied here: [Config.StartRate] zero means *no limit*, so an operational default cannot
	// live in a setDefault the way a duration's does. It is the flag's default
	// (`--agent-start-rate`), the same place `--agent-flow-idle-after` and `--agent-port-range`
	// keep theirs, and these constants exist so the two spellings cannot drift.
	//
	// **Deliberately conservative, and it costs real re-establishment time.** Two at once and one
	// every two seconds means a node re-establishing 50 workers is not done for a minute and a
	// half, where §6.1 budgets 1–2 s for a flow. That is the trade taken on purpose: the failure
	// being prevented takes out the *whole node* — every worker on it, including the ones that
	// were already running fine — while what it costs is a slower recovery on a node that is
	// already recovering. A deployment with headroom should raise it; the number to raise first is
	// the burst, because that is the one the host is actually measured against.
	DefaultStartRate  = 0.5
	DefaultStartBurst = 2
)

// Config configures an [Agent].
type Config struct {
	// Node is this node's fleet-wide unique name (§7.1).
	Node string

	// Instance is a fresh identifier per agent *process*, generated when empty. It is what makes
	// a lease claim attributable, so that a second claimant of a node name is rejected rather
	// than quietly taking over.
	Instance string

	// Client speaks the agent API. Required.
	Client *client.Client

	// Launcher starts workers. Required, and the only way this package starts anything.
	Launcher worker.Launcher

	// Inventory observes this node's domains and flows. Required.
	Inventory *inventory.Inventory

	// Ports allocates fabric services from the configured range (§7.4). Required.
	Ports *ports.Allocator

	// Probe reports what this node can actually do: fabric attachments joined against the
	// worker's --interfaces output, and the worker's own version triple (§10.2, §10.5).
	//
	// Called at startup and again on every re-registration, never on a heartbeat. Only the
	// fields the node itself knows need filling in; the agent adds its protocol and build
	// versions and its port range.
	Probe func(ctx context.Context) (api.Capabilities, error)

	Logger *slog.Logger
	Now    func() time.Time

	PollWait          time.Duration
	ReportInterval    time.Duration
	RestartWindow     time.Duration
	BackoffMin        time.Duration
	BackoffMax        time.Duration
	BackoffReset      time.Duration
	TargetInfoTimeout time.Duration
	StopGrace         time.Duration

	// StartRate and StartBurst pace worker starts on this node (§6.3).
	//
	// StartRate is starts per second and **zero means no limit** — the sentinel points that way
	// deliberately, because a zero meaning "admit nothing" would be a typo that silently stops
	// every flow on the node. StartBurst is how many workers may go into setup at the same
	// instant, which is the number the exhaustion this exists for is measured against.
	//
	// Agent-local, and not a session-level knob like the idle timeouts above (§5.5, §6.2): it
	// describes what this host can absorb, there is nothing for two nodes to agree about, and the
	// server could not know the answer for a node it has never run on.
	StartRate  float64
	StartBurst int

	// ScrapeConcurrency is how many worker sockets [Agent.Collector] reads at once.
	ScrapeConcurrency int

	// WorkerScrapeTimeout and ScrapeTimeout bound one worker's scrape and a whole collection
	// (§12). The second is the one that keeps wedged workers from costing the endpoint.
	WorkerScrapeTimeout time.Duration
	ScrapeTimeout       time.Duration
}

// Agent is one node's control-plane presence and worker supervisor.
type Agent struct {
	cfg Config
	log *slog.Logger

	// mu guards units, rejected, outputs and caps. reconcile is the only writer of units, and it
	// runs on one goroutine, so this protects readers rather than serialising reconciles.
	mu       sync.Mutex
	units    map[unitKey]*unit
	rejected map[unitKey]string
	caps     api.Capabilities

	// outputs are the domains currently materialised on this node, resolved path → rendered name
	// (§10.6). Derived from the target assignments on every reconcile rather than counted, so it
	// cannot drift from the workers it exists for.
	outputs map[string]string

	// starts paces worker starts (§6.3). Read by every supervision goroutine and by the metrics
	// collector; set once in New and never replaced.
	starts *startGate

	// notify wakes the report loop. Capacity one and a non-blocking send: it is a "something
	// changed" edge, and coalescing several into one report is the point.
	notify chan struct{}

	// root is the context every supervised worker hangs off: the one [Agent.Run] was given, not
	// a poll's and not a reconcile pass's. Set once, before anything reads it.
	//
	// This is the fail-static invariant made structural. A worker whose life is bound to the pass
	// that started it is torn down by a cancelled poll, so a re-registration or a lost server
	// would stop media — §4.2 read backwards. Workers are ended by [unit.stop], and by this
	// context only when the whole agent is going away.
	root context.Context

	// reported holds the last snapshot successfully accepted for each report, so an unchanged
	// one is never sent again.
	//
	// The server writes what it is given without comparing, and every write advances the store
	// revision and wakes every watcher in the fleet — including every agent's assignment poll,
	// where a spurious wakeup costs a reconcile. So compare-before-send is not an optimisation
	// here, it is what keeps §8.3's steady state at zero writes.
	reportedInventory []byte
	reportedStatus    []byte
}

// New validates the configuration and builds an agent. It talks to nothing until [Agent.Run].
func New(cfg Config) (*Agent, error) {
	switch {
	case cfg.Node == "":
		return nil, errors.New("agent: no node name")
	case cfg.Client == nil:
		return nil, errors.New("agent: no client")
	case cfg.Launcher == nil:
		return nil, errors.New("agent: no worker launcher")
	case cfg.Inventory == nil:
		return nil, errors.New("agent: no inventory")
	case cfg.Ports == nil:
		return nil, errors.New("agent: no port allocator")
	case cfg.Probe == nil:
		return nil, errors.New("agent: no capability probe")
	case cfg.StartRate < 0:
		// Zero is a value with a meaning — no rate control at all — so it cannot double as
		// "unset", and a negative one is a mistake rather than a third mode (§6.3).
		return nil, errors.New("agent: negative worker start rate")
	}

	if cfg.Instance == "" {
		cfg.Instance = rand.Text()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	setDefault(&cfg.PollWait, DefaultPollWait)
	setDefault(&cfg.ReportInterval, DefaultReportInterval)
	setDefault(&cfg.RestartWindow, DefaultRestartWindow)
	setDefault(&cfg.BackoffMin, DefaultBackoffMin)
	setDefault(&cfg.BackoffMax, DefaultBackoffMax)
	setDefault(&cfg.BackoffReset, DefaultBackoffReset)
	setDefault(&cfg.TargetInfoTimeout, DefaultTargetInfoTimeout)
	setDefault(&cfg.StopGrace, DefaultStopGrace)
	setDefault(&cfg.WorkerScrapeTimeout, DefaultWorkerScrapeTimeout)
	setDefault(&cfg.ScrapeTimeout, DefaultScrapeTimeout)
	if cfg.ScrapeConcurrency <= 0 {
		cfg.ScrapeConcurrency = DefaultScrapeConcurrency
	}

	return &Agent{
		cfg:      cfg,
		log:      cfg.Logger,
		units:    map[unitKey]*unit{},
		rejected: map[unitKey]string{},
		starts:   newStartGate(cfg.StartRate, cfg.StartBurst),
		notify:   make(chan struct{}, 1),
	}, nil
}

func setDefault(field *time.Duration, value time.Duration) {
	if *field <= 0 {
		*field = value
	}
}

// Notify wakes the report loop. Safe to call from anywhere, including from the inventory's own
// goroutines, and never blocks.
func (a *Agent) Notify() {
	select {
	case a.notify <- struct{}{}:
	default:
	}
}

func (a *Agent) now() time.Time { return a.cfg.Now() }

// Run observes, registers and supervises until ctx ends.
//
// The loop is registration-shaped rather than connection-shaped: a session with the server runs
// until something says this agent's identity is gone, at which point it registers again. Workers
// are deliberately *not* part of that scope — they hang off ctx, so losing a lease, losing the
// server, or re-registering leaves running media exactly where it is (§4.2).
func (a *Agent) Run(ctx context.Context) error {
	a.root = ctx

	// At startup, so only the leaf MkdirAll for an output domain is ever on the establishment
	// path (§6.1, §10.6). *There used to be a second call beside it, pre-creating the configured
	// domain directories; there are no configured domains any more (§6).*
	if err := a.cfg.Inventory.CreateAreas(); err != nil {
		return fmt.Errorf("agent: %w", err)
	}

	var observing sync.WaitGroup
	observing.Go(func() {
		if err := a.cfg.Inventory.Run(ctx); err != nil {
			a.log.Error("flow observation stopped", "error", err)
		}
	})

	a.log.Info("agent starting",
		"node", a.cfg.Node, "instance", a.cfg.Instance,
		"servers", a.cfg.Client.Servers(), "port_range", a.cfg.Ports.Range())

	for ctx.Err() == nil {
		session, ok := a.register(ctx)
		if !ok {
			break
		}
		a.serve(ctx, session)
	}

	// Stop the workers on the way out. The agent execs them as children, so leaving them behind
	// would leave ports, memory registrations and flows held by processes nothing is supervising.
	a.stopAll()
	observing.Wait()
	return nil
}

// session is one accepted registration.
type session struct {
	lease     string
	heartbeat time.Duration
}

// areas renders this node's configured areas for registration (§10.2, §10.6).
//
// Empty is the ordinary case and not an error: a node with no area at all offers no sources and
// accepts no destinations, which is the correct posture for the one piece of configuration that
// grants this project any authority over a host's filesystem. The server refuses any request
// aimed at it with a reason that says so, which is the right place for that to surface — an
// operator setting up a destination hears about it when they ask for one.
//
// **Readable areas are advertised as well as writable ones**, which is a change (§10.2): a
// domain's name is `<area>/<elements>`, so a server that does not hold this table cannot render a
// domain's identity or resolve a name in a request.
func (a *Agent) areas() []api.Area {
	return a.cfg.Inventory.Areas()
}

// register loops until the server accepts this node, or ctx ends.
//
// Every attempt re-probes (§10.5): capabilities are static in the sense that they change only by
// re-registering, and re-registering is exactly when they are worth re-reading — an attachment
// that came back, a worker binary that was upgraded underneath a running agent.
func (a *Agent) register(ctx context.Context) (session, bool) {
	backoff := a.cfg.BackoffMin

	for ctx.Err() == nil {
		capabilities, err := a.probe(ctx)
		if err == nil {
			// Areas are not probed: they are static agent configuration, and the probe reports
			// what the host offers rather than what the operator permits (§10.2, §10.6). They are
			// added here so that every registration and re-registration advertises the current
			// set, and so that what this node advertises is read off the same inventory its
			// destination resolver answers from — one list, not two that agree today.
			capabilities.Areas = a.areas()

			var accepted *api.RegistrationResponse
			accepted, err = a.cfg.Client.Register(ctx, api.NodeRegistration{
				Node:         a.cfg.Node,
				Instance:     a.cfg.Instance,
				Capabilities: capabilities,
			})
			if err == nil {
				a.setCapabilities(capabilities)
				return a.accepted(accepted), true
			}
		}

		if ctx.Err() != nil {
			return session{}, false
		}

		switch {
		case client.IsNodeClaimed(err):
			// Loud, every time, and never fatal. Two agents claiming one node name is a
			// copy-pasted config or an overlapping rollout, and the loser must not start workers
			// — but the holder may go away, so this keeps asking rather than exiting (§7.1).
			a.log.Error("another agent instance holds this node name; not starting any workers",
				"node", a.cfg.Node, "instance", a.cfg.Instance,
				"holder", client.Detail(err, "holder"))
		case client.IsVersionSkew(err):
			// The one direction the compatibility promise does not cover: this agent is newer
			// than the server, and the server is always upgraded first (§13.1).
			a.log.Error("this server cannot serve this agent's protocol version; upgrade the server first",
				"protocol", api.ProtocolVersion, "error", err)
		default:
			a.log.Warn("registration failed; retrying", "error", err)
		}

		if !sleep(ctx, backoff) {
			return session{}, false
		}
		backoff = nextBackoff(backoff, a.cfg.BackoffMin, DefaultRegisterBackoffMax)
	}

	return session{}, false
}

// probe asks the node what it can do, and stamps on what only this process knows.
func (a *Agent) probe(ctx context.Context) (api.Capabilities, error) {
	capabilities, err := a.cfg.Probe(ctx)
	if err != nil {
		return api.Capabilities{}, fmt.Errorf("probe this node's capabilities: %w", err)
	}

	capabilities.Versions.Protocol = api.ProtocolVersion
	capabilities.Versions.Replicator = version.String()
	// Diagnostics only: the agent allocates and reports what it actually bound, and the server is
	// deliberately kept out of a job it cannot verify (§7.4).
	capabilities.PortRange = a.cfg.Ports.Range().String()

	return capabilities, nil
}

func (a *Agent) accepted(accepted *api.RegistrationResponse) session {
	heartbeat := accepted.HeartbeatInterval.Duration()
	if heartbeat <= 0 {
		heartbeat = 5 * time.Second
	}

	a.log.Info("registered",
		"node", a.cfg.Node,
		"heartbeat", heartbeat,
		"lease_ttl", accepted.TTL.Duration(),
		"server_version", accepted.Server.Replicator,
		"server_protocol", accepted.Server.Protocol)

	if accepted.Server.Protocol < api.ProtocolVersion {
		// Visible in agent logs and not only server-side, because an operator debugging a
		// replication that will not come up looks here first.
		a.log.Warn("this agent is newer than the server it registered with; the server is always upgraded first",
			"agent_protocol", api.ProtocolVersion, "server_protocol", accepted.Server.Protocol)
	}

	return session{lease: accepted.Lease, heartbeat: heartbeat}
}

func (a *Agent) setCapabilities(capabilities api.Capabilities) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.caps = capabilities
}

func (a *Agent) capabilities() api.Capabilities {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.caps
}

// serve runs one registration's worth of work: heartbeat, reports and the assignment poll.
//
// It returns when any of the three concludes this agent's identity is gone, and the caller
// registers again. Workers are untouched by that: their contexts come from Run's, not from here,
// because a lease that expired says the fleet has forgotten this node, not that its media should
// stop (§4.2, §7.1).
func (a *Agent) serve(ctx context.Context, sess session) {
	// Observed state is written under the lease. A new lease means the old key may be gone, so
	// nothing may be suppressed as "already reported".
	a.forgetReports()

	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var running sync.WaitGroup
	for _, loop := range []func(context.Context, session){a.heartbeatLoop, a.reportLoop, a.pollLoop} {
		running.Go(func() {
			defer cancel()
			loop(sessCtx, sess)
		})
	}
	running.Wait()
}

// heartbeatLoop renews the liveness lease (§7.1).
//
// A failed heartbeat is not an event: the lease expires on its own if they keep failing, and the
// next report comes back asking to re-register. Treating a transport failure as a reason to do
// anything would be fail-static read backwards.
func (a *Agent) heartbeatLoop(ctx context.Context, sess session) {
	ticker := time.NewTicker(sess.heartbeat)
	defer ticker.Stop()

	beat := api.Heartbeat{Node: a.cfg.Node, Instance: a.cfg.Instance, Lease: sess.lease}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		accepted, err := a.cfg.Client.Heartbeat(ctx, beat)
		switch {
		case ctx.Err() != nil:
			return
		case err == nil && accepted.Reregister:
			a.log.Warn("the server no longer holds this node's lease; registering again")
			return
		case err == nil:
		case a.lostIdentity(err, "heartbeat"):
			return
		default:
			a.log.Warn("heartbeat failed", "error", err)
		}
	}
}

// reportLoop sends inventory and status snapshots as they change.
func (a *Agent) reportLoop(ctx context.Context, sess session) {
	ticker := time.NewTicker(a.cfg.ReportInterval)
	defer ticker.Stop()

	for {
		if lost := a.report(ctx); lost {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-a.notify:
			// A worker changed state. This is the establishment path — a target's epoch reaches
			// the server from here, and the peer's initiator cannot be assigned until it does
			// (§5.3), so it does not wait for a tick.
		}
	}
}

// report sends whichever snapshots differ from the last one the server accepted.
func (a *Agent) report(ctx context.Context) (lostIdentity bool) {
	inventorySnapshot := api.InventorySnapshot{
		Node:     a.cfg.Node,
		Instance: a.cfg.Instance,
		Domains:  a.cfg.Inventory.Snapshot(),
	}
	statusSnapshot := a.statusSnapshot()

	if encoded, changed := a.inventoryChanged(inventorySnapshot); changed {
		if err := a.cfg.Client.ReportInventory(ctx, inventorySnapshot); err != nil {
			return a.reportFailed(ctx, err, "inventory")
		}
		a.acceptedInventory(encoded)
	}

	if encoded, changed := a.statusChanged(statusSnapshot); changed {
		if err := a.cfg.Client.ReportStatus(ctx, statusSnapshot); err != nil {
			return a.reportFailed(ctx, err, "status")
		}
		a.acceptedStatus(encoded)
	}

	return false
}

func (a *Agent) reportFailed(ctx context.Context, err error, what string) bool {
	if ctx.Err() != nil {
		return true
	}
	if a.lostIdentity(err, what) {
		return true
	}
	// Deliberately nothing else. The snapshot is not marked as sent, so the next pass tries
	// again; a report that cannot be delivered says nothing about whether the workers it
	// describes should keep running.
	a.log.Warn("report failed", "report", what, "error", err)
	return false
}

// lostIdentity reports whether an error means this node's registration or lease is gone, and
// logs it. It is never a teardown signal (§4.2).
func (a *Agent) lostIdentity(err error, what string) bool {
	switch {
	case client.IsReregister(err):
		a.log.Warn("the server has forgotten this node; registering again, workers keep running",
			"during", what, "error", err)
		return true
	case client.IsNodeClaimed(err):
		a.log.Error("another agent instance has taken this node name",
			"during", what, "node", a.cfg.Node, "holder", client.Detail(err, "holder"))
		return true
	default:
		return false
	}
}

// pollLoop is the fail-static reconcile loop (§4.2, invariant 1).
//
// The structure is the invariant. A poll either produces an assignment set or produces an error,
// and [Agent.reconcile] is called on exactly one branch — there is no path on which a failure and
// an empty set reach the same call, and no timeout after which this agent gives up and tears
// workers down. A server outage of any duration leaves running sessions running.
func (a *Agent) pollLoop(ctx context.Context, sess session) {
	cursor := int64(0)
	backoff := a.cfg.BackoffMin

	for {
		if ctx.Err() != nil {
			return
		}

		set, err := a.cfg.Client.Assignments(ctx, a.cfg.Node, cursor, a.cfg.PollWait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if a.lostIdentity(err, "assignment poll") {
				return
			}
			if client.IsNotReady(err) {
				// Not an empty set. The server is settling, has no observed state to reconcile
				// against, or holds a view behind this cursor — all of which mean "I do not
				// know yet", and none of which is a reason to change anything (plan §4.2).
				a.log.Debug("the server is not ready to answer; nothing changes", "error", err)
			} else {
				a.log.Warn("assignment poll failed; nothing changes", "error", err)
			}

			if !sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, a.cfg.BackoffMin, a.cfg.BackoffMax)
			continue
		}

		backoff = a.cfg.BackoffMin
		cursor = set.Revision
		// a.root, deliberately: reconcile starts workers, and a worker started on this loop's
		// context would be stopped by the next re-registration (§4.2).
		a.reconcile(a.root, set)
	}
}

// statusSnapshot renders every session this agent is running, as the server sees it (§9.2).
//
// Every session it is running, not merely the ones it was assigned in this server's process
// lifetime: that is what lets a restarted server recognise a worker by its session ID and adopt
// it rather than issuing a fresh assignment and glitching media that was fine (§7.3).
//
// Deterministically ordered, because it is compared against the last one sent.
func (a *Agent) statusSnapshot() api.StatusSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.now()
	sessions := make([]api.SessionStatus, 0, len(a.units)+len(a.rejected))

	for _, u := range a.units {
		sessions = append(sessions, u.status(now, a.cfg.RestartWindow))
	}
	for key, reason := range a.rejected {
		// An assignment this agent cannot turn into a worker. Reported as a failed session
		// rather than dropped, because the alternative is a path that sits in ESTABLISHING with
		// no explanation anywhere the operator can see.
		sessions = append(sessions, api.SessionStatus{
			SessionID: key.Session,
			Role:      key.Role,
			State:     api.WorkerFailed,
			Reason:    reason,
		})
	}

	sortSessions(sessions)
	return api.StatusSnapshot{Node: a.cfg.Node, Instance: a.cfg.Instance, Sessions: sessions}
}

func (a *Agent) inventoryChanged(snapshot api.InventorySnapshot) ([]byte, bool) {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return encoded, !equal(a.reportedInventory, encoded)
}

func (a *Agent) statusChanged(snapshot api.StatusSnapshot) ([]byte, bool) {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return encoded, !equal(a.reportedStatus, encoded)
}

func (a *Agent) acceptedInventory(encoded []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reportedInventory = encoded
}

func (a *Agent) acceptedStatus(encoded []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reportedStatus = encoded
}

func (a *Agent) forgetReports() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reportedInventory, a.reportedStatus = nil, nil
}

func equal(a, b []byte) bool {
	return a != nil && string(a) == string(b)
}

// sleep waits, reporting false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current, minimum, maximum time.Duration) time.Duration {
	if current < minimum {
		current = minimum
	}
	return min(current*2, maximum)
}
