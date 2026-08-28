package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonasohland/mxl-utils/pkg/mxl"
	"github.com/jonasohland/mxl-utils/pkg/testutil"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/agent"
	"github.com/jonasohland/mxl-replicator/internal/agent/inventory"
	"github.com/jonasohland/mxl-replicator/internal/agent/ports"
	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/client"
	"github.com/jonasohland/mxl-replicator/internal/server"
	"github.com/jonasohland/mxl-replicator/internal/server/leader"
	"github.com/jonasohland/mxl-replicator/internal/server/reconcile"
	"github.com/jonasohland/mxl-replicator/internal/store"
	"github.com/jonasohland/mxl-replicator/internal/store/sqlite"
	"github.com/jonasohland/mxl-replicator/internal/worker"
	"github.com/jonasohland/mxl-replicator/internal/worker/fake"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// mxl-utils logs through the default logger, including one benign complaint per watcher as it
// shuts down. Silence it so a failing assertion is the only thing in the output.
func TestMain(m *testing.M) {
	slog.SetDefault(discard())
	os.Exit(m.Run())
}

// Timings.
//
// Every one of these is a real production knob turned down, not a test-only shortcut, and that
// is deliberate: the suite exercises the same code paths an operator does, at a cadence that
// makes a five-second test out of a five-minute one. The ratios between them are preserved —
// the lease still outlives several heartbeats, the long poll still outlives several reconciles
// — because it is the ratios, not the absolute values, that the settling window and the
// fail-static paths are written against.
const (
	heartbeatInterval = 25 * time.Millisecond
	leaseTTL          = 3 * time.Second
	maxLongPollWait   = time.Second

	// storePoll is sqlite's change-detection interval. It is the floor on how fast a write by
	// one participant becomes visible to another's watch, so it bounds every convergence in the
	// suite.
	storePoll = 10 * time.Millisecond

	agentPollWait       = 500 * time.Millisecond
	agentReportInterval = 10 * time.Millisecond
	inventoryInterval   = 5 * time.Millisecond
	inventoryIdleAfter  = 100 * time.Millisecond

	// settleWait bounds a positive assertion, holdFor a negative one. The negative ones are the
	// expensive half of the suite (every one of them sleeps for its whole duration), so they are
	// kept short and are sized against the loop they must outlast: a poll wait plus a couple of
	// reports.
	settleWait = 15 * time.Second
	holdFor    = 750 * time.Millisecond
)

// --- backends ----------------------------------------------------------------------------

// backend supplies the two things a replica needs and the two things that differ between a
// single-process deployment and an HA one: somewhere to keep state, and something to decide who
// reconciles.
//
// It exists so that the HA cases (§8.2, plan §4) are the *same* tests against a different
// backend rather than a second suite. A leader change that produces no worker restarts is a
// claim about the reconciler, and it is only worth making against a real election.
type backend interface {
	// open returns the store one replica uses. Called once per replica, never per server start:
	// [server.Server] does not own the store it is handed, so a server restart must not take the
	// store with it.
	open(t *testing.T, replica string) store.Store

	// elector returns the elector one replica campaigns with.
	elector(t *testing.T, replica string) leader.Elector
}

// sqliteBackend is the single-process deployment: one file, one replica, no election.
type sqliteBackend struct {
	dir string
}

func newSQLiteBackend(t *testing.T) *sqliteBackend {
	t.Helper()
	return &sqliteBackend{dir: t.TempDir()}
}

func (b *sqliteBackend) open(t *testing.T, replica string) store.Store {
	t.Helper()

	opened, err := sqlite.Open(t.Context(), filepath.Join(b.dir, "store.db"), sqlite.Options{
		PollInterval: storePoll,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = opened.Close() })
	return opened
}

func (b *sqliteBackend) elector(t *testing.T, replica string) leader.Elector {
	return leader.Always{Replica: replica}
}

// --- the fleet ---------------------------------------------------------------------------

type fleetOptions struct {
	// backend defaults to sqlite in a temp directory.
	backend backend

	// replicas is how many servers to run. More than one only makes sense on a backend with a
	// real election.
	replicas int

	// settlingHeartbeats sizes the settling window (§7.3). Zero — the default — disables it, so
	// most tests do not pay for it; the tests that are *about* it set it.
	settlingHeartbeats int

	// reconcile is the server's policy. Only the fields a test cares about need setting.
	reconcile reconcile.Config

	// token turns on bearer authentication.
	token string
}

// fleet is a whole control plane in one process: N servers over one backend, and the nodes
// registered with them.
type fleet struct {
	t    *testing.T
	opts fleetOptions

	replicas []*replica

	mu    sync.Mutex
	nodes []*node
}

func newFleet(t *testing.T, opts fleetOptions) *fleet {
	t.Helper()

	if opts.backend == nil {
		opts.backend = newSQLiteBackend(t)
	}
	if opts.replicas <= 0 {
		opts.replicas = 1
	}

	f := &fleet{t: t, opts: opts}
	for i := range opts.replicas {
		f.replicas = append(f.replicas, f.newReplica(fmt.Sprintf("replica-%d", i)))
	}
	for _, r := range f.replicas {
		r.start()
	}

	f.awaitSettled()
	return f
}

// awaitSettled waits for the reconciler's first pass. Until it lands, every assignment poll is
// answered CodeNotReady rather than with an empty set, so a test that started asserting before
// this point would be asserting against a control plane that is deliberately refusing to speak
// (§7.3).
func (f *fleet) awaitSettled() {
	f.t.Helper()

	f.eventually("the reconciler to settle", func() bool {
		resp, err := http.Get(f.leaderURL() + "/readyz") //nolint:noctx // bounded by the test's own deadline
		if err != nil {
			return false
		}
		defer resp.Body.Close() //nolint:errcheck // a drained health check
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode == http.StatusOK
	})
}

// urls is every replica's base URL, in order — what an agent's client is configured with.
func (f *fleet) urls() []string {
	out := make([]string, 0, len(f.replicas))
	for _, r := range f.replicas {
		out = append(out, r.url)
	}
	return out
}

// leaderURL is a replica to ask questions of. Readiness is a property of the fleet's control
// plane rather than of one process (§7.3), so for a read it does not matter which — but it does
// have to be one that is running.
func (f *fleet) leaderURL() string {
	for _, r := range f.replicas {
		if r.running() {
			return r.url
		}
	}
	f.t.Fatal("no replica is running")
	return ""
}

func (f *fleet) eventually(what string, cond func() bool) {
	f.t.Helper()
	eventually(f.t, what, cond)
}

func (f *fleet) consistently(what string, cond func() bool) {
	f.t.Helper()
	consistently(f.t, what, cond)
}

// eventually polls until cond holds, or fails the test.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(settleWait)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// consistently checks that cond keeps holding — the shape of every assertion about something
// that must *not* happen, which in this package is most of them.
func consistently(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(holdFor)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("%s stopped holding", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// --- replicas ----------------------------------------------------------------------------

// replica is one server process's worth of control plane, restartable on the same address.
//
// The address is fixed rather than ephemeral because half the point of the restart tests is that
// the agents do not notice: an agent's client is configured with a URL, and a restart that moved
// the port would be testing the test harness's ability to reconfigure clients rather than the
// server's ability to adopt what is already running (§7.3).
type replica struct {
	t     *testing.T
	name  string
	url   string
	cfg   server.Config
	store store.Store

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan error
}

func (f *fleet) newReplica(name string) *replica {
	f.t.Helper()

	backing := f.opts.backend.open(f.t, name)
	port := freePort(f.t)

	return &replica{
		t:     f.t,
		name:  name,
		url:   fmt.Sprintf("http://127.0.0.1:%d", port),
		store: backing,
		cfg: server.Config{
			Store:              backing,
			Elector:            f.opts.backend.elector(f.t, name),
			Logger:             discard(),
			Listen:             fmt.Sprintf("127.0.0.1:%d", port),
			Token:              f.opts.token,
			HeartbeatInterval:  heartbeatInterval,
			LeaseTTL:           leaseTTL,
			SettlingHeartbeats: f.opts.settlingHeartbeats,
			MaxLongPollWait:    maxLongPollWait,
			Reconcile:          f.opts.reconcile,
		},
	}
}

// start brings this replica up. The listener, the elector and the reconcile loop are all the
// real ones — [server.Server.Run], not a hand-assembled subset of it — because a leader change
// and a shutdown are among the things this suite is here to exercise.
func (r *replica) start() {
	r.t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.t.Fatalf("replica %s is already running", r.name)
	}

	srv, err := server.New(r.cfg)
	require.NoError(r.t, err)

	ctx, cancel := context.WithCancel(context.WithoutCancel(r.t.Context()))
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	r.cancel, r.done = cancel, done
	r.t.Cleanup(r.stop)

	// The listener is bound inside Run, so nothing may be sent until it answers.
	eventually(r.t, "replica "+r.name+" to listen", func() bool {
		conn, err := net.DialTimeout("tcp", strings.TrimPrefix(r.url, "http://"), 200*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
}

// stop shuts this replica down and waits for it. Idempotent, so a test may stop a replica
// explicitly and still be cleaned up.
func (r *replica) stop() {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.cancel, r.done = nil, nil
	r.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()

	select {
	case err := <-done:
		require.NoError(r.t, err)
	case <-time.After(15 * time.Second):
		r.t.Errorf("replica %s did not shut down", r.name)
	}
}

func (r *replica) running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancel != nil
}

// --- nodes -------------------------------------------------------------------------------

type nodeOptions struct {
	// domains are configured domain names. Each gets a directory of its own under this node's
	// temp directory; the agent creates them.
	domains []string

	// searchPaths add discovered-domain roots. A discovered domain may be a replication source
	// and never a destination (§7.2).
	searchPaths []string

	// domainRoot places this node's domain directories somewhere other than a temp directory.
	// The loopback suite puts them on /dev/shm, where a real MXL domain lives.
	domainRoot string

	// capabilities overrides what this node advertises. Zero is one tcp attachment on fabric
	// "dc1" at 127.0.0.1, which pairs with every other default node. Ignored when probe is set.
	capabilities *api.Capabilities

	// probe replaces the whole capability probe. The loopback suite passes the real one, which
	// runs the worker binary's --interfaces and joins it against configured attachments (§10.5).
	probe func(context.Context) (api.Capabilities, error)

	// launcher replaces the fake worker launcher with a real one.
	launcher worker.Launcher

	// contested says this agent is expected to lose a race for its node name, so [fleet.addNode]
	// must not wait for a registration that is never going to be accepted (§7.1).
	contested bool

	// tweak adjusts the agent config before it is built.
	tweak func(*agent.Config)
}

// node is one fleet node: a real agent, a real inventory over real directories on disk, and a
// fake worker launcher.
type node struct {
	*agent.Agent

	t        *testing.T
	fleet    *fleet
	name     string
	launcher *fake.Launcher
	rewriter *assignmentRewriter

	// domains maps the fleet-wide name to this node's local path, which is the mapping the rest
	// of the system deliberately never sees (§7.2).
	domains map[string]string

	cancel  context.CancelFunc
	stopped chan struct{}
	once    sync.Once
}

// addNode builds an agent and runs it for the rest of the test.
func (f *fleet) addNode(name string, opts nodeOptions) *node {
	f.t.Helper()

	f.mu.Lock()
	index := len(f.nodes)
	f.mu.Unlock()

	root := opts.domainRoot
	if root == "" {
		root = f.t.TempDir()
	}
	domains := map[string]string{}
	mappings := make([]inventory.Domain, 0, len(opts.domains))
	for _, domain := range opts.domains {
		path := filepath.Join(root, domain)
		domains[domain] = path
		mappings = append(mappings, inventory.Domain{Name: domain, Path: path})
	}

	var built *agent.Agent
	inv, err := inventory.New(inventory.Options{
		Domains:     mappings,
		SearchPaths: opts.searchPaths,
		Interval:    inventoryInterval,
		IdleAfter:   inventoryIdleAfter,
		Logger:      discard(),
		OnChange:    func() { built.Notify() },
	})
	require.NoError(f.t, err)

	// A range per node. The allocator bind-probes, and several agents in one process share this
	// host's port space in a way a real fleet does not.
	allocator, err := ports.NewAllocator(ports.Range{
		Low:  uint16(24000 + 100*index),
		High: uint16(24000 + 100*index + 19),
	})
	require.NoError(f.t, err)

	rewriter := &assignmentRewriter{base: http.DefaultTransport.(*http.Transport).Clone()}
	apiClient, err := client.New(client.Options{
		Servers:        f.urls(),
		Token:          f.opts.token,
		HTTP:           &http.Client{Transport: rewriter},
		RequestTimeout: 5 * time.Second,
		Logger:         discard(),
	})
	require.NoError(f.t, err)

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

	// The fake launcher is built either way: a node driving a real worker keeps it unused, and
	// its accessors then report nothing, which is what a test asserting on real processes wants
	// anyway.
	fakeLauncher := fake.New()
	var launcher worker.Launcher = fakeLauncher
	if opts.launcher != nil {
		launcher = opts.launcher
	}

	probe := opts.probe
	if probe == nil {
		probe = func(context.Context) (api.Capabilities, error) { return capabilities, nil }
	}

	cfg := agent.Config{
		// Instance is left to the agent, which generates one per process — which is what makes a
		// second claimant of a node name a different claimant (§7.1).
		Node:              name,
		Client:            apiClient,
		Launcher:          launcher,
		Inventory:         inv,
		Ports:             allocator,
		Probe:             probe,
		Logger:            discard(),
		PollWait:          agentPollWait,
		ReportInterval:    agentReportInterval,
		BackoffMin:        5 * time.Millisecond,
		BackoffMax:        50 * time.Millisecond,
		BackoffReset:      time.Second,
		TargetInfoTimeout: 2 * time.Second,
		StopGrace:         time.Second,
	}
	if opts.tweak != nil {
		opts.tweak(&cfg)
	}

	built, err = agent.New(cfg)
	require.NoError(f.t, err)

	n := &node{
		Agent:    built,
		t:        f.t,
		fleet:    f,
		name:     name,
		launcher: fakeLauncher,
		rewriter: rewriter,
		domains:  domains,
		stopped:  make(chan struct{}),
	}

	f.mu.Lock()
	f.nodes = append(f.nodes, n)
	f.mu.Unlock()

	n.run()
	if !opts.contested {
		f.awaitRegistered(name)
	}
	return n
}

// awaitRegistered waits for a node to appear in the fleet view.
//
// Not a nicety. A request naming a node that has never registered is INVALID and never resolves
// by itself, deliberately — an unregistered name is a typo or a node that was never deployed,
// which is something only a user can fix (§7.2). A test that raced its own agents would be
// asserting on that instead of on what it meant to.
func (f *fleet) awaitRegistered(name string) {
	f.t.Helper()

	f.eventually("node "+name+" to register", func() bool {
		resp := f.do(http.MethodGet, api.PathNodes, nil)
		if resp.status != http.StatusOK {
			return false
		}
		var list api.NodeList
		if json.Unmarshal(resp.body, &list) != nil {
			return false
		}
		for _, node := range list.Nodes {
			if node.Name == name && node.Live {
				return true
			}
		}
		return false
	})
}

func (n *node) run() {
	n.t.Helper()

	ctx, cancel := context.WithCancel(context.WithoutCancel(n.t.Context()))
	n.cancel = cancel

	go func() {
		defer close(n.stopped)
		require.NoError(n.t, n.Agent.Run(ctx))
	}()
	n.t.Cleanup(n.stop)
}

// stop shuts the agent down and waits for it. Idempotent.
func (n *node) stop() {
	n.once.Do(func() {
		n.cancel()
		select {
		case <-n.stopped:
		case <-time.After(15 * time.Second):
			n.t.Errorf("agent %s did not shut down", n.name)
		}
	})
}

// path returns this node's local path for a configured domain.
func (n *node) path(domain string) string {
	path, ok := n.domains[domain]
	require.True(n.t, ok, "node %s has no domain %q", n.name, domain)
	return path
}

// worker returns the running fake worker for a session and role, or nil.
func (n *node) worker(sessionID string, role api.Role) *fake.Handle {
	return n.launcher.Find(sessionID, role)
}

// starts is how many workers this node has ever been asked to start. The history rather than the
// current state, because the assertions that matter here are negative: a server restart, a
// re-registration or an incidentally different assignment must add nothing to it (§7.3).
func (n *node) starts() int { return n.launcher.StartCount() }

// searchRoot is a directory this node scans for unconfigured domains, created up front so a test
// can put a domain under it. Pass it as [nodeOptions.searchPaths].
func searchRoot(t *testing.T) string {
	t.Helper()

	root := filepath.Join(t.TempDir(), "discovered")
	require.NoError(t, os.MkdirAll(root, 0o755))
	return root
}

// --- perturbing the wire -------------------------------------------------------------------

// assignmentRewriter rewrites assignment responses between the server and one agent.
//
// It is how the incidental-difference case (§7.3, invariant 5) is driven end to end. The
// alternative was to have the server produce the perturbation, which it cannot be made to do
// without changing it, or to hand the agent a fabricated set, which is what the agent's own
// tests already do. Sitting on the wire keeps both ends real: the server derives the assignment
// it would derive, the agent decodes what it would decode, and only the bytes in between differ.
//
// Applies to the assignment poll alone. Registration, heartbeats and reports pass through
// untouched.
type assignmentRewriter struct {
	base http.RoundTripper

	mu sync.Mutex
	fn func([]byte) []byte
}

// set installs the rewrite, or removes it when fn is nil. Safe to call while the agent is
// polling.
func (r *assignmentRewriter) set(fn func([]byte) []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fn = fn
}

func (r *assignmentRewriter) rewrite() func([]byte) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fn
}

func (r *assignmentRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.HasSuffix(req.URL.Path, "/assignments") {
		return resp, err
	}

	rewrite := r.rewrite()
	if rewrite == nil {
		return resp, nil
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}

	rewritten := rewrite(body)
	resp.Body = io.NopCloser(bytes.NewReader(rewritten))
	resp.ContentLength = int64(len(rewritten))
	return resp, nil
}

// --- the user API ---------------------------------------------------------------------------

type response struct {
	status int
	body   []byte
}

func (r response) decode(t *testing.T, into any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.body, into), "body: %s", r.body)
}

func (f *fleet) do(method, path string, body any) response {
	f.t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(f.t, err)
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(f.t.Context(), method, f.leaderURL()+path, reader)
	require.NoError(f.t, err)
	if f.opts.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.opts.token)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer resp.Body.Close() //nolint:errcheck // read to completion below

	raw, err := io.ReadAll(resp.Body)
	require.NoError(f.t, err)
	return response{status: resp.StatusCode, body: raw}
}

// request creates a replication request and returns it, failing the test if the server refuses.
func (f *fleet) request(spec api.RequestSpec) api.Request {
	f.t.Helper()

	resp := f.do(http.MethodPost, api.PathRequests, spec)
	require.Equal(f.t, http.StatusCreated, resp.status, "body: %s", resp.body)

	var out api.Request
	resp.decode(f.t, &out)
	return out
}

// cancel deletes a request.
func (f *fleet) cancel(id string) {
	f.t.Helper()

	resp := f.do(http.MethodDelete, api.RequestPath(id), nil)
	require.Equal(f.t, http.StatusNoContent, resp.status, "body: %s", resp.body)
}

func (f *fleet) paths() api.PathsResponse {
	f.t.Helper()

	resp := f.do(http.MethodGet, api.PathPaths, nil)
	require.Equal(f.t, http.StatusOK, resp.status, "body: %s", resp.body)

	var out api.PathsResponse
	resp.decode(f.t, &out)
	return out
}

// onlyPath returns the single path the fleet holds, or fails.
func (f *fleet) onlyPath() api.Path {
	f.t.Helper()

	paths := f.paths().Paths
	require.Len(f.t, paths, 1, "expected exactly one path")
	return paths[0]
}

// pathState is the state of the fleet's single path, or "" while it has none. Written to be
// polled, so it never fails the test on its own.
func (f *fleet) pathState() api.State {
	paths := f.paths().Paths
	if len(paths) != 1 {
		return ""
	}
	return paths[0].State
}

func (f *fleet) flows() api.FlowList {
	f.t.Helper()

	resp := f.do(http.MethodGet, api.PathFlows, nil)
	require.Equal(f.t, http.StatusOK, resp.status, "body: %s", resp.body)

	var out api.FlowList
	resp.decode(f.t, &out)
	return out
}

// --- flows on disk ---------------------------------------------------------------------------

// sourceFlow is a testutil flow in a node's domain, plus the goroutine that keeps its head index
// moving.
//
// Producing has to be *driven* rather than declared: liveness is derived from head-index deltas
// across samples and never from a timestamp, so a flow that exists but is not written to is
// correctly reported as not producing (§11.1). That is a property the suite depends on in both
// directions — it is what holds a path in WAITING until a source is live, and what separates
// PAUSED from ACTIVE.
type sourceFlow struct {
	*testutil.DummyFlow

	t *testing.T

	mu       sync.Mutex
	stop     chan struct{}
	stopped  chan struct{}
	produced bool
}

// createFlow builds a flow in a node's domain. It is not producing until [sourceFlow.produce] is
// called.
func (n *node) createFlow(domain string, def *mxl.FlowDefinition) *sourceFlow {
	n.t.Helper()

	// The agent creates its configured domain directories at startup; wait for the one this flow
	// belongs in rather than racing it.
	path := n.path(domain)
	eventually(n.t, "domain "+domain+" on "+n.name, func() bool {
		info, err := os.Stat(path)
		return err == nil && info.IsDir()
	})

	return createFlowAt(n.t, path, def)
}

// createFlowAt builds a flow in a directory, which is how a domain the agent did not configure
// comes into existence — the case a search path exists to find.
func createFlowAt(t *testing.T, dir string, def *mxl.FlowDefinition) *sourceFlow {
	t.Helper()

	require.NoError(t, os.MkdirAll(dir, 0o755))

	dummy, err := testutil.NewDummyFlow(dir, def)
	require.NoError(t, err)
	require.NoError(t, dummy.Create())

	flow := &sourceFlow{DummyFlow: dummy, t: t}
	t.Cleanup(flow.stopProducing)
	return flow
}

// produce starts advancing the head index, the way a producer does.
//
// One increment is not enough, and that is not an artefact of the harness: the first sample of a
// newly observed flow establishes a baseline and claims nothing, because a dormant flow has a
// head index too. Liveness is movement *seen*.
func (f *sourceFlow) produce() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.produced {
		return
	}
	f.produced = true
	f.stop, f.stopped = make(chan struct{}), make(chan struct{})

	stop, stopped := f.stop, f.stopped
	go func() {
		defer close(stopped)
		runtime := f.Runtime()
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.HeadIndex++
			runtime.LastWriteTime++
			if err := f.UpdateRuntime(runtime); err != nil {
				return
			}
			time.Sleep(inventoryInterval / 2)
		}
	}()
}

// stopProducing stops advancing the head index, leaving the flow in place. Idempotent.
func (f *sourceFlow) stopProducing() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.produced {
		return
	}
	f.produced = false
	close(f.stop)
	<-f.stopped
}

// destroy removes the flow directory, as a producer republishing under a new ID does.
func (f *sourceFlow) destroy() {
	f.stopProducing()
	require.NoError(f.t, f.Remove())
}

// videoFlowDef is a 1080p flow definition carrying a group hint the tests can select on.
//
// testutil's own group hint embeds a random cookie, which is right for its purpose and useless
// for a selector a test has to write down, so the tag is replaced with a known one.
func videoFlowDef(hintName, hintType string) *mxl.FlowDefinition {
	def := testutil.NewVideoFlowDef(testutil.FlowSize1080, testutil.FlowRate50)
	def.Tags = map[string][]string{
		"urn:x-nmos:tag:grouphint/v1.0": {hintName + ":" + hintType},
	}
	return def
}

// audioFlowDef is the other format, carrying a group hint the tests can select on. A camera's
// video and audio share a group *name* and differ in type, which is what makes a type-less
// group-hint selector replicate a camera whole (§9.1).
func audioFlowDef(hintName, hintType string) *mxl.FlowDefinition {
	def := testutil.NewAudioFlowDef()
	def.Tags = map[string][]string{
		"urn:x-nmos:tag:grouphint/v1.0": {hintName + ":" + hintType},
	}
	return def
}

// --- misc --------------------------------------------------------------------------------

// freePort picks a port that is free right now.
//
// Racy by construction, and the alternative is worse: [server.Server.Run] binds its own listener
// and a test cannot hand it one, so the address has to be decided before the server starts. It
// has to be decided before the server starts anyway, because these tests restart a replica on
// the address its agents are already configured with.
func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close() //nolint:errcheck // the point is to release the port

	return l.Addr().(*net.TCPAddr).Port
}
