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
				{Name: "ingest", Configured: true},
				{Name: "found", Configured: false},
			}),
		},
	}
}

func spec() api.RequestSpec {
	return api.RequestSpec{
		Name:        "studio-a-cam1-to-edge",
		Source:      api.Source{Node: "studio-a", Domain: "cameras", Select: api.Selector{Flow: "flow-1"}},
		Destination: api.Destination{Node: "edge-01", Domain: "ingest"},
	}
}

func TestSpecAccepts(t *testing.T) {
	t.Parallel()

	result, bad := Spec(spec(), fleet(), negotiate.Config{})
	require.Nil(t, bad)
	assert.Equal(t, api.ProviderTCP, result.Provider())
	assert.Equal(t, "dc1", result.Fabric)
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
			name:   "same node and domain",
			mutate: func(s *api.RequestSpec) { s.Destination = api.Destination{Node: "studio-a", Domain: "cameras"} },
			want:   api.ReasonSameEndpoint,
		},
		{
			// Same node, different domain is legitimate — it is exactly what the legacy loopback
			// configuration does, and it is how shm ever gets used.
			name: "same node different domain is fine",
			mutate: func(s *api.RequestSpec) {
				s.Destination = api.Destination{Node: "studio-a", Domain: "local"}
			},
			prepare: func(f *state.Fleet) {
				f.Nodes["studio-a"] = node("studio-a", []api.DomainMapping{
					{Name: "cameras", Configured: true},
					{Name: "local", Configured: true},
				})
			},
		},
		{
			name:   "unknown source node",
			mutate: func(s *api.RequestSpec) { s.Source.Node = "typo" },
			want:   api.ReasonNodeNotRegistered,
		},
		{
			name:   "unknown destination node",
			mutate: func(s *api.RequestSpec) { s.Destination.Node = "typo" },
			want:   api.ReasonNodeNotRegistered,
		},
		{
			name:    "destination domain not mapped",
			mutate:  func(s *api.RequestSpec) { s.Destination.Domain = "/dev/shm/anything" },
			want:    api.ReasonDomainNotMapped,
			message: `"ingest"`,
		},
		{
			// A domain the agent merely discovered by a search path is reportable as a source and
			// is never a destination. This is the invariant that stops the API being a remote
			// arbitrary-filesystem-write (§13, invariant 6).
			name:   "discovered destination domain",
			mutate: func(s *api.RequestSpec) { s.Destination.Domain = "found" },
			want:   api.ReasonDomainNotMapped,
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
				f.Nodes["edge-01"] = node("edge-01", []api.DomainMapping{{Name: "ingest", Configured: true}},
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

			_, bad := Spec(s, f, negotiate.Config{})
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
			Destination: api.Destination{Node: dstNode, Domain: dstDomain},
		},
		Since: since,
	}
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
