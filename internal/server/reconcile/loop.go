package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
	"github.com/jonasohland/mxl-replicator/internal/store"
)

// LoopOptions configures the reconcile loop that wraps [Compute] and [Apply].
type LoopOptions struct {
	Store  store.Store
	Config Config
	Logger *slog.Logger

	// Leader identifies this replica in the published reconciler record. Diagnostics only —
	// leadership is held by the elector, not by that key.
	Leader string

	// Heartbeat is the interval agents renew their leases at. It sizes the settling window and
	// the periodic re-reconcile.
	Heartbeat time.Duration

	// SettlingHeartbeats is how many heartbeat intervals to wait before the first reconcile
	// (§7.3), ending early once every leased agent has reported.
	//
	// Deterministic session IDs are necessary but not sufficient on a restart: the server has
	// desired state and **no observed state**, so a reconcile that runs immediately concludes no
	// sessions exist and issues fresh assignments for sessions that are already running — a new
	// nonce, a new epoch and a needless glitch on every server restart and every leader change.
	SettlingHeartbeats int

	// Now is the clock, injectable for tests.
	Now func() time.Time

	// Hooks report what the loop did, for metrics. Optional.
	Hooks Hooks

	// Journal is where this loop records what it observed (§12.1). Optional: nil disables the
	// event log entirely, which is what the read-only handlers and most tests want.
	Journal Journal

	// NoInventoryEvents suppresses the flow and domain entries of §12.1 on each node's ring.
	//
	// **Spelled negatively so that the zero value is the default behaviour**, which is on — the
	// same shape [Config.NoNetworkLatencyMeasurement] takes, and for the same reason: a
	// default-constructed config must behave the way the product does, or an embedder silently
	// gets something the flags say they would not.
	//
	// It has a switch at all because this is the one part of the log whose volume is set by the
	// **fleet** rather than by the control plane: a node's flows are whatever its producers are
	// doing.
	NoInventoryEvents bool
}

// Journal is the event log as the reconcile loop needs it (§12.1).
//
// An interface rather than the concrete recorder, for the same reason [Hooks] is a set of
// callbacks: it keeps this package testable without a store behind it, and it makes the
// dependency one-way — the log observes the reconciler and the reconciler never reads the log,
// which is what keeps [Compute] a pure function of one snapshot (§7.3).
type Journal interface {
	// Record appends a batch to one object's ring. Called at most once per object per pass:
	// store revisions are not free (§8.1).
	Record(ctx context.Context, key string, batch ...api.Event) error

	// ForgetPath drops everything retained about a path, because a path's log dies with the
	// path (§12.1).
	ForgetPath(ctx context.Context, pathID string) error
}

// Outcome is how one reconcile pass ended.
type Outcome string

const (
	// OutcomeOK is a pass that computed and applied.
	OutcomeOK Outcome = "ok"

	// OutcomeRefused is the store-wipe guard declining to act: leased agents, no observed state
	// (plan §4.2). Correct, and invisible without a count — the fleet keeps running on assignments
	// nobody is rewriting, so nothing else in the system looks wrong.
	OutcomeRefused Outcome = "refused"

	// OutcomeFailed is a pass that could not read or write the store.
	OutcomeFailed Outcome = "failed"
)

// Hooks are the loop's metric callbacks (§12).
//
// Callbacks rather than a registry here, so this package stays free of a metrics dependency and
// its tests can assert on what the loop *decided* without gathering an exposition. Nil functions
// are not called.
type Hooks struct {
	// Pass is called once per completed pass.
	Pass func(outcome Outcome, took time.Duration)

	// EpochChanged is called once per session seen to change epoch, with the node hosting the
	// target that produced it. Never called for a session acquiring its first epoch, which is
	// establishment rather than a transition.
	EpochChanged func(node string)
}

func (h Hooks) pass(outcome Outcome, took time.Duration) {
	if h.Pass != nil {
		h.Pass(outcome, took)
	}
}

func (h Hooks) epochChanged(node string) {
	if h.EpochChanged != nil {
		h.EpochChanged(node)
	}
}

// Loop runs Compute → Apply whenever anything relevant changes.
type Loop struct {
	opts LoopOptions
	idle *idleTracker

	mu      sync.Mutex
	settled bool
	leading bool

	// observed is the last completed pass, for metrics (§12), and epochs is what that pass saw
	// each session's epoch to be.
	//
	// Both are leader-local memory, for the same reason [idleTracker] is: the alternative is
	// putting a continuously-changing value into the store, which is the churn §8.3's sizing
	// depends on not existing. A leader change resets them, and the first pass after one reports
	// no epoch transitions — which is right, because this replica did not see one.
	observed *Observation
	epochs   map[string]string

	// journal is the previous pass's view, for computing transitions. Leader-local for the same
	// reason as the two above, and reset on losing leadership so that the next leader declares a
	// gap rather than inventing transitions across it.
	journal *journal
}

// Observation is what one reconcile pass saw, kept so that metrics can be served without a
// second full read of the store (§12).
//
// Counts rather than the objects they were counted from: this is retained between passes, and
// holding a whole [Result] would keep every path, session and assignment alive for as long as
// the leader runs.
type Observation struct {
	At       time.Time
	Duration time.Duration
	Revision int64

	Nodes  int
	Leases int

	// Versions counts leased agents by what they reported at registration. The fleet's version
	// spread is something §13.1 asks the server to surface, and it is the server's alone to know
	// — an agent cannot see another agent's build.
	Versions map[VersionKey]int

	// Requests, Paths and Workers are counts by status. Every status in each vocabulary is
	// present, including the zeroes: a state that vanished from the exposition because nothing is
	// in it reads as a gap in the graph rather than a floor.
	Requests map[api.State]int
	Paths    map[api.State]int
	Workers  map[api.WorkerState]int

	// Frozen is sessions carried forward because an endpoint's agent is not leased — the
	// mechanism that keeps a control-plane blip from stopping media, worth seeing when it engages.
	Frozen int

	// EpochChanges counts sessions whose epoch differed from the previous pass, by the node
	// hosting the target that produced it.
	//
	// This is the flapping signal the epoch cannot carry on its own: it is a content hash with no
	// ordering, so nothing downstream can tell a new incarnation from a reordering without
	// remembering the old value, and only the reconciler sees every session (§5.2, §12).
	EpochChanges map[string]int
}

// VersionKey identifies one build of the agent, for counting the fleet's spread.
type VersionKey struct {
	Replicator string
	Protocol   int
}

// Observation returns the last completed pass, or nil if this replica has not reconciled — which
// is the ordinary state of a follower, since only the leader runs a loop.
func (l *Loop) Observation() *Observation {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.observed
}

// Leading reports whether this replica is running the reconciler.
func (l *Loop) Leading() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.leading
}

func (l *Loop) lead(leading bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.leading = leading
	if !leading {
		l.observed, l.epochs, l.settled = nil, nil, false
		l.journal.reset()
	}
}

// NewLoop builds a reconcile loop. It does nothing until [Loop.Run] is called, which the server
// does only while it holds leadership.
func NewLoop(opts LoopOptions) *Loop {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = 5 * time.Second
	}
	opts.Config.Now = opts.Now

	return &Loop{
		opts:    opts,
		idle:    newIdleTracker(opts.Now),
		journal: newJournal(!opts.NoInventoryEvents),
	}
}

// SettlingWindow is how long the first reconcile is held back for.
func (l *Loop) SettlingWindow() time.Duration {
	return time.Duration(l.opts.SettlingHeartbeats) * l.opts.Heartbeat
}

// Settled reports whether this replica's loop has completed a reconcile. It is local knowledge;
// what the *fleet* goes by is the published record, because followers do not run a loop at all.
func (l *Loop) Settled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.settled
}

// Config returns the loop's policy, for callers that need to compute a read-only view with the
// same settings.
func (l *Loop) Config() Config { return l.opts.Config }

// Run reconciles until ctx ends. It returns nil on cancellation — losing leadership is an
// ordinary event, not a failure.
func (l *Loop) Run(ctx context.Context) error {
	l.lead(true)
	// Leadership ending drops the observation with it. A demoted replica that kept exporting the
	// fleet gauges would put a second, frozen copy of every one of them next to the new leader's
	// live ones, and nothing in the exposition would say which was which (§12).
	defer l.lead(false)

	for {
		err := l.session(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			// A watch that ended, a store that hiccuped. Nothing is torn down when a reconcile
			// does not happen: the store still holds the assignments the agents are already
			// running, and they are level-triggered against those.
			l.opts.Hooks.pass(OutcomeFailed, 0)
			l.opts.Logger.Warn("reconcile loop restarting", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(l.opts.Heartbeat):
			}
		}
	}
}

// session is one watch's worth of reconciling: settle, then reconcile on every change.
func (l *Loop) session(ctx context.Context) error {
	fleet, err := state.Load(ctx, l.opts.Store)
	if err != nil {
		return err
	}

	// The watch is established from the snapshot's own revision, so nothing that happens between
	// the read and the watch is missed. That handoff is a property the store guarantees and the
	// conformance suite pins; building the cursor any other way leaves a window in which a
	// change is silently skipped (§9.2).
	//
	// Scoped to the same prefix the snapshot loads, which is what keeps this loop from waking
	// itself: this pass writes events (§12.1), and a watch on the whole store would see those
	// writes and reconcile again for them. It would converge — the second pass changes nothing and
	// writes nothing — but it would double every pass that recorded anything, and it would put the
	// event log on the establishment path, which is the one place §12.1 must not reach.
	events, err := l.opts.Store.Watch(ctx, store.PrefixSnapshot, fleet.Revision+1)
	if err != nil {
		return err
	}

	if err := l.settle(ctx, fleet); err != nil {
		return err
	}

	ticker := time.NewTicker(l.opts.Heartbeat)
	defer ticker.Stop()

	for {
		if err := l.once(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-events:
			if !ok {
				return errors.New("store watch closed")
			}
			if event.Err != nil {
				return event.Err
			}
			// Coalesce: one agent's report is several keys, and a lease expiring is every key
			// that agent wrote at one revision. Reconciling once per event would be correct and
			// wasteful; reconciling once per burst is both.
			drain(events)
		case <-ticker.C:
			// The periodic pass exists for the transitions no write announces: a source that has
			// now been idle long enough to tear down, a session that has been coming up for too
			// long. Everything else arrives as an event.
		}
	}
}

func drain(events <-chan store.Event) {
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()

	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-timer.C:
			return
		}
	}
}

// settle holds back the first reconcile until the fleet has had a chance to report (§7.3).
//
// It ends early — as soon as every leased agent has reported both inventory and status — because
// the window is a bound on how long to wait for the answer, not a delay for its own sake. A
// fleet that reports promptly reconciles promptly.
func (l *Loop) settle(ctx context.Context, fleet *state.Fleet) error {
	window := l.SettlingWindow()
	if window <= 0 || l.Settled() {
		return nil
	}

	deadline := l.opts.Now().Add(window)
	l.opts.Logger.Info("settling before the first reconcile",
		"window", window, "leased_agents", len(fleet.Leases))

	poll := max(l.opts.Heartbeat/5, 50*time.Millisecond)

	for {
		if allReported(fleet) {
			l.opts.Logger.Info("settled early: every leased agent has reported",
				"agents", len(fleet.Leases))
			return nil
		}
		remaining := deadline.Sub(l.opts.Now())
		if remaining <= 0 {
			l.opts.Logger.Info("settling window elapsed", "reported", reportedCount(fleet), "leased", len(fleet.Leases))
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(poll, remaining)):
		}

		var err error
		if fleet, err = state.Load(ctx, l.opts.Store); err != nil {
			return err
		}
	}
}

func allReported(fleet *state.Fleet) bool {
	for node := range fleet.Leases {
		if _, ok := fleet.Inventory[node]; !ok {
			return false
		}
		if _, ok := fleet.Status[node]; !ok {
			return false
		}
	}
	return true
}

func reportedCount(fleet *state.Fleet) int {
	n := 0
	for node := range fleet.Leases {
		if _, ok := fleet.Inventory[node]; ok {
			n++
		}
	}
	return n
}

// once is a single load → compute → apply pass.
func (l *Loop) once(ctx context.Context) error {
	started := l.opts.Now()

	fleet, err := state.Load(ctx, l.opts.Store)
	if err != nil {
		return err
	}
	for _, bad := range fleet.Malformed {
		l.opts.Logger.Error("ignoring an unreadable key", "key", bad.Key, "error", bad.Err)
	}

	// **A fleet with leased agents and no observed state is a store that lost its contents, not
	// a fleet with nothing to do** (plan §4.2). Acting on it would compute an empty assignment
	// set for every node and tear down every worker in the fleet — successfully, from each
	// agent's point of view, which is exactly why fail-static on the agent side cannot catch it.
	if len(fleet.Leases) > 0 && len(fleet.Inventory) == 0 {
		l.opts.Logger.Warn("refusing to reconcile: agents are leased but none has reported inventory",
			"leased", len(fleet.Leases))
		l.opts.Hooks.pass(OutcomeRefused, l.opts.Now().Sub(started))
		return nil
	}

	cfg := l.opts.Config
	cfg.Idle = l.idle.observe

	result := Compute(fleet, cfg)
	l.idle.retain(result.Paths)

	changes, err := Apply(ctx, l.opts.Store, fleet, result)
	if err != nil {
		return err
	}

	if changes.Any() {
		l.opts.Logger.Info("reconciled",
			"revision", fleet.Revision,
			"paths", len(result.Paths),
			"sessions", len(result.Sessions),
			"sessions_written", changes.SessionsWritten,
			"sessions_deleted", changes.SessionsDeleted,
			"assignments_written", changes.AssignmentsWritten,
			"assignments_deleted", changes.AssignmentsDeleted,
		)
	}
	if len(result.Frozen) > 0 {
		l.opts.Logger.Info("holding sessions whose endpoints cannot be observed",
			"sessions", len(result.Frozen))
	}

	l.record(ctx, fleet, result)

	l.observe(fleet, result, l.opts.Now().Sub(started))
	if err := l.publish(ctx, fleet); err != nil {
		return err
	}

	l.opts.Hooks.pass(OutcomeOK, l.opts.Now().Sub(started))
	return nil
}

// record writes what this pass changed to the event log (§12.1).
//
// **One store write per object, never one per event.** The journal returns a batch, this groups it
// by ring, and each ring is written once — which is what keeps the log from becoming the writer
// that breaks §8.3, and what keeps it from burning the revisions sqlite's watch history is bounded
// by (§8.1).
//
// Failures are logged and swallowed. This is a diagnostic aid, not an audit log (§12.1), and a
// reconcile that refused to finish because it could not write down what it had done would trade
// media for bookkeeping. Called after Apply for the same reason [Loop.observe] is: a pass that
// failed to write has nothing to report about.
func (l *Loop) record(ctx context.Context, fleet *state.Fleet, result *Result) {
	if l.opts.Journal == nil {
		return
	}

	entries, dropped := l.journal.diff(fleet, result, l.opts.Now())

	batches := map[string][]api.Event{}
	order := make([]string, 0, len(entries))
	for _, e := range entries {
		if _, seen := batches[e.key]; !seen {
			order = append(order, e.key)
		}
		batches[e.key] = append(batches[e.key], e.event)
	}

	for _, key := range order {
		if err := l.opts.Journal.Record(ctx, key, batches[key]...); err != nil {
			l.opts.Logger.Warn("recording events failed", "key", key, "error", err)
		}
	}

	for _, gone := range dropped {
		if err := l.opts.Journal.ForgetPath(ctx, gone.pathID); err != nil {
			l.opts.Logger.Warn("dropping a path's event log failed", "path", gone.pathID, "error", err)
		}
	}
}

// observe records what this pass saw.
//
// Called after Apply rather than before, so that a pass which failed to write leaves the previous
// observation in place. A gauge describing a reconcile that did not complete would be worse than
// a stale one, because the failure counter is what says a pass failed.
func (l *Loop) observe(fleet *state.Fleet, result *Result, took time.Duration) {
	l.mu.Lock()
	previous := l.epochs
	l.mu.Unlock()

	observation := &Observation{
		At:           l.opts.Now(),
		Duration:     took,
		Revision:     fleet.Revision,
		Nodes:        len(fleet.Nodes),
		Leases:       len(fleet.Leases),
		Versions:     map[VersionKey]int{},
		Requests:     zeroed(api.RequestStates()),
		Paths:        zeroed(api.States()),
		Workers:      zeroed(api.WorkerStates()),
		Frozen:       len(result.Frozen),
		EpochChanges: map[string]int{},
	}

	for _, lease := range fleet.Leases {
		observation.Versions[VersionKey{
			Replicator: lease.Value.Versions.Replicator,
			Protocol:   lease.Value.Versions.Protocol,
		}]++
	}
	for _, status := range result.Requests {
		observation.Requests[status.State]++
	}

	epochs := make(map[string]string, len(result.Paths))
	for _, path := range result.Paths {
		observation.Paths[path.State]++
		if path.Session == nil {
			continue
		}
		for _, endpoint := range []*api.SessionEndpoint{path.Session.Target, path.Session.Initiator} {
			if endpoint != nil {
				observation.Workers[endpoint.State]++
			}
		}
		if path.Session.Epoch == "" {
			// A target that has died reports no epoch at all until it comes back — its old blob
			// describes memory registrations that died with it, so the agent clears it (§5.2). The
			// last known value has to be carried across that gap, or the epoch that arrives when
			// the target returns looks like a session establishing for the first time and the
			// restart this metric exists to catch is the one it misses.
			if was, ok := previous[path.Session.ID]; ok {
				epochs[path.Session.ID] = was
			}
			continue
		}
		epochs[path.Session.ID] = path.Session.Epoch
		if was, seen := previous[path.Session.ID]; seen && was != path.Session.Epoch {
			// A session first acquiring an epoch is not a transition — it is establishment. Only a
			// session that had one and now has a different one is a target that restarted.
			node := targetNode(path)
			observation.EpochChanges[node]++
			l.opts.Hooks.epochChanged(node)
		}
	}

	l.mu.Lock()
	l.observed, l.epochs = observation, epochs
	l.mu.Unlock()
}

func targetNode(path api.Path) string {
	if path.Session != nil && path.Session.Target != nil {
		return path.Session.Target.Node
	}
	return path.Destination.Node
}

// zeroed seeds a count map with every member of a vocabulary, so a status nothing is currently in
// exports as 0 rather than disappearing.
func zeroed[T comparable](values []T) map[T]int {
	out := make(map[T]int, len(values))
	for _, value := range values {
		out[value] = 0
	}
	return out
}

// publish records that the reconciler has settled, so that every replica — including the ones
// that never run a loop — can tell an empty assignment set from an unanswerable question.
func (l *Loop) publish(ctx context.Context, fleet *state.Fleet) error {
	record := state.ReconcilerRecord{
		Leader:    l.opts.Leader,
		Settled:   true,
		SettledAt: l.opts.Now(),
	}
	if prior := fleet.Reconciler; prior.Found && prior.Value.Settled && prior.Value.Leader == record.Leader {
		// Unchanged apart from the timestamp, which is exactly the field that must not cause a
		// write: the record is watched by this very loop.
		record.SettledAt = prior.Value.SettledAt
	}

	if _, _, err := state.PutJSON(ctx, l.opts.Store, store.KeyReconciler, record, fleet.Reconciler.Prior(), state.WriteOptions{CAS: true}); err != nil {
		return err
	}

	l.mu.Lock()
	l.settled = true
	l.mu.Unlock()
	return nil
}

// idleTracker remembers when each path's source stopped producing.
//
// It exists because `producing` is a coarse hysteretic boolean with no timestamp (§11.1) — the
// agent deliberately does not report a head index or a time, since either would make every
// inventory snapshot differ and turn inventory into a per-heartbeat write stream. So the
// duration is kept here, in the leader's memory.
//
// Leader-local and deliberately not persisted. A leader change resets it, which delays a
// long-idle teardown by one threshold and costs nothing; persisting it would put a
// continuously-changing value back into the store, which is the churn this whole mechanism
// exists to remove.
type idleTracker struct {
	now func() time.Time

	mu    sync.Mutex
	since map[string]time.Time
}

func newIdleTracker(now func() time.Time) *idleTracker {
	return &idleTracker{now: now, since: map[string]time.Time{}}
}

// observe records a path's current production state and returns how long it has been idle.
func (t *idleTracker) observe(pathID string, producing bool) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	if producing {
		delete(t.since, pathID)
		return 0
	}

	since, ok := t.since[pathID]
	if !ok {
		since = t.now()
		t.since[pathID] = since
	}
	return t.now().Sub(since)
}

// retain drops paths that no longer exist, so a fleet that churns through selectors does not
// leak an entry per flow that ever matched one.
func (t *idleTracker) retain(paths map[string]api.Path) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for id := range t.since {
		if _, ok := paths[id]; !ok {
			delete(t.since, id)
		}
	}
}
