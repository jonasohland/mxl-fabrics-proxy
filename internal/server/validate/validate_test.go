package validate

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/negotiate"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
)

func node(name string, opts ...func(*state.NodeRecord)) state.Entry[state.NodeRecord] {
	record := state.NodeRecord{
		Node: name,
		Capabilities: api.Capabilities{
			Fabrics: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.1.1.1",
				CapFlags: []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive},
			}},
			// One writable area and one read-only one. A destination always names its area now
			// (§10.6), so both halves of resolveArea are reachable from this fixture.
			Areas: []api.Area{
				{Name: "fast", Path: "/dev/shm/mxl", Read: true, Write: true},
				{Name: "media", Path: "/dev/shm/mxl-in", Read: true},
			},
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
			"studio-a": node("studio-a"),
			"edge-01":  node("edge-01"),
		},
	}
}

func spec() api.RequestSpec {
	return api.RequestSpec{
		Name:         "studio-a-cam1-to-edge",
		Sources:      []api.Source{{Node: "studio-a", Domain: named("media/cameras"), Select: api.Selector{Flow: "flow-1"}}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	}
}

func TestSpecAccepts(t *testing.T) {
	t.Parallel()

	s := spec()
	result, bad := Pairing(s, 0, 0, fleet(), negotiate.Config{})
	require.Nil(t, bad)
	assert.Equal(t, api.ProviderTCP, result.Provider())
	assert.Equal(t, "dc1", result.Fabric)

	// *There used to be a resolved output root returned alongside the verdict.* A destination's
	// identity is complete the moment the request is read now — the area is the first segment of
	// the domain's name (§10.6) — so there is nothing left to resolve into it.
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
				s.Sources[0].Domain = named("fast/cameras")
				s.Destinations[0] = api.Destination{
					Node: "studio-a", Domain: api.Domain{Area: "fast", Elements: []string{"cameras"}},
				}
			},
			want: api.ReasonSameEndpoint,
		},
		{
			// Same node, different domain is legitimate — it is exactly what the loopback
			// configuration does, and it is how shm ever gets used. One kind of domain now, so
			// the two sides differ only in which one they name (§10.6).
			name: "same node different domain is fine",
			mutate: func(s *api.RequestSpec) {
				s.Destinations[0] = api.Destination{
					Node: "studio-a", Domain: api.Domain{Area: "fast", Elements: []string{"local"}},
				}
			},
		},
		{
			name:   "unknown source node",
			mutate: func(s *api.RequestSpec) { s.Sources[0].Node = "typo" },
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
			mutate: func(s *api.RequestSpec) { s.Destinations[0].Domain.Elements = []string{"/dev/shm/anything"} },
			want:   api.ReasonMalformedDomainName,
		},
		{
			name: "destination node advertises no area at all",
			prepare: func(f *state.Fleet) {
				entry := f.Nodes["edge-01"]
				entry.Value.Capabilities.Areas = nil
				f.Nodes["edge-01"] = entry
			},
			want:    api.ReasonUnknownArea,
			message: "advertises no area",
		},
		{
			name:    "unknown area",
			mutate:  func(s *api.RequestSpec) { s.Destinations[0].Domain.Area = "bulk" },
			want:    api.ReasonUnknownArea,
			message: `"fast" (writable)`,
		},
		{
			// An area that exists and is read-only. A different operator problem from a typo,
			// which is why it is a code of its own (§7.2).
			name:    "area not writable",
			mutate:  func(s *api.RequestSpec) { s.Destinations[0].Domain.Area = "media" },
			want:    api.ReasonAreaNotWritable,
			message: "does not grant writing",
		},
		{
			// *There used to be an `ambiguous_output_root` case here — a node advertising several
			// roots and a request naming none.* It is structurally unreachable: a destination
			// always names its area, because the area is the first segment of the domain's name
			// (§7.2, §10.6). A second area changes nothing.
			name: "a second area is not an ambiguity",
			prepare: func(f *state.Fleet) {
				entry := f.Nodes["edge-01"]
				entry.Value.Capabilities.Areas = append(entry.Value.Capabilities.Areas,
					api.Area{Name: "bulk", Path: "/mnt/nvme/mxl", Read: true, Write: true})
				f.Nodes["edge-01"] = entry
			},
		},
		{
			name:   "a destination in the second area",
			mutate: func(s *api.RequestSpec) { s.Destinations[0].Domain.Area = "bulk" },
			prepare: func(f *state.Fleet) {
				entry := f.Nodes["edge-01"]
				entry.Value.Capabilities.Areas = append(entry.Value.Capabilities.Areas,
					api.Area{Name: "bulk", Path: "/mnt/nvme/mxl", Read: true, Write: true})
				f.Nodes["edge-01"] = entry
			},
		},
		{
			name:   "sched_prio on a node without the capability",
			mutate: func(s *api.RequestSpec) { s.SchedPrio = ptr(50) },
			want:   api.ReasonSchedPrioUnavailable,
		},
		{
			// A named source resolving to a destination is an operator having written the same
			// string twice — decidable from the request, and refused (§7.2).
			name: "source and destination are the same endpoint",
			mutate: func(s *api.RequestSpec) {
				s.Sources[0].Domain = named("fast/ingest")
				s.Sources[0].Node = "edge-01"
			},
			want:    api.ReasonSameEndpoint,
			message: "sources[0] and destinations[0]",
		},
		{
			// Checked over the whole cross product, and the message names **both** indices —
			// "source and destination are both edge-01/fast/ingest" does not say which of several
			// sources it is (§7.2, §9.1).
			//
			// This is also the test that an intra-request cycle is unwritable: a cycle always puts
			// some endpoint on both sides, and a self-pair in the cross product is exactly what this
			// refuses.
			name: "one bad pairing among several refuses the request",
			mutate: func(s *api.RequestSpec) {
				s.Sources = append(s.Sources, api.Source{
					Node: "edge-01", Domain: named("fast/ingest"), Select: api.Selector{All: true},
				})
				s.Destinations = append(s.Destinations, api.Destination{
					Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"other"}},
				})
			},
			want:    api.ReasonSameEndpoint,
			message: "sources[1] and destinations[0]",
		},
		{
			// Two sources pinning one flow UUID into a shared destination is two initiators writing
			// one ring buffer — flow_conflict's harm from inside a single request, decidable from
			// the body because both sources pinned rather than selected (§7.2).
			name: "two sources pinning one flow",
			mutate: func(s *api.RequestSpec) {
				s.Sources = append(s.Sources, api.Source{
					Node: "studio-b", Domain: named("media/cameras"), Select: api.Selector{Flow: "flow-1"},
				})
			},
			want:    api.ReasonDuplicateSourceFlow,
			message: "both pin flow flow-1",
		},
		{
			// The undecidable form stays per path: a selector may or may not produce that flow, and
			// refusing on a maybe refuses a request that probably works. It becomes
			// `flow_conflict` in [Conflicts] if and when the collision actually arrives.
			name:    "one pinned and one selected is not decidable here",
			prepare: func(f *state.Fleet) { f.Nodes["studio-b"] = node("studio-b") },
			mutate: func(s *api.RequestSpec) {
				s.Sources = append(s.Sources, api.Source{
					Node: "studio-b", Domain: named("media/cameras"), Select: api.Selector{All: true},
				})
			},
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
				f.Nodes["edge-01"] = node("edge-01",
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

			// Both checks, in the order [reconcile.Compute] runs them: what is wrong with the
			// request as a whole outranks what is wrong with one of its pairings, because no
			// pairing's message would name the thing to change (§7.2). And **every** pairing, not
			// the first — a request viable for eleven pairings and refused for the twelfth is
			// refused (§9.1).
			bad := Request(s, f)
			for i := range s.Sources {
				for j := range s.Destinations {
					if bad != nil {
						break
					}
					_, bad = Pairing(s, i, j, f, negotiate.Config{})
				}
			}
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

// ref builds one expanded path. The destination domain is written the way a manifest spells it,
// `<area>/<elements>`, and split here because a test is allowed the convenience the rest of the
// tree is not.
func ref(id string, srcNode, srcDomain, flow, dstNode, dstDomain string, since time.Time) PathRef {
	segments := strings.Split(dstDomain, "/")
	return PathRef{
		ID: id,
		Path: state.PathIdentity{
			Source:      api.FlowAddress{Node: srcNode, Domain: srcDomain, Flow: flow},
			Destination: api.Destination{Node: dstNode, Domain: api.Domain{Area: segments[0], Elements: segments[1:]}},
		},
		Since: since,
	}
}

// **One name under two areas is two destinations, not a collision.** *This inverts
// TestConflictsRejectsOneNameUnderTwoRoots*, which tested the flat-namespace rule §10.6
// supersedes: with the area in the domain's name, `fast/ingest` and `bulk/ingest` are two strings
// and the collision is unconstructible.
func TestOneNameInTwoAreasIsTwoDestinations(t *testing.T) {
	t.Parallel()

	first := time.Unix(1000, 0)
	out := Conflicts([]PathRef{
		ref("p2", "studio-b", "cameras", "flow-2", "edge-01", "bulk/ingest", first.Add(time.Hour)),
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "fast/ingest", first),
	})
	assert.Empty(t, out)
}

// **One materialised domain may not contain another** (§10.6): the outer one's flows and the
// inner one's would share a tree, and removing either is a question with no answer.
func TestConflictsRejectsNestedDomains(t *testing.T) {
	t.Parallel()

	first := time.Unix(1000, 0)
	out := Conflicts([]PathRef{
		ref("p2", "studio-b", "cameras", "flow-2", "edge-01", "fast/studio-a/cam1", first.Add(time.Hour)),
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "fast/studio-a", first),
	})

	// Oldest-first, like the other conflicts: a new request never invalidates a path that is
	// probably already carrying media.
	require.Contains(t, out, "p2")
	assert.Equal(t, api.ReasonDomainNameInUse, out["p2"].Code)
	assert.Contains(t, out["p2"].Message, "fast/studio-a")
	assert.NotContains(t, out, "p1")

	// Across areas there is nothing to nest: two areas are two directory trees.
	assert.Empty(t, Conflicts([]PathRef{
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "fast/studio-a", first),
		ref("p2", "studio-b", "cameras", "flow-2", "edge-01", "bulk/studio-a/cam1", first.Add(time.Hour)),
	}))

	// And a sibling whose name is a string prefix is not nested — the trap the element form
	// exists to remove.
	assert.Empty(t, Conflicts([]PathRef{
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "fast/studio-a", first),
		ref("p2", "studio-b", "cameras", "flow-2", "edge-01", "fast/studio-ab", first.Add(time.Hour)),
	}))
}

// Two sources into one domain is ordinary and must stay so: it is how a destination collects
// several flows, and it is what the refcount on a materialised domain is for.
func TestConflictsAcceptsTwoFlowsIntoOneDomain(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	out := Conflicts([]PathRef{
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "fast/ingest", now),
		ref("p2", "studio-b", "cameras", "flow-2", "edge-01", "fast/ingest", now.Add(time.Hour)),
	})
	assert.Empty(t, out)
}

func TestConflictsAcceptsIndependentPaths(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	out := Conflicts([]PathRef{
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "fast/ingest", now),
		ref("p2", "studio-a", "cameras", "flow-2", "edge-01", "fast/ingest", now),
		ref("p3", "studio-a", "cameras", "flow-1", "edge-02", "fast/ingest", now),
	})
	assert.Empty(t, out)
}

// Two producers into one flow ID corrupts the ring buffer, and nothing downstream notices:
// both sessions look healthy and the media is garbage.
func TestConflictsRejectsTwoSourcesIntoOneDestinationFlow(t *testing.T) {
	t.Parallel()

	first := time.Unix(1000, 0)
	out := Conflicts([]PathRef{
		ref("p2", "studio-b", "cameras", "flow-1", "edge-01", "fast/ingest", first.Add(time.Hour)),
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "fast/ingest", first),
	})

	// The older path wins: it is the one probably already carrying media, so the newcomer is
	// what fails.
	require.Contains(t, out, "p2")
	assert.Equal(t, api.ReasonFlowConflict, out["p2"].Code)
	assert.NotContains(t, out, "p1")

	// **Both sources, not the winner alone** (§7.5). Fan-in makes two paths of one request
	// colliding routine, and there the tie falls through to the path ID — deterministic, and
	// arbitrary from the operator's point of view, since nothing in the request says which of two
	// sources of one flow ID was meant. Naming only the incumbent reads as an explanation when it
	// is really a coin toss.
	assert.Contains(t, out["p2"].Message, "studio-a/cameras")
	assert.Contains(t, out["p2"].Message, "studio-b/cameras")
}

// The same edge appearing twice is deduplication, not a conflict: N requests naming one edge
// share one path and one session.
func TestConflictsIgnoresADuplicatedEdge(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	out := Conflicts([]PathRef{
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "fast/ingest", now),
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "fast/ingest", now),
	})
	assert.Empty(t, out)
}

// A→B→C is fine and useful. A→B plus B→A for one flow feeds a flow back into itself, and so
// does the same mistake spelled longer (§7.2).
//
// The source domains are written `fast/d` here, the same grammar the destinations use, because
// that is the point of one identity grammar: the middle hop of a chain is the *same* domain seen
// from both ends, and the cycle detector compares those two strings (§10.6).
func TestConflictsRejectsCyclesButNotChains(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)

	chain := Conflicts([]PathRef{
		ref("p1", "a", "fast/d", "flow-1", "b", "fast/d", now),
		ref("p2", "b", "fast/d", "flow-1", "c", "fast/d", now.Add(time.Second)),
	})
	assert.Empty(t, chain)

	loop := Conflicts([]PathRef{
		ref("p1", "a", "fast/d", "flow-1", "b", "fast/d", now),
		ref("p2", "b", "fast/d", "flow-1", "a", "fast/d", now.Add(time.Second)),
	})
	require.Contains(t, loop, "p2")
	assert.Equal(t, api.ReasonLoop, loop["p2"].Code)
	assert.NotContains(t, loop, "p1")

	longer := Conflicts([]PathRef{
		ref("p1", "a", "fast/d", "flow-1", "b", "fast/d", now),
		ref("p2", "b", "fast/d", "flow-1", "c", "fast/d", now.Add(time.Second)),
		ref("p3", "c", "fast/d", "flow-1", "a", "fast/d", now.Add(2*time.Second)),
	})
	require.Contains(t, longer, "p3")
	assert.Equal(t, api.ReasonLoop, longer["p3"].Code)

	// A cycle in *another* flow's graph is not this flow's problem: the graphs are per flow ID.
	unrelated := Conflicts([]PathRef{
		ref("p1", "a", "fast/d", "flow-1", "b", "fast/d", now),
		ref("p2", "b", "d", "flow-2", "a", "fast/d", now.Add(time.Second)),
	})
	assert.Empty(t, unrelated)
}

// Every replica must reach the same verdict from the same paths, whatever order they were
// listed in, or one replica's INVALID is another's ACTIVE.
func TestConflictsAreOrderIndependent(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	paths := []PathRef{
		ref("p3", "studio-c", "cameras", "flow-1", "edge-01", "fast/ingest", now.Add(2*time.Second)),
		ref("p1", "studio-a", "cameras", "flow-1", "edge-01", "fast/ingest", now),
		ref("p2", "studio-b", "cameras", "flow-1", "edge-01", "fast/ingest", now.Add(time.Second)),
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
		ref("zzz", "studio-b", "cameras", "flow-9", "edge-01", "fast/ingest", now),
		ref("aaa", "studio-a", "cameras", "flow-9", "edge-01", "fast/ingest", now),
	})
	require.Contains(t, tied, "zzz")
	assert.NotContains(t, tied, "aaa")
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
				Destination: api.Destination{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: domain}},
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

// named builds a `name` domain selector from the `<area>/<elements>` spelling, splitting it the
// way a manifest does. Tests are allowed the convenience the rest of the tree is not (§10.6).
func named(domain string) api.DomainSelector {
	segments := strings.Split(domain, "/")
	return api.SelectDomain(api.Domain{Area: segments[0], Elements: segments[1:]})
}
