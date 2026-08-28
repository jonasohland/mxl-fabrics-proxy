package inventory

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jonasohland/mxl-utils/pkg/mxl"
	"github.com/jonasohland/mxl-utils/pkg/mxlsys"
	"github.com/jonasohland/mxl-utils/pkg/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mxl-utils logs through the default logger, including one benign complaint per watcher as it
// shuts down. Silence it so a failing assertion is the only thing in the output.
func TestMain(m *testing.M) {
	slog.SetDefault(discard())
	os.Exit(m.Run())
}

// keepWriting advances a flow's head index the way a producer does, until the returned function
// is called.
//
// One increment is not enough and that is not an artefact of the test: the first sample of a
// newly observed flow establishes a baseline and claims nothing, because a dormant flow has a
// head index too. Liveness is movement *seen*, so something has to keep moving.
func keepWriting(t *testing.T, flow *testutil.DummyFlow) func() {
	t.Helper()

	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		runtime := flow.Runtime()
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.HeadIndex++
			if err := flow.UpdateRuntime(runtime); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-stopped
		})
	}
}

// clock is a manual clock, so that hysteresis is tested by moving time rather than by sleeping
// through it.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Unix(1700000000, 0)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// harness is an inventory over a temp directory, running for the duration of a test.
type harness struct {
	*Inventory
	clock   *clock
	domain  string
	changes chan struct{}
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()

	domain := t.TempDir()
	clk := newClock()
	changes := make(chan struct{}, 128)

	if opts.Domains == nil && opts.SearchPaths == nil {
		opts.Domains = []Domain{{Name: "cameras", Path: domain}}
	}
	opts.Logger = discard()
	opts.Now = clk.Now
	opts.Interval = 5 * time.Millisecond
	if opts.IdleAfter == 0 {
		opts.IdleAfter = 3 * time.Second
	}
	opts.OnChange = func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	}

	inv, err := New(opts)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		assert.NoError(t, inv.Run(ctx))
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	return &harness{Inventory: inv, clock: clk, domain: domain, changes: changes}
}

// eventually polls the snapshot until want is satisfied. Sampling is time-driven and the watcher
// is inotify-driven, so every observation in these tests is inherently a wait.
func (h *harness) eventually(t *testing.T, what string, want func([]api.DomainInventory) bool) []api.DomainInventory {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var last []api.DomainInventory
	for time.Now().Before(deadline) {
		last = h.Snapshot()
		if want(last) {
			return last
		}
		time.Sleep(2 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s; last snapshot: %+v", what, last)
	return nil
}

// settle waits until the sampler has caught up with the last write.
//
// Necessary before advancing the fake clock: the writer and the sampler are independent, so the
// last increment may still be unobserved when the writer stops, and observing it afterwards
// moves lastMove forward to whatever the clock says *then*. Without this the test measures a
// stillness that started later than it thinks it did.
func (h *harness) settle(t *testing.T, id string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var last uint64
	stable := 0
	for time.Now().Before(deadline) {
		h.mu.Lock()
		domain, ok := h.domains[h.domain]
		var head uint64
		if ok {
			if flow, ok := domain.flows[id]; ok {
				head = flow.written.value
			}
		}
		h.mu.Unlock()

		if head == last {
			stable++
		} else {
			stable, last = 0, head
		}
		if stable >= 10 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("head index of %s never settled", id)
}

func flowOf(snapshot []api.DomainInventory, domain, id string) (api.FlowInventory, bool) {
	for _, d := range snapshot {
		if d.Name != domain {
			continue
		}
		for _, f := range d.Flows {
			if f.ID == id {
				return f, true
			}
		}
	}
	return api.FlowInventory{}, false
}

func TestNewRejectsAmbiguousMappings(t *testing.T) {
	_, err := New(Options{Domains: []Domain{{Name: "a", Path: "relative"}}})
	assert.ErrorContains(t, err, "not absolute")

	_, err = New(Options{Domains: []Domain{{Path: "/dev/shm/mxl0"}}})
	assert.ErrorContains(t, err, "has no name")

	_, err = New(Options{Domains: []Domain{{Name: "a", Path: "/x"}, {Name: "a", Path: "/y"}}})
	assert.ErrorContains(t, err, "mapped twice")

	// Two names for one directory would put one flow in the fleet inventory twice, under two
	// addresses, and make the reverse lookup ambiguous.
	_, err = New(Options{Domains: []Domain{{Name: "a", Path: "/x"}, {Name: "b", Path: "/x/"}}})
	assert.ErrorContains(t, err, "mapped twice")
}

// A configured domain goes in as a *static* domain, so it stays visible while it is empty —
// which is exactly what that parameter is for, and what stops a request for a not-yet-created
// flow reading as "that node has no such domain".
func TestConfiguredDomainIsVisibleWhileEmpty(t *testing.T) {
	h := newHarness(t, Options{})

	snapshot := h.eventually(t, "the empty domain", func(s []api.DomainInventory) bool { return len(s) == 1 })
	assert.Equal(t, "cameras", snapshot[0].Name)
	assert.True(t, snapshot[0].Configured)
	assert.Empty(t, snapshot[0].Flows)
}

func TestFlowIsReportedWithItsDefinitionAndGroupHint(t *testing.T) {
	h := newHarness(t, Options{})

	flow, err := testutil.RandomVideoFlow(h.domain)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	h.eventually(t, "the flow", func(s []api.DomainInventory) bool {
		_, ok := flowOf(s, "cameras", flow.ID())
		return ok
	})

	observed, _ := flowOf(h.Snapshot(), "cameras", flow.ID())
	require.NotNil(t, observed.GroupHint)
	assert.Equal(t, "video", observed.GroupHint.Type)
	assert.Contains(t, observed.GroupHint.Name, "Test Flow")

	// The definition travels as the bytes on disk. Verbatim is load-bearing: the destination
	// worker reproduces the source definition exactly, including fields nothing in this tree
	// models, and the session identity hashes these bytes (§5.4).
	raw, err := os.ReadFile(filepath.Join(h.domain, flow.ID()+".mxl-flow", "flow_def.json"))
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(observed.Definition))
}

// A field no struct in this tree models must survive to the destination worker, which is why the
// definition is never decoded and re-encoded on the way through.
func TestUnmodelledDefinitionFieldsSurvive(t *testing.T) {
	h := newHarness(t, Options{})

	flow, err := testutil.RandomVideoFlow(h.domain)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	path := filepath.Join(h.domain, flow.ID()+".mxl-flow", "flow_def.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	decoded["some_future_nmos_field"] = map[string]any{"deeply": "nested"}
	patched, err := json.Marshal(decoded)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, patched, 0o644))

	h.eventually(t, "the patched definition", func(s []api.DomainInventory) bool {
		observed, ok := flowOf(s, "cameras", flow.ID())
		return ok && len(observed.Definition) > 0
	})

	// The flow was already open, so the patched bytes only appear once the definition is re-read.
	// Removing and recreating is how a producer republishes, and it is the path that matters.
	require.NoError(t, flow.Remove())
	h.eventually(t, "the flow to go away", func(s []api.DomainInventory) bool {
		_, ok := flowOf(s, "cameras", flow.ID())
		return !ok
	})

	require.NoError(t, flow.Create())
	require.NoError(t, os.WriteFile(filepath.Join(h.domain, flow.ID()+".mxl-flow", "flow_def.json"), patched, 0o644))

	h.eventually(t, "the republished definition", func(s []api.DomainInventory) bool {
		observed, ok := flowOf(s, "cameras", flow.ID())
		if !ok {
			return false
		}
		var got map[string]any
		return json.Unmarshal(observed.Definition, &got) == nil && got["some_future_nmos_field"] != nil
	})
}

// The hysteresis of §11.1: idle → advancing on the first movement, advancing → idle only after
// the threshold. Never a raw head index, which would make every snapshot differ.
func TestProducingIsHystereticAndDerivedFromHeadIndexDeltas(t *testing.T) {
	h := newHarness(t, Options{IdleAfter: 3 * time.Second})

	flow, err := testutil.RandomVideoFlow(h.domain)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	// A flow that merely *has* a head index is not producing. Only having been seen to advance
	// makes it so — a dormant flow has a head index too.
	h.eventually(t, "the flow", func(s []api.DomainInventory) bool {
		_, ok := flowOf(s, "cameras", flow.ID())
		return ok
	})
	observed, _ := flowOf(h.Snapshot(), "cameras", flow.ID())
	assert.False(t, observed.Producing)

	stop := keepWriting(t, flow)
	h.eventually(t, "producing", func(s []api.DomainInventory) bool {
		observed, ok := flowOf(s, "cameras", flow.ID())
		return ok && observed.Producing
	})
	stop()
	h.settle(t, flow.ID())

	// Still producing well before the threshold: a gap between grains is not a stopped producer.
	h.clock.advance(2 * time.Second)
	time.Sleep(20 * time.Millisecond)
	observed, _ = flowOf(h.Snapshot(), "cameras", flow.ID())
	assert.True(t, observed.Producing, "a pause shorter than the threshold must not flip the bit")

	h.clock.advance(2 * time.Second)
	h.eventually(t, "not producing", func(s []api.DomainInventory) bool {
		observed, ok := flowOf(s, "cameras", flow.ID())
		return ok && !observed.Producing
	})

	// And back on movement, with no threshold to wait out on the way in.
	stop = keepWriting(t, flow)
	defer stop()
	h.eventually(t, "producing again", func(s []api.DomainInventory) bool {
		observed, ok := flowOf(s, "cameras", flow.ID())
		return ok && observed.Producing
	})
}

// §11.1's third detail, and the one with the worst failure mode: a flow deleted and recreated
// under the same ID is a different data file. Without an IsValid check on every sample the old
// mapping keeps returning the values it had when the file went away, so a republished flow
// reports producing=false forever and is never replicated again.
func TestRepublishedFlowIsReopened(t *testing.T) {
	h := newHarness(t, Options{})

	def := testutil.NewVideoFlowDef(testutil.FlowSize1080, testutil.FlowRate50)
	flow, err := testutil.NewDummyFlow(h.domain, def)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	h.eventually(t, "the flow", func(s []api.DomainInventory) bool {
		_, ok := flowOf(s, "cameras", flow.ID())
		return ok
	})

	stop := keepWriting(t, flow)
	h.eventually(t, "producing", func(s []api.DomainInventory) bool {
		observed, ok := flowOf(s, "cameras", flow.ID())
		return ok && observed.Producing
	})
	stop()

	// Replace the flow under the same ID. The old mapping still resolves and still reports the
	// head index it had when the file went away; only the inode says anything happened.
	require.NoError(t, os.RemoveAll(filepath.Join(h.domain, flow.ID()+".mxl-flow")))

	replacement, err := testutil.NewDummyFlow(h.domain, def)
	require.NoError(t, err)
	require.NoError(t, replacement.Create())

	h.eventually(t, "the reopened flow to go quiet", func(s []api.DomainInventory) bool {
		observed, ok := flowOf(s, "cameras", flow.ID())
		return ok && !observed.Producing
	})

	// The point of reopening: the new file's movements are seen. Without the IsValid check this
	// flow reports producing=false forever and is never replicated again.
	stop = keepWriting(t, replacement)
	defer stop()
	h.eventually(t, "the replacement to be seen producing", func(s []api.DomainInventory) bool {
		observed, ok := flowOf(s, "cameras", flow.ID())
		return ok && observed.Producing
	})
}

func TestFlowRemovalIsObserved(t *testing.T) {
	h := newHarness(t, Options{})

	flow, err := testutil.RandomAudioFlow(h.domain)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	h.eventually(t, "the flow", func(s []api.DomainInventory) bool {
		_, ok := flowOf(s, "cameras", flow.ID())
		return ok
	})

	require.NoError(t, flow.Remove())
	h.eventually(t, "the flow to go away", func(s []api.DomainInventory) bool {
		_, ok := flowOf(s, "cameras", flow.ID())
		return !ok
	})

	// The domain itself is configured, so it stays.
	assert.Len(t, h.Snapshot(), 1)
}

// A discovered domain is a source and never a destination, which is the invariant that stops the
// API being a remote arbitrary-filesystem-write primitive (§7.2, §13).
func TestDiscoveredDomainsAreSourcesOnly(t *testing.T) {
	root := t.TempDir()
	found := filepath.Join(root, "nested", "domain")
	require.NoError(t, os.MkdirAll(found, 0o755))

	flow, err := testutil.RandomVideoFlow(found)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	h := newHarness(t, Options{SearchPaths: []string{root}})

	snapshot := h.eventually(t, "the discovered domain", func(s []api.DomainInventory) bool {
		return len(s) == 1 && len(s[0].Flows) == 1
	})
	assert.False(t, snapshot[0].Configured)
	assert.Equal(t, found, snapshot[0].Name, "a discovered domain is named by its path")

	assert.False(t, h.Configured(found))
	path, ok := h.Path(found)
	assert.True(t, ok, "a discovered domain still resolves, so it can be an initiator's source")
	assert.Equal(t, found, path)

	// Registration advertises configured mappings only: it is durable state, and discovery comes
	// and goes with whatever a producer created.
	assert.Empty(t, h.Mappings())
}

// Resolution is a strict map lookup. An agent that fell back to treating an unmapped name as a
// path would hand the API the filesystem.
func TestPathResolutionNeverInterpretsAName(t *testing.T) {
	h := newHarness(t, Options{Domains: []Domain{{Name: "cameras", Path: t.TempDir()}}})

	_, ok := h.Path("/etc")
	assert.False(t, ok)
	_, ok = h.Path("../../etc")
	assert.False(t, ok)
	_, ok = h.Path("unmapped")
	assert.False(t, ok)

	assert.True(t, h.Configured("cameras"))
	assert.False(t, h.Configured("/etc"))
}

func TestMappingsAreWhatRegistrationAdvertises(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	inv, err := New(Options{
		Domains: []Domain{{Name: "ingest", Path: b}, {Name: "cameras", Path: a}},
		Logger:  discard(),
	})
	require.NoError(t, err)

	assert.Equal(t, []api.DomainMapping{
		{Name: "cameras", Path: a, Configured: true},
		{Name: "ingest", Path: b, Configured: true},
	}, inv.Mappings())
}

func TestCreateDomainsPreCreatesConfiguredPaths(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "does", "not", "exist", "yet")

	inv, err := New(Options{Domains: []Domain{{Name: "ingest", Path: path}}, Logger: discard()})
	require.NoError(t, err)
	require.NoError(t, inv.CreateDomains())

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

// A flow directory that exists before its definition does is a producer part-way through
// creating it, not a flow anyone can replicate: the destination worker cannot create its local
// flow without the definition (§5.3 step 2).
func TestFlowWithoutADefinitionIsNotReported(t *testing.T) {
	h := newHarness(t, Options{})

	dir := filepath.Join(h.domain, "5592a23b-0974-45bb-9388-89ea81c42537.mxl-flow")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	// Give the watcher and a few samples time to see it and decline to report it.
	time.Sleep(60 * time.Millisecond)
	snapshot := h.Snapshot()
	require.Len(t, snapshot, 1)
	assert.Empty(t, snapshot[0].Flows)
}

// The snapshot is compared against the previous one before it is reported, so a stable order is
// what stops inventory advancing the store revision on every heartbeat forever (§8.3).
func TestSnapshotOrderIsStable(t *testing.T) {
	h := newHarness(t, Options{})

	for range 5 {
		flow, err := testutil.RandomVideoFlow(h.domain)
		require.NoError(t, err)
		require.NoError(t, flow.Create())
	}

	h.eventually(t, "all five flows", func(s []api.DomainInventory) bool {
		return len(s) == 1 && len(s[0].Flows) == 5
	})

	first, err := json.Marshal(h.Snapshot())
	require.NoError(t, err)
	for range 20 {
		again, err := json.Marshal(h.Snapshot())
		require.NoError(t, err)
		require.JSONEq(t, string(first), string(again))
	}

	assert.True(t, slices.IsSortedFunc(h.Snapshot()[0].Flows, func(a, b api.FlowInventory) int {
		return cmpString(a.ID, b.ID)
	}))
}

// The hysteresis rule in isolation, without a filesystem in the way.
func TestObserveHysteresis(t *testing.T) {
	now := time.Unix(0, 0)
	var l liveness

	l.observe(10, now, time.Second)
	assert.False(t, l.active, "a baseline claims nothing")

	l.observe(10, now.Add(2*time.Second), time.Second)
	assert.False(t, l.active)

	l.observe(11, now.Add(3*time.Second), time.Second)
	assert.True(t, l.active, "the first movement is enough")

	l.observe(11, now.Add(3500*time.Millisecond), time.Second)
	assert.True(t, l.active, "still inside the threshold")

	l.observe(11, now.Add(4*time.Second), time.Second)
	assert.False(t, l.active, "exactly at the threshold")

	// The same rule drives read activity, and it is the reason the two counters share a type:
	// LastReadTime is treated as a number that changes when a reader reads, never as a TAI
	// timestamp compared against a clock (§11.1).
	l.reset()
	assert.False(t, l.active, "a reset claims nothing either")
	l.observe(1_700_000_000_000_000_000, now, time.Second)
	assert.False(t, l.active)
	l.observe(1_700_000_000_500_000_000, now.Add(time.Millisecond), time.Second)
	assert.True(t, l.active)
}

// mxlsys and mxl are imported for the runtime type and the flow package; keep the compiler
// honest about them being genuinely used.
var (
	_ = mxlsys.FlowRuntimeInfo{}
	_ = mxl.ErrMissingGroupHint
)
