package server

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// expose scrapes the server the way Prometheus would, through the real unauthenticated route.
func (h *harness) expose() string {
	h.t.Helper()

	resp := h.do(http.MethodGet, "/metrics", nil)
	require.Equal(h.t, http.StatusOK, resp.status, "body: %s", resp.body)
	return string(resp.body)
}

func TestMetricsNeedNoToken(t *testing.T) {
	t.Parallel()

	// A Prometheus is not going to carry a bearer token, and the exposition carries no domain
	// paths, no flow definitions and no target_info (§13).
	h := newHarness(t, func(cfg *Config) { cfg.Token = "secret" })

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.http.URL+"/metrics", nil)
	require.NoError(t, err)
	resp, err := h.http.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// And the user API on the same listener is still closed.
	assert.Equal(t, http.StatusUnauthorized, func() int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.http.URL+api.PathPaths, nil)
		require.NoError(t, err)
		resp, err := h.http.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		return resp.StatusCode
	}())
}

func TestAReplicaThatIsNotReconcilingReportsOnlyThatItIsNotTheLeader(t *testing.T) {
	t.Parallel()

	// A follower has no observation, and zeroes would be indistinguishable from a fleet that
	// genuinely has no paths — "nothing is replicating" and "ask the other replica" must not
	// render the same.
	h := newHarness(t)
	h.fleet()

	body := h.expose()
	assert.Contains(t, body, "mxl_repl_leader 0")
	assert.NotContains(t, body, "mxl_repl_paths")
	assert.NotContains(t, body, "mxl_repl_agents_leased")

	// The per-replica instruments are there regardless: they describe this process, not the fleet.
	assert.Contains(t, body, "mxl_repl_store_operation_duration_seconds")
	assert.Contains(t, body, "mxl_repl_build_info")
}

func TestTheLeaderReportsTheFleet(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fleet()
	h.reconciling()

	require.Equal(t, http.StatusCreated, h.do(http.MethodPost, api.PathRequests, flowRequestSpec("cam1")).status)

	var body string
	require.Eventually(t, func() bool {
		body = h.expose()
		return regexp.MustCompile(`mxl_repl_paths\{state="ESTABLISHING"\} 1`).MatchString(body)
	}, 5*time.Second, 20*time.Millisecond, "the path never reached the exposition:\n%s", body)

	assert.Contains(t, body, "mxl_repl_leader 1")
	assert.Contains(t, body, "mxl_repl_agents_leased 2")
	assert.Contains(t, body, "mxl_repl_nodes_registered 2")
	assert.Contains(t, body, `mxl_repl_requests{state="ESTABLISHING"} 1`)

	// Every state in the vocabulary, including the empty ones. A state that vanished because
	// nothing is in it reads as a gap in the graph rather than a floor.
	for _, state := range api.States() {
		assert.Contains(t, body, `mxl_repl_paths{state="`+string(state)+`"}`)
		assert.Contains(t, body, `mxl_repl_requests{state="`+string(state)+`"}`)
	}
	for _, state := range api.WorkerStates() {
		assert.Contains(t, body, `mxl_repl_sessions{state="`+string(state)+`"}`)
	}

	// The fleet's version spread, which only the server can see (§13.1).
	assert.Contains(t, body, `mxl_repl_agent_versions{protocol="`+
		strconv.Itoa(api.ProtocolVersion)+`",replicator="test"} 2`)

	// A pass completed, so the histogram has an observation and the outcome is `ok`.
	assert.Regexp(t, `mxl_repl_reconciles_total\{outcome="ok"\} [1-9]`, body)
	assert.Regexp(t, `mxl_repl_reconcile_duration_seconds_count [1-9]`, body)
}

func TestLosingLeadershipDropsTheFleetGauges(t *testing.T) {
	t.Parallel()

	// Two replicas both exporting `mxl_repl_paths` — one live, one frozen at whatever it last
	// saw — would put two copies of every fleet gauge in front of the operator with nothing in
	// the exposition to say which was which.
	h := newHarness(t)
	h.fleet()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		assert.NoError(t, h.server.loop.Run(ctx))
	}()

	// Waiting on a fleet gauge rather than on the leader gauge: leadership is held from the moment
	// Run starts, which is before the first pass has anything to report.
	require.Eventually(t, func() bool {
		return strings.Contains(h.expose(), "mxl_repl_agents_leased 2")
	}, 5*time.Second, 20*time.Millisecond)
	require.Contains(t, h.expose(), "mxl_repl_leader 1")

	cancel()
	<-done

	body := h.expose()
	assert.Contains(t, body, "mxl_repl_leader 0")
	assert.NotContains(t, body, "mxl_repl_agents_leased")
	assert.NotContains(t, body, "mxl_repl_paths")
}

func TestEpochTransitionsAreCounted(t *testing.T) {
	t.Parallel()

	// The flapping signal the epoch cannot carry on its own: it is a content hash with no
	// ordering, so a session changing epoch is a target that restarted and nothing downstream can
	// see that by looking at one value (§5.2).
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
	epochReached := func(epoch string) bool {
		set, _ := h.assignments("studio-a", 0, 0)
		return len(set.Assignments) == 1 && set.Assignments[0].Epoch == epoch
	}

	report("epoch-a")
	require.Eventually(t, func() bool { return epochReached("epoch-a") }, 5*time.Second, 20*time.Millisecond)

	// A session acquiring its *first* epoch is establishment, not a transition. Nothing is counted
	// for it, and the series does not exist yet.
	assert.NotContains(t, h.expose(), "mxl_repl_epoch_transitions_total")

	report("epoch-b")
	require.Eventually(t, func() bool { return epochReached("epoch-b") }, 5*time.Second, 20*time.Millisecond)

	// Labelled by the node hosting the target, which is the node whose worker restarted.
	require.Eventually(t, func() bool {
		return strings.Contains(h.expose(), `mxl_repl_epoch_transitions_total{node="edge-01"} 1`)
	}, 5*time.Second, 20*time.Millisecond, "the transition was never counted:\n%s", h.expose())

	// And the shape a *real* restart has, which the two reports above skip: the target dies, so
	// its epoch goes away entirely — a dead target's blob describes registrations that died with
	// it — and only then does a new one arrive. The last known value has to survive that gap or
	// this counts as establishment and misses the restart.
	h.reportStatus("edge-01", "i-edge", api.SessionStatus{
		SessionID: sessionID, Role: api.RoleTarget, State: api.WorkerFailed,
		Reason: "worker exited unexpectedly",
	})
	require.Eventually(t, func() bool {
		set, _ := h.assignments("studio-a", 0, 0)
		return len(set.Assignments) == 0 || set.Assignments[0].Epoch == ""
	}, 5*time.Second, 20*time.Millisecond, "the initiator was never withdrawn")

	report("epoch-c")
	require.Eventually(t, func() bool { return epochReached("epoch-c") }, 5*time.Second, 20*time.Millisecond)

	require.Eventually(t, func() bool {
		return strings.Contains(h.expose(), `mxl_repl_epoch_transitions_total{node="edge-01"} 2`)
	}, 5*time.Second, 20*time.Millisecond, "a restart through a dead target was not counted:\n%s", h.expose())
}

func TestRejectedRegistrationsAreCounted(t *testing.T) {
	t.Parallel()

	// Both refusals are loud in the log already; a count is what an operator can alert on (§7.1,
	// §13.1).
	h := newHarness(t)
	h.register("edge-01", "i-1")

	resp := h.do(http.MethodPost, api.PathRegister, api.NodeRegistration{
		Node: "edge-01", Instance: "i-2",
		Capabilities: api.Capabilities{
			Versions: api.Versions{Protocol: api.ProtocolVersion, Replicator: "test"},
			Fabrics: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.0.0.2",
				CapFlags: []api.CapFlag{api.CapRemoteWrite},
			}},
		},
	})
	require.Equal(t, http.StatusConflict, resp.status, "body: %s", resp.body)

	assert.Contains(t, h.expose(), `mxl_repl_registrations_rejected_total{reason="node_claimed"} 1`)
}

func TestStoreOperationsAreTimed(t *testing.T) {
	t.Parallel()

	// Timed at the seam rather than in the backends, so sqlite and etcd are measured identically
	// (§8.1).
	h := newHarness(t)
	h.fleet()
	// A read handler is what produces a List; registering and reporting are Gets and Puts.
	require.Equal(t, http.StatusOK, h.do(http.MethodGet, api.PathPaths, nil).status)

	body := h.expose()
	assert.Regexp(t, `mxl_repl_store_operation_duration_seconds_count\{operation="put"\} [1-9]`, body)
	assert.Regexp(t, `mxl_repl_store_operation_duration_seconds_count\{operation="list"\} [1-9]`, body)

	// Nothing failed. A not-found or a lost compare is an ordinary answer this control plane asks
	// for on purpose, and counting either would make the failure rate a measure of how busy the
	// reconciler is.
	assert.NotContains(t, body, "mxl_repl_store_operations_failed_total")
}

func TestEachServerHasItsOwnInstruments(t *testing.T) {
	t.Parallel()

	// Two servers in one process is a real configuration — the in-process suite builds several
	// (§17). Package-level counters would make each one's numbers depend on what the other did.
	first, second := newHarness(t), newHarness(t)
	first.register("edge-01", "i-1")

	assert.Regexp(t, `mxl_repl_store_operation_duration_seconds_count\{operation="put"\} [1-9]`, first.expose())
	assert.NotContains(t, second.expose(), `mxl_repl_store_operation_duration_seconds_count{operation="put"}`)
}
