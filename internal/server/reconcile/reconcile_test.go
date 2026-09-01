package reconcile

import (
	"encoding/json"
	"fmt"
	"strings"
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
		Revision:     100,
		Nodes:        map[string]state.Entry[state.NodeRecord]{},
		Leases:       map[string]state.Entry[state.LeaseRecord]{},
		Inventory:    map[string]state.Entry[api.InventorySnapshot]{},
		Status:       map[string]state.Entry[api.StatusSnapshot]{},
		Namespaces:   map[string]state.Entry[state.NamespaceRecord]{},
		DomainLabels: map[state.DomainKey]state.Entry[api.DomainLabels]{},
		Requests:     map[api.RequestID]state.Entry[state.RequestRecord]{},
		Sessions:     map[string]state.Entry[state.SessionRecord]{},
		Assignments:  map[string]state.Entry[api.AssignmentSet]{},
	}}
}

func (b *fleetBuilder) node(name string) *fleetBuilder {
	b.fleet.Nodes[name] = state.Entry[state.NodeRecord]{Found: true, Value: state.NodeRecord{
		Node: name,
		Capabilities: api.Capabilities{
			Fabrics: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.0.0." + name[len(name)-1:],
				CapFlags: []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive},
			}},
			// One writable area on every node, so any of them can be a destination; a
			// destination always names its area now (§10.6).
			Areas: []api.Area{{Name: "fast", Path: "/dev/shm/mxl", Read: true, Write: true}},
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

// flow adds one observed flow. The domain is written the way a manifest spells it,
// `<area>/<elements>`, and split here because a test is allowed the convenience the rest of the
// tree is not — an agent computes the identity from its own area table (§10.6).
func (b *fleetBuilder) flow(node, domain string, flow api.FlowInventory) *fleetBuilder {
	entry := b.fleet.Inventory[node]
	entry.Found = true
	entry.Value.Node = node

	for i := range entry.Value.Domains {
		if entry.Value.Domains[i].Domain.String() == domain {
			entry.Value.Domains[i].Flows = append(entry.Value.Domains[i].Flows, flow)
			b.fleet.Inventory[node] = entry
			return b
		}
	}
	segments := strings.Split(domain, "/")
	entry.Value.Domains = append(entry.Value.Domains, api.DomainInventory{
		Domain: api.Domain{Area: segments[0], Elements: segments[1:]},
		Flows:  []api.FlowInventory{flow},
	})
	b.fleet.Inventory[node] = entry
	return b
}

// request adds one request. The name is a bare string for brevity; the ID is the pair, defaulted
// from the spec's namespace exactly as the server does on write (§9.3).
func (b *fleetBuilder) request(name string, spec api.RequestSpec) *fleetBuilder {
	spec.Name = name
	id := spec.RequestID()
	b.fleet.Requests[id] = state.Entry[state.RequestRecord]{Found: true, Value: state.RequestRecord{
		ID: id, Spec: spec, CreatedAt: created,
	}}
	return b
}

// namespace declares one, which is what makes its `paths` policy anything but the default.
func (b *fleetBuilder) namespace(name string, paths api.PathPolicy) *fleetBuilder {
	b.fleet.Namespaces[name] = state.Entry[state.NamespaceRecord]{Found: true, Value: state.NamespaceRecord{
		Name: name, Spec: api.Namespace{Name: name, Paths: paths},
	}}
	return b
}

// rid is the ID of a request in the default namespace, which is where the tests below put
// everything they do not say otherwise about.
func rid(name string) api.RequestID {
	return api.RequestID{Namespace: api.DefaultNamespace, Name: name}
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
		Sources:      []api.Source{{Node: "studio-a", Domain: named("media/cameras"), Select: api.Selector{Flow: "flow-1"}}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	}
}

// inNamespace puts a spec in a namespace. Sharing a path is refused only inside a namespace whose
// `paths` policy is exclusive (§9.3), so the tests that exercise the rule say which namespace each
// request is in *and* declare that namespace exclusive.
func inNamespace(spec api.RequestSpec, ns string) api.RequestSpec {
	spec.Namespace = ns
	return spec
}

// base is the ordinary fleet: two registered nodes, one request, one flow being produced.
func base() *fleetBuilder {
	return newFleet().
		node("studio-a").
		node("edge-01").
		flow("studio-a", "media/cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true}).
		request("cam1", flowRequest("cam1"))
}

// --- fan-in: many sources, many destinations (§9.1) --------------------------------------

// wallSource is one studio publishing `flow-<n>` into `media/cameras`.
func wallSource(node string) api.Source {
	return api.Source{Node: node, Domain: named("media/cameras"), Select: api.Selector{All: true}}
}

// wall is the arrangement fan-in exists for: three studios into one ingest domain. Each studio
// publishes one flow with a distinct ID, because two studios publishing one ID is the corruption
// case and has its own tests.
func wall(t *testing.T, producing map[string]bool) *state.Fleet {
	t.Helper()

	spec := api.RequestSpec{
		Name:         "wall",
		Sources:      []api.Source{wallSource("studio-a"), wallSource("studio-b"), wallSource("studio-c")},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	}

	b := newFleet().node("edge-01").request("wall", spec)
	for _, node := range []string{"studio-a", "studio-b", "studio-c"} {
		b.node(node).flow(node, "media/cameras", api.FlowInventory{
			ID: "flow-" + node, Definition: flowDef, Producing: true,
		})

		// A destination flow that is advancing is what makes a path ACTIVE — never the worker
		// (§11) — so the sessions below only reach ACTIVE for the sources named here.
		identity := state.PathIdentity{
			Source:      api.FlowAddress{Node: node, Domain: "media/cameras", Flow: "flow-" + node},
			Destination: api.Destination{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}},
		}
		sessionID := state.SessionID(identity, state.FlowDefHash(flowDef))

		b.flow("edge-01", "fast/ingest", api.FlowInventory{
			ID: "flow-" + node, Definition: flowDef, Producing: producing[node], Replicated: true,
		})
		b.session(state.SessionRecord{
			ID: sessionID, Path: identity, FlowDefHash: state.FlowDefHash(flowDef),
			Fabric: "dc1", Interface: tcpInterface,
		})
		b.sessionStatus("edge-01", api.SessionStatus{
			SessionID: sessionID, Role: api.RoleTarget, State: api.WorkerReady,
			Epoch: "epoch-" + node, TargetInfo: `{"id":"x"}`, Address: "10.0.0.1", Service: "24001",
		})
		b.sessionStatus(node, api.SessionStatus{
			SessionID: sessionID, Role: api.RoleInitiator, State: api.WorkerReady, Epoch: "epoch-" + node,
		})
	}
	return b.build()
}

// A request is the cross product of its two lists: three sources into one destination is three
// paths, one materialised domain and one target worker each (§9.1).
func TestARequestFansInFromEverySource(t *testing.T) {
	t.Parallel()

	result := Compute(wall(t, map[string]bool{"studio-a": true, "studio-b": true, "studio-c": true}), Config{})

	require.Len(t, result.Paths, 3)
	seen := map[string]bool{}
	for _, path := range result.Paths {
		seen[path.Source.Node] = true
		assert.Equal(t, "edge-01/fast/ingest", path.Destination.Endpoint(),
			"every path lands in the one destination domain")
	}
	assert.Equal(t, map[string]bool{"studio-a": true, "studio-b": true, "studio-c": true}, seen)

	// Three targets on the destination node — 3× ingress there, which is the grouping fan-in makes
	// legible and the binding direction for an ingest wall (§9.1).
	assert.Len(t, result.Assignments["edge-01"].Assignments, 3)
	for _, node := range []string{"studio-a", "studio-b", "studio-c"} {
		assert.Len(t, result.Assignments[node].Assignments, 1, "one initiator on %s", node)
	}

	status := result.Requests[rid("wall")]
	assert.Equal(t, api.StateActive, status.State)
	require.Len(t, status.Sources, 3, "a row per source, in the order the request lists them")
	for i, node := range []string{"studio-a", "studio-b", "studio-c"} {
		assert.Equal(t, node, status.Sources[i].Source.Node)
		assert.Equal(t, api.StateActive, status.Sources[i].State)
		assert.Len(t, status.Sources[i].Paths, 1)
	}
}

// **The point of validating per pairing.** One unusable pairing makes the request INVALID without
// stopping its siblings — the fan-in mirror of the fan-out case above.
func TestOneInvalidPairingDoesNotStopTheOtherSources(t *testing.T) {
	t.Parallel()

	spec := api.RequestSpec{
		Name:         "wall",
		Sources:      []api.Source{wallSource("studio-a"), wallSource("typo")},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	}
	fleet := newFleet().
		node("studio-a").node("edge-01").
		flow("studio-a", "media/cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true}).
		request("wall", spec).build()

	result := Compute(fleet, Config{})
	status := result.Requests[rid("wall")]

	assert.Equal(t, api.StateInvalid, status.State)
	assert.Equal(t, api.ReasonNodeNotRegistered, status.ReasonCode)

	// The good source still has a real path with a real assignment.
	require.Len(t, result.Paths, 1)
	assert.Len(t, result.Assignments["edge-01"].Assignments, 1)
	require.Len(t, status.Sources, 2)
	assert.Equal(t, api.StateEstablishing, status.Sources[0].State)
	assert.Equal(t, api.StateInvalid, status.Sources[1].State)
	assert.Empty(t, status.Sources[1].Paths, "a source that expanded to nothing still gets a row")
}

// Three-way attribution. Naming the wrong end sends an operator to a node where everything is
// fine, so each case is asserted separately: a message that is merely non-empty passes all three
// (§9.1, §11).
func TestTheReasonNamesTheEndThatFailed(t *testing.T) {
	t.Parallel()

	fast := api.Domain{Area: "fast", Elements: []string{"ingest"}}
	bulk := api.Domain{Area: "bulk", Elements: []string{"ingest"}}

	base := func() *fleetBuilder {
		return newFleet().node("studio-a").node("studio-b").node("edge-01").node("edge-02").
			flow("studio-a", "media/cameras", api.FlowInventory{ID: "flow-a", Definition: flowDef, Producing: true}).
			flow("studio-b", "media/cameras", api.FlowInventory{ID: "flow-b", Definition: flowDef, Producing: true})
	}
	spec := func(sources []api.Source, destinations []api.Destination) api.RequestSpec {
		return api.RequestSpec{Name: "wall", Sources: sources, Destinations: destinations}
	}

	// One source failed against every destination: blame the source. `studio-z` has never
	// registered, and both destinations are perfectly healthy.
	t.Run("common to one source", func(t *testing.T) {
		t.Parallel()

		status := Compute(base().request("wall", spec(
			[]api.Source{wallSource("studio-a"), wallSource("studio-z")},
			[]api.Destination{{Node: "edge-01", Domain: fast}, {Node: "edge-02", Domain: fast}},
		)).build(), Config{}).Requests[rid("wall")]

		assert.Equal(t, api.ReasonNodeNotRegistered, status.ReasonCode)
		assert.Contains(t, status.Reason, "studio-z")
		assert.NotContains(t, status.Reason, "destination edge-01", "the destinations are fine")
		assert.NotContains(t, status.Reason, "destination edge-02")
	})

	// One destination failed against every source: blame the destination. `edge-02` advertises no
	// `bulk` area, and both sources are fine.
	t.Run("common to one destination", func(t *testing.T) {
		t.Parallel()

		status := Compute(base().request("wall", spec(
			[]api.Source{wallSource("studio-a"), wallSource("studio-b")},
			[]api.Destination{{Node: "edge-01", Domain: fast}, {Node: "edge-02", Domain: bulk}},
		)).build(), Config{}).Requests[rid("wall")]

		assert.Equal(t, api.ReasonUnknownArea, status.ReasonCode)
		assert.Contains(t, status.Reason, "destination edge-02/bulk/ingest")
		assert.NotContains(t, status.Reason, "studio-a")
		assert.NotContains(t, status.Reason, "studio-b")
	})

	// Every pairing failed identically: blame neither end. `sched_prio` is a property of the host
	// and no fixture node has the capability, so all four pairings fail the same way — and
	// "(and 3 more)" would suggest three further problems rather than one counted four times.
	t.Run("common to every pairing", func(t *testing.T) {
		t.Parallel()

		prio := 10
		request := spec(
			[]api.Source{wallSource("studio-a"), wallSource("studio-b")},
			[]api.Destination{{Node: "edge-01", Domain: fast}, {Node: "edge-02", Domain: fast}},
		)
		request.SchedPrio = &prio

		status := Compute(base().request("wall", request).build(), Config{}).Requests[rid("wall")]

		assert.Equal(t, api.ReasonSchedPrioUnavailable, status.ReasonCode)
		assert.NotContains(t, status.Reason, "and 3 more")
		assert.NotContains(t, status.Reason, "destination edge-")
	})
}

// --- PARTIAL: the aggregate-only state (§11) ---------------------------------------------

// One dark camera among three is the ordinary state of an ingest wall, and the old fold called
// that request PAUSED — a true statement about one path and a false one about the request.
func TestOneDarkSourceMakesTheRequestPartial(t *testing.T) {
	t.Parallel()

	result := Compute(wall(t, map[string]bool{"studio-a": true, "studio-b": true}), Config{})
	status := result.Requests[rid("wall")]

	assert.Equal(t, api.StatePartial, status.State)
	assert.Equal(t, 2, status.Counts[api.StateActive])
	assert.Equal(t, 1, status.Counts[api.StatePaused])
	assert.Contains(t, status.Reason, "2 of 3")

	// The per-source breakdown is what says *which* studio is dark. The aggregate cannot.
	require.Len(t, status.Sources, 3)
	assert.Equal(t, api.StateActive, status.Sources[0].State)
	assert.Equal(t, api.StateActive, status.Sources[1].State)
	assert.Equal(t, api.StatePaused, status.Sources[2].State)
	assert.Equal(t, "studio-c", status.Sources[2].Source.Node)
}

// The surprising half, and the property a worst-wins fold passes every other test without having:
// PARTIAL outranks INVALID. §7.2 already settled that one bad path among twenty does not condemn
// the other nineteen, and promoting it to the top line would undo that where it is read first.
func TestPartialOutranksAnInvalidLeg(t *testing.T) {
	t.Parallel()

	fleet := wall(t, map[string]bool{"studio-a": true, "studio-b": true, "studio-c": true})

	// A fourth source that can never work, alongside three that are carrying media.
	record := fleet.Requests[rid("wall")]
	record.Value.Spec.Sources = append(record.Value.Spec.Sources, wallSource("studio-z"))
	fleet.Requests[rid("wall")] = record

	status := Compute(fleet, Config{}).Requests[rid("wall")]

	assert.Equal(t, api.StatePartial, status.State)
	assert.Equal(t, api.ReasonNodeNotRegistered, status.ReasonCode, "the code still says what is wrong")
	assert.Contains(t, status.Reason, "studio-z")
	assert.Equal(t, 3, status.Counts[api.StateActive])

	// Still refused at POST: the aggregate softening is a *display* decision and must not change
	// what admission does (§7.2).
	assert.Contains(t, Compute(fleet, Config{}).Structural, rid("wall"))
}

// PARTIAL claims something is working, so it must not be said when nothing is.
func TestARequestWithNoActivePathIsNeverPartial(t *testing.T) {
	t.Parallel()

	status := Compute(wall(t, nil), Config{}).Requests[rid("wall")]

	assert.Equal(t, api.StatePaused, status.State)
	assert.Equal(t, 3, status.Counts[api.StatePaused])
}

// PARTIAL is the one aggregate-only value in the vocabulary: it describes disagreement among many
// things, and a path, a session and a worker are one thing each (§11).
func TestPartialNeverAppearsBelowTheRequest(t *testing.T) {
	t.Parallel()

	result := Compute(wall(t, map[string]bool{"studio-a": true}), Config{})
	require.Equal(t, api.StatePartial, result.Requests[rid("wall")].State)

	for _, path := range result.Paths {
		assert.NotEqual(t, api.StatePartial, path.State)
		if path.Session != nil {
			for _, endpoint := range []*api.SessionEndpoint{path.Session.Target, path.Session.Initiator} {
				if endpoint != nil {
					assert.NotEqual(t, api.WorkerState(api.StatePartial), endpoint.State)
				}
			}
		}
	}
	assert.NotContains(t, api.States(), api.StatePartial, "not a state one thing can be in")
	assert.Contains(t, api.RequestStates(), api.StatePartial)
}

// --- the `all` selector (§9.1) -----------------------------------------------------------

// A source that names a domain and says nothing else replicates every flow in it, and gains a path
// when a producer adds one.
func TestAllSelectsEveryFlowInTheDomain(t *testing.T) {
	t.Parallel()

	spec := api.RequestSpec{
		Name:         "whole",
		Sources:      []api.Source{{Node: "studio-a", Domain: named("media/cameras"), Select: api.Selector{All: true}}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	}

	build := func(flows ...string) *state.Fleet {
		b := newFleet().node("studio-a").node("edge-01").request("whole", spec)
		for _, id := range flows {
			b.flow("studio-a", "media/cameras", api.FlowInventory{ID: id, Definition: flowDef, Producing: true})
		}
		return b.build()
	}

	assert.Len(t, Compute(build("flow-1", "flow-2"), Config{}).Paths, 2)
	assert.Len(t, Compute(build("flow-1", "flow-2", "flow-3"), Config{}).Paths, 3,
		"a new producer joins the expansion, like a group hint and unlike a pinned flow")

	// Matching nothing is a request with zero paths, which composes with WAITING at no extra cost.
	empty := Compute(build(), Config{}).Requests[rid("whole")]
	assert.Equal(t, api.StateWaiting, empty.State)
	assert.Equal(t, api.ReasonFlowNotFound, empty.ReasonCode)
}

// `all` against a *label-matched* domain is filtered by provenance like any other matched
// selector: it must not pick up what this project is itself writing, or a receiver that forwards
// what it receives is an amplifier (§10.7).
func TestAllAgainstALabelSelectorStillExcludesOwnOutput(t *testing.T) {
	t.Parallel()

	spec := api.RequestSpec{
		Name: "onward",
		Sources: []api.Source{{
			Node:   "edge-01",
			Domain: api.SelectLabels(map[string]string{"role": "onward"}),
			Select: api.Selector{All: true},
		}},
		Destinations: []api.Destination{{Node: "edge-02", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	}

	result := Compute(newFleet().
		node("edge-01").node("edge-02").
		label("edge-01", "fast/ingest", map[string]string{"role": "onward"}).
		flow("edge-01", "fast/ingest", api.FlowInventory{ID: "replicated", Definition: flowDef, Producing: true, Replicated: true}).
		flow("edge-01", "fast/ingest", api.FlowInventory{ID: "local", Definition: flowDef, Producing: true}).
		request("onward", spec).build(), Config{})

	require.Len(t, result.Paths, 1, "the locally produced flow still matches")
	assert.Equal(t, "local", onlyPath(t, result).Source.Flow)

	excluded := result.Requests[rid("onward")].Excluded
	require.Len(t, excluded, 1)
	assert.Equal(t, "replicated", excluded[0].Flow)
	assert.Equal(t, api.ExclusionSelfOutput, excluded[0].Reason)
}

// --- fan-out: one source, many destinations (§9.1, 8a) -----------------------------------

// A request fans out to a path per destination, and each path is its own session and its own
// worker pair. The request's status aggregates over all of them (§11).
func TestARequestFansOutToOnePathPerDestination(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Destinations = []api.Destination{
		{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}},
		{Node: "edge-02", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}},
		{Node: "archive-01", Domain: api.Domain{Area: "fast", Elements: []string{"capture"}}},
	}

	fleet := base().node("edge-02").node("archive-01").request("cam1", spec).build()
	result := Compute(fleet, Config{})

	require.Len(t, result.Paths, 3)
	require.Len(t, result.Requests[rid("cam1")].Paths, 3)

	// Every destination gets its own path, its own session, and a target assignment on its own
	// node. The source node carries one initiator per destination: fan-out is N workers reading
	// the same local flow, which is the cost the grouping makes legible.
	seen := map[string]bool{}
	for _, path := range result.Paths {
		seen[path.Destination.Endpoint()] = true
		assert.Equal(t, "flow-1", path.Source.Flow)
		assert.Equal(t, "fast", path.Destination.Domain.Area, "the area is inside the domain's name")
		assert.Equal(t, []string{"default/cam1"}, path.Requests)
	}
	assert.Equal(t, map[string]bool{
		"edge-01/fast/ingest": true, "edge-02/fast/ingest": true, "archive-01/fast/capture": true,
	}, seen)

	for _, node := range []string{"edge-01", "edge-02", "archive-01"} {
		assert.Len(t, result.Assignments[node].Assignments, 1, "one target on %s", node)
	}
	assert.Len(t, result.Assignments["studio-a"].Assignments, 0,
		"no initiator until each target has reported an epoch (invariant 3)")

	assert.Equal(t, api.StateEstablishing, result.Requests[rid("cam1")].State)
	assert.Equal(t, 3, result.Requests[rid("cam1")].Counts[api.StateEstablishing])
}

// **The point of validating per destination.** One unusable destination makes the request
// INVALID, but it must not stop its siblings: they are separate paths on separate nodes and
// nothing about them changed.
func TestOneInvalidDestinationDoesNotStopTheOthers(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Destinations = []api.Destination{
		{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}},
		{Node: "edge-02", Domain: api.Domain{Area: "bulk", Elements: []string{"ingest"}}}, // edge-02 advertises only "fast"
	}

	fleet := base().node("edge-02").request("cam1", spec).build()
	result := Compute(fleet, Config{})

	status := result.Requests[rid("cam1")]
	assert.Equal(t, api.StateInvalid, status.State)
	assert.Equal(t, api.ReasonUnknownArea, status.ReasonCode)
	assert.Contains(t, status.Reason, "edge-02/bulk/ingest",
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
	spec.Sources[0].Node = "typo" // never registered: nothing about any destination is wrong
	spec.Destinations = []api.Destination{
		{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}},
		{Node: "edge-02", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}},
	}

	fleet := base().node("edge-02").request("cam1", spec).build()
	status := Compute(fleet, Config{}).Requests[rid("cam1")]

	assert.Equal(t, api.StateInvalid, status.State)
	assert.Equal(t, api.ReasonNodeNotRegistered, status.ReasonCode)
	assert.Contains(t, status.Reason, `source node "typo"`)
	assert.NotContains(t, status.Reason, "destination edge-01/fast/ingest")
	assert.NotContains(t, status.Reason, "more destination")
}

// A leg that expands to no paths at all still has to say why it is unusable. Without this a
// request whose source flow does not exist *yet* and whose destination can never work reports
// WAITING, and POST lets it through — which is exactly what §7.2 requires be rejected.
func TestAnUnusableDestinationIsInvalidEvenWithNothingToExpandOnto(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Sources[0].Select = api.Selector{Flow: "flow-does-not-exist-yet"}
	spec.Destinations = []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "bulk", Elements: []string{"ingest"}}}}

	result := Compute(base().request("cam1", spec).build(), Config{})

	status := result.Requests[rid("cam1")]
	assert.Equal(t, api.StateInvalid, status.State, "not WAITING: the destination can never work")
	assert.Equal(t, api.ReasonUnknownArea, status.ReasonCode)
}

// A per-destination pin overrides the request-level one rather than intersecting it, so
// "verbs here, tcp there" is an ordinary request and not a pin conflict (§10.4).
func TestAPerDestinationPinOverridesTheRequestPin(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Provider = api.ProviderPin{api.ProviderEFA} // not viable for these fixture nodes
	spec.Destinations = []api.Destination{
		{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}, Provider: api.ProviderPin{api.ProviderTCP}},
		{Node: "edge-02", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}},
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
		{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}},
		{Node: "edge-02", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}},
	}
	narrow := inNamespace(flowRequest("narrow"), "archive")
	narrow.Destinations = []api.Destination{{Node: "edge-02", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}}

	fleet := newFleet().
		node("studio-a").
		node("edge-01").node("edge-02").
		flow("studio-a", "media/cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true}).
		request("wide", wide).request("narrow", narrow).
		build()
	result := Compute(fleet, Config{})

	require.Len(t, result.Paths, 2, "three destination entries, two distinct edges")
	for _, path := range result.Paths {
		if path.Destination.Node == "edge-02" {
			assert.Equal(t, []string{"archive/narrow", "default/wide"}, path.Requests)
		} else {
			assert.Equal(t, []string{"default/wide"}, path.Requests)
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
	assert.Equal(t, api.Domain{Area: "fast", Elements: []string{"ingest"}}, target.Domain)
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
	assert.Equal(t, api.Domain{Area: "media", Elements: []string{"cameras"}}, initiator.Domain,
		"an initiator reads its own local domain, in the same grammar the target's carries")
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
		node("studio-a").
		node("edge-01").
		flow("studio-a", "media/cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: sourceProducing}).
		flow("edge-01", "fast/ingest", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: destinationProducing}).
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
			node("studio-a").
			node("edge-01").
			flow("studio-a", "media/cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true}).
			flow("edge-01", "fast/ingest", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true}).
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
		node("studio-a").
		node("edge-01").
		flow("studio-a", "media/cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: false}).
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
			node("studio-a").
			node("edge-01").
			flow("studio-a", "media/cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: false}).
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
		node("studio-a").
		node("edge-01").
		flow("studio-a", "media/cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: false}).
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
		SessionID: sessionID, Role: api.RoleInitiator,
		Domain: api.Domain{Area: "media", Elements: []string{"cameras"}}, FlowID: "flow-1",
		Epoch: "epoch-a", TargetInfo: `{"id":"x"}`,
	}}}

	b := newFleet().
		node("studio-a").
		node("edge-01").
		request("cam1", flowRequest("cam1")).
		session(state.SessionRecord{ID: sessionID, Path: pathOf(fleet), FlowDefHash: state.FlowDefHash(flowDef), Fabric: "dc1", Interface: tcpInterface}).
		assignments("studio-a", running).
		assignments("edge-01", api.AssignmentSet{Assignments: []api.Assignment{{
			SessionID: sessionID, Role: api.RoleTarget,
			Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}, FlowID: "flow-1", FlowDef: flowDef,
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

// **The two ends of a path may be one node.** That is what shm is structurally for and what the
// single-host quick start does, so it is an ordinary arrangement rather than a corner.
//
// Carrying a frozen session forward reads every assignment the node holds *for that session*
// rather than the one belonging to the end being visited — so visiting one node once per end
// copies both roles twice. And because each pass reads back what the last one wrote, that is not
// an off-by-one but a doubling: 2^n after n passes. Observed in the field as a 258 MB assignment
// document holding 262,144 copies of one assignment, which took the server out with it.
//
// Four passes rather than two: one pass would pass against an implementation that deduplicated
// the *result* while still reading the node twice, and it is the compounding that does the damage.
func TestALoopbackSessionCarriedForwardDoesNotDoubleItsAssignments(t *testing.T) {
	t.Parallel()

	sourceDomain := api.Domain{Area: "media", Elements: []string{"cameras"}}
	destDomain := api.Domain{Area: "fast", Elements: []string{"ingest"}}

	spec := api.RequestSpec{
		Name:         "loopback",
		Sources:      []api.Source{{Node: "n0", Domain: named("media/cameras"), Select: api.Selector{Flow: "flow-1"}}},
		Destinations: []api.Destination{{Node: "n0", Domain: destDomain}},
	}
	path := state.PathIdentity{
		Source:      api.FlowAddress{Node: "n0", Domain: "media/cameras", Flow: "flow-1"},
		Destination: spec.Destinations[0],
	}
	sessionID := state.SessionID(path, state.FlowDefHash(flowDef))

	// Both roles on the one node, which is what a running loopback session looks like.
	carried := api.AssignmentSet{Node: "n0", Assignments: []api.Assignment{
		{SessionID: sessionID, Role: api.RoleTarget, Domain: destDomain, FlowID: "flow-1", FlowDef: flowDef},
		{SessionID: sessionID, Role: api.RoleInitiator, Domain: sourceDomain, FlowID: "flow-1",
			Epoch: "epoch-a", TargetInfo: `{"id":"x"}`},
	}}

	// Unleased, so the session is frozen and carried forward rather than replanned — the path
	// this bug lives on. Rebuilt from `carried` each pass, which is the store round-trip.
	for pass := 1; pass <= 4; pass++ {
		fleet := newFleet().
			node("n0").
			request("loopback", spec).
			session(state.SessionRecord{
				ID: sessionID, Path: path,
				FlowDefHash: state.FlowDefHash(flowDef), Fabric: "dc1", Interface: tcpInterface,
			}).
			assignments("n0", carried).
			unlease("n0").
			build()

		result := Compute(fleet, Config{})
		carried = result.Assignments["n0"]
		require.Len(t, carried.Assignments, 2, "pass %d: one target and one initiator, not %d",
			pass, len(carried.Assignments))
	}
}

// The same rule for a group-hint request: inventory went away with the lease, so the selector
// expands to nothing — and "matched nothing" must not be read as "delete everything".
func TestAGroupHintRequestDoesNotCollapseWhenItsSourceAgentIsGone(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Sources[0].Select = api.Selector{GroupHint: &api.GroupHintSelector{Name: "Studio A:Camera 1"}}

	fleet := base().build()
	sessionID := sessionIDFor(fleet)

	b := newFleet().
		node("studio-a").
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
	assert.Equal(t, []string{"default/cam1"}, path.Requests)
	assert.Equal(t, api.StateWaiting, result.Requests[rid("cam1")].State)
}

// With both agents reporting, a flow that is not there really is not there — that is the case
// the freeze above must not swallow.
func TestAMissingFlowWithBothAgentsLiveWithdrawsTheSession(t *testing.T) {
	t.Parallel()

	fleet := base().build()
	sessionID := sessionIDFor(fleet)

	b := newFleet().
		node("studio-a").
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
	spec.Sources[0].Select = api.Selector{GroupHint: &api.GroupHintSelector{Name: "Studio A:Camera 1"}}

	fleet := newFleet().
		node("studio-a").
		node("edge-01").
		flow("studio-a", "media/cameras", api.FlowInventory{
			ID: "flow-video", Definition: flowDef, Producing: true,
			GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "video"},
		}).
		flow("studio-a", "media/cameras", api.FlowInventory{
			ID: "flow-audio", Definition: flowDef, Producing: true,
			GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "audio"},
		}).
		flow("studio-a", "media/cameras", api.FlowInventory{
			ID: "flow-other", Definition: flowDef, Producing: true,
			GroupHint: &api.GroupHint{Name: "Studio A:Camera 2", Type: "video"},
		}).
		flow("studio-a", "media/cameras", api.FlowInventory{ID: "flow-untagged", Definition: flowDef, Producing: true}).
		request("camera-1", spec).
		build()

	result := Compute(fleet, Config{})

	// Omitting the type selects everything sharing the name, which is how a camera's video and
	// audio are replicated together with one request.
	assert.Len(t, result.Paths, 2)
	assert.Len(t, result.Sessions, 2)
	assert.Len(t, result.Assignments["edge-01"].Assignments, 2)
	assert.Len(t, result.Requests[rid("camera-1")].Paths, 2)

	spec.Sources[0].Select.GroupHint.Type = "video"
	narrowed := Compute(newFleetFrom(fleet, "camera-1", spec), Config{})
	assert.Len(t, narrowed.Paths, 1)
}

func newFleetFrom(fleet *state.Fleet, name string, spec api.RequestSpec) *state.Fleet {
	entry := fleet.Requests[rid(name)]
	entry.Value.Spec = spec
	fleet.Requests[rid(name)] = entry
	return fleet
}

// N requests naming one edge share one path, one session and one worker pair; the path goes
// away when the last of them is cancelled (§3).
func TestRequestsSharingAPathAreRefcounted(t *testing.T) {
	t.Parallel()

	// Both in the default namespace, which is `shared`: refcounting is the base model and
	// forbidding it is the special case a namespace opts into (§9.3).
	two := base().request("cam1-again", flowRequest("cam1-again")).build()
	result := Compute(two, Config{})

	assert.Len(t, result.Paths, 1)
	assert.Len(t, result.Sessions, 1)
	assert.Equal(t, []string{"default/cam1", "default/cam1-again"}, onlyPath(t, result).Requests)
	assert.Len(t, result.Assignments["edge-01"].Assignments, 1)

	// One cancelled: the path stays, because the other request still wants it.
	delete(two.Requests, rid("cam1-again"))
	assert.Len(t, Compute(two, Config{}).Sessions, 1)

	// The last one cancelled: it goes.
	delete(two.Requests, rid("cam1"))
	assert.Empty(t, Compute(two, Config{}).Sessions)
	assert.Empty(t, Compute(two, Config{}).Assignments["edge-01"].Assignments)
}

// Requests sharing a path have to agree on the settings the shared workers get, and every
// disagreement resolves toward not breaking the other request.
func TestSharedPathSettingsResolveConservatively(t *testing.T) {
	t.Parallel()

	// Both in the default namespace, which is `shared` — sharing a path is the ordinary case
	// and only an `exclusive` namespace forbids it (§9.3).
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
	delete(fleet.Requests, rid("cam1"))
	fleet.Requests[rid("pinned")] = state.Entry[state.RequestRecord]{Found: true, Value: state.RequestRecord{ID: rid("pinned"), Spec: pinned, CreatedAt: created}}
	fleet.Requests[rid("other")] = state.Entry[state.RequestRecord]{Found: true, Value: state.RequestRecord{ID: rid("other"), Spec: other, CreatedAt: created}}

	result := Compute(fleet, Config{})

	target := find(result.Assignments["edge-01"], api.RoleTarget)
	require.NotNil(t, target)
	require.NotNil(t, target.SchedPrio)
	assert.Equal(t, 80, *target.SchedPrio, "the shared workers get the highest priority asked for")
	assert.Equal(t, map[string]string{"a": "1", "b": "2"}, target.Labels)
	// The namespace is a metric dimension of its own now (§12), merged like a label: on a path
	// shared across two of them the last merged one wins — arbitrary, but deterministic, which is
	// what matters, since a value that differed between replicas would restart workers.
	assert.Equal(t, api.DefaultNamespace, target.Namespace)

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
	fleet.Requests[rid("pinned")] = state.Entry[state.RequestRecord]{Found: true, Value: state.RequestRecord{ID: rid("pinned"), Spec: pinned, CreatedAt: created}}
	other.Provider = api.ProviderPin{api.ProviderVerbs}
	fleet.Requests[rid("other")] = state.Entry[state.RequestRecord]{Found: true, Value: state.RequestRecord{ID: rid("other"), Spec: other, CreatedAt: created}}
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
		node("studio-a").
		node("edge-01").
		flow("studio-a", "media/cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef2, Producing: true}).
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
			SessionID: sessionID, Role: api.RoleTarget,
			Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}, FlowID: "flow-1", FlowDef: flowDef,
			Fabric: "dc1", Interface: tcpInterface,
		}}}).
		assignments("studio-a", api.AssignmentSet{Assignments: []api.Assignment{{
			SessionID: sessionID, Role: api.RoleInitiator,
			Domain: api.Domain{Area: "media", Elements: []string{"cameras"}}, FlowID: "flow-1",
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
	spec.Destinations[0].Domain.Area = "bulk"

	fleet := base().request("bad", spec).build()
	result := Compute(fleet, Config{})

	assert.Equal(t, api.StateInvalid, result.Requests[rid("bad")].State)
	assert.Equal(t, api.ReasonUnknownArea, result.Requests[rid("bad")].ReasonCode)

	require.Len(t, result.Requests[rid("bad")].Paths, 1)
	assert.Equal(t, api.StateInvalid, result.Requests[rid("bad")].Paths[0].State)
	assert.Empty(t, result.Requests[rid("bad")].Paths[0].SessionID)
	assert.Len(t, result.Sessions, 1, "only the valid request has a session")

	// And the valid request alongside it is untouched.
	assert.Equal(t, api.StateEstablishing, result.Requests[rid("cam1")].State)
	assert.Len(t, result.Assignments["edge-01"].Assignments, 1)
}

func TestTwoSourcesIntoOneDestinationFlowIsRejected(t *testing.T) {
	t.Parallel()

	second := flowRequest("from-b")
	second.Sources[0].Node = "studio-b"

	fleet := base().
		node("studio-b").
		flow("studio-b", "media/cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true}).
		request("from-b", second).
		build()

	// The later request is the one that fails; the older path is the one probably already
	// carrying media.
	entry := fleet.Requests[rid("from-b")]
	entry.Value.CreatedAt = created.Add(time.Hour)
	fleet.Requests[rid("from-b")] = entry

	result := Compute(fleet, Config{})

	assert.Equal(t, api.StateEstablishing, result.Requests[rid("cam1")].State)
	assert.Equal(t, api.StateInvalid, result.Requests[rid("from-b")].State)
	assert.Equal(t, api.ReasonFlowConflict, result.Requests[rid("from-b")].ReasonCode)
	assert.Len(t, result.Sessions, 1)
}

// The undecidable half of the corruption case (§7.2, §7.5). Two sources of *one* request select
// rather than pin, and the fleet then publishes one flow UUID from both — which cannot be caught at
// POST, because the second producer may appear months later.
//
// It is `flow_conflict` on the path, resolved by §7.5 and torn down there, not
// `duplicate_source_flow`: a code has one disposition, and this one tears the loser down.
func TestOneFlowIDFromTwoSelectedSourcesIsAFlowConflict(t *testing.T) {
	t.Parallel()

	spec := api.RequestSpec{
		Name:         "wall",
		Sources:      []api.Source{wallSource("studio-a"), wallSource("studio-b")},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	}

	// Both studios publish `flow-1`, which neither source named.
	fleet := newFleet().
		node("studio-a").node("studio-b").node("edge-01").
		flow("studio-a", "media/cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true}).
		flow("studio-b", "media/cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true}).
		request("wall", spec).build()

	result := Compute(fleet, Config{})

	// One winner, one loser, one session: two initiators into one ring buffer is the corruption
	// this exists to stop, and nothing downstream would detect it.
	assert.Len(t, result.Sessions, 1)

	var loser api.Path
	for _, path := range result.Paths {
		if path.ReasonCode == api.ReasonFlowConflict {
			loser = path
		}
	}
	require.NotEmpty(t, loser.ID, "one of the two paths must lose")
	assert.Equal(t, api.StateInvalid, loser.State)

	// Both sources named, because the tie between two paths of one request falls through to the
	// path ID and is arbitrary from the operator's point of view (§7.5).
	assert.Contains(t, loser.Reason, "studio-a/media/cameras")
	assert.Contains(t, loser.Reason, "studio-b/media/cameras")

	// Not refused at POST: this is not the decidable form, and refusing it would refuse a request
	// that worked perfectly well until a producer somewhere else republished (§7.2).
	assert.NotContains(t, result.Structural, rid("wall"))
}

// --- aggregation and determinism ---------------------------------------------------------

// "1 of 3 active" is the answer an operator needs from a group-hint request, and it has no
// meaning in a one-flow-per-request model (§9.1).
func TestRequestStatusAggregatesOverItsPaths(t *testing.T) {
	t.Parallel()

	spec := flowRequest("camera-1")
	spec.Sources[0].Select = api.Selector{GroupHint: &api.GroupHintSelector{Name: "Studio A:Camera 1"}}

	fleet := newFleet().
		node("studio-a").
		node("edge-01").
		flow("studio-a", "media/cameras", api.FlowInventory{
			ID: "flow-video", Definition: flowDef, Producing: true,
			GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "video"},
		}).
		flow("studio-a", "media/cameras", api.FlowInventory{
			ID: "flow-audio", Definition: flowDef, Producing: false,
			GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "audio"},
		}).
		request("camera-1", spec).
		build()

	status := Compute(fleet, Config{}).Requests[rid("camera-1")]

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
	spec.Sources[0].Select = api.Selector{GroupHint: &api.GroupHintSelector{Name: "nothing"}}

	result := Compute(base().request("camera-9", spec).build(), Config{})
	assert.Equal(t, api.StateWaiting, result.Requests[rid("camera-9")].State)
	assert.Equal(t, api.ReasonFlowNotFound, result.Requests[rid("camera-9")].ReasonCode)

	gone := base().request("camera-9", spec).unlease("studio-a").build()
	waiting := Compute(gone, Config{}).Requests[rid("camera-9")]
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
	spec.Sources[0].Select = api.Selector{GroupHint: &api.GroupHintSelector{Name: "Studio A:Camera 1"}}

	fleet := newFleet().
		node("studio-a").
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
		entry.Value.Domains = []api.DomainInventory{{Domain: api.Domain{Area: "media", Elements: []string{"cameras"}}}}
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
		Source:      api.FlowAddress{Node: "studio-a", Domain: "media/cameras", Flow: "flow-1"},
		Destination: api.Destination{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}},
	}
}

func sessionIDFor(fleet *state.Fleet) string {
	return state.SessionID(pathOf(fleet), state.FlowDefHash(flowDef))
}

// --- namespaces: an opt-in partition (§9.3) -----------------------------------------------

// requestAt is `request` with an explicit UpdatedAt, which is what namespace-overlap precedence
// turns on. The builder's default is the zero time, so tests that do not care get a tie broken
// by request ID.
func requestAt(b *fleetBuilder, name string, spec api.RequestSpec, updated time.Time) *fleetBuilder {
	spec.Name = name
	id := spec.RequestID()
	b.fleet.Requests[id] = state.Entry[state.RequestRecord]{Found: true, Value: state.RequestRecord{
		ID: id, Spec: spec, CreatedAt: created, UpdatedAt: updated,
	}}
	return b
}

// **The default is `shared`, and overlap in it is free.** Two requests expanding onto one path
// share one path, one session and one worker pair, which is §9.1's refcounting working exactly as
// designed: nothing is doubled and nothing is corrupted.
//
// *This inverts the behaviour the tree had, where every namespace was exclusive.* That was the
// position §9.3 supersedes, and the direction of the change is the permissive one — requests the
// server used to refuse are now accepted.
func TestOverlapIsPermittedInASharedNamespace(t *testing.T) {
	t.Parallel()

	fleet := base().build()
	requestAt(&fleetBuilder{fleet: fleet}, "cam1-again", flowRequest("cam1-again"), created.Add(time.Hour))

	result := Compute(fleet, Config{})

	require.Len(t, result.Paths, 1, "one edge, held twice")
	assert.Equal(t, []string{"default/cam1", "default/cam1-again"}, onlyPath(t, result).Requests,
		"the refcount carries both")
	assert.Equal(t, api.StateEstablishing, result.Requests[rid("cam1")].State)
	assert.Equal(t, api.StateEstablishing, result.Requests[rid("cam1-again")].State)
	assert.Empty(t, result.Requests[rid("cam1-again")].ReasonCode)
	assert.Len(t, result.Sessions, 1)
}

// An explicit `shared` reads the same as an unset one. The zero value has a meaning and a record
// that spells it out must not mean something else, or a re-apply of a namespace document would
// change behaviour.
func TestAnExplicitlySharedNamespaceIsTheSameAsNone(t *testing.T) {
	t.Parallel()

	fleet := base().namespace(api.DefaultNamespace, api.PathsShared).build()
	requestAt(&fleetBuilder{fleet: fleet}, "cam1-again", flowRequest("cam1-again"), created.Add(time.Hour))

	assert.Len(t, Compute(fleet, Config{}).Paths, 1)
	assert.Equal(t, api.StateEstablishing, Compute(fleet, Config{}).Requests[rid("cam1-again")].State)
}

// Inside an `exclusive` namespace, two requests may not carry the same path. The path is not the
// problem — it is refcounted and works — but such a namespace claims to be a partition, and a set
// of requests that can quietly share an edge is not one.
func TestTwoRequestsInAnExclusiveNamespaceMayNotShareAPath(t *testing.T) {
	t.Parallel()

	fleet := base().namespace(api.DefaultNamespace, api.PathsExclusive).build()
	requestAt(&fleetBuilder{fleet: fleet}, "cam1-again", flowRequest("cam1-again"), created.Add(time.Hour))

	result := Compute(fleet, Config{})

	// The winner is untouched: one path, one session, and it is *not* refcounted to the loser.
	require.Len(t, result.Paths, 1)
	assert.Equal(t, []string{"default/cam1"}, onlyPath(t, result).Requests)
	assert.Equal(t, api.StateEstablishing, result.Requests[rid("cam1")].State)
	assert.Empty(t, result.Requests[rid("cam1")].ReasonCode)

	// The loser is INVALID and the message names who has it and what to do.
	loser := result.Requests[rid("cam1-again")]
	assert.Equal(t, api.StateInvalid, loser.State)
	assert.Equal(t, api.ReasonNamespaceOverlap, loser.ReasonCode)
	assert.Contains(t, loser.Reason, `request "cam1" already replicates`)
	assert.Contains(t, loser.Reason, "edge-01/fast/ingest")
	assert.Contains(t, loser.Reason, `namespace "default"`)
}

// The rule is per namespace, and a namespace's policy reaches only its own requests. An exclusive
// namespace beside a shared one must not make the shared one's overlaps illegal.
func TestOneNamespacesPolicyDoesNotReachAnother(t *testing.T) {
	t.Parallel()

	fleet := base().
		namespace("strict", api.PathsExclusive).
		request("arch", inNamespace(flowRequest("arch"), "archive")).
		build()

	result := Compute(fleet, Config{})

	require.Len(t, result.Paths, 1)
	assert.Equal(t, []string{"archive/arch", "default/cam1"}, onlyPath(t, result).Requests)
	assert.Equal(t, api.StateEstablishing, result.Requests[rid("cam1")].State)
	assert.Equal(t, api.StateEstablishing,
		result.Requests[api.RequestID{Namespace: "archive", Name: "arch"}].State)
}

// Across namespaces sharing stays legal even when both are exclusive — that is how fan-in is
// expressed, and the rule must not reach it (§9.3).
func TestSharingAcrossExclusiveNamespacesIsUntouched(t *testing.T) {
	t.Parallel()

	fleet := base().
		namespace(api.DefaultNamespace, api.PathsExclusive).
		namespace("archive", api.PathsExclusive).
		request("arch", inNamespace(flowRequest("arch"), "archive")).
		build()

	result := Compute(fleet, Config{})

	require.Len(t, result.Paths, 1)
	assert.Equal(t, []string{"archive/arch", "default/cam1"}, onlyPath(t, result).Requests)
	assert.Equal(t, api.StateEstablishing, result.Requests[rid("cam1")].State)
	assert.Equal(t, api.StateEstablishing,
		result.Requests[api.RequestID{Namespace: "archive", Name: "arch"}].State)
}

// Two requests of one *name* in two namespaces are two requests. That is the whole point of
// scoping names to the namespace rather than fleet-wide (§9.3).
func TestOneNameInTwoNamespacesIsTwoRequests(t *testing.T) {
	t.Parallel()

	fleet := base().request("cam1", inNamespace(flowRequest("cam1"), "archive")).build()

	require.Len(t, fleet.Requests, 2)
	result := Compute(fleet, Config{})
	assert.Equal(t, api.StateEstablishing, result.Requests[rid("cam1")].State)
	assert.Equal(t, api.StateEstablishing,
		result.Requests[api.RequestID{Namespace: "archive", Name: "cam1"}].State)
	assert.Equal(t, []string{"archive/cam1", "default/cam1"}, onlyPath(t, result).Requests,
		"the refcount carries the qualified ID, or the two would be indistinguishable")
}

// **Losing an overlap does not stop media.** An overlapping leg goes down the same route as any
// other invalid leg: it stops the request gaining new sessions, and the path itself — held by
// the winner — carries on. This is the property that makes the rule safe to turn on for a
// namespace that already has overlaps in it.
func TestAnOverlapLoserDoesNotDisturbTheRunningPath(t *testing.T) {
	t.Parallel()

	fleet := base().namespace(api.DefaultNamespace, api.PathsExclusive).build()
	requestAt(&fleetBuilder{fleet: fleet}, "cam1-again", flowRequest("cam1-again"), created.Add(time.Hour))
	result := Compute(fleet, Config{})

	assert.Len(t, result.Sessions, 1, "the winner's session is created as normal")
	assert.Len(t, result.Assignments["edge-01"].Assignments, 1)

	// And the loser still reports the path, so an operator sees what it collided with rather
	// than an empty request.
	require.Len(t, result.Requests[rid("cam1-again")].Paths, 1)
	assert.Equal(t, api.StateEstablishing, result.Requests[rid("cam1-again")].Paths[0].State)
}

// An overlap makes the loser INVALID and does **not** make it unacceptable: it depends on another
// request's expansion, which its author did not write and cannot enumerate. So it stays out of
// [Result.Structural], which is the only thing a POST refuses (§7.2).
func TestAnOverlapIsNotStructural(t *testing.T) {
	t.Parallel()

	fleet := base().namespace(api.DefaultNamespace, api.PathsExclusive).build()
	requestAt(&fleetBuilder{fleet: fleet}, "cam1-again", flowRequest("cam1-again"), created.Add(time.Hour))
	result := Compute(fleet, Config{})

	assert.Equal(t, api.StateInvalid, result.Requests[rid("cam1-again")].State)
	assert.NotContains(t, result.Structural, rid("cam1-again"),
		"a POST would accept it and report the collision")
	assert.Empty(t, result.Structural)
}

// Precedence is by UpdatedAt, so the request that was written most recently is the one refused.
// That is what puts the refusal in front of whoever typed, rather than flipping an untouched
// request to INVALID because somebody else edited theirs.
func TestTheMoreRecentlyWrittenRequestLosesTheOverlap(t *testing.T) {
	t.Parallel()

	older := base().namespace(api.DefaultNamespace, api.PathsExclusive).build()
	requestAt(&fleetBuilder{fleet: older}, "cam1", flowRequest("cam1"), created)
	requestAt(&fleetBuilder{fleet: older}, "cam1-again", flowRequest("cam1-again"), created.Add(time.Hour))
	assert.Equal(t, api.StateInvalid, Compute(older, Config{}).Requests[rid("cam1-again")].State)

	// Flip which one was written last and the verdict flips with it, even though "cam1" sorts
	// first and was created at the same instant.
	newer := base().namespace(api.DefaultNamespace, api.PathsExclusive).build()
	requestAt(&fleetBuilder{fleet: newer}, "cam1", flowRequest("cam1"), created.Add(time.Hour))
	requestAt(&fleetBuilder{fleet: newer}, "cam1-again", flowRequest("cam1-again"), created)
	flipped := Compute(newer, Config{})
	assert.Equal(t, api.StateInvalid, flipped.Requests[rid("cam1")].State)
	assert.Equal(t, api.StateEstablishing, flipped.Requests[rid("cam1-again")].State)
}

// Only the colliding leg is refused. A request fanning out to three destinations where one
// collides keeps the other two, which is the same per-destination discipline every other
// validation follows.
func TestOnlyTheOverlappingLegIsRefused(t *testing.T) {
	t.Parallel()

	wide := flowRequest("wide")
	wide.Destinations = []api.Destination{
		{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}},
		{Node: "edge-02", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}},
	}

	fleet := base().namespace(api.DefaultNamespace, api.PathsExclusive).node("edge-02").build()
	requestAt(&fleetBuilder{fleet: fleet}, "wide", wide, created.Add(time.Hour))
	result := Compute(fleet, Config{})

	// edge-01 collides with `cam1`; edge-02 does not and establishes.
	require.Len(t, result.Paths, 2)
	assert.Equal(t, api.StateInvalid, result.Requests[rid("wide")].State)
	assert.Equal(t, api.ReasonNamespaceOverlap, result.Requests[rid("wide")].ReasonCode)
	assert.Contains(t, result.Requests[rid("wide")].Reason, "destination edge-01/fast/ingest",
		"the leg is named, because the sibling leg is fine")

	for _, path := range result.Paths {
		if path.Destination.Node == "edge-02" {
			assert.Equal(t, []string{"default/wide"}, path.Requests)
		} else {
			assert.Equal(t, []string{"default/cam1"}, path.Requests)
		}
	}
}

// The overlap that cannot be decided when the request is written: a pinned flow against a group
// hint that does not match it yet. A producer republishing under that hint makes them collide
// with nothing written, which is why this rule lives in the reconcile and not only in validation.
func TestAnOverlapCanArriveWhenAProducerRetagsAFlow(t *testing.T) {
	t.Parallel()

	hint := flowRequest("camera-1")
	hint.Sources[0].Select = api.Selector{GroupHint: &api.GroupHintSelector{Name: "Studio A:Camera 1"}}

	build := func(tag *api.GroupHint) *state.Fleet {
		b := newFleet().
			namespace(api.DefaultNamespace, api.PathsExclusive).
			node("studio-a").
			node("edge-01").
			flow("studio-a", "media/cameras", api.FlowInventory{
				ID: "flow-video", Definition: flowDef, Producing: true,
				GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "video"},
			}).
			flow("studio-a", "media/cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true, GroupHint: tag})
		requestAt(b, "camera-1", hint, created)
		requestAt(b, "pinned", flowRequest("pinned"), created.Add(time.Hour))
		return b.build()
	}

	// While flow-1 carries no hint, the two selectors are disjoint and both are fine.
	before := Compute(build(nil), Config{})
	assert.Equal(t, api.StateEstablishing, before.Requests[rid("pinned")].State)
	assert.Equal(t, api.StateEstablishing, before.Requests[rid("camera-1")].State)
	assert.Len(t, before.Paths, 2)

	// The producer republishes flow-1 under the hint the other request selects. Nothing was
	// written, and the more recently written request is now INVALID.
	after := Compute(build(&api.GroupHint{Name: "Studio A:Camera 1", Type: "audio"}), Config{})
	assert.Equal(t, api.StateInvalid, after.Requests[rid("pinned")].State)
	assert.Equal(t, api.ReasonNamespaceOverlap, after.Requests[rid("pinned")].ReasonCode)
	assert.Equal(t, api.StateEstablishing, after.Requests[rid("camera-1")].State,
		"the standing selector keeps both of its flows")
	assert.Len(t, after.Requests[rid("camera-1")].Paths, 2)
}

// named builds a `name` domain selector from the `<area>/<elements>` spelling, splitting it the
// way a manifest does. Tests are allowed the convenience the rest of the tree is not (§10.6).
func named(domain string) api.DomainSelector {
	segments := strings.Split(domain, "/")
	return api.SelectDomain(api.Domain{Area: segments[0], Elements: segments[1:]})
}

// --- domain label selectors (§10.7) ------------------------------------------------------------

// label attaches labels to one (node, domain).
func (b *fleetBuilder) label(node, domain string, labels map[string]string) *fleetBuilder {
	segments := strings.Split(domain, "/")
	key := state.DomainKey{Node: node, Domain: domain}
	b.fleet.DomainLabels[key] = state.Entry[api.DomainLabels]{Found: true, Value: api.DomainLabels{
		Node:   node,
		Domain: api.Domain{Area: segments[0], Elements: segments[1:]},
		Labels: labels,
	}}
	return b
}

// selects returns a request whose source selects domains by label.
func labelRequest(name string, labels map[string]string) api.RequestSpec {
	spec := flowRequest(name)
	spec.Sources[0].Domain = api.SelectLabels(labels)
	spec.Sources[0].Select = api.Selector{GroupHint: &api.GroupHintSelector{Name: "Studio A:Camera 1"}}
	return spec
}

// **The test to write first, and the one whose omission is silent** (§17). A broad label selector
// on a node that is *also* a replication destination expands to a fixed path set and does not grow
// on subsequent reconciles, because a label selector never matches a flow this project is itself
// writing (§10.7).
//
// Every other label test passes with that filter missing.
func TestALabelSelectorNeverMatchesThisProjectsOwnOutput(t *testing.T) {
	t.Parallel()

	hint := &api.GroupHint{Name: "Studio A:Camera 1", Type: "video"}

	// One node, both a source and a destination: `media/cameras` holds a locally produced flow,
	// and `fast/ingest` holds one this node's own target worker is writing.
	fleet := newFleet().
		node("studio-a").
		node("edge-01").
		flow("studio-a", "media/cameras", api.FlowInventory{
			ID: "flow-local", Definition: flowDef, Producing: true, GroupHint: hint,
		}).
		flow("studio-a", "fast/ingest", api.FlowInventory{
			ID: "flow-replicated", Definition: flowDef, Producing: true, GroupHint: hint, Replicated: true,
		}).
		// A sibling in the *same* domain that this node did not write. It is selectable like any
		// other, which is the precision the per-flow rule buys over the directory-granular one it
		// replaces (§10.7).
		flow("studio-a", "fast/ingest", api.FlowInventory{
			ID: "flow-sibling", Definition: flowDef, Producing: true, GroupHint: hint,
		}).
		label("studio-a", "media/cameras", map[string]string{"role": "cameras"}).
		label("studio-a", "fast/ingest", map[string]string{"role": "cameras"}).
		request("wide", labelRequest("wide", map[string]string{"role": "cameras"})).
		build()

	result := Compute(fleet, Config{})

	flows := map[string]bool{}
	for _, path := range result.Paths {
		flows[path.Source.Flow] = true
	}
	assert.Equal(t, map[string]bool{"flow-local": true, "flow-sibling": true}, flows,
		"the replicated flow is skipped; the one beside it is not")

	// **And the skip is reported**, or the finer granularity is the less legible one: the domain is
	// present, the flow is in GET /v1/flows, it matches the labels, and it quietly does not appear
	// in the expansion (§9.1, §10.7).
	status := result.Requests[rid("wide")]
	require.Len(t, status.Excluded, 1)
	assert.Equal(t, api.Exclusion{
		Node: "studio-a", Domain: "fast/ingest", Flow: "flow-replicated",
		Reason: api.ExclusionSelfOutput,
	}, status.Excluded[0])
	assert.Zero(t, status.ExcludedDropped)

	// It does not grow on a second pass — the property this whole rule exists for. Compute is pure,
	// so running it again over the same snapshot is the honest form of "the next reconcile".
	assert.Len(t, Compute(fleet, Config{}).Paths, len(result.Paths))
}

// **Naming a domain directly reaches everything**, including a flow this node is writing (§10.7).
// Explicit chaining is intent; matched chaining is emergence, and that is the cut.
func TestNamingADomainDirectlyReachesItsReplicatedFlows(t *testing.T) {
	t.Parallel()

	spec := flowRequest("chain")
	spec.Sources[0].Domain = named("fast/ingest")
	spec.Sources[0].Select = api.Selector{Flow: "flow-replicated"}

	fleet := newFleet().
		node("studio-a").
		node("edge-01").
		flow("studio-a", "fast/ingest", api.FlowInventory{
			ID: "flow-replicated", Definition: flowDef, Producing: true, Replicated: true,
		}).
		request("chain", spec).
		build()

	result := Compute(fleet, Config{})
	require.Len(t, result.Paths, 1)
	assert.Equal(t, "flow-replicated", onlyPath(t, result).Source.Flow)
	assert.Empty(t, result.Requests[rid("chain")].Excluded, "nothing was skipped")
}

// **A label on a domain the node does not report is inert** (§10.7): it must expand to nothing
// rather than to a path that cannot resolve. That is the pending case §10.7's "before or after" is
// entirely about, and it resolves by itself when a producer appears.
func TestALabelOnAnUnobservedDomainExpandsToNothing(t *testing.T) {
	t.Parallel()

	build := func(observed bool) *state.Fleet {
		b := newFleet().node("studio-a").node("edge-01").
			label("studio-a", "media/cameras", map[string]string{"role": "cameras"}).
			request("wide", labelRequest("wide", map[string]string{"role": "cameras"}))
		if observed {
			b.flow("studio-a", "media/cameras", api.FlowInventory{
				ID: "flow-1", Definition: flowDef, Producing: true,
				GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "video"},
			})
		}
		return b.build()
	}

	pending := Compute(build(false), Config{})
	assert.Empty(t, pending.Paths)
	assert.Equal(t, api.StateWaiting, pending.Requests[rid("wide")].State,
		"WAITING, which §7.2 already files as legitimately not an error")
	assert.Empty(t, pending.Requests[rid("wide")].Excluded,
		`"there is no such domain" is not an exclusion; nothing matched`)

	assert.Len(t, Compute(build(true), Config{}).Paths, 1, "a producer appearing resolves it")
}

// Two keys ANDed: a domain carrying one of them does not match (§10.7).
func TestALabelSelectorAndsItsKeys(t *testing.T) {
	t.Parallel()

	hint := &api.GroupHint{Name: "Studio A:Camera 1", Type: "video"}
	fleet := newFleet().
		node("studio-a").
		node("edge-01").
		flow("studio-a", "media/both", api.FlowInventory{ID: "flow-both", Definition: flowDef, Producing: true, GroupHint: hint}).
		flow("studio-a", "media/one", api.FlowInventory{ID: "flow-one", Definition: flowDef, Producing: true, GroupHint: hint}).
		label("studio-a", "media/both", map[string]string{"role": "cameras", "site": "studio-a"}).
		label("studio-a", "media/one", map[string]string{"role": "cameras"}).
		request("wide", labelRequest("wide", map[string]string{"role": "cameras", "site": "studio-a"})).
		build()

	result := Compute(fleet, Config{})
	require.Len(t, result.Paths, 1)
	assert.Equal(t, "flow-both", onlyPath(t, result).Source.Flow)
}

// **A self-pair a selector produced is elided, not rejected** (§7.2, §10.7).
//
// A named source resolving to the destination is an operator having written the same string twice
// — a typo, decidable from the request, and refused as `same_endpoint`. A label selector matching
// the destination's own domain is not a typo: it is the selector doing what it was asked to, and
// refusing the request would put its author at the mercy of which domains happen to carry a label.
func TestASelectorMatchingItsOwnDestinationIsElided(t *testing.T) {
	t.Parallel()

	hint := &api.GroupHint{Name: "Studio A:Camera 1", Type: "video"}

	// Both domains are on the destination node, and both carry the label. One of them *is* the
	// destination.
	spec := labelRequest("wide", map[string]string{"role": "cameras"})
	spec.Sources[0].Node = "edge-01"
	spec.Destinations = []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}}

	fleet := newFleet().
		node("edge-01").
		node("edge-02").
		flow("edge-01", "fast/ingest", api.FlowInventory{ID: "flow-a", Definition: flowDef, Producing: true, GroupHint: hint}).
		flow("edge-01", "media/cameras", api.FlowInventory{ID: "flow-b", Definition: flowDef, Producing: true, GroupHint: hint}).
		label("edge-01", "fast/ingest", map[string]string{"role": "cameras"}).
		label("edge-01", "media/cameras", map[string]string{"role": "cameras"}).
		request("wide", spec).
		build()

	result := Compute(fleet, Config{})

	// The self-pairing is gone and the rest of the expansion stands.
	require.Len(t, result.Paths, 1)
	assert.Equal(t, "media/cameras", onlyPath(t, result).Source.Domain)
	assert.NotEqual(t, api.StateInvalid, result.Requests[rid("wide")].State,
		"eliding is not refusing: the request is perfectly good")

	// The named form of the same pairing is refused, which is the other half of the cut.
	named := flowRequest("typo")
	named.Sources[0].Node = "edge-01"
	named.Sources[0].Domain = api.SelectDomain(api.Domain{Area: "fast", Elements: []string{"ingest"}})
	named.Sources[0].Select = api.Selector{Flow: "flow-a"}
	named.Destinations = spec.Destinations

	refused := Compute(newFleet().node("edge-01").node("edge-02").request("typo", named).build(), Config{})
	assert.Equal(t, api.ReasonSameEndpoint, refused.Requests[rid("typo")].ReasonCode)
}

// The exclusion list is capped, and a truncated one **reports its own count** — a silent cap here
// reads as "nothing else was excluded" (§9.1).
func TestTheExclusionListReportsWhatItDropped(t *testing.T) {
	t.Parallel()

	hint := &api.GroupHint{Name: "Studio A:Camera 1", Type: "video"}
	b := newFleet().node("studio-a").node("edge-01").
		label("studio-a", "fast/ingest", map[string]string{"role": "cameras"}).
		request("wide", labelRequest("wide", map[string]string{"role": "cameras"}))

	for i := range api.MaxExclusions + 5 {
		b.flow("studio-a", "fast/ingest", api.FlowInventory{
			ID: fmt.Sprintf("flow-%03d", i), Definition: flowDef,
			Producing: true, GroupHint: hint, Replicated: true,
		})
	}

	status := Compute(b.build(), Config{}).Requests[rid("wide")]
	assert.Len(t, status.Excluded, api.MaxExclusions)
	assert.Equal(t, 5, status.ExcludedDropped)
}

// A relabel moves a request's expansion, and it does so through the **ordinary reconcile**: label
// records are desired state under `/desired/`, so they are inside the single List("") the snapshot
// already takes and there is no new signalling (§8, §10.7).
//
// And a path the relabel still matches keeps its identity, which is the property the
// annotate-don't-rename decision exists for (§5.4, §17).
func TestARelabelMovesTheExpansionWithoutMovingIdentity(t *testing.T) {
	t.Parallel()

	hint := &api.GroupHint{Name: "Studio A:Camera 1", Type: "video"}
	build := func(labels map[string]string) *state.Fleet {
		return newFleet().node("studio-a").node("edge-01").
			flow("studio-a", "media/cameras", api.FlowInventory{ID: "flow-1", Definition: flowDef, Producing: true, GroupHint: hint}).
			flow("studio-a", "media/audio", api.FlowInventory{ID: "flow-2", Definition: flowDef, Producing: true, GroupHint: hint}).
			label("studio-a", "media/cameras", map[string]string{"role": "cameras"}).
			label("studio-a", "media/audio", labels).
			request("wide", labelRequest("wide", map[string]string{"role": "cameras"})).
			build()
	}

	before := Compute(build(map[string]string{"role": "audio"}), Config{})
	require.Len(t, before.Paths, 1)

	after := Compute(build(map[string]string{"role": "cameras"}), Config{})
	require.Len(t, after.Paths, 2, "the relabel joined the second domain to the expansion")

	// The path that matched before still has the same ID, so its session — and its worker — is
	// untouched. A label is not in path identity (§5.4).
	for id := range before.Paths {
		assert.Contains(t, after.Paths, id, "an existing path must not be re-identified by a relabel")
	}
}
