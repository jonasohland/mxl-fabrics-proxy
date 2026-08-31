package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-utils/pkg/testutil"

	"github.com/jonasohland/mxl-replicator/internal/agent/inventory"
	"github.com/jonasohland/mxl-replicator/internal/agent/ports"
	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/client"
	"github.com/jonasohland/mxl-replicator/internal/worker/fake"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// mxl-utils logs through the default logger, including one benign complaint per watcher as it
// shuts down.
func TestMain(m *testing.M) {
	slog.SetDefault(discard())
	os.Exit(m.Run())
}

// server is a control plane good enough to drive an agent: it records what it is told and hands
// back whatever assignment set the test last set.
type server struct {
	t *testing.T

	mu            sync.Mutex
	revision      int64
	assignments   map[string][]api.Assignment
	inventories   []api.InventorySnapshot
	statuses      []api.StatusSnapshot
	registrations []api.NodeRegistration
	heartbeats    int
	polls         int

	// notReady, claimed and reregister let a test drive the three answers that must never look
	// like an empty assignment set.
	notReady   bool
	claimed    bool
	reregister bool

	changed chan struct{}
	http    *httptest.Server
}

func newServer(t *testing.T) *server {
	t.Helper()

	s := &server{
		t:           t,
		assignments: map[string][]api.Assignment{},
		changed:     make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+api.PathRegister, s.handleRegister)
	mux.HandleFunc("POST "+api.AgentPrefix+"/{node}/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("POST "+api.AgentPrefix+"/{node}/inventory", s.handleInventory)
	mux.HandleFunc("POST "+api.AgentPrefix+"/{node}/status", s.handleStatus)
	mux.HandleFunc("GET "+api.AgentPrefix+"/{node}/assignments", s.handleAssignments)

	s.http = httptest.NewServer(mux)
	t.Cleanup(s.http.Close)
	return s
}

func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var registration api.NodeRegistration
	require.NoError(s.t, json.NewDecoder(r.Body).Decode(&registration))

	s.mu.Lock()
	claimed := s.claimed
	s.registrations = append(s.registrations, registration)
	s.mu.Unlock()

	if claimed {
		writeJSON(w, http.StatusConflict, api.Error{
			Code:    api.CodeNodeClaimed,
			Message: "already claimed",
			Details: map[string]string{"holder": "someone-else"},
		})
		return
	}

	writeJSON(w, http.StatusOK, api.RegistrationResponse{
		Lease:             "1",
		TTL:               api.Millis(time.Second),
		HeartbeatInterval: api.Millis(20 * time.Millisecond),
		Server:            api.Versions{Protocol: api.ProtocolVersion, Replicator: "test"},
	})
}

func (s *server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.heartbeats++
	reregister := s.reregister
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, api.HeartbeatResponse{Reregister: reregister})
}

func (s *server) handleInventory(w http.ResponseWriter, r *http.Request) {
	var snapshot api.InventorySnapshot
	require.NoError(s.t, json.NewDecoder(r.Body).Decode(&snapshot))

	s.mu.Lock()
	s.inventories = append(s.inventories, snapshot)
	s.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var snapshot api.StatusSnapshot
	require.NoError(s.t, json.NewDecoder(r.Body).Decode(&snapshot))

	s.mu.Lock()
	s.statuses = append(s.statuses, snapshot)
	s.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleAssignments(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("node")

	s.mu.Lock()
	s.polls++
	notReady := s.notReady
	s.mu.Unlock()

	if notReady {
		writeJSON(w, http.StatusServiceUnavailable, api.Error{
			Code:    api.CodeNotReady,
			Message: "the reconciler has not settled",
		})
		return
	}

	cursor := int64(0)
	if raw := r.URL.Query().Get(api.QueryRevision); raw != "" {
		require.NoError(s.t, json.Unmarshal([]byte(raw), &cursor))
	}

	s.mu.Lock()
	revision, set, changed := s.revision, s.assignments[node], s.changed
	s.mu.Unlock()

	if cursor != 0 && revision <= cursor {
		select {
		case <-changed:
			s.mu.Lock()
			revision, set = s.revision, s.assignments[node]
			s.mu.Unlock()
		case <-time.After(200 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}

	if set == nil {
		set = []api.Assignment{}
	}
	writeJSON(w, http.StatusOK, api.AssignmentSet{Node: node, Revision: revision, Assignments: set})
}

// assign publishes a new assignment set and releases every held poll.
func (s *server) assign(node string, assignments ...api.Assignment) {
	s.mu.Lock()
	s.revision++
	s.assignments[node] = assignments
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func (s *server) setNotReady(notReady bool) {
	s.mu.Lock()
	s.notReady = notReady
	s.mu.Unlock()
}

func (s *server) setClaimed(claimed bool) {
	s.mu.Lock()
	s.claimed = claimed
	s.mu.Unlock()
}

func (s *server) setReregister(reregister bool) {
	s.mu.Lock()
	s.reregister = reregister
	s.mu.Unlock()
}

func (s *server) counts() (registrations, heartbeats, polls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.registrations), s.heartbeats, s.polls
}

// lastStatus returns the most recent status reported for one worker, if any.
func (s *server) lastStatus(sessionID string, role api.Role) (api.SessionStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.statuses) - 1; i >= 0; i-- {
		for _, session := range s.statuses[i].Sessions {
			if session.SessionID == sessionID && session.Role == role {
				return session, true
			}
		}
	}
	return api.SessionStatus{}, false
}

func (s *server) statusSnapshots() []api.StatusSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]api.StatusSnapshot(nil), s.statuses...)
}

func (s *server) statusReports() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.statuses)
}

func (s *server) inventoryReports() []api.InventorySnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]api.InventorySnapshot(nil), s.inventories...)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// harness is an agent wired to a fake server, a fake launcher and a real inventory over a temp
// directory.
type harness struct {
	*Agent

	t        *testing.T
	server   *server
	launcher *fake.Launcher

	// domain is the *discovered* source domain — a directory under the harness's search path,
	// holding one flow so the discoverer reports it. outputDomain is where a target assignment
	// from targetAssignment materialises, which is a directory that does not exist until one is
	// accepted (§10.6).
	domain       string
	name         api.Domain
	root         string
	outputDomain string
	stopped      chan struct{}
	cancel       context.CancelFunc
	once         sync.Once
}

type harnessOptions struct {
	// capabilities overrides what this node advertises. Zero means one tcp attachment on fabric
	// "dc1" at 127.0.0.1.
	capabilities *api.Capabilities

	// probeErr makes the capability probe fail.
	probeErr error

	// areas replaces the harness's own two. Empty takes the default: one readable area holding the
	// source domain, and one writable area destinations are materialised in (§10.6).
	areas []api.Area

	// extraAreas are added alongside the harness's own, for tests that want a third.
	extraAreas []api.Area

	// tweak adjusts the agent config before it is built.
	tweak func(*Config)
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()

	srv := newServer(t)

	// **The source domain is discovered, not configured** (§6). There is no name→path mapping any
	// more, so the harness gives the inventory a readable area and puts a flow in the domain — a
	// directory with no flow in it is not reported by the discoverer at all, which is a real
	// property of the design and not a fixture detail (§10.7).
	areaRoot := t.TempDir()
	domain := filepath.Join(areaRoot, "cameras")
	require.NoError(t, os.MkdirAll(domain, 0o755))
	sourceFlow, err := testutil.RandomVideoFlow(domain)
	require.NoError(t, err)
	require.NoError(t, sourceFlow.Create())

	root := t.TempDir()

	areas := opts.areas
	if areas == nil {
		// Most of these tests assign a target, and a target's domain is a name inside an area the
		// operator granted `write` on (§10.6). Two areas: one this node reads, one it writes.
		areas = []api.Area{
			{Name: "media", Path: areaRoot, Read: true},
			{Name: "fast", Path: root, Read: true, Write: true},
		}
	}
	areas = append(areas, opts.extraAreas...)

	inv, err := inventory.New(inventory.Options{
		Areas:     areas,
		Interval:  5 * time.Millisecond,
		IdleAfter: 50 * time.Millisecond,
		Logger:    discard(),
	})
	require.NoError(t, err)

	allocator, err := ports.NewAllocator(ports.Range{Low: 24000, High: 24009})
	require.NoError(t, err)

	cl, err := client.New(client.Options{Servers: []string{srv.http.URL}, RequestTimeout: 2 * time.Second})
	require.NoError(t, err)

	capabilities := api.Capabilities{
		Fabrics: []api.FabricAttachment{{
			Provider:       api.ProviderTCP,
			Fabric:         "dc1",
			Address:        "127.0.0.1",
			CapFlags:       []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive},
			MaxMessageSize: 1 << 20,
		}},
	}
	if opts.capabilities != nil {
		capabilities = *opts.capabilities
	}

	launcher := fake.New()
	cfg := Config{
		Node:      "edge-01",
		Instance:  "i-1",
		Client:    cl,
		Launcher:  launcher,
		Inventory: inv,
		Ports:     allocator,
		Probe: func(ctx context.Context) (api.Capabilities, error) {
			if opts.probeErr != nil {
				return api.Capabilities{}, opts.probeErr
			}
			return capabilities, nil
		},
		Logger:            discard(),
		PollWait:          200 * time.Millisecond,
		ReportInterval:    10 * time.Millisecond,
		BackoffMin:        5 * time.Millisecond,
		BackoffMax:        20 * time.Millisecond,
		BackoffReset:      time.Second,
		TargetInfoTimeout: time.Second,
		StopGrace:         time.Second,
	}
	if opts.tweak != nil {
		opts.tweak(&cfg)
	}

	ag, err := New(cfg)
	require.NoError(t, err)

	h := &harness{
		Agent:        ag,
		t:            t,
		server:       srv,
		launcher:     launcher,
		domain:       domain,
		name:         api.Domain{Area: "media", Elements: []string{"cameras"}},
		root:         root,
		outputDomain: filepath.Join(root, "ingest"),
		stopped:      make(chan struct{}),
	}
	return h
}

// run starts the agent for the duration of the test.
func (h *harness) run() {
	h.t.Helper()

	ctx, cancel := context.WithCancel(h.t.Context())
	h.cancel = cancel

	go func() {
		defer close(h.stopped)
		require.NoError(h.t, h.Agent.Run(ctx))
	}()
	h.t.Cleanup(h.stop)
}

// stop shuts the agent down and waits for it. Idempotent, so a test may call it explicitly and
// still be cleaned up.
func (h *harness) stop() {
	h.once.Do(func() {
		h.cancel()
		select {
		case <-h.stopped:
		case <-time.After(10 * time.Second):
			h.t.Error("the agent did not shut down")
		}
	})
}

// eventually polls until cond holds, or fails the test.
func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

// consistently checks that cond keeps holding for a while — the shape of every assertion about
// something that must *not* happen.
func (h *harness) consistently(what string, d time.Duration, cond func() bool) {
	h.t.Helper()

	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !cond() {
			h.t.Fatalf("%s stopped holding", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// targetAssignment is the destination end of a session (§5.3 step 2). Its domain names the
// harness's writable area, which is what makes it a destination at all (§10.6).
func targetAssignment(sessionID string) api.Assignment {
	return api.Assignment{
		SessionID: sessionID,
		Role:      api.RoleTarget,
		// **One structured domain for both roles.** The resolver takes the area name and the
		// elements, so nothing outside the CLI's manifest parser turns a domain string back into
		// path elements (§10.6).
		Domain:    api.Domain{Area: "fast", Elements: []string{"ingest"}},
		Namespace: api.DefaultNamespace,
		FlowID:    "5592a23b-0974-45bb-9388-89ea81c42537",
		FlowDef:   json.RawMessage(`{"id":"5592a23b-0974-45bb-9388-89ea81c42537","format":"urn:x-nmos:format:video"}`),
		Fabric:    "dc1",
		Interface: api.InterfaceConfig{
			Provider:       api.ProviderTCP,
			CapFlags:       []api.CapFlag{api.CapRemoteWrite},
			MaxMessageSize: 1 << 20,
		},
	}
}

// initiatorAssignment is the source end, which needs an epoch and the blob it was computed from
// (§5.3 step 5).
// The domain is the harness's own discovered source domain, `media/cameras` (§6, §10.6). Passed
// in rather than hardcoded so that a test can point it somewhere else.
func initiatorAssignment(domain api.Domain, sessionID, epochValue, blob string) api.Assignment {
	return api.Assignment{
		SessionID:  sessionID,
		Role:       api.RoleInitiator,
		Domain:     domain,
		FlowID:     "5592a23b-0974-45bb-9388-89ea81c42537",
		Epoch:      epochValue,
		TargetInfo: blob,
		Fabric:     "dc1",
		Interface: api.InterfaceConfig{
			Provider:       api.ProviderTCP,
			CapFlags:       []api.CapFlag{api.CapRemoteWrite},
			MaxMessageSize: 1 << 20,
		},
		Peer: &api.PeerEndpoint{Node: "edge-02", Address: "10.0.0.2", Service: "24000"},
	}
}
