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
	return response{status: resp.StatusCode, body: raw}
}

// register brings a node up the way an agent does, and returns its lease.
func (h *harness) register(node, instance string, domains ...api.DomainMapping) api.RegistrationResponse {
	h.t.Helper()

	resp := h.do(http.MethodPost, api.PathRegister, api.NodeRegistration{
		Node:     node,
		Instance: instance,
		Domains:  domains,
		Capabilities: api.Capabilities{
			Versions: api.Versions{Protocol: api.ProtocolVersion, Replicator: "test"},
			Fabrics: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.0.0.1",
				CapFlags: []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive},
			}},
		},
	})
	require.Equal(h.t, http.StatusOK, resp.status, "body: %s", resp.body)

	var out api.RegistrationResponse
	resp.decode(h.t, &out)
	return out
}

var testFlowDef = json.RawMessage(`{"id":"flow-1","format":"urn:x-nmos:format:video"}`)

func (h *harness) reportInventory(node, instance, domain string, flows ...api.FlowInventory) {
	h.t.Helper()

	resp := h.do(http.MethodPost, api.InventoryPath(node), api.InventorySnapshot{
		Node: node, Instance: instance,
		Domains: []api.DomainInventory{{Name: domain, Configured: true, Flows: flows}},
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

func flowRequestSpec(name string) api.RequestSpec {
	return api.RequestSpec{
		Name:        name,
		Source:      api.Source{Node: "studio-a", Domain: "cameras", Select: api.Selector{Flow: "flow-1"}},
		Destination: api.Destination{Node: "edge-01", Domain: "ingest"},
	}
}

// fleet registers the two ordinary nodes and reports one produced flow.
func (h *harness) fleet() {
	h.t.Helper()

	h.register("studio-a", "i-studio", api.DomainMapping{Name: "cameras", Path: "/dev/shm/a", Configured: true})
	h.register("edge-01", "i-edge", api.DomainMapping{Name: "ingest", Path: "/dev/shm/b", Configured: true})
	h.reportInventory("studio-a", "i-studio", "cameras", api.FlowInventory{
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
	resp := h.register("edge-01", "i-1", api.DomainMapping{Name: "ingest", Configured: true})

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
	second := h.register("edge-01", "i-1", api.DomainMapping{Name: "ingest", Configured: true})

	assert.NotEqual(t, first.Lease, second.Lease, "a fresh lease is granted")
	h.reportStatus("edge-01", "i-1")

	// The durable half kept its identity across the re-registration.
	var nodes api.NodeList
	h.do(http.MethodGet, api.PathNodes, nil).decode(t, &nodes)
	require.Len(t, nodes.Nodes, 1)
	assert.True(t, nodes.Nodes[0].Live)
	assert.Len(t, nodes.Nodes[0].Domains, 1)
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

	resp = h.do(http.MethodPost, api.PathRequests, flowRequestSpec("cam1"))
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

	resp := h.do(http.MethodPost, api.PathRequests, flowRequestSpec("cam1"))
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
	assert.Equal(t, "ingest", target.Domain)
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
	assert.Equal(t, []string{"cam1"}, paths.Paths[0].Requests)
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

	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, api.PathRequests, flowRequestSpec("cam1")).status)

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

	resp := h.do(http.MethodPost, api.PathRequests, flowRequestSpec("cam1"))
	require.Equal(t, http.StatusCreated, resp.status, "body: %s", resp.body)

	var created api.Request
	resp.decode(t, &created)
	assert.Equal(t, "cam1", created.ID)
	require.Len(t, created.Status.Paths, 1)

	// The name is an idempotency key: POSTing it again returns the existing request rather than
	// creating a second one. The Kubernetes adapter re-reconciles on every resync, and anything
	// hand-rolling a POST has the same problem on retry.
	resp = h.do(http.MethodPost, api.PathRequests, flowRequestSpec("cam1"))
	require.Equal(t, http.StatusOK, resp.status)
	var again api.Request
	resp.decode(t, &again)
	assert.Equal(t, created.CreatedAt, again.CreatedAt)

	var list api.RequestList
	h.do(http.MethodGet, api.PathRequests, nil).decode(t, &list)
	assert.Len(t, list.Requests, 1)

	resp = h.do(http.MethodGet, api.RequestPath("cam1"), nil)
	require.Equal(t, http.StatusOK, resp.status)

	assert.Equal(t, http.StatusNoContent, h.do(http.MethodDelete, api.RequestPath("cam1"), nil).status)
	assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, api.RequestPath("cam1"), nil).status)
	assert.Equal(t, http.StatusNotFound, h.do(http.MethodDelete, api.RequestPath("cam1"), nil).status)
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
		{"unmapped destination domain", func(s *api.RequestSpec) { s.Destination.Domain = "/dev/shm/anything" }, api.ReasonDomainNotMapped},
		{"same endpoint", func(s *api.RequestSpec) { s.Destination = api.Destination{Node: "studio-a", Domain: "cameras"} }, api.ReasonSameEndpoint},
		{"unknown node", func(s *api.RequestSpec) { s.Destination.Node = "typo" }, api.ReasonNodeNotRegistered},
		{"pin not viable", func(s *api.RequestSpec) { s.Provider = api.ProviderPin{api.ProviderEFA} }, api.ReasonPinNotViable},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := flowRequestSpec("bad-" + strconv.Itoa(i))
			tc.mutate(&spec)

			resp := h.do(http.MethodPost, api.PathRequests, spec)
			require.Equal(t, http.StatusBadRequest, resp.status, "body: %s", resp.body)

			failure := resp.apiError(t)
			assert.Equal(t, api.CodeInvalidRequest, failure.Code)
			assert.Equal(t, string(tc.code), failure.Details["reason_code"])
			assert.NotEmpty(t, failure.Message)

			// Nothing was stored: a refused request must not come back in a listing.
			assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, api.RequestPath(spec.Name), nil).status)
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
	spec.Source.Select = api.Selector{Flow: "not-yet-published"}

	resp := h.do(http.MethodPost, api.PathRequests, spec)
	require.Equal(t, http.StatusCreated, resp.status, "body: %s", resp.body)

	var created api.Request
	resp.decode(t, &created)
	assert.Equal(t, api.StateWaiting, created.Status.State)
	assert.Equal(t, api.ReasonFlowNotFound, created.Status.ReasonCode)
}

func TestRequestNameValidation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()

	for _, name := range []string{"", "with space", "with/slash", ".leading-dot", "with\nnewline"} {
		spec := flowRequestSpec(name)
		resp := h.do(http.MethodPost, api.PathRequests, spec)
		assert.Equal(t, http.StatusBadRequest, resp.status, "name %q should be refused", name)
	}

	spec := flowRequestSpec("studio-a:cam1_to.edge-01")
	assert.Equal(t, http.StatusCreated, h.do(http.MethodPost, api.PathRequests, spec).status)
}

func TestNodeAndFlowViews(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()
	h.reportInventory("studio-a", "i-studio", "cameras",
		api.FlowInventory{ID: "flow-1", Definition: testFlowDef, Producing: true,
			GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "video"}},
		api.FlowInventory{ID: "flow-2", Definition: testFlowDef, Producing: false,
			GroupHint: &api.GroupHint{Name: "Studio A:Camera 1", Type: "audio"}},
	)

	var nodes api.NodeList
	h.do(http.MethodGet, api.PathNodes, nil).decode(t, &nodes)
	require.Len(t, nodes.Nodes, 2)
	assert.True(t, nodes.Nodes[0].Live)

	var domains api.DomainList
	h.do(http.MethodGet, api.NodeDomainsPath("edge-01"), nil).decode(t, &domains)
	require.Len(t, domains.Domains, 1)
	assert.Equal(t, "/dev/shm/b", domains.Domains[0].Path)

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
	body := `{"name":"x","source":{"node":"a","domain":"d","select":{"flow":"f","group_hint":{"name":"n"}}},` +
		`"destination":{"node":"b","domain":"e"}}`
	req, err = http.NewRequestWithContext(t.Context(), http.MethodPost, h.http.URL+api.PathRequests,
		bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	resp, err = h.http.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
