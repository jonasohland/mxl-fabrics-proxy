package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/client"
	"github.com/jonasohland/mxl-replicator/internal/server"
	"github.com/jonasohland/mxl-replicator/internal/server/leader"
	"github.com/jonasohland/mxl-replicator/internal/store/sqlite"
)

// --- the verbs against a real server (M8c) --------------------------------------------------

// fleet stands a real server on a real store in front of the verbs, and registers the two nodes a
// request needs. Nothing below the HTTP boundary is faked: the point is the wiring between the
// manifest, the client and the API.
func fleet(t *testing.T) string {
	t.Helper()

	backing, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "store.db"), sqlite.Options{
		PollInterval: 20 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, backing.Close()) })

	srv, err := server.New(server.Config{
		Store:             backing,
		Elector:           leader.Always{Replica: "test"},
		Logger:            slog.New(slog.DiscardHandler),
		Listen:            "127.0.0.1:0",
		HeartbeatInterval: 50 * time.Millisecond,
		LeaseTTL:          5 * time.Second,
		MaxLongPollWait:   time.Second,
	})
	require.NoError(t, err)

	http := httptest.NewServer(srv.Handler())
	t.Cleanup(http.Close)

	for _, node := range []string{"studio-a", "edge-01", "edge-02"} {
		register(t, http.URL, node)
	}

	// One produced flow on studio-a, so a request expands onto a real path with a real session
	// rather than sitting in WAITING. Producing matters: the server starts no workers at all
	// until the source is actually being written to (§11.1).
	post(t, http.URL, api.InventoryPath("studio-a"), api.InventorySnapshot{
		Node: "studio-a", Instance: "i-studio-a",
		Domains: []api.DomainInventory{{Domain: api.Domain{Area: "media", Elements: []string{"cameras"}}, Flows: []api.FlowInventory{{
			ID:         "flow-1",
			Definition: json.RawMessage(`{"id":"flow-1","label":"Camera 1","format":"urn:x-nmos:format:video","media_type":"video/v210"}`),
			GroupHint:  &api.GroupHint{Name: "Studio A Camera 1", Type: "video"},
			Producing:  true,
		}}}},
	}, 204)

	return http.URL
}

func post(t *testing.T, base, path string, body any, want int) {
	t.Helper()

	encoded, err := json.Marshal(body)
	require.NoError(t, err)

	resp, err := http.Post(base+path, "application/json", bytes.NewReader(encoded))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, want, resp.StatusCode)
}

func register(t *testing.T, base, node string) {
	t.Helper()

	body, err := json.Marshal(api.NodeRegistration{
		Node:     node,
		Instance: "i-" + node,
		Capabilities: api.Capabilities{
			Versions: api.Versions{Protocol: api.ProtocolVersion, Replicator: "test"},
			Fabrics: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.0.0.1",
				CapFlags: []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive},
			}},
			Areas: []api.Area{
				{Name: "media", Path: "/dev/shm/mxl-in", Read: true},
				{Name: "fast", Path: "/dev/shm/mxl", Read: true, Write: true},
			},
		},
	})
	require.NoError(t, err)

	resp, err := http.Post(base+api.PathRegister, "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
}

// userClient builds the same client the verbs build, for asserting on what they did.
func userClient(t *testing.T, flags ClientFlags) *client.Client {
	t.Helper()
	c, err := flags.client()
	require.NoError(t, err)
	return c
}

func manifestFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

const twoRequests = `
name: cam1
source: {node: studio-a, domain: media/cameras, flow: flow-1}
destinations:
  - {node: edge-01, domain: fast/ingest}
  - {node: edge-02, domain: fast/ingest}
labels: {show: nab}
---
name: cam2
source: {node: studio-a, domain: media/cameras, flow: flow-2}
destinations: [{node: edge-01, domain: fast/ingest2}]
labels: {show: nab}
`

func TestApplyThenDelete(t *testing.T) {
	t.Parallel()

	base := fleet(t)
	path := manifestFile(t, twoRequests)
	flags := ClientFlags{Server: []string{base}}

	require.NoError(t, (&ApplyCmd{Files: []string{path}, ClientFlags: flags}).Run(t.Context()))

	user := userClient(t, flags)
	requests, err := user.Requests(t.Context(), "")
	require.NoError(t, err)
	require.Len(t, requests, 2)

	// The fan-out survives the round trip: two destinations in, two destinations stored.
	assert.Len(t, requests[0].Destinations, 2)

	// Idempotent, which is the whole reason apply works at all: the second run is a no-op that
	// says so rather than a rewrite.
	require.NoError(t, (&ApplyCmd{Files: []string{path}, ClientFlags: flags}).Run(t.Context()))

	applied, err := user.Apply(t.Context(), requests[0].RequestSpec, false)
	require.NoError(t, err)
	assert.Equal(t, api.OutcomeUnchanged, applied.Outcome)

	// delete -f takes only the names, so the same file removes what it created.
	require.NoError(t, (&DeleteCmd{Files: []string{path}, ClientFlags: flags}).Run(t.Context()))

	requests, err = user.Requests(t.Context(), "")
	require.NoError(t, err)
	assert.Empty(t, requests)

	// And deleting again succeeds: removing what a manifest names is idempotent by nature, so the
	// second run of a delete must not fail because the first one worked.
	require.NoError(t, (&DeleteCmd{Files: []string{path}, ClientFlags: flags}).Run(t.Context()))
}

func TestApplyDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	base := fleet(t)
	flags := ClientFlags{Server: []string{base}}

	cmd := &ApplyCmd{Files: []string{manifestFile(t, twoRequests)}, DryRun: true, ClientFlags: flags}
	require.NoError(t, cmd.Run(t.Context()))

	requests, err := userClient(t, flags).Requests(t.Context(), "")
	require.NoError(t, err)
	assert.Empty(t, requests, "a dry run must create nothing")
}

// **Invariant 14.** --prune cancels live media, so it is impossible without a scope, and a dry
// run cancels nothing.
//
// **A namespace is that scope**, not a label selector: it is a declared partition rather than an
// ad-hoc tag, so `-n` is the primary spelling and `-l` narrows within it (§9.1). *This supersedes
// "--prune requires --selector".*
func TestPruneRequiresANamespace(t *testing.T) {
	t.Parallel()

	base := fleet(t)
	flags := ClientFlags{Server: []string{base}}
	path := manifestFile(t, twoRequests)

	err := (&ApplyCmd{Files: []string{path}, Prune: true, ClientFlags: flags}).Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--prune requires --namespace")

	// A selector alone is not a scope, because it does not bound the fleet the way a partition
	// does.
	err = (&ApplyCmd{Files: []string{path}, Prune: true, Selector: []string{"show=nab"}, ClientFlags: flags}).Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--prune requires --namespace")

	// And either flag without --prune is a mistake worth naming rather than ignoring: both read
	// as if they were scoping the apply, which they are not.
	err = (&ApplyCmd{Files: []string{path}, Selector: []string{"show=nab"}, ClientFlags: flags}).Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only meaningful with --prune")

	err = (&ApplyCmd{Files: []string{path}, Namespace: "nab", ClientFlags: flags}).Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only meaningful with --prune")
}

func TestPruneCancelsOnlyUnnamedMatches(t *testing.T) {
	t.Parallel()

	base := fleet(t)
	flags := ClientFlags{Server: []string{base}}
	user := userClient(t, flags)

	// Three requests: two labelled show=nab, one belonging to somebody else.
	require.NoError(t, (&ApplyCmd{
		Files:       []string{manifestFile(t, twoRequests)},
		ClientFlags: flags,
	}).Run(t.Context()))

	other := manifestFile(t, `
name: someone-elses
source: {node: studio-a, domain: media/cameras, flow: flow-9}
destinations: [{node: edge-01, domain: fast/other}]
labels: {show: other-show}
`)
	require.NoError(t, (&ApplyCmd{Files: []string{other}, ClientFlags: flags}).Run(t.Context()))

	// Now apply a manifest naming only cam1, pruning show=nab.
	narrowed := manifestFile(t, `
name: cam1
source: {node: studio-a, domain: media/cameras, flow: flow-1}
destinations:
  - {node: edge-01, domain: fast/ingest}
  - {node: edge-02, domain: fast/ingest}
labels: {show: nab}
`)

	// A dry run first: it must report the prune and cancel nothing.
	require.NoError(t, (&ApplyCmd{
		Files: []string{narrowed}, Prune: true, Namespace: api.DefaultNamespace,
		Selector: []string{"show=nab"}, DryRun: true, ClientFlags: flags,
	}).Run(t.Context()))

	requests, err := user.Requests(t.Context(), "")
	require.NoError(t, err)
	assert.Len(t, requests, 3, "a dry run must cancel nothing")

	require.NoError(t, (&ApplyCmd{
		Files: []string{narrowed}, Prune: true, Namespace: api.DefaultNamespace,
		Selector: []string{"show=nab"}, ClientFlags: flags,
	}).Run(t.Context()))

	requests, err = user.Requests(t.Context(), "")
	require.NoError(t, err)

	names := map[string]bool{}
	for _, request := range requests {
		names[request.Name] = true
	}
	assert.Equal(t, map[string]bool{"cam1": true, "someone-elses": true}, names,
		"cam2 matched the selector and was not named, so it goes; someone-elses does not match and stays")
}

// A mistyped verb must be an unknown command, not `run` with a stray argument — which is what
// kong's default:"withargs" would otherwise make of it.
//
// The verb list comes from the real parser rather than a literal, so this also catches the failure
// that shipped: a verb present on [CLI] and absent from the guard's list is not rejected as
// unknown — it falls through to `run` and is diagnosed in terms of flags nobody typed.
func TestAMistypedVerbIsNotSwallowedByTheDefaultCommand(t *testing.T) {
	t.Parallel()

	commands := commandNames(kong.Must(&CLI{}, kong.Vars{"version": "test"}).Model)

	err := guardDefaultCommand(commands, []string{"aply", "-f", "x.yaml"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown command "aply"`)

	// Every verb the binary actually has, so adding one to [CLI] cannot leave it unreachable.
	assert.ElementsMatch(t,
		[]string{"run", "apply", "delete", "label", "status", "get", "describe"}, commands)
	for _, verb := range commands {
		assert.NoError(t, guardDefaultCommand(commands, []string{verb}), "verb %q", verb)
	}

	// The forms that must keep working besides: the bare default, and a flag going straight to it.
	for _, args := range [][]string{{}, {"run", "--agent"}, {"--agent"}, {"apply", "-f", "x.yaml"}} {
		assert.NoError(t, guardDefaultCommand(commands, args), "args %v", args)
	}
}

func TestSelectorParsing(t *testing.T) {
	t.Parallel()

	selector, err := parseSelector([]string{"show=nab", "tier=1"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"show": "nab", "tier": "1"}, selector)

	_, err = parseSelector([]string{"nonsense"})
	assert.Error(t, err)

	assert.True(t, matchesSelector(map[string]string{"show": "nab", "x": "y"}, selector0("show", "nab")))
	assert.False(t, matchesSelector(map[string]string{"show": "other"}, selector0("show", "nab")))
	assert.False(t, matchesSelector(nil, selector0("show", "nab")))

	// An empty selector matches everything, which is the usual convention and is now correct
	// here: prune's blast radius is bounded by its required --namespace, so -l narrows within a
	// scope rather than being the scope. *Under the superseded rule this was false.*
	assert.True(t, matchesSelector(map[string]string{"show": "nab"}, nil))
}

func selector0(key, value string) map[string]string { return map[string]string{key: value} }

// --- describe (M8h) --------------------------------------------------------------------------

// describe covers the five nouns of §3, and path and session stay separate even though they are
// 1:1 in practice: a path is derived state that outlives any particular session, and collapsing
// them would suggest a path dies when its workers do.
func TestDescribeEveryKind(t *testing.T) {
	t.Parallel()

	base := fleet(t)
	flags := ClientFlags{Server: []string{base}}
	require.NoError(t, (&ApplyCmd{Files: []string{manifestFile(t, twoRequests)}, ClientFlags: flags}).Run(t.Context()))

	// The IDs are server-derived, so read them back rather than inventing them.
	paths, err := userClient(t, flags).Paths(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, paths.Paths, "cam1 should have expanded onto paths")

	path := paths.Paths[0]
	require.NotNil(t, path.Session, "a path over a produced flow should have a session")

	for name, kind := range map[string][2]string{
		"node":    {"node", "studio-a"},
		"flow":    {"flow", "flow-1"},
		"request": {"request", "cam1"},
		"path":    {"path", path.ID},
		"session": {"session", path.Session.ID},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := &DescribeCmd{Kind: kind[0], Name: kind[1], ClientFlags: flags}
			require.NoError(t, cmd.Run(t.Context()))

			// The machine formats come verbatim from the API types, so a script written against
			// -o json is written against the documented API rather than against this command.
			cmd.Output = "json"
			require.NoError(t, cmd.Run(t.Context()))
		})
	}
}

// Every kind must fail with a message that says what was looked for, rather than printing an
// empty record.
func TestDescribeUnknownNames(t *testing.T) {
	t.Parallel()

	base := fleet(t)
	flags := ClientFlags{Server: []string{base}}

	for _, kind := range []string{"node", "flow", "request", "path", "session"} {
		t.Run(kind, func(t *testing.T) {
			err := (&DescribeCmd{Kind: kind, Name: "nope", ClientFlags: flags}).Run(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "nope")
		})
	}

	// An unknown node names the fleet, because the answer is usually a typo and the candidates
	// are two lines away.
	err := (&DescribeCmd{Kind: "node", Name: "studio-b", ClientFlags: flags}).Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "studio-a")
}

// A flow id is unique to the media, not to a location: after replication the same id exists on
// both nodes, and describe must list every one of them rather than picking one (§3).
func TestDescribeFlowListsEveryLocation(t *testing.T) {
	t.Parallel()

	base := fleet(t)
	flags := ClientFlags{Server: []string{base}}

	// The destination agent reports the replicated flow in its own inventory, exactly as the
	// source agent reports the original.
	post(t, base, api.InventoryPath("edge-01"), api.InventorySnapshot{
		Node: "edge-01", Instance: "i-edge-01",
		Domains: []api.DomainInventory{{Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}, Flows: []api.FlowInventory{{
			ID:         "flow-1",
			Definition: json.RawMessage(`{"id":"flow-1"}`),
			Producing:  true,
		}}}},
	}, 204)

	entries, err := userClient(t, flags).Flows(t.Context(), client.FlowFilter{Flow: "flow-1"})
	require.NoError(t, err)

	places := make([]string, 0, len(entries))
	for _, entry := range entries {
		places = append(places, entry.Node+"/"+entry.Domain)
	}
	assert.ElementsMatch(t, []string{"studio-a/media/cameras", "edge-01/fast/ingest"}, places)

	require.NoError(t, (&DescribeCmd{Kind: "flow", Name: "flow-1", ClientFlags: flags}).Run(t.Context()))
}

// --- get and status (M8i) --------------------------------------------------------------------

func TestGetEveryKind(t *testing.T) {
	t.Parallel()

	base := fleet(t)
	flags := ClientFlags{Server: []string{base}}
	require.NoError(t, (&ApplyCmd{Files: []string{manifestFile(t, twoRequests)}, ClientFlags: flags}).Run(t.Context()))

	// Plural and singular are the same command. Insisting on one would be a rule with nothing
	// behind it.
	for _, kind := range []string{
		"nodes", "node", "flows", "flow", "requests", "request", "paths", "path", "sessions", "session",
	} {
		t.Run(kind, func(t *testing.T) {
			cmd := &GetCmd{Kind: kind, ClientFlags: flags}
			require.NoError(t, cmd.Run(t.Context()))

			cmd.Output = "json"
			require.NoError(t, cmd.Run(t.Context()))
		})
	}
}

// A filter that cannot apply is an error, not a no-op. Silently ignoring one is how somebody
// concludes a flow is missing when they only narrowed on the wrong field.
func TestGetRejectsInapplicableFilters(t *testing.T) {
	t.Parallel()

	base := fleet(t)
	flags := ClientFlags{Server: []string{base}}

	for name, tc := range map[string]struct {
		cmd  GetCmd
		want string
	}{
		"node filter on nodes":   {GetCmd{Kind: "nodes", Node: "studio-a"}, "--node applies to"},
		"domain filter on paths": {GetCmd{Kind: "paths", Domain: "cameras"}, "--domain applies to"},
		"selector on flows":      {GetCmd{Kind: "flows", Selector: []string{"a=b"}}, "--selector applies to"},
		"selector on sessions":   {GetCmd{Kind: "sessions", Selector: []string{"a=b"}}, "--selector applies to"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := tc.cmd
			cmd.ClientFlags = flags
			err := cmd.Run(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	// The ones that do apply must still work.
	for _, cmd := range []GetCmd{
		{Kind: "flows", Node: "studio-a", Domain: "cameras"},
		{Kind: "paths", Node: "studio-a"},
		{Kind: "sessions", Node: "studio-a"},
		{Kind: "requests", Selector: []string{"show=nab"}},
	} {
		cmd.ClientFlags = flags
		assert.NoError(t, cmd.Run(t.Context()), "kind %s", cmd.Kind)
	}
}

// --node on paths matches *either* end, because a node is routinely both: same node, different
// domain is legitimate and is what the loopback configuration does (§7.2).
func TestGetPathsFiltersOnEitherEnd(t *testing.T) {
	t.Parallel()

	base := fleet(t)
	flags := ClientFlags{Server: []string{base}}
	require.NoError(t, (&ApplyCmd{Files: []string{manifestFile(t, twoRequests)}, ClientFlags: flags}).Run(t.Context()))

	paths, err := userClient(t, flags).Paths(t.Context())
	require.NoError(t, err)

	// cam1 fans out from studio-a to edge-01 and edge-02; cam2 pins a flow id that nothing
	// produces, and a *pinned* selector expands to its one path regardless of inventory — it is
	// WAITING, not absent. So studio-a is the source of all three.
	assert.Len(t, filterPaths(paths.Paths, "studio-a"), 3, "the source end of everything")
	assert.Len(t, filterPaths(paths.Paths, "edge-01"), 2, "cam1's leg plus cam2's")
	assert.Len(t, filterPaths(paths.Paths, "edge-02"), 1, "cam1's other leg")
	assert.Empty(t, filterPaths(paths.Paths, "nowhere"))
}

// status counts and then names only what is not healthy, which is what makes it actionable rather
// than a second list.
func TestStatusSummarises(t *testing.T) {
	t.Parallel()

	base := fleet(t)
	flags := ClientFlags{Server: []string{base}}

	require.NoError(t, (&StatusCmd{ClientFlags: flags}).Run(t.Context()))

	require.NoError(t, (&ApplyCmd{Files: []string{manifestFile(t, twoRequests)}, ClientFlags: flags}).Run(t.Context()))
	require.NoError(t, (&StatusCmd{ClientFlags: flags, OutputFlags: OutputFlags{Output: "json"}}).Run(t.Context()))

	// cam2 selects flow-2, which nothing produces, so it is WAITING and must be named. cam1 is
	// ESTABLISHING — also not ACTIVE, also named. That is the point: the summary is the two lines
	// worth reading, not the whole fleet.
	user := userClient(t, flags)
	requests, err := user.Requests(t.Context(), "")
	require.NoError(t, err)

	var waiting int
	for _, request := range requests {
		if request.Status.State == api.StateWaiting {
			waiting++
		}
	}
	assert.Equal(t, 1, waiting, "cam2's selector matches no flow")
}

// named builds a `name` domain selector from the `<area>/<elements>` spelling, splitting it the
// way a manifest does. Tests are allowed the convenience the rest of the tree is not (§10.6).
func named(domain string) api.DomainSelector {
	segments := strings.Split(domain, "/")
	return api.SelectDomain(api.Domain{Area: segments[0], Elements: segments[1:]})
}

// --- label (§9.1, §10.7) ------------------------------------------------------------------------

// The argument form: `<node>:<area>/<elements>`, and the domain half is not re-split anywhere
// else — it goes through the one parser in the tree (§10.6).
func TestLabelTargetParsing(t *testing.T) {
	t.Parallel()

	node, domain, err := parseLabelTarget("studio-a:media/cameras")
	require.NoError(t, err)
	assert.Equal(t, "studio-a", node)
	assert.Equal(t, api.Domain{Area: "media", Elements: []string{"cameras"}}, domain)

	// A node name may itself contain a colon — it is operator-assigned free text — so the split is
	// on the *last* one, which is the only reading that makes this unambiguous.
	node, domain, err = parseLabelTarget("rack:1:media/cameras")
	require.NoError(t, err)
	assert.Equal(t, "rack:1", node)
	assert.Equal(t, "media/cameras", domain.String())

	for _, bad := range []string{
		"studio-a",              // no domain
		":media/cameras",        // no node
		"studio-a:media",        // an area's own directory is not a domain
		"studio-a:/dev/shm/mxl", // a path where a domain belongs
		"studio-a:media/../etc", // traversal
	} {
		_, _, err := parseLabelTarget(bad)
		assert.Error(t, err, "target %q", bad)
	}
}

// `key=value` sets, `key-` removes — the convention operators already have (§9.1).
func TestLabelEditParsing(t *testing.T) {
	t.Parallel()

	patch, err := parseLabelEdits([]string{"role=cameras", "name=cameras", "site-"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"role": "cameras", "name": "cameras"}, patch.Set)
	assert.Equal(t, []string{"site"}, patch.Remove)

	// An empty value is a set, not a remove: `role=` says the key exists and is blank.
	patch, err = parseLabelEdits([]string{"role="})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"role": ""}, patch.Set)
	assert.Empty(t, patch.Remove)

	for _, bad := range [][]string{
		{}, // nothing to do
		{"nonsense"},
		{"=value"},
		{"-"},
	} {
		_, err := parseLabelEdits(bad)
		assert.Error(t, err, "%v", bad)
	}
}

// **The verb sends a patch, and that is what makes an interactive edit durable**: the server
// merges a patch against nothing, so a key set here survives a later apply that does not mention
// it (§9.1).
func TestLabelSendsAPatchThatSurvivesAnApply(t *testing.T) {
	t.Parallel()

	base := fleet(t)
	flags := ClientFlags{Server: []string{base}}

	require.NoError(t, (&LabelCmd{
		Kind: "domain", Target: "studio-a:media/cameras",
		Labels: []string{"tier=1"}, ClientFlags: flags,
	}).Run(t.Context()))

	// An apply from a file that knows nothing about `tier`.
	path := manifestFile(t, `
kind: domain
node: studio-a
domain: media/cameras
labels: {role: cameras}
`)
	require.NoError(t, (&ApplyCmd{Files: []string{path}, ClientFlags: flags}).Run(t.Context()))

	list, err := userClient(t, flags).NodeDomains(t.Context(), "studio-a")
	require.NoError(t, err)

	var labels map[string]string
	for _, domain := range list.Domains {
		if domain.Domain.String() == "media/cameras" {
			labels = domain.Labels
		}
	}
	assert.Equal(t, map[string]string{"tier": "1", "role": "cameras"}, labels,
		"apply owns `role`; the interactive edit's `tier` is left alone")

	// And a removal takes only what it names.
	require.NoError(t, (&LabelCmd{
		Kind: "domain", Target: "studio-a:media/cameras",
		Labels: []string{"tier-"}, ClientFlags: flags,
	}).Run(t.Context()))

	list, err = userClient(t, flags).NodeDomains(t.Context(), "studio-a")
	require.NoError(t, err)
	for _, domain := range list.Domains {
		if domain.Domain.String() == "media/cameras" {
			assert.Equal(t, map[string]string{"role": "cameras"}, domain.Labels)
		}
	}
}

// A dry run writes nothing, on the same argument requests take: a label starts and stops media one
// level of indirection away, which makes it easier to do by accident rather than harder (§9.1).
func TestLabelDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	base := fleet(t)
	flags := ClientFlags{Server: []string{base}}

	require.NoError(t, (&LabelCmd{
		Kind: "domain", Target: "studio-a:media/cameras",
		Labels: []string{"role=cameras"}, DryRun: true, ClientFlags: flags,
	}).Run(t.Context()))

	list, err := userClient(t, flags).NodeDomains(t.Context(), "studio-a")
	require.NoError(t, err)
	for _, domain := range list.Domains {
		assert.Empty(t, domain.Labels, "a dry run must write nothing")
	}
}

// **`--prune` never removes a domain label**, even when the file contains `kind: domain`
// documents: a file naming three domains would otherwise prune the other forty on the fleet, and a
// label is a fact about a host rather than intent (§9.1).
func TestPruneNeverTouchesLabels(t *testing.T) {
	t.Parallel()

	base := fleet(t)
	flags := ClientFlags{Server: []string{base}}

	labelled := manifestFile(t, `
kind: domain
node: studio-a
domain: media/cameras
labels: {role: cameras}
`)
	require.NoError(t, (&ApplyCmd{Files: []string{labelled}, ClientFlags: flags}).Run(t.Context()))

	// A second apply that names no domain at all, pruning the default namespace.
	other := manifestFile(t, `
name: cam1
source: {node: studio-a, domain: media/cameras, flow: flow-1}
destinations: [{node: edge-01, domain: fast/ingest}]
`)
	require.NoError(t, (&ApplyCmd{
		Files: []string{other}, Prune: true, Namespace: api.DefaultNamespace, ClientFlags: flags,
	}).Run(t.Context()))

	list, err := userClient(t, flags).NodeDomains(t.Context(), "studio-a")
	require.NoError(t, err)
	for _, domain := range list.Domains {
		if domain.Domain.String() == "media/cameras" {
			assert.Equal(t, map[string]string{"role": "cameras"}, domain.Labels)
		}
	}
}
