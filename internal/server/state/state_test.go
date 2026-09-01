package state

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/store"
	"github.com/jonasohland/mxl-replicator/internal/store/sqlite"
)

func testStore(t *testing.T) store.Store {
	t.Helper()

	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "store.db"), sqlite.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, s.Close()) })
	return s
}

func put(t *testing.T, s store.Store, key string, value any, opts ...store.PutOpt) {
	t.Helper()

	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	_, err = s.Put(t.Context(), key, encoded, opts...)
	require.NoError(t, err)
}

func TestLoadRoutesEveryLayer(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	ctx := context.Background()

	lease, err := s.GrantLease(ctx, time.Minute)
	require.NoError(t, err)

	put(t, s, store.NodeKey("edge-01"), NodeRecord{Node: "edge-01"})
	// A request always carries a valid selector — an invalid one refuses to marshal, which is
	// the tagged union defending itself on the way *out* as well as in (§9.1).
	put(t, s, store.NamespaceKey("nab"), NamespaceRecord{
		Name: "nab", Spec: api.Namespace{Name: "nab", Paths: api.PathsExclusive},
	})
	put(t, s, store.RequestKey("nab", "cam1"), RequestRecord{
		ID: api.RequestID{Namespace: "nab", Name: "cam1"},
		Spec: api.RequestSpec{
			Namespace:    "nab",
			Name:         "cam1",
			Sources:      []api.Source{{Node: "studio-a", Domain: named("media/cameras"), Select: api.Selector{Flow: "flow-1"}}},
			Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
		},
	})
	put(t, s, store.LeaseKey("edge-01"), LeaseRecord{Node: "edge-01", Instance: "i-1"}, store.WithLease(lease))
	put(t, s, store.InventoryKey("edge-01"), api.InventorySnapshot{Node: "edge-01"}, store.WithLease(lease))
	put(t, s, store.StatusKey("edge-01"), api.StatusSnapshot{Node: "edge-01"}, store.WithLease(lease))
	put(t, s, store.SessionKey("s-1"), SessionRecord{ID: "s-1"})
	put(t, s, store.AssignmentsKey("edge-01"), api.AssignmentSet{Node: "edge-01"})
	put(t, s, store.KeyReconciler, ReconcilerRecord{Leader: "replica-a", Settled: true})

	// Election keys share the key space and must be ignored rather than reported as damage.
	_, err = s.Put(ctx, store.PrefixElection+"leader", []byte("not json"))
	require.NoError(t, err)

	fleet, err := Load(ctx, s)
	require.NoError(t, err)

	assert.Empty(t, fleet.Malformed)
	assert.Positive(t, fleet.Revision)

	require.Contains(t, fleet.Nodes, "edge-01")
	require.Contains(t, fleet.Namespaces, "nab")
	require.Contains(t, fleet.Requests, api.RequestID{Namespace: "nab", Name: "cam1"})
	require.Contains(t, fleet.Leases, "edge-01")
	require.Contains(t, fleet.Inventory, "edge-01")
	require.Contains(t, fleet.Status, "edge-01")
	require.Contains(t, fleet.Sessions, "s-1")
	require.Contains(t, fleet.Assignments, "edge-01")
	assert.True(t, fleet.Reconciler.Found)
	assert.True(t, fleet.Reconciler.Value.Settled)

	// The lease travels with the entry: the reconciler has to pass it back on every rewrite of
	// observed state, or the key outlives the agent that reported it.
	assert.Equal(t, lease, fleet.Inventory["edge-01"].Lease)
	assert.Equal(t, store.LeaseID(0), fleet.Nodes["edge-01"].Lease)

	assert.True(t, fleet.Live("edge-01"))
	assert.False(t, fleet.Live("edge-02"))

	// A namespace with no record answers with defaults rather than refusing: auto-create is a
	// real write, so the window between a request landing and its namespace record existing is a
	// real one and a reader must not report the request as broken during it (§9.3).
	assert.True(t, fleet.Namespace("nab").Paths.Exclusive())
	assert.False(t, fleet.Namespace("never-declared").Paths.Exclusive())
	assert.Equal(t, api.PathsShared, fleet.Namespace("never-declared").Paths)

	assert.Equal(t, map[string]int{"nab": 1}, fleet.NamespacesInUse())
}

// One unreadable key must not wedge the reconciler for the whole fleet.
func TestLoadCollectsMalformedKeys(t *testing.T) {
	t.Parallel()

	s := testStore(t)

	_, err := s.Put(t.Context(), store.NodeKey("broken"), []byte("{not json"))
	require.NoError(t, err)
	put(t, s, store.NodeKey("fine"), NodeRecord{Node: "fine"})

	// A record carrying no name has nowhere to go in the snapshot: the map key comes from the
	// record, not from the store key.
	put(t, s, store.NodeKey("anonymous"), NodeRecord{})

	fleet, err := Load(t.Context(), s)
	require.NoError(t, err)

	assert.Len(t, fleet.Malformed, 2)
	assert.Contains(t, fleet.Nodes, "fine")
	assert.Len(t, fleet.Nodes, 1)
}

// Every map in a snapshot is as of one revision. Three lists would give three, and a reconcile
// computed across a skewed snapshot can conclude that a session both should and should not
// exist.
func TestLoadIsOneConsistentRead(t *testing.T) {
	t.Parallel()

	s := testStore(t)

	put(t, s, store.NodeKey("edge-01"), NodeRecord{Node: "edge-01"})
	fleet, err := Load(t.Context(), s)
	require.NoError(t, err)

	put(t, s, store.NodeKey("edge-02"), NodeRecord{Node: "edge-02"})
	later, err := Load(t.Context(), s)
	require.NoError(t, err)

	assert.Greater(t, later.Revision, fleet.Revision)
	assert.Len(t, fleet.Nodes, 1, "the earlier snapshot must not see the later write")
	assert.Len(t, later.Nodes, 2)
}

func TestFleetLookups(t *testing.T) {
	t.Parallel()

	fleet := &Fleet{
		Inventory: map[string]Entry[api.InventorySnapshot]{
			"studio-a": {Found: true, Value: api.InventorySnapshot{
				Node: "studio-a",
				Domains: []api.DomainInventory{{
					Domain: api.Domain{Area: "media", Elements: []string{"cameras"}},
					Flows:  []api.FlowInventory{{ID: "flow-1", Producing: true}},
				}},
			}},
		},
		Status: map[string]Entry[api.StatusSnapshot]{
			"edge-01": {Found: true, Value: api.StatusSnapshot{
				Node: "edge-01",
				Sessions: []api.SessionStatus{
					{SessionID: "s-1", Role: api.RoleTarget, State: api.WorkerReady, Epoch: "e-1"},
					{SessionID: "s-1", Role: api.RoleInitiator, State: api.WorkerStarting},
				},
			}},
		},
	}

	flow, ok := fleet.Flow("studio-a", "media/cameras", "flow-1")
	require.True(t, ok)
	assert.True(t, flow.Producing)

	_, ok = fleet.Flow("studio-a", "media/cameras", "flow-2")
	assert.False(t, ok)

	// The structured identity comes back from the agent's own report, never from splitting the
	// rendered string: that is what keeps "a domain string is parsed at exactly one boundary"
	// true (§10.6).
	domain, ok := fleet.Domain("studio-a", "media/cameras")
	require.True(t, ok)
	assert.Equal(t, api.Domain{Area: "media", Elements: []string{"cameras"}}, domain)
	_, ok = fleet.Domain("studio-a", "media/other")
	assert.False(t, ok)

	// A node reporting no inventory is "no observation", never "the flow is gone" — the caller
	// must be able to tell those apart, so an absent node reports false rather than empty.
	_, ok = fleet.Flow("edge-99", "media/cameras", "flow-1")
	assert.False(t, ok)
	assert.Empty(t, fleet.Flows("edge-99", "media/cameras"))

	// The same session has one status per role, and asking for the wrong role must not return
	// the other end's epoch.
	target, ok := fleet.SessionStatus("edge-01", "s-1", api.RoleTarget)
	require.True(t, ok)
	assert.Equal(t, "e-1", target.Epoch)

	initiator, ok := fleet.SessionStatus("edge-01", "s-1", api.RoleInitiator)
	require.True(t, ok)
	assert.Empty(t, initiator.Epoch)

	_, ok = fleet.SessionStatus("edge-01", "s-2", api.RoleTarget)
	assert.False(t, ok)
}

func identity() PathIdentity {
	return PathIdentity{
		Source:      api.FlowAddress{Node: "studio-a", Domain: "cameras", Flow: "flow-1"},
		Destination: api.Destination{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}},
	}
}

// A recomputed ID that differed would orphan the workers realising it, which is exactly what
// the settling window exists to prevent — so determinism here is load-bearing, not tidiness
// (§5.4, §7.3).
func TestIdentityIsDeterministic(t *testing.T) {
	t.Parallel()

	path := identity()
	assert.Equal(t, path.ID(), identity().ID())
	assert.Len(t, path.ID(), idLength)

	def := json.RawMessage(`{"id":"flow-1","format":"video"}`)
	session := SessionID(path, FlowDefHash(def))
	assert.Equal(t, session, SessionID(identity(), FlowDefHash(def)))
	assert.NotEqual(t, path.ID(), session, "a path ID and a session ID must never collide")
}

// Every component of the identity has to move the ID, or two different edges share a session
// and one destination's workers are configured from the other's flow.
func TestIdentityFieldsAllMatter(t *testing.T) {
	t.Parallel()

	base := identity()

	for _, tc := range []struct {
		name  string
		apply func(*PathIdentity)
	}{
		{"source node", func(p *PathIdentity) { p.Source.Node = "studio-b" }},
		{"source domain", func(p *PathIdentity) { p.Source.Domain = "other" }},
		{"flow", func(p *PathIdentity) { p.Source.Flow = "flow-2" }},
		{"destination node", func(p *PathIdentity) { p.Destination.Node = "edge-02" }},
		{"destination domain", func(p *PathIdentity) { p.Destination.Domain.Elements = []string{"other"} }},
		// **The area is a term of the identity**, because it is the first segment of the domain's
		// name (§5.4, §10.6). Moving a domain to another area is an operator choosing a different
		// destination, which is a different path by any reading; repointing an area's *directory*
		// while keeping its name changes nothing here, which is the property that buy pays for.
		{"destination area", func(p *PathIdentity) { p.Destination.Domain.Area = "bulk" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.apply(&changed)
			assert.NotEqual(t, base.ID(), changed.ID())
			assert.NotEqual(t, SessionID(base, "h"), SessionID(changed, "h"))
		})
	}

	// Length-prefixed fields: moving a boundary between two adjacent components must not
	// produce the same digest.
	shifted := base
	shifted.Source.Domain = "cameras" + "flow-1"
	shifted.Source.Flow = ""
	assert.NotEqual(t, base.ID(), shifted.ID())
}

// A flow deleted and recreated with a different definition makes the destination's local flow
// wrong in a way no reconnect fixes. The session must be rebuilt — which happens for free,
// because its ID changes — while the *path* stays the same path (§5.4).
func TestFlowDefinitionChangeRebuildsTheSessionButNotThePath(t *testing.T) {
	t.Parallel()

	path := identity()
	before := FlowDefHash(json.RawMessage(`{"id":"flow-1","grainRate":{"numerator":25}}`))
	after := FlowDefHash(json.RawMessage(`{"id":"flow-1","grainRate":{"numerator":50}}`))

	assert.NotEqual(t, before, after)
	assert.NotEqual(t, SessionID(path, before), SessionID(path, after))
	assert.Equal(t, path.ID(), identity().ID())
}

// Definitions travel as verbatim bytes and the API compacts insignificant whitespace on the
// way through (§2a). The hash has to be blind to exactly that and to nothing else, or a
// definition that took one more hop through the API would rebuild a healthy session.
func TestFlowDefHashIgnoresOnlyWhitespace(t *testing.T) {
	t.Parallel()

	pretty := json.RawMessage("{\n  \"id\": \"flow-1\",\n  \"format\": \"video\"\n}")
	compacted := json.RawMessage(`{"id":"flow-1","format":"video"}`)
	assert.Equal(t, FlowDefHash(pretty), FlowDefHash(compacted))

	// Key order is *not* normalised: the bytes are what the destination worker creates its flow
	// from, and canonicalising here would put this hash at odds with them.
	reordered := json.RawMessage(`{"format":"video","id":"flow-1"}`)
	assert.NotEqual(t, FlowDefHash(compacted), FlowDefHash(reordered))

	// A definition that is not valid JSON still hashes rather than panicking; the worker will
	// report the real problem far more clearly than this function could.
	assert.NotEmpty(t, FlowDefHash(json.RawMessage(`{"id":`)))
}

func TestPutJSONSkipsUnchangedWrites(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	ctx := context.Background()
	key := store.NodeKey("edge-01")
	record := NodeRecord{Node: "edge-01", RegisteredAt: time.Unix(1000, 0).UTC()}

	rev, wrote, err := PutJSON(ctx, s, key, record, Prior{}, WriteOptions{})
	require.NoError(t, err)
	assert.True(t, wrote)

	fleet, err := Load(ctx, s)
	require.NoError(t, err)

	// The identical value again: no write, no revision, and — the reason this matters — no
	// wakeup for anything watching, which downstream is a worker restart on another node.
	sameRev, wrote, err := PutJSON(ctx, s, key, record, fleet.Nodes["edge-01"].Prior(), WriteOptions{})
	require.NoError(t, err)
	assert.False(t, wrote)
	assert.Equal(t, rev, sameRev)

	after, err := Load(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, fleet.Revision, after.Revision)

	record.Capabilities.SchedPrio = true
	_, wrote, err = PutJSON(ctx, s, key, record, after.Nodes["edge-01"].Prior(), WriteOptions{})
	require.NoError(t, err)
	assert.True(t, wrote)
}

// Forgetting the lease on a rewrite of observed state detaches it, turning leased state into
// state that outlives its agent. The skip has to notice a lease change even when the bytes are
// identical, or the first unchanged report after a re-registration would strand the key.
func TestPutJSONRewritesWhenOnlyTheLeaseChanged(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	ctx := context.Background()

	first, err := s.GrantLease(ctx, time.Minute)
	require.NoError(t, err)
	second, err := s.GrantLease(ctx, time.Minute)
	require.NoError(t, err)

	snapshot := api.InventorySnapshot{Node: "edge-01", Instance: "i-1"}
	key := store.InventoryKey("edge-01")

	_, wrote, err := PutJSON(ctx, s, key, snapshot, Prior{}, WriteOptions{Lease: first})
	require.NoError(t, err)
	assert.True(t, wrote)

	fleet, err := Load(ctx, s)
	require.NoError(t, err)

	_, wrote, err = PutJSON(ctx, s, key, snapshot, fleet.Inventory["edge-01"].Prior(), WriteOptions{Lease: second})
	require.NoError(t, err)
	assert.True(t, wrote)

	reloaded, err := Load(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, second, reloaded.Inventory["edge-01"].Lease)
}

// The CAS is the guard against a demoted leader: one that was partitioned and has not noticed
// is computing from a stale read, and its write must lose rather than fight (§4.6).
func TestPutJSONCASFailsAgainstAStaleRead(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	ctx := context.Background()
	key := store.AssignmentsKey("edge-01")

	_, _, err := PutJSON(ctx, s, key, api.AssignmentSet{Node: "edge-01"}, Prior{}, WriteOptions{CAS: true})
	require.NoError(t, err)

	stale, err := Load(ctx, s)
	require.NoError(t, err)

	// Somebody else writes in between.
	_, _, err = PutJSON(ctx, s, key,
		api.AssignmentSet{Node: "edge-01", Assignments: []api.Assignment{{SessionID: "s-1"}}},
		stale.Assignments["edge-01"].Prior(), WriteOptions{CAS: true})
	require.NoError(t, err)

	_, _, err = PutJSON(ctx, s, key,
		api.AssignmentSet{Node: "edge-01", Assignments: []api.Assignment{{SessionID: "s-2"}}},
		stale.Assignments["edge-01"].Prior(), WriteOptions{CAS: true})
	require.ErrorIs(t, err, store.ErrCompareFailed)

	// A create racing another create loses the same way, which is what makes an absent prior
	// with CAS an IfAbsent rather than an unconditional write.
	_, _, err = PutJSON(ctx, s, store.AssignmentsKey("edge-02"), api.AssignmentSet{Node: "edge-02"}, Prior{}, WriteOptions{CAS: true})
	require.NoError(t, err)
	_, _, err = PutJSON(ctx, s, store.AssignmentsKey("edge-02"), api.AssignmentSet{Node: "edge-02", Revision: 7}, Prior{}, WriteOptions{CAS: true})
	require.ErrorIs(t, err, store.ErrCompareFailed)
}

// named builds a `name` domain selector from the `<area>/<elements>` spelling, splitting it the
// way a manifest does. Tests are allowed the convenience the rest of the tree is not (§10.6).
func named(domain string) api.DomainSelector {
	segments := strings.Split(domain, "/")
	return api.SelectDomain(api.Domain{Area: segments[0], Elements: segments[1:]})
}
