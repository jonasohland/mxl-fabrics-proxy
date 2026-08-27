package reconcile

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
	"github.com/jonasohland/mxl-replicator/internal/store"
	"github.com/jonasohland/mxl-replicator/internal/store/sqlite"
)

func testStore(t *testing.T) store.Store {
	t.Helper()

	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "store.db"), sqlite.Options{
		PollInterval: 20 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, s.Close()) })
	return s
}

// seed writes the desired and observed state of a fleet snapshot into a real store, so that what
// comes back out of [state.Load] is what the server would actually be reconciling.
func seed(t *testing.T, s store.Store, fleet *state.Fleet) {
	t.Helper()
	ctx := context.Background()

	write := func(key string, value any, leased bool) {
		var opts state.WriteOptions
		if leased {
			lease, err := s.GrantLease(ctx, time.Minute)
			require.NoError(t, err)
			opts.Lease = lease
		}
		_, _, err := state.PutJSON(ctx, s, key, value, state.Prior{}, opts)
		require.NoError(t, err)
	}

	for name, entry := range fleet.Nodes {
		write(store.NodeKey(name), entry.Value, false)
	}
	for name, entry := range fleet.Leases {
		write(store.LeaseKey(name), entry.Value, true)
	}
	for name, entry := range fleet.Inventory {
		write(store.InventoryKey(name), entry.Value, true)
	}
	for name, entry := range fleet.Status {
		write(store.StatusKey(name), entry.Value, true)
	}
	for id, entry := range fleet.Requests {
		write(store.RequestKey(id), entry.Value, false)
	}
	for id, entry := range fleet.Sessions {
		write(store.SessionKey(id), entry.Value, false)
	}
	for name, entry := range fleet.Assignments {
		write(store.AssignmentsKey(name), entry.Value, false)
	}
}

func TestApplyWritesTheDerivedKeySpace(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	seed(t, s, base().build())
	ctx := context.Background()

	fleet, err := state.Load(ctx, s)
	require.NoError(t, err)

	changes, err := Apply(ctx, s, fleet, Compute(fleet, Config{}))
	require.NoError(t, err)
	assert.Equal(t, 1, changes.SessionsWritten)
	assert.Equal(t, 2, changes.AssignmentsWritten, "both nodes get a set, including the empty one")

	after, err := state.Load(ctx, s)
	require.NoError(t, err)
	require.Len(t, after.Sessions, 1)
	require.Contains(t, after.Assignments, "edge-01")
	assert.Len(t, after.Assignments["edge-01"].Value.Assignments, 1)

	// A node with nothing to do is told so positively. The absence of a key and an empty set are
	// indistinguishable to a poll, and that difference is a fleet-wide outage (§4.2).
	require.Contains(t, after.Assignments, "studio-a")
	assert.Empty(t, after.Assignments["studio-a"].Value.Assignments)
}

// The property the whole settling-window design rests on: a fleet that is already in the desired
// state produces **no writes at all**. Every write wakes every agent's long poll, and a spurious
// wakeup on the far side is a worker restart (§7.3, plan M7 test 4).
func TestASecondReconcileChangesNothing(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	seed(t, s, base().build())
	ctx := context.Background()

	first, err := state.Load(ctx, s)
	require.NoError(t, err)
	_, err = Apply(ctx, s, first, Compute(first, Config{}))
	require.NoError(t, err)

	settled, err := state.Load(ctx, s)
	require.NoError(t, err)

	changes, err := Apply(ctx, s, settled, Compute(settled, Config{}))
	require.NoError(t, err)
	assert.False(t, changes.Any(), "a steady-state reconcile must touch nothing: %+v", changes)

	quiet, err := state.Load(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, settled.Revision, quiet.Revision, "the store revision must not move")
}

// The same property across a restart, which is what makes deterministic session IDs worth
// having: a fresh server that has just observed the fleet recomputes byte-identical assignments
// and adopts the running workers instead of re-establishing them.
func TestARestartedServerAdoptsRunningSessions(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	ctx := context.Background()

	fleet := base().build()
	sessionID := sessionIDFor(fleet)
	seed(t, s, base().sessionStatus("edge-01", api.SessionStatus{
		SessionID: sessionID, Role: api.RoleTarget, State: api.WorkerReady,
		Epoch: "epoch-a", TargetInfo: `{"id":"x"}`, Address: "10.0.0.1", Service: "24001",
	}).build())

	loaded, err := state.Load(ctx, s)
	require.NoError(t, err)
	_, err = Apply(ctx, s, loaded, Compute(loaded, Config{}))
	require.NoError(t, err)

	before, err := state.Load(ctx, s)
	require.NoError(t, err)

	// A new process: nothing carried over but the store itself, and a fresh Config with its own
	// idle tracker.
	restarted, err := state.Load(ctx, s)
	require.NoError(t, err)
	changes, err := Apply(ctx, s, restarted, Compute(restarted, Config{Now: func() time.Time { return time.Now().Add(time.Hour) }}))
	require.NoError(t, err)

	assert.False(t, changes.Any(), "a restart must not rewrite anything: %+v", changes)
	assert.Equal(t, before.Revision, mustRevision(t, ctx, s))
}

func mustRevision(t *testing.T, ctx context.Context, s store.Store) int64 {
	t.Helper()
	_, rev, err := s.List(ctx, "")
	require.NoError(t, err)
	return rev
}

func TestApplyWithdrawsWhatIsNoLongerWanted(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	seed(t, s, base().build())
	ctx := context.Background()

	fleet, err := state.Load(ctx, s)
	require.NoError(t, err)
	_, err = Apply(ctx, s, fleet, Compute(fleet, Config{}))
	require.NoError(t, err)

	// The request is cancelled: the path's refcount hits zero, the session goes, and the workers
	// with it.
	_, err = s.Delete(ctx, store.RequestKey("cam1"))
	require.NoError(t, err)

	fleet, err = state.Load(ctx, s)
	require.NoError(t, err)
	changes, err := Apply(ctx, s, fleet, Compute(fleet, Config{}))
	require.NoError(t, err)
	assert.Equal(t, 1, changes.SessionsDeleted)

	after, err := state.Load(ctx, s)
	require.NoError(t, err)
	assert.Empty(t, after.Sessions)
	assert.Empty(t, after.Assignments["edge-01"].Value.Assignments)
}

// Every derived write is a CAS against the revision the snapshot was read at. A leader that was
// partitioned and has not noticed is computing from a stale read, so its write loses rather than
// fighting the new leader's (§4.6).
func TestApplyFromAStaleSnapshotLoses(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	seed(t, s, base().build())
	ctx := context.Background()

	stale, err := state.Load(ctx, s)
	require.NoError(t, err)

	current, err := state.Load(ctx, s)
	require.NoError(t, err)
	_, err = Apply(ctx, s, current, Compute(current, Config{}))
	require.NoError(t, err)

	// The demoted leader computes something different from what it last saw — here, the same
	// sessions but written against revisions that have moved on.
	_, err = Apply(ctx, s, stale, Compute(stale, Config{}))
	require.ErrorIs(t, err, store.ErrCompareFailed)
}

// --- the loop ----------------------------------------------------------------------------

func testLoop(t *testing.T, s store.Store, opts LoopOptions) *Loop {
	t.Helper()

	opts.Store = s
	opts.Logger = slog.New(slog.DiscardHandler)
	if opts.Heartbeat == 0 {
		opts.Heartbeat = 50 * time.Millisecond
	}
	if opts.Leader == "" {
		opts.Leader = "replica-a"
	}
	return NewLoop(opts)
}

func TestLoopReconcilesAndPublishes(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	seed(t, s, base().build())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	loop := testLoop(t, s, LoopOptions{})
	go func() { assert.NoError(t, loop.Run(ctx)) }()

	require.Eventually(t, func() bool {
		fleet, err := state.Load(ctx, s)
		return err == nil && len(fleet.Sessions) == 1 && fleet.Reconciler.Value.Settled
	}, 5*time.Second, 10*time.Millisecond)

	fleet, err := state.Load(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, "replica-a", fleet.Reconciler.Value.Leader)

	// And then it goes quiet. A loop that rewrites its own record on every pass would wake
	// itself, which is a feedback loop with a store write in it.
	revision := fleet.Revision
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, revision, mustRevision(t, ctx, s))
}

// Plan §4.2: a fleet with leased agents and no observed state is a store that lost its contents,
// not a fleet with nothing to do. Acting on it computes an empty assignment set for every node
// and tears down every worker in the fleet — successfully, from each agent's point of view,
// which is exactly why the agent's own fail-static rule cannot catch it.
func TestLoopRefusesToActWithNoObservedState(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	ctx := context.Background()

	// A leased agent and nothing else: the store came back empty.
	lease, err := s.GrantLease(ctx, time.Minute)
	require.NoError(t, err)
	_, _, err = state.PutJSON(ctx, s, store.LeaseKey("edge-01"),
		state.LeaseRecord{Node: "edge-01", Instance: "i-1"}, state.Prior{}, state.WriteOptions{Lease: lease})
	require.NoError(t, err)

	_, _, err = state.PutJSON(ctx, s, store.AssignmentsKey("edge-01"),
		api.AssignmentSet{Node: "edge-01", Assignments: []api.Assignment{{SessionID: "s-1", Role: api.RoleTarget}}},
		state.Prior{}, state.WriteOptions{})
	require.NoError(t, err)

	loop := testLoop(t, s, LoopOptions{})
	require.NoError(t, loop.once(ctx))

	fleet, err := state.Load(ctx, s)
	require.NoError(t, err)
	assert.Len(t, fleet.Assignments["edge-01"].Value.Assignments, 1, "the assignment must survive")
	assert.False(t, fleet.Reconciler.Found, "and the fleet must not be told the reconciler has settled")
}

// The settling window ends early once every leased agent has reported, so a fleet that reports
// promptly reconciles promptly — the window is a bound on waiting, not a delay for its own sake.
func TestSettlingEndsEarlyOnceEveryAgentHasReported(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	seed(t, s, base().build())

	loop := testLoop(t, s, LoopOptions{Heartbeat: time.Minute, SettlingHeartbeats: 3})
	assert.Equal(t, 3*time.Minute, loop.SettlingWindow())

	fleet, err := state.Load(t.Context(), s)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- loop.settle(t.Context(), fleet) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("settling did not end early even though every leased agent had reported")
	}
}

// And it does wait when an agent has not reported: the fleet is not observable yet, and
// reconciling against half of it is what re-establishes sessions that were fine.
func TestSettlingWaitsForASilentAgent(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	fleet := base().build()
	delete(fleet.Status, "studio-a")
	seed(t, s, fleet)

	loop := testLoop(t, s, LoopOptions{Heartbeat: 200 * time.Millisecond, SettlingHeartbeats: 3})

	loaded, err := state.Load(t.Context(), s)
	require.NoError(t, err)

	start := time.Now()
	require.NoError(t, loop.settle(t.Context(), loaded))
	assert.GreaterOrEqual(t, time.Since(start), 500*time.Millisecond, "the window must be observed in full")
}

func TestIdleTrackerMeasuresFromTheFirstIdleObservation(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	tracker := newIdleTracker(func() time.Time { return now })

	assert.Zero(t, tracker.observe("p1", true))

	assert.Zero(t, tracker.observe("p1", false))
	now = now.Add(time.Minute)
	assert.Equal(t, time.Minute, tracker.observe("p1", false))

	// A single grain resets it: idle → advancing on first movement, which is the hysteresis the
	// agent applies to `producing` seen from the other end (§11.1).
	assert.Zero(t, tracker.observe("p1", true))
	assert.Zero(t, tracker.observe("p1", false))

	// Paths that no longer exist are dropped, so a fleet churning through selectors does not
	// leak an entry per flow that ever matched one.
	tracker.retain(map[string]api.Path{})
	tracker.mu.Lock()
	assert.Empty(t, tracker.since)
	tracker.mu.Unlock()
}

// A flow definition round-trips through the store unchanged, which is what makes the session ID
// stable: a re-serialisation that reordered keys would look like a different flow and rebuild a
// healthy session (§5.4).
func TestFlowDefinitionSurvivesTheStore(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	definition := json.RawMessage(`{"zeta":1,"alpha":{"nested":"value"},"unmodelled":[1,2,3]}`)

	fleet := base().build()
	entry := fleet.Inventory["studio-a"]
	entry.Value.Domains[0].Flows[0].Definition = definition
	fleet.Inventory["studio-a"] = entry
	seed(t, s, fleet)

	loaded, err := state.Load(t.Context(), s)
	require.NoError(t, err)

	flow, ok := loaded.Flow("studio-a", "cameras", "flow-1")
	require.True(t, ok)
	assert.JSONEq(t, string(definition), string(flow.Definition))
	assert.Equal(t, state.FlowDefHash(definition), state.FlowDefHash(flow.Definition))

	result := Compute(loaded, Config{})
	target := find(result.Assignments["edge-01"], api.RoleTarget)
	require.NotNil(t, target)
	assert.Equal(t, string(definition), string(target.FlowDef), "verbatim, key order and all")
}
