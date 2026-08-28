package reconcile

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
)

// --- fixtures ---------------------------------------------------------------------------

var (
	created  = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	flowDef  = json.RawMessage(`{"id":"flow-1","format":"urn:x-nmos:format:video"}`)
	flowDef2 = json.RawMessage(`{"id":"flow-1","format":"urn:x-nmos:format:video","grainRate":{"numerator":50}}`)

	// What Negotiate produces for the fixture nodes below, and therefore what a session record
	// for them carries: the negotiated config is pinned into the record for the session's
	// lifetime (§10.4).
	tcpInterface = api.InterfaceConfig{
		Provider: api.ProviderTCP,
		CapFlags: []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive},
	}
)

// builder assembles a fleet snapshot the way the store would hand one back.
type fleetBuilder struct {
	fleet *state.Fleet
}

func newFleet() *fleetBuilder {
	return &fleetBuilder{fleet: &state.Fleet{
		Revision:    100,
		Nodes:       map[string]state.Entry[state.NodeRecord]{},
		Leases:      map[string]state.Entry[state.LeaseRecord]{},
		Inventory:   map[string]state.Entry[api.InventorySnapshot]{},
		Status:      map[string]state.Entry[api.StatusSnapshot]{},
		Requests:    map[string]state.Entry[state.RequestRecord]{},
		Sessions:    map[string]state.Entry[state.SessionRecord]{},
		Assignments: map[string]state.Entry[api.AssignmentSet]{},
	}}
}

func (b *fleetBuilder) node(name string, domains ...api.DomainMapping) *fleetBuilder {
	b.fleet.Nodes[name] = state.Entry[state.NodeRecord]{Found: true, Value: state.NodeRecord{
		Node:    name,
		Domains: domains,
		Capabilities: api.Capabilities{
			Fabrics: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.0.0." + name[len(name)-1:],
				CapFlags: []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive},
			}},
			// One output root on every node, so any of them can be a destination and a request
			// can name one without spelling the root (§10.6).
			OutputRoots: []api.OutputRoot{{Name: "fast", Path: "/dev/shm/mxl"}},
		},
	}}
	b.fleet.Leases[name] = state.Entry[state.LeaseRecord]{Found: true, Value: state.LeaseRecord{Node: name, Instance: "i-" + name}}
	b.fleet.Status[name] = state.Entry[api.StatusSnapshot]{Found: true, Value: api.StatusSnapshot{Node: name}}
	b.fleet.Inventory[name] = state.Entry[api.InventorySnapshot]{Found: true, Value: api.InventorySnapshot{Node: name}}
	return b
}

func (b *fleetBuilder) unlease(name string) *fleetBuilder {
	// A lease going away takes the agent's observed state with it, because it is written under
	// that lease. Reproducing that here is the point: the naive reading of the result is "this
	// node has no flows and no sessions".
	delete(b.fleet.Leases, name)
	delete(b.fleet.Inventory, name)
	delete(b.fleet.Status, name)
	return b
}

func (b *fleetBuilder) flow(node, domain string, flow api.FlowInventory) *fleetBuilder {
	entry := b.fleet.Inventory[node]
	entry.Found = true
	entry.Value.Node = node

	for i := range entry.Value.Domains {
		if entry.Value.Domains[i].Name == domain {
			entry.Value.Domains[i].Flows = append(entry.Value.Domains[i].Flows, flow)
			b.fleet.Inventory[node] = entry
			return b
		}
	}
	entry.Value.Domains = append(entry.Value.Domains, api.DomainInventory{
		Name: domain, Configured: true, Flows: []api.FlowInventory{flow},
	})
	b.fleet.Inventory[node] = entry
	return b
}

func (b *fleetBuilder) request(id string, spec api.RequestSpec) *fleetBuilder {
	b.fleet.Requests[id] = state.Entry[state.RequestRecord]{Found: true, Value: state.RequestRecord{
		ID: id, Spec: spec, CreatedAt: created,
	}}
	return b
}

func (b *fleetBuilder) sessionStatus(node string, status api.SessionStatus) *fleetBuilder {
	entry := b.fleet.Status[node]
	entry.Found = true
	entry.Value.Node = node
	entry.Value.Sessions = append(entry.Value.Sessions, status)
	b.fleet.Status[node] = entry
	return b
}

func (b *fleetBuilder) session(record state.SessionRecord) *fleetBuilder {
	b.fleet.Sessions[record.ID] = state.Entry[state.SessionRecord]{Found: true, Value: record}
	return b
}

func (b *fleetBuilder) assignments(node string, set api.AssignmentSet) *fleetBuilder {
	set.Node = node
	b.fleet.Assignments[node] = state.Entry[api.AssignmentSet]{Found: true, Value: set}
	return b
}

func (b *fleetBuilder) build() *state.Fleet { return b.fleet }

func flowRequest(name string) api.RequestSpec {
	return api.RequestSpec{
		Name:         name,
		Source:       api.Source{Node: "studio-a", Domain: "cameras", Select: api.Selector{Flow: "flow-1"}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: []string{"ingest"}}},
	}
}

// base is the ordinary fleet: two registered nodes, one request, one flow being produced.
func base() *fleetBuilder {
	return newFleet().
		node("studio-a", api.DomainMapping{Name: "cameras", Configured: true}).
		node("edge-01").
		flow("studio-a", "cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true}).
		request("cam1", flowRequest("cam1"))
}

// --- fan-out: one source, many destinations (§9.1, 8a) -----------------------------------

// A request fans out to a path per destination, and each path is its own session and its own
// worker pair. The request's status aggregates over all of them (§11).
func TestARequestFansOutToOnePathPerDestination(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Destinations = []api.Destination{
		{Node: "edge-01", Domain: []string{"ingest"}},
		{Node: "edge-02", Domain: []string{"ingest"}},
		{Node: "archive-01", Domain: []string{"capture"}},
	}

	fleet := base().node("edge-02").node("archive-01").request("cam1", spec).build()
	result := Compute(fleet, Config{})

	require.Len(t, result.Paths, 3)
	require.Len(t, result.Requests["cam1"].Paths, 3)

	// Every destination gets its own path, its own session, and a target assignment on its own
	// node. The source node carries one initiator per destination: fan-out is N workers reading
	// the same local flow, which is the cost the grouping makes legible.
	seen := map[string]bool{}
	for _, path := range result.Paths {
		seen[path.Destination.Endpoint()] = true
		assert.Equal(t, "flow-1", path.Source.Flow)
		assert.Equal(t, "fast", path.Destination.Root, "the resolved root goes into the identity")
		assert.Equal(t, []string{"cam1"}, path.Requests)
	}
	assert.Equal(t, map[string]bool{"edge-01/ingest": true, "edge-02/ingest": true, "archive-01/capture": true}, seen)

	for _, node := range []string{"edge-01", "edge-02", "archive-01"} {
		assert.Len(t, result.Assignments[node].Assignments, 1, "one target on %s", node)
	}
	assert.Len(t, result.Assignments["studio-a"].Assignments, 0,
		"no initiator until each target has reported an epoch (invariant 3)")

	assert.Equal(t, api.StateEstablishing, result.Requests["cam1"].State)
	assert.Equal(t, 3, result.Requests["cam1"].Counts[api.StateEstablishing])
}

// **The point of validating per destination.** One unusable destination makes the request
// INVALID, but it must not stop its siblings: they are separate paths on separate nodes and
// nothing about them changed.
func TestOneInvalidDestinationDoesNotStopTheOthers(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Destinations = []api.Destination{
		{Node: "edge-01", Domain: []string{"ingest"}},
		{Node: "edge-02", Domain: []string{"ingest"}, Root: "bulk"}, // edge-02 advertises only "fast"
	}

	fleet := base().node("edge-02").request("cam1", spec).build()
	result := Compute(fleet, Config{})

	status := result.Requests["cam1"]
	assert.Equal(t, api.StateInvalid, status.State)
	assert.Equal(t, api.ReasonUnknownOutputRoot, status.ReasonCode)
	assert.Contains(t, status.Reason, "edge-02/ingest",
		"the reason must name which destination failed, or an operator cannot find it")

	// The good leg is a real path with a real session and a real assignment...
	require.Len(t, result.Paths, 2)
	assert.Len(t, result.Assignments["edge-01"].Assignments, 1)
	assert.Equal(t, 1, status.Counts[api.StateEstablishing])

	// ...and the bad one is a shadow path that starts nothing.
	assert.Empty(t, result.Assignments["edge-02"].Assignments)
	assert.Equal(t, 1, status.Counts[api.StateInvalid])
}

// A failure that is really about the *source* fails every leg identically, and naming one
// destination for it points at the wrong end — "(and 2 more)" would suggest two further problems
// rather than the same one counted three times.
func TestARequestWideFailureDoesNotBlameADestination(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Source.Node = "typo" // never registered: nothing about any destination is wrong
	spec.Destinations = []api.Destination{
		{Node: "edge-01", Domain: []string{"ingest"}},
		{Node: "edge-02", Domain: []string{"ingest"}},
	}

	fleet := base().node("edge-02").request("cam1", spec).build()
	status := Compute(fleet, Config{}).Requests["cam1"]

	assert.Equal(t, api.StateInvalid, status.State)
	assert.Equal(t, api.ReasonNodeNotRegistered, status.ReasonCode)
	assert.Contains(t, status.Reason, `source node "typo"`)
	assert.NotContains(t, status.Reason, "destination edge-01/ingest")
	assert.NotContains(t, status.Reason, "more destination")
}

// A leg that expands to no paths at all still has to say why it is unusable. Without this a
// request whose source flow does not exist *yet* and whose destination can never work reports
// WAITING, and POST lets it through — which is exactly what §7.2 requires be rejected.
func TestAnUnusableDestinationIsInvalidEvenWithNothingToExpandOnto(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Source.Select = api.Selector{Flow: "flow-does-not-exist-yet"}
	spec.Destinations = []api.Destination{{Node: "edge-01", Domain: []string{"ingest"}, Root: "bulk"}}

	result := Compute(base().request("cam1", spec).build(), Config{})

	status := result.Requests["cam1"]
	assert.Equal(t, api.StateInvalid, status.State, "not WAITING: the destination can never work")
	assert.Equal(t, api.ReasonUnknownOutputRoot, status.ReasonCode)
}

// A per-destination pin overrides the request-level one rather than intersecting it, so
// "verbs here, tcp there" is an ordinary request and not a pin conflict (§10.4).
func TestAPerDestinationPinOverridesTheRequestPin(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Provider = api.ProviderPin{api.ProviderEFA} // not viable for these fixture nodes
	spec.Destinations = []api.Destination{
		{Node: "edge-01", Domain: []string{"ingest"}, Provider: api.ProviderPin{api.ProviderTCP}},
		{Node: "edge-02", Domain: []string{"ingest"}},
	}

	fleet := base().node("edge-02").request("cam1", spec).build()
	result := Compute(fleet, Config{})

	byNode := map[string]api.Path{}
	for _, path := range result.Paths {
		byNode[path.Destination.Node] = path
	}
	require.Contains(t, byNode, "edge-01")
	require.Contains(t, byNode, "edge-02")

	// The overriding destination negotiates tcp and comes up; the inheriting one takes the
	// request's unviable efa pin and is refused, without ever being substituted onto tcp.
	assert.Equal(t, api.StateEstablishing, byNode["edge-01"].State)
	assert.Equal(t, api.StateInvalid, byNode["edge-02"].State)
	assert.Equal(t, api.ReasonPinNotViable, byNode["edge-02"].ReasonCode)
}

// Two requests naming the same edge still share one path and one session, whether or not either
// of them fans out elsewhere. Refcounting is at path level and the destination list changes
// nothing about it (§9.1).
func TestFanOutStillDeduplicatesSharedPaths(t *testing.T) {
	t.Parallel()

	wide := flowRequest("wide")
	wide.Destinations = []api.Destination{
		{Node: "edge-01", Domain: []string{"ingest"}},
		{Node: "edge-02", Domain: []string{"ingest"}},
	}
	narrow := flowRequest("narrow")
	narrow.Destinations = []api.Destination{{Node: "edge-02", Domain: []string{"ingest"}}}

	fleet := newFleet().
		node("studio-a", api.DomainMapping{Name: "cameras", Configured: true}).
		node("edge-01").node("edge-02").
		flow("studio-a", "cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true}).
		request("wide", wide).request("narrow", narrow).
		build()
	result := Compute(fleet, Config{})

	require.Len(t, result.Paths, 2, "three destination entries, two distinct edges")
	for _, path := range result.Paths {
		if path.Destination.Node == "edge-02" {
			assert.Equal(t, []string{"narrow", "wide"}, path.Requests)
		} else {
			assert.Equal(t, []string{"wide"}, path.Requests)
		}
	}
	assert.Len(t, result.Sessions, 2)
}

func onlyPath(t *testing.T, result *Result) api.Path {
	t.Helper()
	require.Len(t, result.Paths, 1)
	for _, path := range result.Paths {
		return path
	}
	return api.Path{}
}

func find(set api.AssignmentSet, role api.Role) *api.Assignment {
	for i := range set.Assignments {
		if set.Assignments[i].Role == role {
			return &set.Assignments[i]
		}
	}
	return nil
}

// --- establishment ----------------------------------------------------------------------

// §5.3 steps 1–2: the session exists, the target is assigned, and the initiator is not — because
// no epoch has been reported. Invariant 3, and the ordering is mandatory rather than an
// optimisation: openFlow fails outright if the destination flow does not exist yet.
func TestTargetIsAssignedFirstAndAloneUntilAnEpochExists(t *testing.T) {
	t.Parallel()

	result := Compute(base().build(), Config{})

	path := onlyPath(t, result)
	assert.Equal(t, api.StateEstablishing, path.State)
	require.Len(t, result.Sessions, 1)

	target := find(result.Assignments["edge-01"], api.RoleTarget)
	require.NotNil(t, target)
	assert.Equal(t, "ingest", target.Domain)
	assert.Equal(t, "flow-1", target.FlowID)
	assert.JSONEq(t, string(flowDef), string(target.FlowDef))
	assert.Equal(t, api.ProviderTCP, target.Interface.Provider)
	assert.Equal(t, "dc1", target.Fabric)
	assert.Empty(t, target.Epoch)

	assert.Nil(t, find(result.Assignments["studio-a"], api.RoleInitiator))
	assert.Empty(t, result.Assignments["studio-a"].Assignments)
}

// §5.3 steps 4–5: the epoch and the blob come off the destination agent's report and are handed
// to the source agent, with the peer endpoint the target actually bound.
func TestInitiatorIsAssignedOnceTheEpochIsReported(t *testing.T) {
	t.Parallel()

	fleet := base().build()
	sessionID := sessionIDFor(fleet)

	fleet = base().sessionStatus("edge-01", api.SessionStatus{
		SessionID: sessionID, Role: api.RoleTarget, State: api.WorkerReady,
		Epoch: "epoch-a", TargetInfo: `{"id":"x"}`, Address: "10.0.0.1", Service: "24001",
	}).build()

	result := Compute(fleet, Config{})

	initiator := find(result.Assignments["studio-a"], api.RoleInitiator)
	require.NotNil(t, initiator)
	assert.Equal(t, "epoch-a", initiator.Epoch)
	assert.Equal(t, `{"id":"x"}`, initiator.TargetInfo)
	assert.Equal(t, "cameras", initiator.Domain, "an initiator reads its own local domain")
	assert.Empty(t, initiator.FlowDef, "only a target creates a flow")
	require.NotNil(t, initiator.Peer)
	assert.Equal(t, api.PeerEndpoint{Node: "edge-01", Address: "10.0.0.1", Service: "24001"}, *initiator.Peer)

	// Both ends are configured from one negotiated result. Deciding it per side is not a
	// configuration choice, it is a bug (§10.3, invariant 8).
	target := find(result.Assignments["edge-01"], api.RoleTarget)
	assert.Equal(t, target.Interface, initiator.Interface)
	assert.Equal(t, target.Fabric, initiator.Fabric)
	assert.Equal(t, target.NoNetworkLatencyMeasurement, initiator.NoNetworkLatencyMeasurement)
}

// The convergence rule of §5.2, seen from the server: a target that restarts reports a new
// epoch, and the initiator's assignment changes. That is the whole mechanism — no keepalive, no
// change-detection RPC, no teardown negotiation.
func TestANewEpochChangesTheInitiatorAssignment(t *testing.T) {
	t.Parallel()

	fleet := base().build()
	sessionID := sessionIDFor(fleet)

	status := func(epoch, blob string) *state.Fleet {
		return base().sessionStatus("edge-01", api.SessionStatus{
			SessionID: sessionID, Role: api.RoleTarget, State: api.WorkerReady,
			Epoch: epoch, TargetInfo: blob, Address: "10.0.0.1", Service: "24001",
		}).build()
	}

	first := Compute(status("epoch-a", `{"id":"x"}`), Config{})
	second := Compute(status("epoch-b", `{"id":"x"}`), Config{})

	// The degenerate case the nonce exists for: a byte-identical blob after a restart still
	// carries a new epoch, and the initiator must still reconnect (§5.2).
	assert.Equal(t, "epoch-a", find(first.Assignments["studio-a"], api.RoleInitiator).Epoch)
	assert.Equal(t, "epoch-b", find(second.Assignments["studio-a"], api.RoleInitiator).Epoch)
	assert.Equal(t,
		find(first.Assignments["studio-a"], api.RoleInitiator).TargetInfo,
		find(second.Assignments["studio-a"], api.RoleInitiator).TargetInfo)
}

// A target that is restarting holds a blob describing memory registrations that died with the
// old process. Leaving the initiator assigned to it is the worst failure in the system: no
// error, no data, everything upstream reporting healthy (§5.2).
func TestInitiatorIsWithdrawnWhileTheTargetIsNotReady(t *testing.T) {
	t.Parallel()

	fleet := base().build()
	sessionID := sessionIDFor(fleet)

	result := Compute(base().sessionStatus("edge-01", api.SessionStatus{
		SessionID: sessionID, Role: api.RoleTarget, State: api.WorkerFailed,
		Epoch: "epoch-a", TargetInfo: `{"id":"x"}`,
	}).build(), Config{})

	assert.Nil(t, find(result.Assignments["studio-a"], api.RoleInitiator))
	assert.NotNil(t, find(result.Assignments["edge-01"], api.RoleTarget), "the target itself stays assigned and is restarted by its agent")
}

// --- status derivation (§11, M4h) -------------------------------------------------------

func established(t *testing.T, destinationProducing bool, sourceProducing bool) *Result {
	t.Helper()

	fleet := base().build()
	sessionID := sessionIDFor(fleet)

	b := newFleet().
		node("studio-a", api.DomainMapping{Name: "cameras", Configured: true}).
		node("edge-01").
		flow("studio-a", "cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: sourceProducing}).
		flow("edge-01", "ingest", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: destinationProducing}).
		request("cam1", flowRequest("cam1")).
		session(state.SessionRecord{
			ID: sessionID, Path: pathOf(fleet), FlowDefHash: state.FlowDefHash(flowDef),
			Fabric: "dc1", Interface: tcpInterface,
		}).
		sessionStatus("edge-01", api.SessionStatus{
			SessionID: sessionID, Role: api.RoleTarget, State: api.WorkerReady,
			Epoch: "epoch-a", TargetInfo: `{"id":"x"}`, Address: "10.0.0.1", Service: "24001",
		}).
		sessionStatus("studio-a", api.SessionStatus{
			SessionID: sessionID, Role: api.RoleInitiator, State: api.WorkerReady, Epoch: "epoch-a",
		})

	return Compute(b.build(), Config{})
}

// ACTIVE is determined from the destination flow's own liveness, never from worker accounting: a
// worker can report healthy transfers while producing a flow nothing can read (§11).
func TestActiveComesFromTheDestinationFlow(t *testing.T) {
	t.Parallel()

	assert.Equal(t, api.StateActive, onlyPath(t, established(t, true, true)).State)

	// The valuable distinction: the plumbing is fine and nobody is writing on the far end.
	paused := onlyPath(t, established(t, false, false))
	assert.Equal(t, api.StatePaused, paused.State)
	assert.Equal(t, api.ReasonSourceIdle, paused.ReasonCode)

	// Source producing, destination not: also PAUSED, but for the opposite reason, and the
	// reason string has to say so — these have completely different owners.
	broken := onlyPath(t, established(t, false, true))
	assert.Equal(t, api.StatePaused, broken.State)
	assert.NotEqual(t, api.ReasonSourceIdle, broken.ReasonCode)
	assert.Contains(t, broken.Reason, "destination")
}

// Flapping is classified from restart rate, never from an exit code (§15.1, invariant 10), and it
// outranks moving media: a session that transfers between six restarts is a problem an operator
// has to see.
func TestDegradedAndFailedComeFromRestartRate(t *testing.T) {
	t.Parallel()

	fleet := base().build()
	sessionID := sessionIDFor(fleet)

	withRestarts := func(n int) *Result {
		b := newFleet().
			node("studio-a", api.DomainMapping{Name: "cameras", Configured: true}).
			node("edge-01").
			flow("studio-a", "cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true}).
			flow("edge-01", "ingest", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true}).
			request("cam1", flowRequest("cam1")).
			session(state.SessionRecord{ID: sessionID, Path: pathOf(fleet), FlowDefHash: state.FlowDefHash(flowDef), Fabric: "dc1", Interface: tcpInterface}).
			sessionStatus("edge-01", api.SessionStatus{
				SessionID: sessionID, Role: api.RoleTarget, State: api.WorkerReady,
				Epoch: "e", TargetInfo: "{}", Restarts: n,
			}).
			sessionStatus("studio-a", api.SessionStatus{
				SessionID: sessionID, Role: api.RoleInitiator, State: api.WorkerReady, Epoch: "e",
			})
		return Compute(b.build(), Config{DegradedRestarts: 3, FailedRestarts: 10})
	}

	assert.Equal(t, api.StateActive, onlyPath(t, withRestarts(2)).State)

	degraded := onlyPath(t, withRestarts(3))
	assert.Equal(t, api.StateDegraded, degraded.State)
	assert.Equal(t, api.ReasonWorkerRestarts, degraded.ReasonCode)

	assert.Equal(t, api.StateFailed, onlyPath(t, withRestarts(10)).State)
}

// --- admission and idleness (§11.1) -----------------------------------------------------

// A request for a camera that is not currently live is an ordinary thing to ask for, and it must
// cost nothing: no workers at all until the source is actually being produced.
func TestADormantSourceStartsNoWorkers(t *testing.T) {
	t.Parallel()

	fleet := newFleet().
		node("studio-a", api.DomainMapping{Name: "cameras", Configured: true}).
		node("edge-01").
		flow("studio-a", "cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: false}).
		request("cam1", flowRequest("cam1")).
		build()

	result := Compute(fleet, Config{})

	path := onlyPath(t, result)
	assert.Equal(t, api.StatePaused, path.State)
	assert.Equal(t, api.ReasonSourceIdle, path.ReasonCode)
	assert.Empty(t, result.Sessions)
	assert.Empty(t, result.Assignments["edge-01"].Assignments)
	assert.Empty(t, result.Assignments["studio-a"].Assignments)
}

// The two-tier policy: an established session survives a short gap with its workers running, so
// the first grain after a pause moves immediately, and is torn down only past the threshold.
func TestIdleTeardownIsTwoTiered(t *testing.T) {
	t.Parallel()

	fleet := base().build()
	sessionID := sessionIDFor(fleet)

	withIdle := func(idle time.Duration) *Result {
		b := newFleet().
			node("studio-a", api.DomainMapping{Name: "cameras", Configured: true}).
			node("edge-01").
			flow("studio-a", "cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: false}).
			request("cam1", flowRequest("cam1")).
			session(state.SessionRecord{ID: sessionID, Path: pathOf(fleet), FlowDefHash: state.FlowDefHash(flowDef), Fabric: "dc1", Interface: tcpInterface}).
			sessionStatus("edge-01", api.SessionStatus{
				SessionID: sessionID, Role: api.RoleTarget, State: api.WorkerReady,
				Epoch: "epoch-a", TargetInfo: `{"id":"x"}`,
			}).
			sessionStatus("studio-a", api.SessionStatus{
				SessionID: sessionID, Role: api.RoleInitiator, State: api.WorkerReady, Epoch: "epoch-a",
			})
		return Compute(b.build(), Config{
			IdleTeardown: 5 * time.Minute,
			Idle:         func(string, bool) time.Duration { return idle },
		})
	}

	hot := withIdle(30 * time.Second)
	assert.Equal(t, api.StatePaused, onlyPath(t, hot).State)
	assert.Len(t, hot.Sessions, 1, "a short gap keeps the workers up")
	assert.NotEmpty(t, hot.Assignments["edge-01"].Assignments)

	cold := withIdle(10 * time.Minute)
	assert.Equal(t, api.StatePaused, onlyPath(t, cold).State)
	assert.Empty(t, cold.Sessions, "past the threshold the workers go")
	assert.Empty(t, cold.Assignments["edge-01"].Assignments)
	assert.Contains(t, onlyPath(t, cold).Reason, "stopped")

	// Teardown disabled keeps a dormant session hot indefinitely, which is what
	// "this feed is bursty, keep it hot" asks for.
	never := Compute(withTeardownDisabled(fleet, sessionID), Config{
		IdleTeardown: 5 * time.Minute,
		Idle:         func(string, bool) time.Duration { return time.Hour },
	})
	assert.Len(t, never.Sessions, 1)
}

func withTeardownDisabled(fleet *state.Fleet, sessionID string) *state.Fleet {
	spec := flowRequest("cam1")
	zero := api.Milliseconds(0)
	spec.IdleTeardown = &zero

	return newFleet().
		node("studio-a", api.DomainMapping{Name: "cameras", Configured: true}).
		node("edge-01").
		flow("studio-a", "cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: false}).
		request("cam1", spec).
		session(state.SessionRecord{ID: sessionID, Path: pathOf(fleet), FlowDefHash: state.FlowDefHash(flowDef), Fabric: "dc1", Interface: tcpInterface}).
		build()
}

// --- the absence-of-observation rule (§4.2) ---------------------------------------------

// An expired lease is not proof that a node's workers stopped. The naive reading — no
// inventory, therefore no flows, therefore no sessions — stops media across the fleet every time
// an agent is upgraded.
func TestAnAgentLosingItsLeaseWithdrawsNothing(t *testing.T) {
	t.Parallel()

	fleet := base().build()
	sessionID := sessionIDFor(fleet)

	running := api.AssignmentSet{Assignments: []api.Assignment{{
		SessionID: sessionID, Role: api.RoleInitiator, Domain: "cameras", FlowID: "flow-1",
		Epoch: "epoch-a", TargetInfo: `{"id":"x"}`,
	}}}

	b := newFleet().
		node("studio-a", api.DomainMapping{Name: "cameras", Configured: true}).
		node("edge-01").
		request("cam1", flowRequest("cam1")).
		session(state.SessionRecord{ID: sessionID, Path: pathOf(fleet), FlowDefHash: state.FlowDefHash(flowDef), Fabric: "dc1", Interface: tcpInterface}).
		assignments("studio-a", running).
		assignments("edge-01", api.AssignmentSet{Assignments: []api.Assignment{{
			SessionID: sessionID, Role: api.RoleTarget, Domain: "ingest", FlowID: "flow-1", FlowDef: flowDef,
		}}}).
		unlease("edge-01")

	result := Compute(b.build(), Config{})

	path := onlyPath(t, result)
	assert.Equal(t, api.StateWaiting, path.State)
	assert.Equal(t, api.ReasonAgentNotLeased, path.ReasonCode)

	require.Len(t, result.Sessions, 1, "the session is retained")
	assert.Equal(t, []string{sessionID}, result.Frozen)
	assert.Equal(t, running.Assignments, result.Assignments["studio-a"].Assignments,
		"the live peer's assignment is carried forward byte for byte")
	assert.Len(t, result.Assignments["edge-01"].Assignments, 1)
}

// The same rule for a group-hint request: inventory went away with the lease, so the selector
// expands to nothing — and "matched nothing" must not be read as "delete everything".
func TestAGroupHintRequestDoesNotCollapseWhenItsSourceAgentIsGone(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Source.Select = api.Selector{GroupHint: &api.GroupHintSelector{Name: "Studio A:Camera 1"}}

	fleet := base().build()
	sessionID := sessionIDFor(fleet)

	b := newFleet().
		node("studio-a", api.DomainMapping{Name: "cameras", Configured: true}).
		node("edge-01").
		request("cam1", spec).
		session(state.SessionRecord{ID: sessionID, Path: pathOf(fleet), FlowDefHash: state.FlowDefHash(flowDef), Fabric: "dc1", Interface: tcpInterface}).
		assignments("edge-01", api.AssignmentSet{Assignments: []api.Assignment{{SessionID: sessionID, Role: api.RoleTarget}}}).
		unlease("studio-a")

	result := Compute(b.build(), Config{})

	require.Len(t, result.Sessions, 1)
	assert.Len(t, result.Assignments["edge-01"].Assignments, 1)

	// And it is attributed to the request rather than showing up as an orphan.
	path := onlyPath(t, result)
	assert.Equal(t, []string{"cam1"}, path.Requests)
	assert.Equal(t, api.StateWaiting, result.Requests["cam1"].State)
}

// With both agents reporting, a flow that is not there really is not there — that is the case
// the freeze above must not swallow.
func TestAMissingFlowWithBothAgentsLiveWithdrawsTheSession(t *testing.T) {
	t.Parallel()

	fleet := base().build()
	sessionID := sessionIDFor(fleet)

	b := newFleet().
		node("studio-a", api.DomainMapping{Name: "cameras", Configured: true}).
		node("edge-01").
		request("cam1", flowRequest("cam1")).
		session(state.SessionRecord{ID: sessionID, Path: pathOf(fleet), FlowDefHash: state.FlowDefHash(flowDef), Fabric: "dc1", Interface: tcpInterface}).
		assignments("edge-01", api.AssignmentSet{Assignments: []api.Assignment{{SessionID: sessionID, Role: api.RoleTarget}}})

	result := Compute(b.build(), Config{})

	path := onlyPath(t, result)
	assert.Equal(t, api.StateWaiting, path.State)
	assert.Equal(t, api.ReasonFlowNotFound, path.ReasonCode)
	assert.Empty(t, result.Sessions)
	assert.Empty(t, result.Assignments["edge-01"].Assignments)
}

// --- selectors, dedup and refcounting (§9.1) --------------------------------------------

func TestGroupHintExpansion(t *testing.T) {
	t.Parallel()

	spec := flowRequest("camera-1")
	spec.Source.Select = api.Selector{GroupHint: &api.GroupHintSelector{Name: "Studio A:Camera 1"}}

	fleet := newFleet().
		node("studio-a", api.DomainMapping{Name: "cameras", Configured: true}).
		node("edge-01").
		flow("studio-a", "cameras", api.FlowInventory{
			ID: "flow-video", Definition: flowDef, Producing: true,
			GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "video"},
		}).
		flow("studio-a", "cameras", api.FlowInventory{
			ID: "flow-audio", Definition: flowDef, Producing: true,
			GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "audio"},
		}).
		flow("studio-a", "cameras", api.FlowInventory{
			ID: "flow-other", Definition: flowDef, Producing: true,
			GroupHint: &api.GroupHint{Name: "Studio A:Camera 2", Type: "video"},
		}).
		flow("studio-a", "cameras", api.FlowInventory{ID: "flow-untagged", Definition: flowDef, Producing: true}).
		request("camera-1", spec).
		build()

	result := Compute(fleet, Config{})

	// Omitting the type selects everything sharing the name, which is how a camera's video and
	// audio are replicated together with one request.
	assert.Len(t, result.Paths, 2)
	assert.Len(t, result.Sessions, 2)
	assert.Len(t, result.Assignments["edge-01"].Assignments, 2)
	assert.Len(t, result.Requests["camera-1"].Paths, 2)

	spec.Source.Select.GroupHint.Type = "video"
	narrowed := Compute(newFleetFrom(fleet, "camera-1", spec), Config{})
	assert.Len(t, narrowed.Paths, 1)
}

func newFleetFrom(fleet *state.Fleet, id string, spec api.RequestSpec) *state.Fleet {
	entry := fleet.Requests[id]
	entry.Value.Spec = spec
	fleet.Requests[id] = entry
	return fleet
}

// N requests naming one edge share one path, one session and one worker pair; the path goes
// away when the last of them is cancelled (§3).
func TestRequestsSharingAPathAreRefcounted(t *testing.T) {
	t.Parallel()

	two := base().request("cam1-again", flowRequest("cam1-again")).build()
	result := Compute(two, Config{})

	assert.Len(t, result.Paths, 1)
	assert.Len(t, result.Sessions, 1)
	assert.Equal(t, []string{"cam1", "cam1-again"}, onlyPath(t, result).Requests)
	assert.Len(t, result.Assignments["edge-01"].Assignments, 1)

	// One cancelled: the path stays, because the other request still wants it.
	delete(two.Requests, "cam1-again")
	assert.Len(t, Compute(two, Config{}).Sessions, 1)

	// The last one cancelled: it goes.
	delete(two.Requests, "cam1")
	assert.Empty(t, Compute(two, Config{}).Sessions)
	assert.Empty(t, Compute(two, Config{}).Assignments["edge-01"].Assignments)
}

// Requests sharing a path have to agree on the settings the shared workers get, and every
// disagreement resolves toward not breaking the other request.
func TestSharedPathSettingsResolveConservatively(t *testing.T) {
	t.Parallel()

	pinned := flowRequest("pinned")
	pinned.Provider = api.ProviderPin{api.ProviderTCP, api.ProviderVerbs}
	pinned.Labels = map[string]string{"a": "1"}
	prio := 40
	pinned.SchedPrio = &prio

	other := flowRequest("other")
	other.Provider = api.ProviderPin{api.ProviderVerbs, api.ProviderTCP}
	other.Labels = map[string]string{"b": "2"}
	higher := 80
	other.SchedPrio = &higher

	fleet := base().build()
	for name, entry := range fleet.Nodes {
		entry.Value.Capabilities.SchedPrio = true
		fleet.Nodes[name] = entry
	}
	delete(fleet.Requests, "cam1")
	fleet.Requests["pinned"] = state.Entry[state.RequestRecord]{Found: true, Value: state.RequestRecord{ID: "pinned", Spec: pinned, CreatedAt: created}}
	fleet.Requests["other"] = state.Entry[state.RequestRecord]{Found: true, Value: state.RequestRecord{ID: "other", Spec: other, CreatedAt: created}}

	result := Compute(fleet, Config{})

	target := find(result.Assignments["edge-01"], api.RoleTarget)
	require.NotNil(t, target)
	require.NotNil(t, target.SchedPrio)
	assert.Equal(t, 80, *target.SchedPrio, "the shared workers get the highest priority asked for")
	assert.Equal(t, map[string]string{"a": "1", "b": "2"}, target.Labels)

	// Pins intersect: a path shared by a verbs-only request and a tcp-only one cannot satisfy
	// both, and says so rather than quietly satisfying one. Both nodes offer both providers, so
	// each request on its own is perfectly valid — the conflict exists only where they meet.
	for name, entry := range fleet.Nodes {
		entry.Value.Capabilities.Fabrics = append(entry.Value.Capabilities.Fabrics, api.FabricAttachment{
			Provider: api.ProviderVerbs, Fabric: "ib-a", Address: "10.9.0.1",
			CapFlags: []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive},
		})
		fleet.Nodes[name] = entry
	}
	pinned.Provider = api.ProviderPin{api.ProviderTCP}
	fleet.Requests["pinned"] = state.Entry[state.RequestRecord]{Found: true, Value: state.RequestRecord{ID: "pinned", Spec: pinned, CreatedAt: created}}
	other.Provider = api.ProviderPin{api.ProviderVerbs}
	fleet.Requests["other"] = state.Entry[state.RequestRecord]{Found: true, Value: state.RequestRecord{ID: "other", Spec: other, CreatedAt: created}}
	conflicted := Compute(fleet, Config{})
	assert.Equal(t, api.StateInvalid, onlyPath(t, conflicted).State)
	assert.Equal(t, api.ReasonPinNotViable, onlyPath(t, conflicted).ReasonCode)
}

// --- session identity (§5.4) ------------------------------------------------------------

// A source flow republished with a different definition makes the destination's local flow
// wrong. The session must be rebuilt, not repaired — which happens for free, because the
// definition is part of the session's identity.
func TestARepublishedFlowRebuildsTheSession(t *testing.T) {
	t.Parallel()

	fleet := base().build()
	oldID := sessionIDFor(fleet)

	changed := newFleet().
		node("studio-a", api.DomainMapping{Name: "cameras", Configured: true}).
		node("edge-01").
		flow("studio-a", "cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef2, Producing: true}).
		request("cam1", flowRequest("cam1")).
		session(state.SessionRecord{ID: oldID, Path: pathOf(fleet), FlowDefHash: state.FlowDefHash(flowDef), Fabric: "dc1", Interface: tcpInterface}).
		build()

	result := Compute(changed, Config{})

	require.Len(t, result.Sessions, 1)
	for id := range result.Sessions {
		assert.NotEqual(t, oldID, id)
	}

	// The path is the same path throughout: it is "this flow, from here to there".
	assert.Equal(t, pathOf(fleet).ID(), onlyPath(t, result).ID)
}

// The negotiated provider is pinned for the session's lifetime. A fabric that goes away fails
// the session loudly rather than quietly re-negotiating onto a slower provider at 3am (§10.4).
func TestAVanishedFabricFailsTheSessionWithoutRenegotiating(t *testing.T) {
	t.Parallel()

	fleet := base().build()
	sessionID := sessionIDFor(fleet)

	b := base().session(state.SessionRecord{
		ID: sessionID, Path: pathOf(fleet), FlowDefHash: state.FlowDefHash(flowDef),
		Fabric: "dc1", Interface: tcpInterface,
	}).
		assignments("edge-01", api.AssignmentSet{Assignments: []api.Assignment{{
			SessionID: sessionID, Role: api.RoleTarget, Domain: "ingest", FlowID: "flow-1", FlowDef: flowDef,
			Fabric: "dc1", Interface: tcpInterface,
		}}}).
		assignments("studio-a", api.AssignmentSet{Assignments: []api.Assignment{{
			SessionID: sessionID, Role: api.RoleInitiator, Domain: "cameras", FlowID: "flow-1",
			Fabric: "dc1", Interface: tcpInterface, Epoch: "epoch-a", TargetInfo: `{"id":"x"}`,
		}}})
	entry := b.fleet.Nodes["edge-01"]
	entry.Value.Capabilities.Fabrics = []api.FabricAttachment{{
		Provider: api.ProviderTCP, Fabric: "dc2", Address: "10.0.0.1",
		CapFlags: []api.CapFlag{api.CapRemoteWrite},
	}}
	b.fleet.Nodes["edge-01"] = entry

	result := Compute(b.build(), Config{})

	path := onlyPath(t, result)
	assert.Equal(t, api.StateFailed, path.State, "FAILED, not INVALID: a fabric can come back on its own")
	assert.Equal(t, api.ReasonFabricGone, path.ReasonCode)
	assert.Equal(t, "dc1", result.Sessions[sessionID].Fabric, "the pinned fabric is not replaced")
	assert.NotEmpty(t, result.Assignments["edge-01"].Assignments, "running workers are left alone")
	assert.NotEmpty(t, result.Assignments["studio-a"].Assignments)
}

// --- invalid requests and conflicts ------------------------------------------------------

// An INVALID request starts nothing: no session, no assignment. Its path is still reported,
// because a request can go invalid *after* it was accepted and may have a session running
// underneath it (see the fabric test below).
func TestAnInvalidRequestStartsNothing(t *testing.T) {
	t.Parallel()

	spec := flowRequest("bad")
	spec.Destinations[0].Root = "bulk"

	fleet := base().request("bad", spec).build()
	result := Compute(fleet, Config{})

	assert.Equal(t, api.StateInvalid, result.Requests["bad"].State)
	assert.Equal(t, api.ReasonUnknownOutputRoot, result.Requests["bad"].ReasonCode)

	require.Len(t, result.Requests["bad"].Paths, 1)
	assert.Equal(t, api.StateInvalid, result.Requests["bad"].Paths[0].State)
	assert.Empty(t, result.Requests["bad"].Paths[0].SessionID)
	assert.Len(t, result.Sessions, 1, "only the valid request has a session")

	// And the valid request alongside it is untouched.
	assert.Equal(t, api.StateEstablishing, result.Requests["cam1"].State)
	assert.Len(t, result.Assignments["edge-01"].Assignments, 1)
}

func TestTwoSourcesIntoOneDestinationFlowIsRejected(t *testing.T) {
	t.Parallel()

	second := flowRequest("from-b")
	second.Source.Node = "studio-b"

	fleet := base().
		node("studio-b", api.DomainMapping{Name: "cameras", Configured: true}).
		flow("studio-b", "cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true}).
		request("from-b", second).
		build()

	// The later request is the one that fails; the older path is the one probably already
	// carrying media.
	entry := fleet.Requests["from-b"]
	entry.Value.CreatedAt = created.Add(time.Hour)
	fleet.Requests["from-b"] = entry

	result := Compute(fleet, Config{})

	assert.Equal(t, api.StateEstablishing, result.Requests["cam1"].State)
	assert.Equal(t, api.StateInvalid, result.Requests["from-b"].State)
	assert.Equal(t, api.ReasonFlowConflict, result.Requests["from-b"].ReasonCode)
	assert.Len(t, result.Sessions, 1)
}

// --- aggregation and determinism ---------------------------------------------------------

// "1 of 3 active" is the answer an operator needs from a group-hint request, and it has no
// meaning in a one-flow-per-request model (§9.1).
func TestRequestStatusAggregatesOverItsPaths(t *testing.T) {
	t.Parallel()

	spec := flowRequest("camera-1")
	spec.Source.Select = api.Selector{GroupHint: &api.GroupHintSelector{Name: "Studio A:Camera 1"}}

	fleet := newFleet().
		node("studio-a", api.DomainMapping{Name: "cameras", Configured: true}).
		node("edge-01").
		flow("studio-a", "cameras", api.FlowInventory{
			ID: "flow-video", Definition: flowDef, Producing: true,
			GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "video"},
		}).
		flow("studio-a", "cameras", api.FlowInventory{
			ID: "flow-audio", Definition: flowDef, Producing: false,
			GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "audio"},
		}).
		request("camera-1", spec).
		build()

	status := Compute(fleet, Config{}).Requests["camera-1"]

	assert.Len(t, status.Paths, 2)
	assert.Equal(t, 1, status.Counts[api.StateEstablishing])
	assert.Equal(t, 1, status.Counts[api.StatePaused])
	assert.Equal(t, api.StateEstablishing, status.State)
}

// A selector matching nothing is a request with zero paths — but only when there is an agent
// reporting that it matched nothing.
func TestASelectorMatchingNothingWaits(t *testing.T) {
	t.Parallel()

	spec := flowRequest("camera-9")
	spec.Source.Select = api.Selector{GroupHint: &api.GroupHintSelector{Name: "nothing"}}

	result := Compute(base().request("camera-9", spec).build(), Config{})
	assert.Equal(t, api.StateWaiting, result.Requests["camera-9"].State)
	assert.Equal(t, api.ReasonFlowNotFound, result.Requests["camera-9"].ReasonCode)

	gone := base().request("camera-9", spec).unlease("studio-a").build()
	waiting := Compute(gone, Config{}).Requests["camera-9"]
	assert.Equal(t, api.StateWaiting, waiting.State)
	assert.Equal(t, api.ReasonAgentNotLeased, waiting.ReasonCode,
		"an agent that is not reporting has not told us the selector matched nothing")
}

// Two replicas computing from one snapshot must produce byte-identical assignments, or an agent
// polling through a load balancer oscillates between two versions and restarts workers on every
// swing (plan §4.5).
func TestComputeIsDeterministic(t *testing.T) {
	t.Parallel()

	spec := flowRequest("camera-1")
	spec.Source.Select = api.Selector{GroupHint: &api.GroupHintSelector{Name: "Studio A:Camera 1"}}

	fleet := newFleet().
		node("studio-a", api.DomainMapping{Name: "cameras", Configured: true}).
		node("edge-01").
		request("camera-1", spec).
		request("cam1", flowRequest("cam1")).
		build()
	for _, id := range []string{"flow-1", "flow-2", "flow-3"} {
		newFleetWithFlow(fleet, id)
	}

	first, err := json.Marshal(Compute(fleet, Config{}).Assignments)
	require.NoError(t, err)
	for range 10 {
		again, err := json.Marshal(Compute(fleet, Config{}).Assignments)
		require.NoError(t, err)
		assert.JSONEq(t, string(first), string(again))
	}
}

func newFleetWithFlow(fleet *state.Fleet, id string) {
	entry := fleet.Inventory["studio-a"]
	entry.Found = true
	entry.Value.Node = "studio-a"
	flow := api.FlowInventory{
		ID: id, Definition: flowDef, Producing: true,
		GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "video"},
	}
	if len(entry.Value.Domains) == 0 {
		entry.Value.Domains = []api.DomainInventory{{Name: "cameras", Configured: true}}
	}
	entry.Value.Domains[0].Flows = append(entry.Value.Domains[0].Flows, flow)
	fleet.Inventory["studio-a"] = entry
}

// --- helpers -----------------------------------------------------------------------------

// pathOf is the identity the base fleet's request expands onto. The root is the *resolved* one:
// the request names none and edge-01 advertises exactly one, and it is the resolved value that
// the identity and the session ID are derived from (§10.6).
func pathOf(fleet *state.Fleet) state.PathIdentity {
	return state.PathIdentity{
		Source:      api.FlowAddress{Node: "studio-a", Domain: "cameras", Flow: "flow-1"},
		Destination: api.Destination{Node: "edge-01", Domain: []string{"ingest"}, Root: "fast"},
	}
}

func sessionIDFor(fleet *state.Fleet) string {
	return state.SessionID(pathOf(fleet), state.FlowDefHash(flowDef))
}
