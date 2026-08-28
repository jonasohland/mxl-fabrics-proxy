package validate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/negotiate"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
)

func node(name string, domains []api.DomainMapping, opts ...func(*state.NodeRecord)) state.Entry[state.NodeRecord] {
	record := state.NodeRecord{
		Node:    name,
		Domains: domains,
		Capabilities: api.Capabilities{
			Fabrics: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.1.1.1",
				CapFlags: []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive},
			}},
			// One output root, which is what makes a node a replication destination at all and
			// lets a request name a destination without spelling the root (§10.6).
			OutputRoots: []api.OutputRoot{{Name: "fast", Path: "/dev/shm/mxl"}},
		},
	}
	for _, opt := range opts {
		opt(&record)
	}
	return state.Entry[state.NodeRecord]{Found: true, Value: record}
}

func fleet() *state.Fleet {
	return &state.Fleet{
		Nodes: map[string]state.Entry[state.NodeRecord]{
			"studio-a": node("studio-a", []api.DomainMapping{{Name: "cameras", Configured: true}}),
			"edge-01": node("edge-01", []api.DomainMapping{
				{Name: "archive", Configured: true},
				{Name: "found", Configured: false},
			}),
		},
	}
}

func spec() api.RequestSpec {
	return api.RequestSpec{
		Name:         "studio-a-cam1-to-edge",
		Source:       api.Source{Node: "studio-a", Domain: "cameras", Select: api.Selector{Flow: "flow-1"}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: []string{"ingest"}}},
	}
}

func TestSpecAccepts(t *testing.T) {
	t.Parallel()

	s := spec()
	result, root, bad := Destination(s, s.Destinations[0], fleet(), negotiate.Config{})
	require.Nil(t, bad)
	assert.Equal(t, api.ProviderTCP, result.Provider())
	assert.Equal(t, "dc1", result.Fabric)

	// A request naming no root resolves to the node's only one, and the *resolved* name is what
	// comes back — it is what the path identity and the assignment carry (§10.6).
	assert.Equal(t, "fast", root)
}

func TestSpecRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*api.RequestSpec)
		prepare func(*state.Fleet)
		want    api.ReasonCode
		message string
	}{
		{
			name: "same node and domain",
			mutate: func(s *api.RequestSpec) {
				s.Destinations[0] = api.Destination{Node: "studio-a", Domain: []string{"cameras"}}
			},
			want: api.ReasonSameEndpoint,
		},
		{
			// Same node, different domain is legitimate — it is exactly what the legacy loopback
			// configuration does, and it is how shm ever gets used. The destination is an output
			// domain under that node's own root; the source stays an input mapping (§10.6).
			name: "same node different domain is fine",
			mutate: func(s *api.RequestSpec) {
				s.Destinations[0] = api.Destination{Node: "studio-a", Domain: []string{"local"}}
			},
		},
		{
			name:   "unknown source node",
			mutate: func(s *api.RequestSpec) { s.Source.Node = "typo" },
			want:   api.ReasonNodeNotRegistered,
		},
		{
			name:   "unknown destination node",
			mutate: func(s *api.RequestSpec) { s.Destinations[0].Node = "typo" },
			want:   api.ReasonNodeNotRegistered,
		},
		{
			// Normally refused at POST by api.RequestSpec.Validate; reachable here for a request
			// written straight into the store, which must still fail rather than reach an agent.
			name:   "destination domain is a path",
			mutate: func(s *api.RequestSpec) { s.Destinations[0].Domain = []string{"/dev/shm/anything"} },
			want:   api.ReasonMalformedDomainName,
		},
		{
			// Names are flat per node: one name cannot mean two directories. A *discovered*
			// domain needs no case of its own — it is named by its path, and a path is refused
			// above (§10.6).
			name:   "destination collides with an input mapping",
			mutate: func(s *api.RequestSpec) { s.Destinations[0].Domain = []string{"archive"} },
			want:   api.ReasonDomainNameInUse,
		},
		{
			name: "destination node advertises no output root",
			prepare: func(f *state.Fleet) {
				entry := f.Nodes["edge-01"]
				entry.Value.Capabilities.OutputRoots = nil
				f.Nodes["edge-01"] = entry
			},
			want:    api.ReasonNoOutputRoot,
			message: "advertises no output root",
		},
		{
			name:    "unknown output root",
			mutate:  func(s *api.RequestSpec) { s.Destinations[0].Root = "bulk" },
			want:    api.ReasonUnknownOutputRoot,
			message: `"fast"`,
		},
		{
			// Never a guess. The friendly single-root case is the common one, and this error
			// carries its own fix (§10.6).
			name: "two roots and none named",
			prepare: func(f *state.Fleet) {
				entry := f.Nodes["edge-01"]
				entry.Value.Capabilities.OutputRoots = append(entry.Value.Capabilities.OutputRoots,
					api.OutputRoot{Name: "bulk", Path: "/mnt/nvme/mxl"})
				f.Nodes["edge-01"] = entry
			},
			want:    api.ReasonAmbiguousOutputRoot,
			message: `"bulk", "fast"`,
		},
		{
			name:   "two roots and one named",
			mutate: func(s *api.RequestSpec) { s.Destinations[0].Root = "bulk" },
			prepare: func(f *state.Fleet) {
				entry := f.Nodes["edge-01"]
				entry.Value.Capabilities.OutputRoots = append(entry.Value.Capabilities.OutputRoots,
					api.OutputRoot{Name: "bulk", Path: "/mnt/nvme/mxl"})
				f.Nodes["edge-01"] = entry
			},
		},
		{
			name:   "sched_prio on a node without the capability",
			mutate: func(s *api.RequestSpec) { s.SchedPrio = ptr(50) },
			want:   api.ReasonSchedPrioUnavailable,
		},
		{
			name:   "sched_prio where both nodes can",
			mutate: func(s *api.RequestSpec) { s.SchedPrio = ptr(50) },
			prepare: func(f *state.Fleet) {
				for name, entry := range f.Nodes {
					entry.Value.Capabilities.SchedPrio = true
					f.Nodes[name] = entry
				}
			},
		},
		{
			name: "no shared fabric",
			prepare: func(f *state.Fleet) {
				f.Nodes["edge-01"] = node("edge-01", []api.DomainMapping{{Name: "archive", Configured: true}},
					func(r *state.NodeRecord) {
						r.Capabilities.Fabrics[0].Fabric = "dc2"
					})
			},
			want: api.ReasonNoSharedFabric,
		},
		{
			name:   "pin not viable",
			mutate: func(s *api.RequestSpec) { s.Provider = api.ProviderPin{api.ProviderEFA} },
			want:   api.ReasonPinNotViable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := fleet()
			if tc.prepare != nil {
				tc.prepare(f)
			}
			s := spec()
			if tc.mutate != nil {
				tc.mutate(&s)
			}

			_, _, bad := Destination(s, s.Destinations[0], f, negotiate.Config{})
			if tc.want == "" {
				assert.Nil(t, bad)
				return
			}

			require.NotNil(t, bad)
			assert.Equal(t, tc.want, bad.Code)
			assert.NotEmpty(t, bad.Message)
			if tc.message != "" {
				assert.Contains(t, bad.Message, tc.message)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func ref(id string, srcNode, srcDomain, flow, dstNode, dstDomain string, since time.Time) PathRef {
	return PathRef{
		ID: id,
		Path: state.PathIdentity{
			Source:      api.FlowAddress{Node: srcNode, Domain: srcDomain, Flow: flow},
			Destination: api.Destination{Node: dstNode, Domain: []string{dstDomain}},
		},
		Since: since,
	}
}

// rooted is ref with an explicit output root, for the flat-namespace cases.
func rooted(id string, srcNode, srcDomain, flow, dstNode, dstDomain, root string, since time.Time) PathRef {
	r := ref(id, srcNode, srcDomain, flow, dstNode, dstDomain, since)
	r.Path.Destination.Root = root
	return r
}

// One domain name cannot mean two directories on one node (§10.6). Checked across paths rather
// than per request because the colliding pair need share no source, no flow and no request —
// which is exactly the case here.
func TestConflictsRejectsOneNameUnderTwoRoots(t *testing.T) {
	t.Parallel()

	first := time.Unix(1000, 0)
	out := Conflicts([]PathRef{
		rooted("p2", "studio-b", "cameras", "flow-2", "edge-01", "ingest", "bulk", first.Add(time.Hour)),
		rooted("p1", "studio-a", "cameras", "flow-1", "edge-01", "ingest", "fast", first),
	})

	// Oldest-first, like the other conflicts: a new request never invalidates a path that is
	// probably already carrying media.
	require.Contains(t, out, "p2")
	assert.Equal(t, api.ReasonDomainNameInUse, out["p2"].Code)
	assert.Contains(t, out["p2"].Message, `"fast"`)
	assert.NotContains(t, out, "p1")
}

// Two sources into one output domain is ordinary and must stay so: it is how a destination
// collects several flows, and it is what the refcount on a materialised domain is for.
func TestConflictsAcceptsTwoFlowsIntoOneOutputDomain(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	out := Conflicts([]PathRef{
		rooted("p1", "studio-a", "cameras", "flow-1", "edge-01", "ingest", "fast", now),
		rooted("p2", "studio-b", "cameras", "flow-2", "edge-01", "ingest", "fast", now.Add(time.Hour)),
	})
	assert.Empty(t, out)
}

func TestConflictsAcceptsIndependentPaths(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	out := Conflicts([]PathRef{
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "ingest", now),
		ref("p2", "studio-a", "cameras", "flow-2", "edge-01", "ingest", now),
		ref("p3", "studio-a", "cameras", "flow-1", "edge-02", "ingest", now),
	})
	assert.Empty(t, out)
}

// Two producers into one flow ID corrupts the ring buffer, and nothing downstream notices:
// both sessions look healthy and the media is garbage.
func TestConflictsRejectsTwoSourcesIntoOneDestinationFlow(t *testing.T) {
	t.Parallel()

	first := time.Unix(1000, 0)
	out := Conflicts([]PathRef{
		ref("p2", "studio-b", "cameras", "flow-1", "edge-01", "ingest", first.Add(time.Hour)),
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "ingest", first),
	})

	// The older path wins: it is the one probably already carrying media, so the newcomer is
	// what fails.
	require.Contains(t, out, "p2")
	assert.Equal(t, api.ReasonFlowConflict, out["p2"].Code)
	assert.NotContains(t, out, "p1")
	assert.Contains(t, out["p2"].Message, "studio-a/cameras")
}

// The same edge appearing twice is deduplication, not a conflict: N requests naming one edge
// share one path and one session.
func TestConflictsIgnoresADuplicatedEdge(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	out := Conflicts([]PathRef{
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "ingest", now),
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "ingest", now),
	})
	assert.Empty(t, out)
}

// A→B→C is fine and useful. A→B plus B→A for one flow feeds a flow back into itself, and so
// does the same mistake spelled longer (§7.2).
func TestConflictsRejectsCyclesButNotChains(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)

	chain := Conflicts([]PathRef{
		ref("p1", "a", "d", "flow-1", "b", "d", now),
		ref("p2", "b", "d", "flow-1", "c", "d", now.Add(time.Second)),
	})
	assert.Empty(t, chain)

	loop := Conflicts([]PathRef{
		ref("p1", "a", "d", "flow-1", "b", "d", now),
		ref("p2", "b", "d", "flow-1", "a", "d", now.Add(time.Second)),
	})
	require.Contains(t, loop, "p2")
	assert.Equal(t, api.ReasonLoop, loop["p2"].Code)
	assert.NotContains(t, loop, "p1")

	longer := Conflicts([]PathRef{
		ref("p1", "a", "d", "flow-1", "b", "d", now),
		ref("p2", "b", "d", "flow-1", "c", "d", now.Add(time.Second)),
		ref("p3", "c", "d", "flow-1", "a", "d", now.Add(2*time.Second)),
	})
	require.Contains(t, longer, "p3")
	assert.Equal(t, api.ReasonLoop, longer["p3"].Code)

	// A cycle in *another* flow's graph is not this flow's problem: the graphs are per flow ID.
	unrelated := Conflicts([]PathRef{
		ref("p1", "a", "d", "flow-1", "b", "d", now),
		ref("p2", "b", "d", "flow-2", "a", "d", now.Add(time.Second)),
	})
	assert.Empty(t, unrelated)
}

// Every replica must reach the same verdict from the same paths, whatever order they were
// listed in, or one replica's INVALID is another's ACTIVE.
func TestConflictsAreOrderIndependent(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	paths := []PathRef{
		ref("p3", "studio-c", "cameras", "flow-1", "edge-01", "ingest", now.Add(2*time.Second)),
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "ingest", now),
		ref("p2", "studio-b", "cameras", "flow-1", "edge-01", "ingest", now.Add(time.Second)),
	}

	want := Conflicts(paths)
	require.Len(t, want, 2)

	for _, order := range [][]PathRef{
		{paths[1], paths[2], paths[0]},
		{paths[2], paths[0], paths[1]},
		{paths[0], paths[2], paths[1]},
	} {
		assert.Equal(t, want, Conflicts(order))
	}

	// Simultaneous creation is broken by ID, so even a tie is deterministic.
	tied := Conflicts([]PathRef{
		ref("zzz", "studio-b", "cameras", "flow-9", "edge-01", "ingest", now),
		ref("aaa", "studio-a", "cameras", "flow-9", "edge-01", "ingest", now),
	})
	require.Contains(t, tied, "zzz")
	assert.NotContains(t, tied, "aaa")
}

// An output root is permitted to be an ancestor of an input mapping (§10.6), and this is the case
// that permission leaves open: the *name* is free but the resolved directory is one the node
// already reads from. The agent refuses it independently and is the authority; this is what turns
// it into a rejection at POST with a reason naming what to change.
func TestADestinationMayNotLandOnAnInputDomainsDirectory(t *testing.T) {
	t.Parallel()

	// The mapping's name deliberately differs from its directory's basename, so the name check
	// cannot see the collision and only the path check can.
	f := fleet()
	f.Nodes["edge-01"] = node("edge-01", []api.DomainMapping{
		{Name: "cams", Path: "/dev/shm/mxl/cameras", Configured: true},
	})

	s := spec()
	s.Destinations[0].Domain = []string{"cameras"}

	_, root, bad := Destination(s, s.Destinations[0], f, negotiate.Config{})
	require.NotNil(t, bad)
	assert.Equal(t, api.ReasonDomainPathInUse, bad.Code)
	assert.Contains(t, bad.Message, "/dev/shm/mxl/cameras")
	assert.Contains(t, bad.Message, `input domain "cams"`)

	// The resolved root still comes back, so a shadow path keeps the identity the real path would
	// have had and a session already running on it stays reported (§10.6).
	assert.Equal(t, "fast", root)

	// A sibling under the same root is unaffected.
	s.Destinations[0].Domain = []string{"ingest"}
	_, _, bad = Destination(s, s.Destinations[0], f, negotiate.Config{})
	assert.Nil(t, bad)
}

// A node that withholds its root's path — advertised for diagnostics only (§10.2) — cannot be
// checked here, and must not be refused on that account. The agent's own check still holds.
func TestAWithheldRootPathSkipsTheCheckRatherThanFailing(t *testing.T) {
	t.Parallel()

	f := fleet()
	f.Nodes["edge-01"] = node("edge-01", nil, func(r *state.NodeRecord) {
		r.Capabilities.OutputRoots = []api.OutputRoot{{Name: "fast"}}
		r.Domains = []api.DomainMapping{{Name: "cams", Path: "/dev/shm/mxl/cameras", Configured: true}}
	})

	s := spec()
	s.Destinations[0].Domain = []string{"cameras"}

	_, _, bad := Destination(s, s.Destinations[0], f, negotiate.Config{})
	assert.Nil(t, bad)
}

// **No output domain inside another output domain.** `studio-a` and `studio-a/cam1` would make one
// domain directory a container for another — a shape nothing else in the design has, and one where
// removing either is a question with no answer.
//
// The exact slice-prefix test is what the element form is worth here: the string spelling has to
// work around `studio-ab` looking like a child of `studio-a`.
func TestOutputDomainsMayNotNest(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	nested := func(id string, domain []string, since time.Time) PathRef {
		return PathRef{
			ID: id,
			Path: state.PathIdentity{
				Source:      api.FlowAddress{Node: "studio-a", Domain: "cameras", Flow: "flow-" + id},
				Destination: api.Destination{Node: "edge-01", Domain: domain, Root: "fast"},
			},
			Since: since,
		}
	}

	out := Conflicts([]PathRef{
		nested("a", []string{"studio-a"}, at),
		nested("b", []string{"studio-a", "cam1"}, at.Add(time.Minute)),
	})
	require.Contains(t, out, "b", "the newer path loses, as every conflict resolves oldest-first")
	assert.NotContains(t, out, "a")
	assert.Equal(t, api.ReasonDomainNameInUse, out["b"].Code)
	assert.Contains(t, out["b"].Message, "nests with")

	// Both directions: a parent arriving after a child is the same conflict.
	out = Conflicts([]PathRef{
		nested("a", []string{"studio-a", "cam1"}, at),
		nested("b", []string{"studio-a"}, at.Add(time.Minute)),
	})
	assert.Contains(t, out, "b")

	// A sibling whose name merely *begins* with another's is not nesting. This is the case the
	// string spelling of the check gets wrong.
	out = Conflicts([]PathRef{
		nested("a", []string{"studio-a"}, at),
		nested("b", []string{"studio-ab"}, at.Add(time.Minute)),
	})
	assert.Empty(t, out)

	// And ordinary hierarchy under one parent is fine.
	out = Conflicts([]PathRef{
		nested("a", []string{"studio-a", "cam1"}, at),
		nested("b", []string{"studio-a", "cam2"}, at.Add(time.Minute)),
	})
	assert.Empty(t, out)
}
