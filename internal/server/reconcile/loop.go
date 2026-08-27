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
}

// Loop runs Compute → Apply whenever anything relevant changes.
type Loop struct {
	opts LoopOptions
	idle *idleTracker

	mu      sync.Mutex
	settled bool
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

	return &Loop{opts: opts, idle: newIdleTracker(opts.Now)}
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
	for {
		err := l.session(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			// A watch that ended, a store that hiccuped. Nothing is torn down when a reconcile
			// does not happen: the store still holds the assignments the agents are already
			// running, and they are level-triggered against those.
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
	events, err := l.opts.Store.Watch(ctx, "", fleet.Revision+1)
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

	return l.publish(ctx, fleet)
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
