package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/leader"
	"github.com/jonasohland/mxl-replicator/internal/server/reconcile"
	"github.com/jonasohland/mxl-replicator/internal/store"
	"github.com/jonasohland/mxl-replicator/internal/store/sqlite"
)

// --- harness -----------------------------------------------------------------------------

type harness struct {
	t      *testing.T
	store  store.Store
	server *Server
	http   *httptest.Server
	token  string
}

// newHarness builds a server on a real sqlite store with its reconcile loop running, and serves
// it over a real HTTP listener. Nothing here is faked below the HTTP boundary: the point of these
// tests is the interaction between the handlers, the store and the reconciler.
func newHarness(t *testing.T, adjust ...func(*Config)) *harness {
	t.Helper()

	backing, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "store.db"), sqlite.Options{
		PollInterval: 20 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, backing.Close()) })

	cfg := Config{
		Store:              backing,
		Elector:            leader.Always{Replica: "test"},
		Logger:             slog.New(slog.DiscardHandler),
		Listen:             "127.0.0.1:0",
		HeartbeatInterval:  50 * time.Millisecond,
		LeaseTTL:           5 * time.Second,
		SettlingHeartbeats: 0,
		MaxLongPollWait:    2 * time.Second,
		Reconcile:          reconcile.Config{},
	}
	for _, apply := range adjust {
		apply(&cfg)
	}

	srv, err := New(cfg)
	require.NoError(t, err)

	h := &harness{t: t, store: backing, server: srv, token: cfg.Token}
	h.http = httptest.NewServer(srv.Handler())
	t.Cleanup(h.http.Close)
	return h
}

// reconciling starts the loop, which is what makes the server able to answer an assignment poll
// at all: until it has settled, every poll is CodeNotReady.
func (h *harness) reconciling() {
	h.t.Helper()

	ctx, cancel := context.WithCancel(h.t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		assert.NoError(h.t, h.server.loop.Run(ctx))
	}()
	h.t.Cleanup(func() {
		cancel()
		<-done
	})

	require.Eventually(h.t, func() bool {
		settled, err := h.server.settled(context.Background())
		return err == nil && settled
	}, 5*time.Second, 5*time.Millisecond, "the reconciler never settled")
}

type response struct {
	status int
	body   []byte
	header http.Header
}

func (r response) decode(t *testing.T, into any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.body, into), "body: %s", r.body)
}

func (r response) apiError(t *testing.T) api.Error {
	t.Helper()
	var out api.Error
	r.decode(t, &out)
	return out
}

func (h *harness) do(method, path string, body any) response {
	h.t.Helper()
	return h.doCtx(h.t.Context(), method, path, body)
}

func (h *harness) doCtx(ctx context.Context, method, path string, body any) response {
	h.t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(h.t, err)
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, h.http.URL+path, reader)
	require.NoError(h.t, err)
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}

	resp, err := h.http.Client().Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	return response{status: resp.StatusCode, body: raw, header: resp.Header}
}

// register brings a node up the way an agent does, and returns its lease.
//
// Every node advertises one output root, so any of them can be a destination and a request can
// name one without spelling the root (§10.6).
func (h *harness) register(node, instance string) api.RegistrationResponse {
	h.t.Helper()

	resp := h.do(http.MethodPost, api.PathRegister, api.NodeRegistration{
		Node:     node,
		Instance: instance,
		Capabilities: api.Capabilities{
			Versions: api.Versions{Protocol: api.ProtocolVersion, Replicator: "test"},
			Fabrics: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.0.0.1",
				CapFlags: []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive},
			}},
			Areas: []api.Area{{Name: "fast", Path: "/dev/shm/mxl", Read: true, Write: true}},
		},
	})
	require.Equal(h.t, http.StatusOK, resp.status, "body: %s", resp.body)

	var out api.RegistrationResponse
	resp.decode(h.t, &out)
	return out
}

var testFlowDef = json.RawMessage(`{"id":"flow-1","format":"urn:x-nmos:format:video"}`)

// reportInventory posts one node's snapshot. The domain is written `<area>/<elements>` and split
// here for convenience; an agent computes the identity from its own area table (§10.6).
func (h *harness) reportInventory(node, instance, domain string, flows ...api.FlowInventory) {
	h.t.Helper()

	segments := strings.Split(domain, "/")
	resp := h.do(http.MethodPost, api.InventoryPath(node), api.InventorySnapshot{
		Node: node, Instance: instance,
		Domains: []api.DomainInventory{{
			Domain: api.Domain{Area: segments[0], Elements: segments[1:]},
			Flows:  flows,
		}},
	})
	require.Equal(h.t, http.StatusNoContent, resp.status, "body: %s", resp.body)
}

func (h *harness) reportStatus(node, instance string, sessions ...api.SessionStatus) {
	h.t.Helper()

	resp := h.do(http.MethodPost, api.StatusPath(node), api.StatusSnapshot{
		Node: node, Instance: instance, Sessions: sessions,
	})
	require.Equal(h.t, http.StatusNoContent, resp.status, "body: %s", resp.body)
}

func (h *harness) assignments(node string, cursor int64, wait time.Duration) (api.AssignmentSet, response) {
	h.t.Helper()

	path := api.AssignmentsPath(node) + "?" + api.QueryRevision + "=" + strconv.FormatInt(cursor, 10) +
		"&" + api.QueryWait + "=" + strconv.FormatFloat(wait.Seconds(), 'f', 3, 64)

	resp := h.do(http.MethodGet, path, nil)
	var set api.AssignmentSet
	if resp.status == http.StatusOK {
		resp.decode(h.t, &set)
	}
	return set, resp
}

// defaultRequests is where a request with no namespace is created (§9.3): the collection under
// the default partition, which is what every test below that does not care about namespaces uses.
var defaultRequests = api.NamespaceRequestsPath(api.DefaultNamespace)

// rid is a request ID in the default namespace.
func rid(name string) api.RequestID {
	return api.RequestID{Namespace: api.DefaultNamespace, Name: name}
}

func flowRequestSpec(name string) api.RequestSpec {
	return api.RequestSpec{
		Name:         name,
		Sources:      []api.Source{{Node: "studio-a", Domain: named("media/cameras"), Select: api.Selector{Flow: "flow-1"}}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	}
}

// fleet registers the two ordinary nodes and reports one produced flow.
func (h *harness) fleet() {
	h.t.Helper()

	h.register("studio-a", "i-studio")
	h.register("edge-01", "i-edge")
	h.reportInventory("studio-a", "i-studio", "media/cameras", api.FlowInventory{
		ID: "flow-1", Definition: testFlowDef, Producing: true,
	})
	h.reportInventory("edge-01", "i-edge", "ingest")
	h.reportStatus("studio-a", "i-studio")
	h.reportStatus("edge-01", "i-edge")
}

// --- registration and fencing (M4a, §7.1) -------------------------------------------------

func TestRegistrationSeparatesTheDurableFromTheLeased(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	resp := h.register("edge-01", "i-1")

	assert.NotEmpty(t, resp.Lease)
	assert.Equal(t, api.Millis(5*time.Second), resp.TTL)
	assert.Equal(t, api.Millis(50*time.Millisecond), resp.HeartbeatInterval)
	assert.Equal(t, api.ProtocolVersion, resp.Server.Protocol)

	// The registration is durable and unleased; the liveness record is leased, so it collects
	// itself when the agent goes away.
	node, err := h.store.Get(t.Context(), store.NodeKey("edge-01"))
	require.NoError(t, err)
	assert.Zero(t, node.Lease)

	lease, err := h.store.Get(t.Context(), store.LeaseKey("edge-01"))
	require.NoError(t, err)
	assert.NotZero(t, lease.Lease)
}

// Two agents claiming one node name is a real failure mode — a copy-pasted config, an
// overlapping rollout — and it is nasty: both would receive the same assignments, start workers,
// fight over ports and write duplicates into the destination flow (§7.1).
func TestASecondClaimantIsRejected(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register("edge-01", "i-1")

	resp := h.do(http.MethodPost, api.PathRegister, api.NodeRegistration{
		Node: "edge-01", Instance: "i-2",
		Capabilities: api.Capabilities{Versions: api.Versions{Protocol: api.ProtocolVersion}},
	})
	require.Equal(t, http.StatusConflict, resp.status)

	failure := resp.apiError(t)
	assert.Equal(t, api.CodeNodeClaimed, failure.Code)
	assert.Equal(t, "i-1", failure.Details["holder"])

	// The first claimant is undisturbed: its lease still holds and it can still report.
	h.reportStatus("edge-01", "i-1")

	// And the loser's rejected registration left no lease behind for a sweeper to trip over.
	lease, err := h.store.Get(t.Context(), store.LeaseKey("edge-01"))
	require.NoError(t, err)
	var record struct {
		Instance string `json:"instance"`
	}
	require.NoError(t, json.Unmarshal(lease.Value, &record))
	assert.Equal(t, "i-1", record.Instance)
}

// An agent told to re-register comes back with the same instance UUID. That is not a second
// claimant and must not be refused, or an agent whose lease expired during a partition could
// never rejoin the fleet it is still carrying media for.
func TestTheSameInstanceMayReregister(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	first := h.register("edge-01", "i-1")
	second := h.register("edge-01", "i-1")

	assert.NotEqual(t, first.Lease, second.Lease, "a fresh lease is granted")
	h.reportStatus("edge-01", "i-1")

	// The durable half kept its identity across the re-registration.
	var nodes api.NodeList
	h.do(http.MethodGet, api.PathNodes, nil).decode(t, &nodes)
	require.Len(t, nodes.Nodes, 1)
	assert.True(t, nodes.Nodes[0].Live)
	assert.NotEmpty(t, nodes.Nodes[0].Capabilities.Areas,
		"a registration carries capabilities and no domains (§6)")
}

// §13.1: the server is always upgraded first, so it tolerates agents that are behind and refuses
// only the one direction the promise does not cover — and it keys that on the *protocol* version,
// because a refusal keyed on the build version turns any rolling upgrade of combined-role nodes
// into a partial outage (plan §4.1).
func TestProtocolSkewGating(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	resp := h.do(http.MethodPost, api.PathRegister, api.NodeRegistration{
		Node: "edge-01", Instance: "i-1",
		Capabilities: api.Capabilities{Versions: api.Versions{Protocol: api.ProtocolVersion + 1}},
	})
	require.Equal(t, http.StatusBadRequest, resp.status)
	assert.Equal(t, api.CodeVersionSkew, resp.apiError(t).Code)

	// An older agent — including one that reports no protocol at all — is accepted.
	resp = h.do(http.MethodPost, api.PathRegister, api.NodeRegistration{
		Node: "edge-02", Instance: "i-2",
	})
	assert.Equal(t, http.StatusOK, resp.status, "body: %s", resp.body)
}

func TestRegistrationValidation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	for _, tc := range []struct {
		name    string
		body    api.NodeRegistration
		message string
	}{
		{"no node", api.NodeRegistration{Instance: "i-1"}, "node is required"},
		{"no instance", api.NodeRegistration{Node: "edge-01"}, "instance is required"},
		{"padded name", api.NodeRegistration{Node: " edge-01 ", Instance: "i-1"}, "whitespace"},
		{
			"attachment without a fabric label",
			api.NodeRegistration{Node: "edge-01", Instance: "i-1", Capabilities: api.Capabilities{
				Fabrics: []api.FabricAttachment{{Provider: api.ProviderTCP}},
			}},
			"fabric label is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(http.MethodPost, api.PathRegister, tc.body)
			require.Equal(t, http.StatusBadRequest, resp.status)
			assert.Contains(t, resp.apiError(t).Message, tc.message)
		})
	}
}

// shm is same-node-only, and that falls out of ordinary fabric matching only if every node
// derives the identical label. The server canonicalises what it stores so an agent that spelled
// it differently still matches itself (§10.1).
func TestSHMFabricLabelIsCanonicalised(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	resp := h.do(http.MethodPost, api.PathRegister, api.NodeRegistration{
		Node: "edge-01", Instance: "i-1",
		Capabilities: api.Capabilities{
			Versions: api.Versions{Protocol: api.ProtocolVersion},
			Fabrics:  []api.FabricAttachment{{Provider: api.ProviderSHM, Fabric: "whatever", Address: "edge-01"}},
		},
	})
	require.Equal(t, http.StatusOK, resp.status)

	var nodes api.NodeList
	h.do(http.MethodGet, api.PathNodes, nil).decode(t, &nodes)
	require.Len(t, nodes.Nodes, 1)
	assert.Equal(t, api.SHMFabric("edge-01"), nodes.Nodes[0].Capabilities.Fabrics[0].Fabric)
}

// --- heartbeats and ingest (M4b) ----------------------------------------------------------

func TestHeartbeat(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register("edge-01", "i-1")

	resp := h.do(http.MethodPost, api.HeartbeatPath("edge-01"), api.Heartbeat{Node: "edge-01", Instance: "i-1"})
	require.Equal(t, http.StatusOK, resp.status)
	var beat api.HeartbeatResponse
	resp.decode(t, &beat)
	assert.False(t, beat.Reregister)

	// A node with no lease is told to register again — not to stop. Losing a lease says the
	// fleet has forgotten this node, not that its media should stop (§4.2).
	resp = h.do(http.MethodPost, api.HeartbeatPath("gone"), api.Heartbeat{Node: "gone", Instance: "i-9"})
	require.Equal(t, http.StatusConflict, resp.status)
	assert.Equal(t, api.CodeReregister, resp.apiError(t).Code)

	// An instance that does not hold the lease cannot renew it: the lease *is* node identity.
	resp = h.do(http.MethodPost, api.HeartbeatPath("edge-01"), api.Heartbeat{Node: "edge-01", Instance: "i-2"})
	require.Equal(t, http.StatusConflict, resp.status)
	assert.Equal(t, api.CodeNodeClaimed, resp.apiError(t).Code)
}

// A heartbeat renews the lease and writes nothing. Rewriting the record would advance the store
// revision several times a minute per node forever, waking every watcher — including every
// agent's long poll, where a spurious wakeup is a worker restart (§8.3).
func TestHeartbeatDoesNotWrite(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register("edge-01", "i-1")

	_, before, err := h.store.List(t.Context(), "")
	require.NoError(t, err)

	for range 5 {
		resp := h.do(http.MethodPost, api.HeartbeatPath("edge-01"), api.Heartbeat{Node: "edge-01", Instance: "i-1"})
		require.Equal(t, http.StatusOK, resp.status)
	}

	_, after, err := h.store.List(t.Context(), "")
	require.NoError(t, err)
	assert.Equal(t, before, after, "heartbeats must not advance the store revision")
}

func TestIngestRequiresTheLease(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register("edge-01", "i-1")

	// A stale instance must not be able to overwrite the holder's view of the world.
	resp := h.do(http.MethodPost, api.InventoryPath("edge-01"), api.InventorySnapshot{Node: "edge-01", Instance: "i-2"})
	require.Equal(t, http.StatusConflict, resp.status)
	assert.Equal(t, api.CodeNodeClaimed, resp.apiError(t).Code)

	resp = h.do(http.MethodPost, api.StatusPath("unknown"), api.StatusSnapshot{Node: "unknown", Instance: "i-1"})
	require.Equal(t, http.StatusConflict, resp.status)
	assert.Equal(t, api.CodeReregister, resp.apiError(t).Code)

	// The body and the path have to agree about which node is reporting.
	resp = h.do(http.MethodPost, api.InventoryPath("edge-01"), api.InventorySnapshot{Node: "studio-a", Instance: "i-1"})
	assert.Equal(t, http.StatusBadRequest, resp.status)
}

// Observed state is written under the reporting agent's lease, so a node that stops heartbeating
// stops being visible without anything having to clean up after it (§8.3).
func TestObservedStateIsLeased(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register("edge-01", "i-1")
	h.reportInventory("edge-01", "i-1", "ingest")

	inventory, err := h.store.Get(t.Context(), store.InventoryKey("edge-01"))
	require.NoError(t, err)
	assert.NotZero(t, inventory.Lease)

	lease, err := h.store.Get(t.Context(), store.LeaseKey("edge-01"))
	require.NoError(t, err)
	assert.Equal(t, lease.Lease, inventory.Lease, "the same lease, so both go together")

	// Reporting the identical snapshot again is not a write. Inventory is a full snapshot on
	// every report, so without this an unchanged fleet would still churn the store (§8.3).
	_, before, err := h.store.List(t.Context(), "")
	require.NoError(t, err)
	h.reportInventory("edge-01", "i-1", "ingest")
	_, after, err := h.store.List(t.Context(), "")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// --- assignments (M4c, §9.2) ---------------------------------------------------------------

// The most important behaviour in the package: while the reconciler has not settled, an
// assignment poll is refused rather than answered with an empty set. An agent that receives an
// empty set correctly tears down every worker it is running, so "I don't know yet" and "nothing
// to do" must never be spelled the same way (§4.2, plan §4.2).
func TestAssignmentsAreRefusedUntilTheReconcilerHasSettled(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	// A registered, reporting fleet — so that what follows is the *reconciler* not having run
	// yet, rather than there being nothing to reconcile.
	h.fleet()

	_, resp := h.assignments("edge-01", 0, 0)
	require.Equal(t, http.StatusServiceUnavailable, resp.status)
	assert.Equal(t, api.CodeNotReady, resp.apiError(t).Code)
	assert.NotContains(t, string(resp.body), `"assignments"`, "not even an empty set may be returned")

	h.reconciling()

	set, resp := h.assignments("edge-01", 0, 0)
	require.Equal(t, http.StatusOK, resp.status)
	assert.Equal(t, "edge-01", set.Node)
	assert.Empty(t, set.Assignments)
	assert.Positive(t, set.Revision, "the cursor for the next poll")
}

func TestAssignmentsForAnUnregisteredNode(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reconciling()

	_, resp := h.assignments("never-seen", 0, 0)
	require.Equal(t, http.StatusConflict, resp.status)
	assert.Equal(t, api.CodeReregister, resp.apiError(t).Code)
}

// The long poll: held until the revision advances, released the moment it does. That is what
// makes a peer learn of a new epoch in well under a second, which is the whole re-establishment
// budget of §6.1.
func TestLongPollReleasesOnChange(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()
	h.reconciling()

	set, resp := h.assignments("edge-01", 0, 0)
	require.Equal(t, http.StatusOK, resp.status)
	cursor := set.Revision

	done := make(chan api.AssignmentSet, 1)
	go func() {
		held, _ := h.assignments("edge-01", cursor, 2*time.Second)
		done <- held
	}()

	// Give the poll a moment to be genuinely waiting rather than racing the request below.
	time.Sleep(50 * time.Millisecond)

	resp = h.do(http.MethodPost, defaultRequests, flowRequestSpec("cam1"))
	require.Equal(t, http.StatusCreated, resp.status, "body: %s", resp.body)

	select {
	case held := <-done:
		require.Len(t, held.Assignments, 1, "the target assignment should have arrived")
		assert.Equal(t, api.RoleTarget, held.Assignments[0].Role)
		assert.Greater(t, held.Revision, cursor)
	case <-time.After(5 * time.Second):
		t.Fatal("the long poll did not release when the assignment set changed")
	}
}

// A poll that times out returns the current set rather than hanging or erroring, so an agent
// behind a proxy that buffers degrades to plain polling (§9.2).
func TestLongPollExpiryReturnsTheCurrentSet(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()
	h.reconciling()

	set, _ := h.assignments("edge-01", 0, 0)

	start := time.Now()
	again, resp := h.assignments("edge-01", set.Revision, 250*time.Millisecond)
	require.Equal(t, http.StatusOK, resp.status)
	assert.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)
	assert.Equal(t, set.Assignments, again.Assignments)
}

// Behind a plain load balancer, consecutive polls land on different replicas. A replica whose
// view is behind the cursor the agent already holds must not answer from it, or the agent
// oscillates between two assignment versions and restarts workers on every swing (plan §4.5).
func TestAReplicaNeverServesBelowTheClientsCursor(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()
	h.reconciling()

	set, _ := h.assignments("edge-01", 0, 0)

	_, resp := h.assignments("edge-01", set.Revision+10_000, 0)
	require.Equal(t, http.StatusServiceUnavailable, resp.status)
	assert.Equal(t, api.CodeNotReady, resp.apiError(t).Code)
}

// --- establishment end to end (§5.3) --------------------------------------------------------

// The whole sequence through the API: request → target assignment → reported epoch → initiator
// assignment, with the ordering that makes it work (invariant 3).
func TestEstablishmentSequence(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()
	h.reconciling()

	resp := h.do(http.MethodPost, defaultRequests, flowRequestSpec("cam1"))
	require.Equal(t, http.StatusCreated, resp.status, "body: %s", resp.body)

	// Step 2: the destination agent is assigned a target, and the source agent nothing at all —
	// an initiator must never be assigned before an epoch exists for the session.
	var target api.Assignment
	require.Eventually(t, func() bool {
		set, _ := h.assignments("edge-01", 0, 0)
		if len(set.Assignments) != 1 {
			return false
		}
		target = set.Assignments[0]
		return true
	}, 5*time.Second, 20*time.Millisecond)

	assert.Equal(t, api.RoleTarget, target.Role)
	assert.Equal(t, api.Domain{Area: "fast", Elements: []string{"ingest"}}, target.Domain)
	assert.JSONEq(t, string(testFlowDef), string(target.FlowDef))
	assert.Equal(t, api.ProviderTCP, target.Interface.Provider)
	assert.Equal(t, "dc1", target.Fabric)

	source, _ := h.assignments("studio-a", 0, 0)
	require.Empty(t, source.Assignments, "no initiator before an epoch is reported")

	// Step 4: the destination agent reports the epoch it computed from its worker's blob.
	h.reportStatus("edge-01", "i-edge", api.SessionStatus{
		SessionID: target.SessionID, Role: api.RoleTarget, State: api.WorkerReady,
		Epoch: "epoch-a", TargetInfo: `{"id":"x"}`, Address: "10.0.0.1", Service: "24001",
	})

	// Step 5: and the source agent is assigned an initiator for exactly that epoch.
	var initiator api.Assignment
	require.Eventually(t, func() bool {
		set, _ := h.assignments("studio-a", 0, 0)
		if len(set.Assignments) != 1 {
			return false
		}
		initiator = set.Assignments[0]
		return true
	}, 5*time.Second, 20*time.Millisecond)

	assert.Equal(t, api.RoleInitiator, initiator.Role)
	assert.Equal(t, target.SessionID, initiator.SessionID)
	assert.Equal(t, "epoch-a", initiator.Epoch)
	assert.Equal(t, `{"id":"x"}`, initiator.TargetInfo)
	require.NotNil(t, initiator.Peer)
	assert.Equal(t, "24001", initiator.Peer.Service)
	assert.Equal(t, target.Interface, initiator.Interface, "one negotiated config, both ends (§10.3)")

	// And the operator's view agrees with what the agents were told.
	var paths api.PathsResponse
	h.do(http.MethodGet, api.PathPaths, nil).decode(t, &paths)
	assert.False(t, paths.Settling)
	require.Len(t, paths.Paths, 1)
	assert.Equal(t, api.StateEstablishing, paths.Paths[0].State)
	assert.Equal(t, []string{"default/cam1"}, paths.Paths[0].Requests)
	require.NotNil(t, paths.Paths[0].Session)
	assert.Equal(t, "epoch-a", paths.Paths[0].Session.Epoch)
}

// A target that restarts produces a new epoch, and the initiator's assignment follows — the
// entire convergence mechanism, with no keepalive and no teardown negotiation (§5.2).
func TestANewEpochPropagatesToTheInitiator(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()
	h.reconciling()

	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, defaultRequests, flowRequestSpec("cam1")).status)

	var sessionID string
	require.Eventually(t, func() bool {
		set, _ := h.assignments("edge-01", 0, 0)
		if len(set.Assignments) == 0 {
			return false
		}
		sessionID = set.Assignments[0].SessionID
		return true
	}, 5*time.Second, 20*time.Millisecond)

	report := func(epoch string) {
		h.reportStatus("edge-01", "i-edge", api.SessionStatus{
			SessionID: sessionID, Role: api.RoleTarget, State: api.WorkerReady,
			Epoch: epoch, TargetInfo: `{"id":"x"}`, Address: "10.0.0.1", Service: "24001",
		})
	}
	epochOf := func() string {
		set, _ := h.assignments("studio-a", 0, 0)
		if len(set.Assignments) == 0 {
			return ""
		}
		return set.Assignments[0].Epoch
	}

	report("epoch-a")
	require.Eventually(t, func() bool { return epochOf() == "epoch-a" }, 5*time.Second, 20*time.Millisecond)

	// The degenerate case the incarnation nonce exists for: the same target_info, a new epoch.
	// The initiator must still be told to reconnect (§5.2).
	report("epoch-b")
	require.Eventually(t, func() bool { return epochOf() == "epoch-b" }, 5*time.Second, 20*time.Millisecond)
}

// --- user API (§9.1) ------------------------------------------------------------------------

func TestRequestLifecycle(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	resp := h.do(http.MethodPost, defaultRequests, flowRequestSpec("cam1"))
	require.Equal(t, http.StatusCreated, resp.status, "body: %s", resp.body)

	var created api.Request
	resp.decode(t, &created)
	assert.Equal(t, "default/cam1", created.ID)
	require.Len(t, created.Status.Paths, 1)

	// The name is an idempotency key: POSTing it again returns the existing request rather than
	// creating a second one. The Kubernetes adapter re-reconciles on every resync, and anything
	// hand-rolling a POST has the same problem on retry.
	resp = h.do(http.MethodPost, defaultRequests, flowRequestSpec("cam1"))
	require.Equal(t, http.StatusOK, resp.status)
	var again api.Request
	resp.decode(t, &again)
	assert.Equal(t, created.CreatedAt, again.CreatedAt)

	var list api.RequestList
	h.do(http.MethodGet, api.PathRequests, nil).decode(t, &list)
	assert.Len(t, list.Requests, 1)

	resp = h.do(http.MethodGet, api.RequestPath(rid("cam1")), nil)
	require.Equal(t, http.StatusOK, resp.status)

	assert.Equal(t, http.StatusNoContent, h.do(http.MethodDelete, api.RequestPath(rid("cam1")), nil).status)
	assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, api.RequestPath(rid("cam1")), nil).status)
	assert.Equal(t, http.StatusNotFound, h.do(http.MethodDelete, api.RequestPath(rid("cam1")), nil).status)
}

// Reject at request time, not by leaving something stuck in WAITING (§7.2). The rejection runs
// the reconciler over the fleet as it *would* be, so what is refused here and what would be
// classified INVALID a moment later are the same rule.
func TestInvalidRequestsAreRefusedAtCreation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	tests := []struct {
		name   string
		mutate func(*api.RequestSpec)
		code   api.ReasonCode
	}{
		{"unknown area", func(s *api.RequestSpec) { s.Destinations[0].Domain.Area = "bulk" }, api.ReasonUnknownArea},
		{"same endpoint", func(s *api.RequestSpec) {
			s.Destinations[0] = api.Destination{Node: "studio-a", Domain: api.Domain{Area: "media", Elements: []string{"cameras"}}}
		}, api.ReasonSameEndpoint},
		{"unknown node", func(s *api.RequestSpec) { s.Destinations[0].Node = "typo" }, api.ReasonNodeNotRegistered},
		{"pin not viable", func(s *api.RequestSpec) { s.Provider = api.ProviderPin{api.ProviderEFA} }, api.ReasonPinNotViable},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := flowRequestSpec("bad-" + strconv.Itoa(i))
			tc.mutate(&spec)

			resp := h.do(http.MethodPost, defaultRequests, spec)
			require.Equal(t, http.StatusBadRequest, resp.status, "body: %s", resp.body)

			failure := resp.apiError(t)
			assert.Equal(t, api.CodeInvalidRequest, failure.Code)
			assert.Equal(t, string(tc.code), failure.Details["reason_code"])
			assert.NotEmpty(t, failure.Message)

			// Nothing was stored: a refused request must not come back in a listing.
			assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, api.RequestPath(spec.RequestID()), nil).status)
		})
	}

	// A destination that is not a domain *name* at all is refused before any of the above runs —
	// structurally, by the request body's own validation, with no reference to the fleet (§10.6).
	// It carries no reason code because it never reaches the fleet-aware validator, and that is
	// the right layering: a path where a name belongs is a malformed request, not a request the
	// fleet cannot satisfy.
	for _, domain := range []string{"/dev/shm/anything", "../etc", "a/b"} {
		t.Run("path as destination domain "+domain, func(t *testing.T) {
			spec := flowRequestSpec("path-" + strings.NewReplacer("/", "-", ".", "").Replace(domain))
			spec.Destinations[0].Domain.Elements = []string{domain}

			resp := h.do(http.MethodPost, defaultRequests, spec)
			require.Equal(t, http.StatusBadRequest, resp.status, "body: %s", resp.body)
			assert.Equal(t, api.CodeInvalidRequest, resp.apiError(t).Code)
			assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, api.RequestPath(spec.RequestID()), nil).status)
		})
	}
}

// A request that is valid but not yet satisfiable is accepted and waits. That split — INVALID
// versus WAITING — is the whole point of validating at request time (§7.2).
func TestAValidButUnsatisfiableRequestIsAccepted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	spec := flowRequestSpec("future")
	spec.Sources[0].Select = api.Selector{Flow: "not-yet-published"}

	resp := h.do(http.MethodPost, defaultRequests, spec)
	require.Equal(t, http.StatusCreated, resp.status, "body: %s", resp.body)

	var created api.Request
	resp.decode(t, &created)
	assert.Equal(t, api.StateWaiting, created.Status.State)
	assert.Equal(t, api.ReasonFlowNotFound, created.Status.ReasonCode)
}

// --- apply semantics (M8d, M8f) -----------------------------------------------------------

// revision reads the store-wide revision, which is what "nothing was written" means: every
// successful Put advances it, including one writing a byte-identical value.
func (h *harness) revision() int64 {
	h.t.Helper()
	_, rev, err := h.store.List(h.t.Context(), "")
	require.NoError(h.t, err)
	return rev
}

// **Invariant 13.** A controller re-reconciling on every resync is exactly what the idempotency
// key exists to support, so an unchanged apply must cost nothing. Without this the store churns
// forever and every pass triggers a reconcile — the naive implementation passes every other test
// in this file.
func TestReApplyingAnUnchangedRequestWritesNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	spec := flowRequestSpec("cam1")
	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, defaultRequests, spec).status)

	before := h.revision()

	// Twice, because a bug that writes once and then settles would still pass a single re-apply.
	for range 2 {
		resp := h.do(http.MethodPost, defaultRequests, spec)
		require.Equal(t, http.StatusOK, resp.status, "an existing request updates rather than creates")

		// The outcome cannot be read off the status code — an unchanged apply and a real update
		// are both 200 — nor off the body, which echoes the spec that was sent either way. It has
		// to come from the server, or `apply` reports "unchanged" for something it just changed.
		assert.Equal(t, api.OutcomeUnchanged, resp.header.Get(api.HeaderOutcome))

		var got api.Request
		resp.decode(t, &got)
		assert.Equal(t, "default/cam1", got.ID)
	}
	assert.Equal(t, before, h.revision(), "an unchanged apply must not advance the store revision")

	// A *changed* spec still writes, or apply would be unable to update anything.
	spec.Labels = map[string]string{"show": "nab"}
	changed := h.do(http.MethodPost, defaultRequests, spec)
	require.Equal(t, http.StatusOK, changed.status)
	assert.Equal(t, api.OutcomeUpdated, changed.header.Get(api.HeaderOutcome))
	assert.Greater(t, h.revision(), before)
}

// A dry run runs the whole accept path — decode, structural validation, fleet-aware validation,
// reconciliation — and writes nothing. That is what lets `apply --dry-run` report the real
// outcome rather than a spec diff.
func TestDryRunDecidesWithoutWriting(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	before := h.revision()
	dryRun := defaultRequests + "?dry_run=true"

	// A request that would be accepted: reported as it would be, 201 for "would create".
	resp := h.do(http.MethodPost, dryRun, flowRequestSpec("cam1"))
	require.Equal(t, http.StatusCreated, resp.status, "body: %s", resp.body)

	var planned api.Request
	resp.decode(t, &planned)
	assert.Equal(t, api.StateEstablishing, planned.Status.State)
	require.Len(t, planned.Status.Paths, 1)

	// A request that would be refused is refused, with the same reason a real POST would give.
	bad := flowRequestSpec("bad")
	bad.Destinations[0].Domain.Area = "bulk"
	refused := h.do(http.MethodPost, dryRun, bad)
	require.Equal(t, http.StatusBadRequest, refused.status)
	assert.Equal(t, string(api.ReasonUnknownArea), refused.apiError(t).Details["reason_code"])

	assert.Equal(t, before, h.revision(), "a dry run must not write")
	assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, api.RequestPath(rid("cam1")), nil).status,
		"and must not create the request it planned")

	// 200 rather than 201 once the request exists, so a client learns created-vs-updated from
	// the dry run without a second round trip.
	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, defaultRequests, flowRequestSpec("cam1")).status)
	assert.Equal(t, http.StatusOK, h.do(http.MethodPost, dryRun, flowRequestSpec("cam1")).status)

	// An unparseable value is an error, not a silent false: ?dry_run=yes must never write.
	assert.Equal(t, http.StatusBadRequest,
		h.do(http.MethodPost, defaultRequests+"?dry_run=yes", flowRequestSpec("other")).status)
}

// A label the metrics path would silently drop must be an error the caller sees (M6b, M8e).
// Under prune a dropped label silently changes what can cancel the request, which is a worse
// failure than a missing series.
func TestLabelValidation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	for name, labels := range map[string]map[string]string{
		"reserved by this project": {"domain": "cameras"},
		"reserved by prometheus":   {"__name__": "x"},
		"quantile":                 {"quantile": "0.5"},
		"not a label name":         {"show-name": "nab"},
		"overlong value":           {"show": strings.Repeat("x", 300)},

		// `namespace` is an ordinary user label again (§9.3) and is refused for the ordinary
		// reason: it collides with a metric dimension this project sets itself (§12).
		"namespace": {"namespace": "nab"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := flowRequestSpec("labelled-" + strings.ReplaceAll(name, " ", "-"))
			spec.Labels = labels

			resp := h.do(http.MethodPost, defaultRequests, spec)
			require.Equal(t, http.StatusBadRequest, resp.status, "body: %s", resp.body)
			assert.Equal(t, api.CodeInvalidRequest, resp.apiError(t).Code)
		})
	}

	ok := flowRequestSpec("labelled-fine")
	ok.Labels = map[string]string{"show": "nab", "tier_1": "yes"}
	assert.Equal(t, http.StatusCreated, h.do(http.MethodPost, defaultRequests, ok).status)
}

// **Overlap is permitted by default and refuses no POST.** The default namespace is `shared`,
// so two requests expanding onto one path share it — one path, one session, one worker pair,
// which is §9.1's refcounting working as designed (§9.3).
//
// *This inverts TestNamespaceOverlapIsRefusedAtPost*, which tested the position §9.3 supersedes.
// The change is in the permissive direction: requests the server used to refuse are accepted.
func TestOverlapIsAcceptedInASharedNamespace(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, defaultRequests, flowRequestSpec("cam1")).status)

	resp := h.do(http.MethodPost, defaultRequests, flowRequestSpec("cam1-again"))
	require.Equal(t, http.StatusCreated, resp.status, "body: %s", resp.body)

	var paths api.PathsResponse
	h.do(http.MethodGet, api.PathPaths, nil).decode(t, &paths)
	require.Len(t, paths.Paths, 1, "one edge")
	assert.Equal(t, []string{"default/cam1", "default/cam1-again"}, paths.Paths[0].Requests)
}

// Inside an `exclusive` namespace the overlap is INVALID — but it is **not** refused at the POST.
// Validation is per path (§7.2), and an overlap depends on another request's expansion, which
// this request's author did not write and cannot enumerate.
func TestOverlapInAnExclusiveNamespaceIsReportedNotRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	require.Equal(t, http.StatusCreated,
		h.do(http.MethodPost, api.PathNamespaces, api.Namespace{Name: "nab", Paths: api.PathsExclusive}).status)

	nab := api.NamespaceRequestsPath("nab")
	first := flowRequestSpec("cam1")
	first.Namespace = "nab"
	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, nab, first).status)

	second := flowRequestSpec("cam1-again")
	second.Namespace = "nab"
	resp := h.do(http.MethodPost, nab, second)
	require.Equal(t, http.StatusCreated, resp.status, "body: %s", resp.body)

	// Accepted, stored, and reporting the collision rather than being turned away at the door.
	var created api.Request
	resp.decode(t, &created)
	assert.Equal(t, api.StateInvalid, created.Status.State)
	assert.Equal(t, api.ReasonNamespaceOverlap, created.Status.ReasonCode)
	assert.Contains(t, created.Status.Reason, `request "cam1" already replicates`)
	assert.Contains(t, created.Status.Reason, `namespace "nab"`)

	// The winner's path carries on, held by it alone.
	var paths api.PathsResponse
	h.do(http.MethodGet, api.PathPaths, nil).decode(t, &paths)
	require.Len(t, paths.Paths, 1)
	assert.Equal(t, []string{"nab/cam1"}, paths.Paths[0].Requests)
}

// Two requests of one name in two namespaces coexist. That is the whole point of scoping names
// to the namespace rather than fleet-wide (§9.3).
func TestOneNameInTwoNamespacesCoexists(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, defaultRequests, flowRequestSpec("cam1")).status)

	other := flowRequestSpec("cam1")
	other.Namespace = "archive"
	require.Equal(t, http.StatusCreated,
		h.do(http.MethodPost, api.NamespaceRequestsPath("archive"), other).status)

	var list api.RequestList
	h.do(http.MethodGet, api.PathRequests, nil).decode(t, &list)
	require.Len(t, list.Requests, 2)
	assert.Equal(t, "archive/cam1", list.Requests[0].ID)
	assert.Equal(t, "default/cam1", list.Requests[1].ID)

	// And the fleet-wide list narrows to one partition on request.
	h.do(http.MethodGet, api.PathRequests+"?namespace=archive", nil).decode(t, &list)
	require.Len(t, list.Requests, 1)
	assert.Equal(t, "archive/cam1", list.Requests[0].ID)

	// The namespaced collection returns the same set.
	h.do(http.MethodGet, api.NamespaceRequestsPath("archive"), nil).decode(t, &list)
	require.Len(t, list.Requests, 1)

	// Deleting one leaves the other alone.
	assert.Equal(t, http.StatusNoContent,
		h.do(http.MethodDelete, api.RequestPath(api.RequestID{Namespace: "archive", Name: "cam1"}), nil).status)
	assert.Equal(t, http.StatusOK, h.do(http.MethodGet, api.RequestPath(rid("cam1")), nil).status)
}

// The URL is authoritative and the body may agree with it or say nothing. Disagreement is refused
// rather than resolved: there is no defensible winner, and silently preferring either would put
// the request in a namespace the caller appears to contradict.
func TestTheBodyMayNotContradictTheURLNamespace(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	spec := flowRequestSpec("cam1")
	spec.Namespace = "archive"
	resp := h.do(http.MethodPost, api.NamespaceRequestsPath("nab"), spec)
	require.Equal(t, http.StatusBadRequest, resp.status)
	assert.Contains(t, resp.apiError(t).Message, "namespace")
}

func TestRequestNameValidation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	for _, name := range []string{"", "with space", "with/slash", ".leading-dot", "with\nnewline"} {
		spec := flowRequestSpec(name)
		resp := h.do(http.MethodPost, defaultRequests, spec)
		assert.Equal(t, http.StatusBadRequest, resp.status, "name %q should be refused", name)
	}

	spec := flowRequestSpec("studio-a:cam1_to.edge-01")
	assert.Equal(t, http.StatusCreated, h.do(http.MethodPost, defaultRequests, spec).status)
}

func TestNodeAndFlowViews(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()
	h.reportInventory("studio-a", "i-studio", "media/cameras",
		api.FlowInventory{ID: "flow-1", Definition: testFlowDef, Producing: true,
			GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "video"}},
		api.FlowInventory{ID: "flow-2", Definition: testFlowDef, Producing: false,
			GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "audio"}},
	)

	var nodes api.NodeList
	h.do(http.MethodGet, api.PathNodes, nil).decode(t, &nodes)
	require.Len(t, nodes.Nodes, 2)
	assert.True(t, nodes.Nodes[0].Live)

	// **Observed domains**, not registration data: there is no configured mapping to report
	// (§6), so this answers from inventory.
	var domains api.DomainList
	h.do(http.MethodGet, api.NodeDomainsPath("studio-a"), nil).decode(t, &domains)
	require.Len(t, domains.Domains, 1)
	assert.Equal(t, "media/cameras", domains.Domains[0].Domain.String())
	assert.Len(t, domains.Domains[0].Flows, 2)

	// A registered node that has reported nothing has no domains, which is a different answer
	// from a node that does not exist.
	var none api.DomainList
	h.do(http.MethodGet, api.NodeDomainsPath("edge-02"), nil).decode(t, &none)
	assert.Empty(t, none.Domains)

	assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, api.NodeDomainsPath("nope"), nil).status)

	var flows api.FlowList
	h.do(http.MethodGet, api.PathFlows, nil).decode(t, &flows)
	assert.Len(t, flows.Flows, 2)

	h.do(http.MethodGet, api.PathFlows+"?type=audio", nil).decode(t, &flows)
	require.Len(t, flows.Flows, 1)
	assert.Equal(t, "flow-2", flows.Flows[0].ID)

	h.do(http.MethodGet, api.PathFlows+"?node=edge-01", nil).decode(t, &flows)
	assert.Empty(t, flows.Flows)
}

// GET /v1/paths says `settling` explicitly rather than reporting everything as WAITING, which
// would look like a fleet-wide outage to whatever is scraping it (§7.3).
func TestPathsReportsSettling(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	var before api.PathsResponse
	h.do(http.MethodGet, api.PathPaths, nil).decode(t, &before)
	assert.True(t, before.Settling)

	h.reconciling()

	var after api.PathsResponse
	h.do(http.MethodGet, api.PathPaths, nil).decode(t, &after)
	assert.False(t, after.Settling)
}

// --- auth and health --------------------------------------------------------------------

func TestBearerToken(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(c *Config) { c.Token = "s3cret" })

	unauthenticated := func(method, path string) response {
		req, err := http.NewRequestWithContext(t.Context(), method, h.http.URL+path, nil)
		require.NoError(t, err)
		resp, err := h.http.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return response{status: resp.StatusCode, body: body}
	}

	// Both surfaces are guarded.
	for _, path := range []string{api.PathRequests, api.AssignmentsPath("edge-01")} {
		resp := unauthenticated(http.MethodGet, path)
		require.Equal(t, http.StatusUnauthorized, resp.status, "path %s", path)
		assert.Equal(t, api.CodeUnauthorized, resp.apiError(t).Code)
	}

	// Health is not: a load balancer and a kubelet are not going to carry a token, and it
	// discloses nothing.
	assert.Equal(t, http.StatusOK, unauthenticated(http.MethodGet, "/healthz").status)

	// And with the token, everything works.
	assert.Equal(t, http.StatusOK, h.do(http.MethodGet, api.PathRequests, nil).status)
}

func TestHealthAndReadiness(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	assert.Equal(t, http.StatusOK, h.do(http.MethodGet, "/healthz", nil).status)

	// Readiness is a property of the fleet's control plane, not of this process: a replica that
	// would answer "I don't know yet" is not ready, even though it is perfectly alive.
	resp := h.do(http.MethodGet, "/readyz", nil)
	require.Equal(t, http.StatusServiceUnavailable, resp.status)
	assert.Equal(t, api.CodeNotReady, resp.apiError(t).Code)

	h.reconciling()
	assert.Equal(t, http.StatusOK, h.do(http.MethodGet, "/readyz", nil).status)
}

// Nothing this server returns is cacheable, and the assignment poll is why: it is a GET with a
// cursor in its query string, so an intermediary with a default cache policy — CloudFront's, for
// one — will happily serve an agent a stale assignment set. Fail-static does not cover that: §4.2
// protects an agent from *no answer*, not from a successfully-retrieved wrong one, so a stale
// epoch reaches a worker and becomes §5.2's silent failure.
func TestResponsesAreUncacheable(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	for _, path := range []string{
		api.AssignmentsPath("edge-01"),
		api.PathRequests,
		api.PathFlows,
		"/healthz",
		"/readyz", // 503 here, and an error is exactly as uncacheable as a success.
	} {
		assert.Equal(t, "no-store", h.do(http.MethodGet, path, nil).header.Get("Cache-Control"), "path %s", path)
	}
}

func TestMalformedBodies(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.http.URL+api.PathRegister,
		bytes.NewReader([]byte("{not json")))
	require.NoError(t, err)
	resp, err := h.http.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// A selector with two kinds set must fail at parse: ignoring the unknown one would silently
	// *widen* the selection, and for something that moves uncompressed video between hosts that
	// is the wrong direction to fail in (§9.1).
	body := `{"name":"x","sources":[{"node":"a","domain":{"name":{"area":"m","elements":["d"]}},` +
		`"select":{"flow":"f","group_hint":{"name":"n"}}}],` +
		`"destinations":[{"node":"b","domain":{"area":"fast","elements":["e"]}}]}`
	req, err = http.NewRequestWithContext(t.Context(), http.MethodPost, h.http.URL+defaultRequests,
		bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	resp, err = h.http.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// Every stored request says which namespace it is in. A request that named none is written into
// the default one rather than left implying it, so that the field means one thing whichever
// request is holding it (§9.3).
func TestRequestsWithoutANamespaceGetTheDefault(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	spec := flowRequestSpec("cam1")
	require.Empty(t, spec.Namespace, "the fixture sends none")

	var created api.Request
	resp := h.do(http.MethodPost, defaultRequests, spec)
	require.Equal(t, http.StatusCreated, resp.status, "body: %s", resp.body)
	resp.decode(t, &created)
	assert.Equal(t, api.DefaultNamespace, created.Namespace,
		"the response already reflects what was stored")
	assert.Equal(t, "default/cam1", created.ID)

	var got api.Request
	h.do(http.MethodGet, api.RequestPath(rid("cam1")), nil).decode(t, &got)
	assert.Equal(t, api.DefaultNamespace, got.Namespace)

	// **And re-applying the same namespace-less body is still unchanged.** Normalising after the
	// comparison rather than before would make every apply of a manifest that names no namespace
	// look like an edit, and write on every pass (invariant 13, §8.3).
	before := h.revision()
	again := h.do(http.MethodPost, defaultRequests, flowRequestSpec("cam1"))
	assert.Equal(t, http.StatusOK, again.status)
	assert.Equal(t, api.OutcomeUnchanged, again.header.Get(api.HeaderOutcome))
	assert.Equal(t, before, h.revision(), "nothing was written")

	// An explicit `namespace: default` is the same request, not a different one.
	explicit := flowRequestSpec("cam1")
	explicit.Namespace = api.DefaultNamespace
	assert.Equal(t, api.OutcomeUnchanged,
		h.do(http.MethodPost, defaultRequests, explicit).header.Get(api.HeaderOutcome))
}

// --- namespaces as objects (§9.3) -----------------------------------------------------------

// **Auto-create on first reference, and it is a real write.** Deriving a missing namespace lazily
// at read time would be cheaper by one write and would quietly give back the property the object
// exists for: a `GET /v1/namespaces` that invents rows is the label spelling again, wearing a
// record's clothes.
func TestANamespaceIsAutoCreatedByFirstReference(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	spec := flowRequestSpec("cam1")
	spec.Namespace = "nab"
	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, api.NamespaceRequestsPath("nab"), spec).status)

	var list api.NamespaceList
	h.do(http.MethodGet, api.PathNamespaces, nil).decode(t, &list)
	require.Len(t, list.Namespaces, 1)
	assert.Equal(t, "nab", list.Namespaces[0].Name)
	assert.Equal(t, api.PathsShared, list.Namespaces[0].Paths, "created with defaults")
	assert.Equal(t, 1, list.Namespaces[0].Requests)

	// It is a stored record, readable on its own.
	var info api.NamespaceInfo
	resp := h.do(http.MethodGet, api.NamespacePath("nab"), nil)
	require.Equal(t, http.StatusOK, resp.status)
	resp.decode(t, &info)
	assert.Equal(t, api.PathsShared, info.Paths)
}

// **Create-if-absent, never write-if-present.** An unconditional write would bump the namespace
// key's revision on every request write and wake every watcher in the fleet, which is the churn
// §8.3 is sized against.
func TestTheAutoCreateDoesNotRewriteAnExistingNamespace(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	require.Equal(t, http.StatusCreated,
		h.do(http.MethodPost, api.PathNamespaces, api.Namespace{Name: "nab", Paths: api.PathsExclusive}).status)

	before := h.revision()

	first := flowRequestSpec("cam1")
	first.Namespace = "nab"
	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, api.NamespaceRequestsPath("nab"), first).status)
	afterFirst := h.revision()
	assert.Greater(t, afterFirst, before, "the request itself was written")

	// A second request in the same namespace writes the request and nothing else.
	second := flowRequestSpec("cam2")
	second.Namespace = "nab"
	second.Sources[0].Select = api.Selector{Flow: "flow-2"}
	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, api.NamespaceRequestsPath("nab"), second).status)

	var info api.NamespaceInfo
	h.do(http.MethodGet, api.NamespacePath("nab"), nil).decode(t, &info)
	assert.Equal(t, api.PathsExclusive, info.Paths, "the explicit declaration was not overwritten")
	assert.Equal(t, 2, info.Requests)
}

// A dry run writes nothing at all — including the namespace. The create is on the write path
// rather than in validation, which is worth pinning because a namespace is exactly the kind of
// side effect that gets attached to admission by accident (§9.3).
func TestADryRunCreatesNoNamespace(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	spec := flowRequestSpec("cam1")
	spec.Namespace = "nab"
	resp := h.do(http.MethodPost, api.NamespaceRequestsPath("nab")+"?dry_run=true", spec)
	require.Equal(t, http.StatusCreated, resp.status, "body: %s", resp.body)

	var list api.NamespaceList
	h.do(http.MethodGet, api.PathNamespaces, nil).decode(t, &list)
	assert.Empty(t, list.Namespaces)
}

// Explicit create-or-update, keyed on the name, with the same no-write-if-unchanged discipline
// the request POST follows — this key is read by every reconcile, so a needless write wakes the
// whole fleet.
func TestNamespaceCreateIsIdempotent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	spec := api.Namespace{Name: "nab", Paths: api.PathsExclusive, Description: "the show floor"}
	created := h.do(http.MethodPost, api.PathNamespaces, spec)
	require.Equal(t, http.StatusCreated, created.status, "body: %s", created.body)
	assert.Equal(t, api.OutcomeCreated, created.header.Get(api.HeaderOutcome))

	before := h.revision()
	again := h.do(http.MethodPost, api.PathNamespaces, spec)
	assert.Equal(t, http.StatusOK, again.status)
	assert.Equal(t, api.OutcomeUnchanged, again.header.Get(api.HeaderOutcome))
	assert.Equal(t, before, h.revision(), "nothing was written")

	// An unset policy and an explicit `shared` are the same intent, so a document that spells it
	// out does not look like a change.
	shared := h.do(http.MethodPost, api.PathNamespaces, api.Namespace{Name: "quiet"})
	require.Equal(t, http.StatusCreated, shared.status)
	assert.Equal(t, api.OutcomeUnchanged,
		h.do(http.MethodPost, api.PathNamespaces, api.Namespace{Name: "quiet", Paths: api.PathsShared}).header.Get(api.HeaderOutcome))

	changed := h.do(http.MethodPost, api.PathNamespaces, api.Namespace{Name: "nab", Paths: api.PathsShared})
	assert.Equal(t, api.OutcomeUpdated, changed.header.Get(api.HeaderOutcome))
}

func TestNamespaceValidation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	for _, spec := range []api.Namespace{
		{Name: ""},
		{Name: "shows/nab"},
		{Name: "nab 2026"},
		{Name: "nab", Paths: "whatever"},
	} {
		resp := h.do(http.MethodPost, api.PathNamespaces, spec)
		assert.Equal(t, http.StatusBadRequest, resp.status, "namespace %+v should be refused", spec)
	}

	// And on the request route, where the namespace is a URL segment.
	assert.Equal(t, http.StatusBadRequest,
		h.do(http.MethodPost, api.PathNamespaces+"/nab%202026/requests", flowRequestSpec("cam1")).status)
}

// **Deletion is refused while any request references it, with the count in the message.** The
// system never cancels intent on the user's behalf (§11), and a cascading delete here is a
// cascading teardown of live media.
func TestANamespaceCannotBeDeletedWhileReferenced(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	spec := flowRequestSpec("cam1")
	spec.Namespace = "nab"
	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, api.NamespaceRequestsPath("nab"), spec).status)

	resp := h.do(http.MethodDelete, api.NamespacePath("nab"), nil)
	require.Equal(t, http.StatusConflict, resp.status)
	assert.Contains(t, resp.apiError(t).Message, "1 request")

	// Cancel the request and the namespace goes.
	require.Equal(t, http.StatusNoContent,
		h.do(http.MethodDelete, api.RequestPath(api.RequestID{Namespace: "nab", Name: "cam1"}), nil).status)
	assert.Equal(t, http.StatusNoContent, h.do(http.MethodDelete, api.NamespacePath("nab"), nil).status)
	assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, api.NamespacePath("nab"), nil).status)
}

// The default namespace is where every request that named none lives, so removing it would make
// the catch-all a dangling reference.
func TestTheDefaultNamespaceCannotBeDeleted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	resp := h.do(http.MethodDelete, api.NamespacePath(api.DefaultNamespace), nil)
	assert.Equal(t, http.StatusConflict, resp.status)
	assert.Contains(t, resp.apiError(t).Message, "cannot be deleted")

	// And it answers a GET whether or not a record was ever written for it.
	assert.Equal(t, http.StatusOK, h.do(http.MethodGet, api.NamespacePath(api.DefaultNamespace), nil).status)
}

// Cancelling the last request in a namespace leaves the namespace behind. Never auto-delete
// (§9.3): removing one on the way past would silently discard the operator's declaration.
func TestDeletingTheLastRequestKeepsTheNamespace(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	require.Equal(t, http.StatusCreated,
		h.do(http.MethodPost, api.PathNamespaces, api.Namespace{Name: "nab", Paths: api.PathsExclusive}).status)

	spec := flowRequestSpec("cam1")
	spec.Namespace = "nab"
	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, api.NamespaceRequestsPath("nab"), spec).status)
	require.Equal(t, http.StatusNoContent,
		h.do(http.MethodDelete, api.RequestPath(api.RequestID{Namespace: "nab", Name: "cam1"}), nil).status)

	var info api.NamespaceInfo
	resp := h.do(http.MethodGet, api.NamespacePath("nab"), nil)
	require.Equal(t, http.StatusOK, resp.status)
	resp.decode(t, &info)
	assert.Equal(t, api.PathsExclusive, info.Paths)
	assert.Zero(t, info.Requests)
}

// named builds a `name` domain selector from the `<area>/<elements>` spelling, splitting it the
// way a manifest does. Tests are allowed the convenience the rest of the tree is not (§10.6).
func named(domain string) api.DomainSelector {
	segments := strings.Split(domain, "/")
	return api.SelectDomain(api.Domain{Area: segments[0], Elements: segments[1:]})
}

// --- domain labels (§9.1, §10.7) ---------------------------------------------------------------

// label posts one write and returns the resulting record.
func (h *harness) label(node string, body api.DomainLabelWrite, want int) api.DomainLabelResult {
	h.t.Helper()

	resp := h.do(http.MethodPost, api.NodeDomainsPath(node), body)
	require.Equal(h.t, want, resp.status, "body: %s", resp.body)

	var out api.DomainLabelResult
	if want < 300 {
		resp.decode(h.t, &out)
	}
	return out
}

func labelApply(domain string, labels map[string]string) api.DomainLabelWrite {
	segments := strings.Split(domain, "/")
	return api.DomainLabelWrite{
		Domain: api.Domain{Area: segments[0], Elements: segments[1:]},
		Apply:  labels,
	}
}

func labelPatch(domain string, set map[string]string, remove ...string) api.DomainLabelWrite {
	segments := strings.Split(domain, "/")
	return api.DomainLabelWrite{
		Domain: api.Domain{Area: segments[0], Elements: segments[1:]},
		Patch:  &api.DomainLabelPatch{Set: set, Remove: remove},
	}
}

// **The ownership rule, and it is three cases rather than one** (§9.1, §17).
//
// A whole-set replace passes the second and third of these without having the property. Only the
// first distinguishes them, and it is the one an implementer will not think to write.
func TestAnApplyOwnsOnlyTheKeysItDeclares(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	// An apply declares two keys.
	h.label("studio-a", labelApply("media/cameras", map[string]string{"role": "cameras", "site": "a"}), http.StatusCreated)

	// An operator adds one interactively. `label` sends a **patch**, which merges against nothing
	// and does not change what a future apply believes it owns.
	patched := h.label("studio-a", labelPatch("media/cameras", map[string]string{"tier": "1"}), http.StatusOK)
	assert.Equal(t, map[string]string{"role": "cameras", "site": "a", "tier": "1"}, patched.Labels)
	assert.Equal(t, []string{"role", "site"}, patched.Declared, "a patch does not claim ownership")

	// **(1) An apply leaves a key it never declared.** This is the property the three-way merge
	// exists for: `label` and `apply` do not fight, because they own different keys — which is the
	// arrangement this project actually has, a fleet whose requests are applied from git by
	// somebody else.
	//
	// **(2) An apply removes a key it declared on an earlier pass and no longer does.** The file
	// stays declarative over its own keys.
	reapplied := h.label("studio-a", labelApply("media/cameras", map[string]string{"role": "cameras"}), http.StatusOK)
	assert.Equal(t, map[string]string{"role": "cameras", "tier": "1"}, reapplied.Labels,
		"site was declared and dropped; tier was never declared")
	assert.Equal(t, []string{"role"}, reapplied.Declared)

	// **(3) An apply leaves a domain the file does not name untouched at all.** Scoping is a
	// different rule that reads similarly, and it is worth pinning separately.
	h.label("studio-a", labelApply("media/audio", map[string]string{"role": "audio"}), http.StatusCreated)
	h.label("studio-a", labelApply("media/cameras", map[string]string{"role": "cameras"}), http.StatusOK)

	var list api.DomainList
	h.do(http.MethodGet, api.NodeDomainsPath("studio-a"), nil).decode(t, &list)

	labels := map[string]map[string]string{}
	for _, domain := range list.Domains {
		labels[domain.Domain.String()] = domain.Labels
	}
	assert.Equal(t, map[string]string{"role": "audio"}, labels["media/audio"])
	assert.Equal(t, map[string]string{"role": "cameras", "tier": "1"}, labels["media/cameras"])
}

// A label record's revision moving wakes every watcher and moves every request's expansion, so a
// controller re-applying an identical set must not cost a fleet-wide reconcile (§9.1).
func TestReApplyingUnchangedLabelsWritesNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	h.label("studio-a", labelApply("media/cameras", map[string]string{"role": "cameras"}), http.StatusCreated)

	before := h.revision()
	resp := h.do(http.MethodPost, api.NodeDomainsPath("studio-a"),
		labelApply("media/cameras", map[string]string{"role": "cameras"}))
	assert.Equal(t, api.OutcomeUnchanged, resp.header.Get(api.HeaderOutcome))
	assert.Equal(t, before, h.revision(), "nothing was written")

	// **`Declared` is part of "unchanged"**: an apply that changes only which keys it owns must
	// write, or it silently does nothing — and what it owns is what a *later* apply will remove.
	h.label("studio-a", labelPatch("media/cameras", map[string]string{"tier": "1"}), http.StatusOK)
	before = h.revision()

	claimed := h.label("studio-a", labelApply("media/cameras", map[string]string{"role": "cameras", "tier": "1"}), http.StatusOK)
	assert.Equal(t, []string{"role", "tier"}, claimed.Declared)
	assert.Greater(t, h.revision(), before, "the declared set changed, so the record changed")
}

// **An empty result deletes the record** rather than storing one with no labels (§9.1). The two
// are indistinguishable to every reader, so the empty one is a key that accumulates and is never
// collected.
func TestAnEmptyLabelSetDeletesTheRecord(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	h.label("studio-a", labelApply("media/cameras", map[string]string{"role": "cameras"}), http.StatusCreated)
	cleared := h.label("studio-a", labelApply("media/cameras", map[string]string{}), http.StatusOK)
	assert.Empty(t, cleared.Labels)

	var list api.DomainList
	h.do(http.MethodGet, api.NodeDomainsPath("studio-a"), nil).decode(t, &list)
	for _, domain := range list.Domains {
		assert.Empty(t, domain.Labels, "no record survives for %s", domain.Domain)
	}

	// **But an imperative key keeps it alive.** The condition is empty labels *and* an empty
	// declared set: an apply that declares nothing while a `label` edit remains must keep both.
	h.label("studio-a", labelPatch("media/cameras", map[string]string{"tier": "1"}), http.StatusCreated)
	kept := h.label("studio-a", labelApply("media/cameras", map[string]string{}), http.StatusOK)
	assert.Equal(t, map[string]string{"tier": "1"}, kept.Labels)
}

// **A label on an unobserved domain — or an unregistered node — is accepted and inert** (§10.7),
// and the read side is what keeps it from being merely lost: a typo'd node name in a manifest is
// otherwise a write that can never be read back.
func TestALabelOnAnUnknownNodeIsAcceptedAndReadable(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	h.label("never-registered", labelApply("media/cameras", map[string]string{"role": "cameras"}), http.StatusCreated)

	var list api.DomainList
	resp := h.do(http.MethodGet, api.NodeDomainsPath("never-registered"), nil)
	require.Equal(t, http.StatusOK, resp.status, "the read must answer, or the typo is invisible")
	resp.decode(t, &list)

	require.Len(t, list.Domains, 1)
	assert.Equal(t, "media/cameras", list.Domains[0].Domain.String())
	assert.False(t, list.Domains[0].Observed, "pending, not observed")
	assert.Equal(t, map[string]string{"role": "cameras"}, list.Domains[0].Labels)

	// A node that neither exists nor has labels is still a 404: the read answers for a *record*,
	// not for any string.
	assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, api.NodeDomainsPath("nothing-at-all"), nil).status)
}

// A label rides into worker metrics, so it takes the same rule a request label does — and one
// addition: the `name` key's *value* is held to the element grammar, because it is rendered as the
// `domain_name` metric label (§10.7, §12).
func TestDomainLabelValidation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	for name, body := range map[string]api.DomainLabelWrite{
		"reserved by this project": labelApply("media/cameras", map[string]string{"domain": "x"}),
		"not a label name":         labelApply("media/cameras", map[string]string{"show-name": "nab"}),
		"name is not an element":   labelApply("media/cameras", map[string]string{"name": "a/b"}),
		"both shapes":              {Domain: api.Domain{Area: "media", Elements: []string{"cameras"}}, Apply: map[string]string{}, Patch: &api.DomainLabelPatch{Set: map[string]string{"a": "b"}}},
		"neither shape":            {Domain: api.Domain{Area: "media", Elements: []string{"cameras"}}},
		"empty patch":              {Domain: api.Domain{Area: "media", Elements: []string{"cameras"}}, Patch: &api.DomainLabelPatch{}},
		"domain names no area":     {Domain: api.Domain{Elements: []string{"cameras"}}, Apply: map[string]string{}},
	} {
		t.Run(name, func(t *testing.T) {
			resp := h.do(http.MethodPost, api.NodeDomainsPath("studio-a"), body)
			assert.Equal(t, http.StatusBadRequest, resp.status, "body: %s", resp.body)
		})
	}

	// And the shapes that are fine, including a `name` value that is a legal element.
	h.label("studio-a", labelApply("media/cameras", map[string]string{"name": "cameras", "role": "cameras"}), http.StatusCreated)
}

// A patch and a concurrent apply both land, which a read-modify-write would have lost one of
// (§9.1, §17).
func TestAPatchAndAnApplyBothLand(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	// Two writers, neither of which read before writing. The patch's key survives the apply and
	// the apply's keys survive the patch, because each merges against a different thing.
	h.label("studio-a", labelPatch("media/cameras", map[string]string{"tier": "1"}), http.StatusCreated)
	h.label("studio-a", labelApply("media/cameras", map[string]string{"role": "cameras"}), http.StatusOK)

	var list api.DomainList
	h.do(http.MethodGet, api.NodeDomainsPath("studio-a"), nil).decode(t, &list)
	for _, domain := range list.Domains {
		if domain.Domain.String() == "media/cameras" {
			assert.Equal(t, map[string]string{"tier": "1", "role": "cameras"}, domain.Labels)
		}
	}
}

// **`?dry_run=true` writes nothing while returning the paths a label removal would stop** (§9.1).
//
// A label joins or removes a domain from a request's expansion, so it starts and stops media
// exactly as a request does — one level of indirection away, which makes it *easier* to do by
// accident rather than harder.
func TestALabelDryRunWritesNothingAndReportsWhatItWouldStop(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()
	h.reportInventory("studio-a", "i-studio", "media/cameras",
		api.FlowInventory{ID: "flow-1", Definition: testFlowDef, Producing: true})

	h.label("studio-a", labelApply("media/cameras", map[string]string{"role": "cameras"}), http.StatusCreated)

	spec := flowRequestSpec("wide")
	spec.Sources[0].Domain = api.SelectLabels(map[string]string{"role": "cameras"})
	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, defaultRequests, spec).status)

	var paths api.PathsResponse
	h.do(http.MethodGet, api.PathPaths, nil).decode(t, &paths)
	require.Len(t, paths.Paths, 1, "the label is what put this path there")

	// Removing the label would stop it. The dry run says so and writes nothing.
	before := h.revision()
	resp := h.do(http.MethodPost, api.NodeDomainsPath("studio-a")+"?dry_run=true",
		labelPatch("media/cameras", nil, "role"))
	require.Equal(t, http.StatusOK, resp.status, "body: %s", resp.body)

	var planned api.DomainLabelResult
	resp.decode(t, &planned)
	require.Len(t, planned.Stopped, 1)
	assert.Equal(t, paths.Paths[0].ID, planned.Stopped[0].ID)
	// The requests that were feeding it, which is what `path.requests[]` already answers — so the
	// blast radius is a renderer and not a computation.
	assert.Equal(t, []string{"default/wide"}, planned.Stopped[0].Requests)
	assert.Empty(t, planned.Started)

	assert.Equal(t, before, h.revision(), "a dry run must not write")
	h.do(http.MethodGet, api.PathPaths, nil).decode(t, &paths)
	assert.Len(t, paths.Paths, 1, "and the path is still there")

	// **The real write reports the same thing.** It prints rather than prompts: the CLI is scripted
	// by the same operators who use it interactively, and a verb that blocks on a tty is a verb
	// that hangs in a pipeline.
	real := h.label("studio-a", labelPatch("media/cameras", nil, "role"), http.StatusOK)
	require.Len(t, real.Stopped, 1)
	assert.Equal(t, paths.Paths[0].ID, real.Stopped[0].ID)

	h.do(http.MethodGet, api.PathPaths, nil).decode(t, &paths)
	assert.Empty(t, paths.Paths, "and this time it really stopped")
}

// The mirror: a label that *joins* a domain to an expansion reports what it would start.
func TestALabelReportsWhatItWouldStart(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()
	h.reportInventory("studio-a", "i-studio", "media/cameras",
		api.FlowInventory{ID: "flow-1", Definition: testFlowDef, Producing: true})

	spec := flowRequestSpec("wide")
	spec.Sources[0].Domain = api.SelectLabels(map[string]string{"role": "cameras"})
	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, defaultRequests, spec).status)

	var paths api.PathsResponse
	h.do(http.MethodGet, api.PathPaths, nil).decode(t, &paths)
	require.Empty(t, paths.Paths, "nothing carries the label yet")

	planned := h.label("studio-a", labelPatch("media/cameras", map[string]string{"role": "cameras"}), http.StatusCreated)
	require.Len(t, planned.Started, 1)
	assert.Equal(t, []string{"default/wide"}, planned.Started[0].Requests)
	assert.Empty(t, planned.Stopped)
}
