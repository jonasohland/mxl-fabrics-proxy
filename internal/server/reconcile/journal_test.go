package reconcile

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
	"github.com/jonasohland/mxl-replicator/internal/store"
)

var journalNow = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

// result builds a one-path result, which is all these tests need: the journal reads [Result], not
// the fleet it was computed from.
func result(path api.Path) *Result {
	return &Result{Paths: map[string]api.Path{path.ID: path}}
}

func activePath() api.Path {
	return api.Path{
		PathStatus: api.PathStatus{
			ID:          "p-1",
			Source:      api.FlowAddress{Node: "studio-a", Domain: "media/cameras", Flow: "f-1"},
			Destination: api.Destination{Node: "edge-01"},
			State:       api.StateActive,
			SessionID:   "s-1",
		},
		Session: &api.Session{ID: "s-1", Epoch: "nonce-a:aaaa", Fabric: "mlx5_0"},
	}
}

func kinds(entries []entry) []api.EventKind {
	out := make([]api.EventKind, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.event.Kind)
	}
	return out
}

// **A newly elected leader emits no state transitions and marks the gap.**
//
// This is the case a naive differ passes by fabricating a storm: with no baseline, every path
// looks new, so every one of them would be reported as having just become whatever it has been
// for hours. That is §7.3's settling argument one layer up, and it would make the log least
// trustworthy exactly when an operator reaches for it — a leader change is often what they are
// investigating.
func TestFirstPassDeclaresAGapInsteadOfInventingTransitions(t *testing.T) {
	t.Parallel()

	j := newJournal(false)
	entries, dropped := j.diff(&state.Fleet{}, result(activePath()), journalNow)

	require.Len(t, entries, 1)
	assert.Equal(t, api.EventReconcilerTookOver, entries[0].event.Kind)
	assert.Equal(t, store.KeyFleetEvents, entries[0].key,
		"the marker belongs to the fleet, not to a thousand path rings")
	assert.Empty(t, dropped)
	assert.NotContains(t, kinds(entries), api.EventPathState)
}

// The property everything in §8.3 rests on, applied to the log: if a quiet fleet produced entries,
// the event log would be the writer that broke it.
func TestAnUnchangedPassRecordsNothing(t *testing.T) {
	t.Parallel()

	j := newJournal(false)
	j.diff(&state.Fleet{}, result(activePath()), journalNow)

	entries, dropped := j.diff(&state.Fleet{}, result(activePath()), journalNow)

	assert.Empty(t, entries)
	assert.Empty(t, dropped)
}

func TestATransitionIsRecordedOnThePath(t *testing.T) {
	t.Parallel()

	j := newJournal(false)
	j.diff(&state.Fleet{}, result(activePath()), journalNow)

	failed := activePath()
	failed.State = api.StateFailed
	failed.ReasonCode = api.ReasonWorkerRestarts
	failed.Reason = "worker restarted 12 times"

	entries, _ := j.diff(&state.Fleet{}, result(failed), journalNow)

	require.Len(t, entries, 1)
	assert.Equal(t, store.PathEventsKey("p-1"), entries[0].key)
	assert.Equal(t, api.EventPathState, entries[0].event.Kind)
	assert.Equal(t, api.StateFailed, entries[0].event.State)
	assert.Equal(t, api.SeverityError, entries[0].event.Severity)
	assert.Equal(t, api.ReasonWorkerRestarts, entries[0].event.ReasonCode)
	assert.Contains(t, entries[0].event.Message, "worker restarted 12 times")
}

// A conflict gets a kind of its own, because it is the case §7.5 calls otherwise undiagnosable —
// a path that went ACTIVE → INVALID overnight with nobody applying anything — and an operator has
// to be able to find it without knowing which reason codes happen to be conflicts.
func TestAConflictIsRecordedAsOne(t *testing.T) {
	t.Parallel()

	j := newJournal(false)
	j.diff(&state.Fleet{}, result(activePath()), journalNow)

	lost := activePath()
	lost.State = api.StateInvalid
	lost.ReasonCode = api.ReasonFlowConflict
	lost.Reason = "edge-01/fast/ingest already holds this flow from nab/other"

	entries, _ := j.diff(&state.Fleet{}, result(lost), journalNow)

	require.Len(t, entries, 1)
	assert.Equal(t, api.EventConflictLost, entries[0].event.Kind)
	assert.Contains(t, entries[0].event.Message, "nab/other", "the winner has to be named")
}

// An epoch change within one session is a target restart and nothing else (§5.2) — but a *new*
// session's first epoch is not one, or every establishment would report a restart that never
// happened.
func TestAnEpochChangeIsRecordedOnlyWithinOneSession(t *testing.T) {
	t.Parallel()

	j := newJournal(false)
	j.diff(&state.Fleet{}, result(activePath()), journalNow)

	restarted := activePath()
	restarted.Session = &api.Session{ID: "s-1", Epoch: "nonce-b:bbbb", Fabric: "mlx5_0"}
	entries, _ := j.diff(&state.Fleet{}, result(restarted), journalNow)
	assert.Contains(t, kinds(entries), api.EventEpochChanged)

	rebuilt := activePath()
	rebuilt.SessionID = "s-2"
	rebuilt.Session = &api.Session{ID: "s-2", Epoch: "nonce-c:cccc", Fabric: "mlx5_0"}
	entries, _ = j.diff(&state.Fleet{}, result(rebuilt), journalNow)

	assert.Contains(t, kinds(entries), api.EventSessionEstablished)
	assert.NotContains(t, kinds(entries), api.EventEpochChanged,
		"a new session's first epoch is not a restart")
}

// A session withdrawn while its path survives — a long-idle teardown (§11.1), a conflict lost — is
// otherwise invisible: the path simply stops having a session, and nothing in its status says that
// used to be different.
func TestAWithdrawnSessionIsRecordedWhileThePathSurvives(t *testing.T) {
	t.Parallel()

	j := newJournal(false)
	j.diff(&state.Fleet{}, result(activePath()), journalNow)

	torn := activePath()
	torn.State = api.StatePaused
	torn.Reason = "the source has been idle beyond the teardown threshold"
	torn.ReasonCode = api.ReasonSourceIdle
	torn.SessionID = ""
	torn.Session = nil

	entries, dropped := j.diff(&state.Fleet{}, result(torn), journalNow)

	assert.Empty(t, dropped, "the path is still here; only its workers went")
	assert.Contains(t, kinds(entries), api.EventSessionWithdrawn)

	for _, e := range entries {
		if e.event.Kind == api.EventSessionWithdrawn {
			assert.Equal(t, "s-1", e.event.Session, "the session that went, not the one that did not arrive")
			// The severity follows the path: a long-idle teardown lands in PAUSED, which is
			// designed behaviour (§11.1) and not something to alarm about.
			assert.Equal(t, api.SeverityInfo, e.event.Severity)
			assert.Contains(t, e.event.Message, "idle beyond the teardown threshold",
				"the reason comes off the path, so the log and the status cannot disagree")
		}
	}
}

// **A rebuild is not a withdrawal.** A new epoch or a republished flow definition replaces the
// session, and that already reports itself as an establishment — recording a withdrawal beside it
// would put two entries on every target restart, which is exactly the flapping this log has to stay
// legible through.
func TestARebuiltSessionIsNotAWithdrawal(t *testing.T) {
	t.Parallel()

	j := newJournal(false)
	j.diff(&state.Fleet{}, result(activePath()), journalNow)

	rebuilt := activePath()
	rebuilt.SessionID = "s-2"
	rebuilt.Session = &api.Session{ID: "s-2", Epoch: "nonce-z:zzzz", Fabric: "mlx5_0"}

	entries, _ := j.diff(&state.Fleet{}, result(rebuilt), journalNow)

	assert.Contains(t, kinds(entries), api.EventSessionEstablished)
	assert.NotContains(t, kinds(entries), api.EventSessionWithdrawn)
}

// A path that is gone has had its ring deleted in the same pass, so an entry about its last session
// would be written to a key that is about to be removed.
func TestADeletedPathRecordsNoWithdrawal(t *testing.T) {
	t.Parallel()

	j := newJournal(false)
	j.diff(&state.Fleet{}, result(activePath()), journalNow)

	entries, dropped := j.diff(&state.Fleet{}, &Result{Paths: map[string]api.Path{}}, journalNow)

	assert.Empty(t, entries, "nothing is written to a ring being deleted")
	assert.Len(t, dropped, 1)
}

// A path's log dies with the path (§12.1): no tombstone, no grace period.
func TestADeletedPathTakesItsRingWithIt(t *testing.T) {
	t.Parallel()

	j := newJournal(false)
	j.diff(&state.Fleet{}, result(activePath()), journalNow)

	entries, dropped := j.diff(&state.Fleet{}, &Result{Paths: map[string]api.Path{}}, journalNow)

	assert.Empty(t, entries)
	require.Len(t, dropped, 1)
	assert.Equal(t, "p-1", dropped[0].pathID)
}

// A node going dark freezes every path touching it rather than converging (§4.2), which is
// deliberate and invisible in a path's own status — the path simply stops changing. This is the
// entry that says why.
func TestALeaseExpiryIsRecordedOnTheNode(t *testing.T) {
	t.Parallel()

	leased := &state.Fleet{Leases: map[string]state.Entry[state.LeaseRecord]{
		"edge-01": {Found: true, Value: state.LeaseRecord{Node: "edge-01"}},
	}}

	j := newJournal(false)
	j.diff(leased, result(activePath()), journalNow)

	entries, _ := j.diff(&state.Fleet{}, result(activePath()), journalNow)

	require.Len(t, entries, 1)
	assert.Equal(t, store.NodeEventsKey("edge-01"), entries[0].key)
	assert.Equal(t, api.EventNodeLeaseExpired, entries[0].event.Kind)
	assert.Contains(t, entries[0].event.Message, "frozen")
}

// Losing leadership must forget the baseline, or a replica that regains it would report every
// transition that happened while it was not watching as though it had seen them.
func TestResetMakesTheNextPassDeclareAGap(t *testing.T) {
	t.Parallel()

	j := newJournal(false)
	j.diff(&state.Fleet{}, result(activePath()), journalNow)
	j.reset()

	entries, _ := j.diff(&state.Fleet{}, result(activePath()), journalNow)

	require.Len(t, entries, 1)
	assert.Equal(t, api.EventReconcilerTookOver, entries[0].event.Kind)
}

// --- inventory events (§12.1) ----------------------------------------------------------------

// leasedFleet builds a fleet where one node is live and holds the given flows in one domain.
func leasedFleet(node string, live bool, flows ...string) *state.Fleet {
	inventory := api.InventorySnapshot{Node: node, Domains: []api.DomainInventory{{
		Domain: api.Domain{Area: "media", Elements: []string{"cameras"}},
	}}}
	for _, flow := range flows {
		inventory.Domains[0].Flows = append(inventory.Domains[0].Flows, api.FlowInventory{ID: flow})
	}

	fleet := &state.Fleet{
		Nodes:     map[string]state.Entry[state.NodeRecord]{node: {Found: true, Value: state.NodeRecord{Node: node}}},
		Leases:    map[string]state.Entry[state.LeaseRecord]{},
		Inventory: map[string]state.Entry[api.InventorySnapshot]{node: {Found: true, Value: inventory}},
	}
	if live {
		fleet.Leases[node] = state.Entry[state.LeaseRecord]{Found: true, Value: state.LeaseRecord{Node: node}}
	}
	return fleet
}

func empty() *Result { return &Result{Paths: map[string]api.Path{}} }

// primed runs the two passes it takes to establish an inventory baseline: the seeded pass, which
// adopts everything *except* inventory, and the one after it, which states the baseline. A test
// about a subsequent change starts here.
func primed(j *journal, fleet *state.Fleet) {
	j.diff(fleet, empty(), journalNow)
	j.diff(fleet, empty(), journalNow)
}

func messages(entries []entry, kind api.EventKind) []string {
	var out []string
	for _, e := range entries {
		if e.event.Kind == kind {
			out = append(out, e.event.Message)
		}
	}
	return out
}

func TestFlowsAppearingAndDisappearingAreRecordedOnTheNode(t *testing.T) {
	t.Parallel()

	j := newJournal(true)
	primed(j, leasedFleet("studio-a", true, "f-1"))

	entries, _ := j.diff(leasedFleet("studio-a", true, "f-1", "f-2"), empty(), journalNow)
	require.Len(t, messages(entries, api.EventFlowAppeared), 1)
	assert.Contains(t, messages(entries, api.EventFlowAppeared)[0], "f-2")
	assert.Equal(t, store.NodeEventsKey("studio-a"), entries[0].key)

	entries, _ = j.diff(leasedFleet("studio-a", true, "f-2"), empty(), journalNow)
	require.Len(t, messages(entries, api.EventFlowDisappeared), 1)
	assert.Contains(t, messages(entries, api.EventFlowDisappeared)[0], "f-1")
}

// **The rule this feature would otherwise get wrong.**
//
// A node's inventory is leased state, so it vanishes from the snapshot the moment the lease
// expires. Diffing against that reports every flow on the node as having disappeared, at the exact
// moment nothing happened to any of them — §4.2's "no observation is never nothing there", one
// layer up. And the return must be just as quiet: the flows were there all along.
func TestANodeGoingDarkReportsNoInventoryChange(t *testing.T) {
	t.Parallel()

	j := newJournal(true)
	primed(j, leasedFleet("studio-a", true, "f-1", "f-2"))

	// The lease expires: observed state is garbage-collected with it.
	dark := leasedFleet("studio-a", false)
	dark.Inventory = map[string]state.Entry[api.InventorySnapshot]{}

	// The lease expiry itself is recorded — that is the entry explaining why the node's paths
	// froze. What must not appear beside it is any claim about its inventory.
	entries, _ := j.diff(dark, empty(), journalNow)
	assert.Equal(t, []api.EventKind{api.EventNodeLeaseExpired}, kinds(entries),
		"a node going dark did not lose its flows")

	// And coming back with exactly what it had is not a fleet-wide reappearance either.
	entries, _ = j.diff(leasedFleet("studio-a", true, "f-1", "f-2"), empty(), journalNow)
	assert.Empty(t, entries, "a node returning with what it had has changed nothing")
}

// A node this leader has never observed is not "everything appeared" — it may have been running for
// days, and saying its whole inventory just arrived is the fabricated storm the takeover marker
// exists to avoid. It is a **baseline**, which says where the record starts without claiming to
// have watched it start.
//
// The silence this replaces was the honest half of that and read exactly like a node whose flows
// never appeared, which is how it was noticed.
func TestAFirstObservationIsABaselineAndNotAnAppearance(t *testing.T) {
	t.Parallel()

	j := newJournal(true)
	j.diff(&state.Fleet{}, empty(), journalNow)

	entries, _ := j.diff(leasedFleet("studio-a", true, "f-1", "f-2"), empty(), journalNow)

	require.Len(t, entries, 1)
	assert.Equal(t, api.EventInventoryBaseline, entries[0].event.Kind)
	assert.Equal(t, store.NodeEventsKey("studio-a"), entries[0].key)
	assert.Equal(t, "first observed holding 2 flows in 1 domain", entries[0].event.Message)
	assert.NotContains(t, kinds(entries), api.EventFlowAppeared)

	// And the pass after it is quiet: a baseline is stated once, not re-stated every pass.
	entries, _ = j.diff(leasedFleet("studio-a", true, "f-1", "f-2"), empty(), journalNow)
	assert.Empty(t, entries)
}

// Losing leadership drops the inventory memory, so the next leader re-baselines rather than
// reporting changes from the gap as though it had watched them happen.
func TestALeaderChangeRebaselinesInventory(t *testing.T) {
	t.Parallel()

	j := newJournal(true)
	j.diff(leasedFleet("studio-a", true, "f-1"), empty(), journalNow)
	j.reset()

	// Seeded pass after the takeover: the marker, and no inventory claims.
	entries, _ := j.diff(leasedFleet("studio-a", true, "f-1", "f-2"), empty(), journalNow)
	assert.Equal(t, []api.EventKind{api.EventReconcilerTookOver}, kinds(entries))

	// Then a fresh baseline rather than "f-2 appeared", which this leader never saw happen.
	entries, _ = j.diff(leasedFleet("studio-a", true, "f-1", "f-2"), empty(), journalNow)
	assert.Equal(t, []api.EventKind{api.EventInventoryBaseline}, kinds(entries))
}

// Batched per pass, never per flow: fifty entries would evict a fifty-entry ring and take the
// registration entry that explains the episode with it.
func TestManyFlowsAppearingAreOneEntry(t *testing.T) {
	t.Parallel()

	j := newJournal(true)
	primed(j, leasedFleet("studio-a", true))

	var many []string
	for i := range 50 {
		many = append(many, fmt.Sprintf("f-%02d", i))
	}
	entries, _ := j.diff(leasedFleet("studio-a", true, many...), empty(), journalNow)

	appeared := messages(entries, api.EventFlowAppeared)
	require.Len(t, appeared, 1)
	assert.Contains(t, appeared[0], "50 flows appeared")
	assert.Contains(t, appeared[0], "and 42 more", "the remainder is counted, never trailed off")
}

// The same flow ID in two domains on one node is two flows (§3), so leaving one of them is a
// disappearance rather than a no-op.
func TestOneFlowIDInTwoDomainsIsTwoEntries(t *testing.T) {
	t.Parallel()

	both := leasedFleet("studio-a", true, "f-1")
	snapshot := both.Inventory["studio-a"].Value
	snapshot.Domains = append(snapshot.Domains, api.DomainInventory{
		Domain: api.Domain{Area: "media", Elements: []string{"backup"}},
		Flows:  []api.FlowInventory{{ID: "f-1"}},
	})
	both.Inventory["studio-a"] = state.Entry[api.InventorySnapshot]{Found: true, Value: snapshot}

	j := newJournal(true)
	primed(j, both)

	entries, _ := j.diff(leasedFleet("studio-a", true, "f-1"), empty(), journalNow)

	assert.Len(t, messages(entries, api.EventDomainDisappeared), 1)
	require.Len(t, messages(entries, api.EventFlowDisappeared), 1)
	assert.Contains(t, messages(entries, api.EventFlowDisappeared)[0], "media/backup/f-1")
}

// Off means off: the entries the switch governs, and nothing else.
func TestInventoryEventsCanBeTurnedOff(t *testing.T) {
	t.Parallel()

	j := newJournal(false)
	primed(j, leasedFleet("studio-a", true, "f-1"))

	entries, _ := j.diff(leasedFleet("studio-a", true, "f-1", "f-2"), empty(), journalNow)
	assert.Empty(t, entries)
}

// The other half of reading a withdrawal's severity off its path: one that lands in INVALID is a
// fault and says so, where the teardown above is not.
func TestAWithdrawalIntoInvalidIsAnError(t *testing.T) {
	t.Parallel()

	j := newJournal(false)
	j.diff(&state.Fleet{}, result(activePath()), journalNow)

	lost := activePath()
	lost.State = api.StateInvalid
	lost.ReasonCode = api.ReasonFlowConflict
	lost.Reason = "edge-01/fast/ingest already holds this flow from nab/other"
	lost.SessionID = ""
	lost.Session = nil

	entries, _ := j.diff(&state.Fleet{}, result(lost), journalNow)

	for _, e := range entries {
		if e.event.Kind == api.EventSessionWithdrawn {
			assert.Equal(t, api.SeverityError, e.event.Severity)
		}
	}
	assert.Contains(t, kinds(entries), api.EventSessionWithdrawn)
}
