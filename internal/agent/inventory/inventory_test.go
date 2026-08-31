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
	clock *clock

	// domain is the source domain's local path; name is its fleet-wide identity, `media/cameras`.
	domain  string
	name    string
	changes chan struct{}
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()

	// The source domain is **discovered**: there are no configured mappings any more (§6), so the
	// harness gives the inventory a readable area and the tests create flows underneath it. A
	// domain's name is `<area>/<elements>`, assigned by the innermost containing area (§10.6).
	areaRoot := t.TempDir()
	domain := filepath.Join(areaRoot, "cameras")
	require.NoError(t, os.MkdirAll(domain, 0o755))

	// A seed flow, because mxl-utils' discoverer only reports a directory that already contains
	// one and rescans on a fixed 7 s period. Without it the domain would not be discovered until
	// long after a test has created its own flow — and the watcher, which is what delivers a flow
	// added later, is only attached to domains the discoverer reported.
	seed, err := testutil.RandomVideoFlow(domain)
	require.NoError(t, err)
	require.NoError(t, seed.Create())

	clk := newClock()
	changes := make(chan struct{}, 128)

	if opts.Areas == nil {
		opts.Areas = []api.Area{{Name: "media", Path: areaRoot, Read: true}}
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

	return &harness{Inventory: inv, clock: clk, domain: domain, name: "media/cameras", changes: changes}
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

// consistently asserts the snapshot keeps satisfying want for a while, which is how "nothing was
// reported" is distinguished from "nothing has been reported *yet*".
func (h *harness) consistently(t *testing.T, what string, d time.Duration, want func([]api.DomainInventory) bool) {
	t.Helper()

	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if last := h.Snapshot(); !want(last) {
			t.Fatalf("%s stopped holding; snapshot: %+v", what, last)
		}
		time.Sleep(2 * time.Millisecond)
	}
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
		if d.Domain.String() != domain {
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

// **The one merged rule left is that no two areas share a path** (§10.6). *This supersedes a table
// of overlap rules — a search path inside a root, a search path equal to a root, a root above an
// input mapping.* All of it was arithmetic on a distinction that no longer exists: there is one
// kind of area, nesting is legal, and the innermost containing one names a directory.
func TestNewRejectsOnlyWhatItMust(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	nested := filepath.Join(base, "replicated")

	// **Nesting is legal**, and is the ordinary one-MXL-area-per-host layout.
	_, err := New(Options{Areas: []api.Area{
		{Name: "media", Path: base, Read: true},
		{Name: "fast", Path: nested, Read: true, Write: true},
	}, Logger: discard()})
	require.NoError(t, err)

	for name, areas := range map[string][]api.Area{
		"relative path":     {{Name: "fast", Path: "relative", Read: true}},
		"filesystem root":   {{Name: "fast", Path: "/", Read: true}},
		"empty name":        {{Name: "", Path: base, Read: true}},
		"name with a slash": {{Name: "a/b", Path: base, Read: true}},
		"no grants":         {{Name: "fast", Path: base}},
		"name twice": {
			{Name: "fast", Path: base, Read: true},
			{Name: "fast", Path: nested, Read: true},
		},
		// The one arrangement the innermost-area rule cannot decide.
		"one path twice": {
			{Name: "fast", Path: base, Read: true},
			{Name: "bulk", Path: base, Read: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := New(Options{Areas: areas, Logger: discard()})
			assert.Error(t, err)
		})
	}
}

// **A domain with no flow in it is not reported at all.** mxl-utils' discoverer only ever reports
// directories that already contain a flow, and with configured mappings gone (§6) there is no
// static list to keep an empty one visible. That is a real property of the design rather than a
// fixture detail: §10.7 records it as the cost of naming domains through the API — a labelled
// domain with no producer is a pending record, not an inventory entry.
func TestAnEmptyDomainIsNotReported(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "cameras"), 0o755))

	h := newHarness(t, Options{Areas: []api.Area{{Name: "media", Path: root, Read: true}}})

	h.consistently(t, "nothing to report", 200*time.Millisecond,
		func(s []api.DomainInventory) bool { return len(s) == 0 })
}

func TestFlowIsReportedWithItsDefinitionAndGroupHint(t *testing.T) {
	h := newHarness(t, Options{})

	flow, err := testutil.RandomVideoFlow(h.domain)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	h.eventually(t, "the flow", func(s []api.DomainInventory) bool {
		_, ok := flowOf(s, h.name, flow.ID())
		return ok
	})

	observed, _ := flowOf(h.Snapshot(), h.name, flow.ID())
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
		observed, ok := flowOf(s, h.name, flow.ID())
		return ok && len(observed.Definition) > 0
	})

	// The flow was already open, so the patched bytes only appear once the definition is re-read.
	// Removing and recreating is how a producer republishes, and it is the path that matters.
	require.NoError(t, flow.Remove())
	h.eventually(t, "the flow to go away", func(s []api.DomainInventory) bool {
		_, ok := flowOf(s, h.name, flow.ID())
		return !ok
	})

	require.NoError(t, flow.Create())
	require.NoError(t, os.WriteFile(filepath.Join(h.domain, flow.ID()+".mxl-flow", "flow_def.json"), patched, 0o644))

	h.eventually(t, "the republished definition", func(s []api.DomainInventory) bool {
		observed, ok := flowOf(s, h.name, flow.ID())
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
		_, ok := flowOf(s, h.name, flow.ID())
		return ok
	})
	observed, _ := flowOf(h.Snapshot(), h.name, flow.ID())
	assert.False(t, observed.Producing)

	stop := keepWriting(t, flow)
	h.eventually(t, "producing", func(s []api.DomainInventory) bool {
		observed, ok := flowOf(s, h.name, flow.ID())
		return ok && observed.Producing
	})
	stop()
	h.settle(t, flow.ID())

	// Still producing well before the threshold: a gap between grains is not a stopped producer.
	h.clock.advance(2 * time.Second)
	time.Sleep(20 * time.Millisecond)
	observed, _ = flowOf(h.Snapshot(), h.name, flow.ID())
	assert.True(t, observed.Producing, "a pause shorter than the threshold must not flip the bit")

	h.clock.advance(2 * time.Second)
	h.eventually(t, "not producing", func(s []api.DomainInventory) bool {
		observed, ok := flowOf(s, h.name, flow.ID())
		return ok && !observed.Producing
	})

	// And back on movement, with no threshold to wait out on the way in.
	stop = keepWriting(t, flow)
	defer stop()
	h.eventually(t, "producing again", func(s []api.DomainInventory) bool {
		observed, ok := flowOf(s, h.name, flow.ID())
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
		_, ok := flowOf(s, h.name, flow.ID())
		return ok
	})

	stop := keepWriting(t, flow)
	h.eventually(t, "producing", func(s []api.DomainInventory) bool {
		observed, ok := flowOf(s, h.name, flow.ID())
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
		observed, ok := flowOf(s, h.name, flow.ID())
		return ok && !observed.Producing
	})

	// The point of reopening: the new file's movements are seen. Without the IsValid check this
	// flow reports producing=false forever and is never replicated again.
	stop = keepWriting(t, replacement)
	defer stop()
	h.eventually(t, "the replacement to be seen producing", func(s []api.DomainInventory) bool {
		observed, ok := flowOf(s, h.name, flow.ID())
		return ok && observed.Producing
	})
}

func TestFlowRemovalIsObserved(t *testing.T) {
	h := newHarness(t, Options{})

	flow, err := testutil.RandomAudioFlow(h.domain)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	h.eventually(t, "the flow", func(s []api.DomainInventory) bool {
		_, ok := flowOf(s, h.name, flow.ID())
		return ok
	})

	require.NoError(t, flow.Remove())
	h.eventually(t, "the flow to go away", func(s []api.DomainInventory) bool {
		_, ok := flowOf(s, h.name, flow.ID())
		return !ok
	})

	// The domain itself stays, because the harness's seed flow is still in it.
	assert.Len(t, h.Snapshot(), 1)
}

// **A domain is named by its innermost containing area**, and that is its identity for life
// (§10.6). Hierarchy under the area survives into the name, which is what makes one grammar work
// for a discovered domain and a materialised one alike.
func TestADomainIsNamedByItsInnermostArea(t *testing.T) {
	root := t.TempDir()
	found := filepath.Join(root, "nested", "domain")
	require.NoError(t, os.MkdirAll(found, 0o755))

	flow, err := testutil.RandomVideoFlow(found)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	h := newHarness(t, Options{Areas: []api.Area{{Name: "media", Path: root, Read: true}}})

	snapshot := h.eventually(t, "the discovered domain", func(s []api.DomainInventory) bool {
		return len(s) == 1 && len(s[0].Flows) == 1
	})
	assert.Equal(t, api.Domain{Area: "media", Elements: []string{"nested", "domain"}}, snapshot[0].Domain)
	assert.Equal(t, "media/nested/domain", snapshot[0].Domain.String())

	path, ok := h.Lookup(snapshot[0].Domain)
	assert.True(t, ok, "an observed domain resolves, so it can be an initiator's source")
	assert.Equal(t, found, path)

	// The destination side is a different function entirely, and it consults no observed state:
	// [Inventory.Resolve] answers from the node's own area table and the assignment's structure
	// alone. That asymmetry is a property of *resolution* rather than of the directory (§10.6).
	_, err = h.Resolve(api.Domain{Area: "media", Elements: []string{"ingest"}})
	assert.ErrorContains(t, err, "does not grant writing")
}

// **Longest prefix wins.** `media` being an ancestor of `fast` produces nothing to disambiguate:
// a directory inside `fast` is `fast/...` and never `media/replicated/...`, because `fast`
// contains it more tightly (§10.6).
func TestTheInnermostAreaWins(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "replicated")
	found := filepath.Join(inner, "ingest")
	require.NoError(t, os.MkdirAll(found, 0o755))

	flow, err := testutil.RandomVideoFlow(found)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	h := newHarness(t, Options{Areas: []api.Area{
		{Name: "media", Path: outer, Read: true},
		{Name: "fast", Path: inner, Read: true, Write: true},
	}})

	snapshot := h.eventually(t, "the discovered domain", func(s []api.DomainInventory) bool {
		return len(s) == 1
	})
	assert.Equal(t, "fast/ingest", snapshot[0].Domain.String())

	// And it is the *same* name the reconciler would have materialised it under, which is the
	// property that makes un-pruning affordable: the two namers cannot disagree (§10.6).
	resolved, err := h.Resolve(api.Domain{Area: "fast", Elements: []string{"ingest"}})
	require.NoError(t, err)
	assert.Equal(t, found, resolved)
}

// An area's own directory is not a domain — a domain has at least one element (§10.6).
func TestAnAreasOwnDirectoryIsNotADomain(t *testing.T) {
	root := t.TempDir()
	flow, err := testutil.RandomVideoFlow(root)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	h := newHarness(t, Options{Areas: []api.Area{{Name: "media", Path: root, Read: true}}})

	h.consistently(t, "nothing to report", 200*time.Millisecond,
		func(s []api.DomainInventory) bool { return len(s) == 0 })
}

// Resolution is a strict map lookup over the domains this agent is *observing*. An agent that
// fell back to treating an unobserved name as a path would hand the API the filesystem.
func TestPathResolutionNeverInterpretsAName(t *testing.T) {
	h := newHarness(t, Options{})

	for _, domain := range []api.Domain{
		{Area: "media", Elements: []string{"unobserved"}},
		{Area: "nowhere", Elements: []string{"cameras"}},
		{Area: "media", Elements: []string{"..", "..", "etc"}},
	} {
		_, ok := h.Lookup(domain)
		assert.False(t, ok, "domain %q must not resolve", domain)
	}
}

// A flow directory that exists before its definition does is a producer part-way through
// creating it, not a flow anyone can replicate: the destination worker cannot create its local
// flow without the definition (§5.3 step 2).
func TestFlowWithoutADefinitionIsNotReported(t *testing.T) {
	h := newHarness(t, Options{})

	dir := filepath.Join(h.domain, "5592a23b-0974-45bb-9388-89ea81c42537.mxl-flow")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	// Give the watcher and a few samples time to see it and decline to report it. The harness's
	// seed flow is still there, so what is asserted is that this one did not join it.
	time.Sleep(60 * time.Millisecond)
	snapshot := h.Snapshot()
	require.Len(t, snapshot, 1)
	for _, flow := range snapshot[0].Flows {
		assert.NotEqual(t, "5592a23b-0974-45bb-9388-89ea81c42537", flow.ID)
	}
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

	// Six, counting the harness's seed flow.
	h.eventually(t, "all five flows", func(s []api.DomainInventory) bool {
		return len(s) == 1 && len(s[0].Flows) == 6
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

// --- the union: discovery and materialisation (§10.6) ------------------------------------------

// **A directory reported by discovery *and* materialised by the reconciler resolves to one name
// and one inventory entry, in both orders.** That is what the innermost-area naming rule buys, and
// it is what makes un-pruning affordable: the two namers cannot disagree, so there is nothing to
// arbitrate.
//
// The second order is the case pruning existed for — a leftover directory holding a flow,
// discovered *before* the assignment that materialises it. Under the old rule it kept its
// path-shaped name through materialisation and the server, which matches a session's destination
// by name, left the path in ESTABLISHING with nothing explaining why (§10.6).
func TestDiscoveryAndMaterialisationAgreeOnOneName(t *testing.T) {
	t.Run("materialised first", func(t *testing.T) {
		root := t.TempDir()
		h := newHarness(t, Options{Areas: []api.Area{{Name: "fast", Path: root, Read: true, Write: true}}})

		resolved, err := h.Resolve(api.Domain{Area: "fast", Elements: []string{"cam1"}})
		require.NoError(t, err)
		require.NoError(t, h.Materialise(resolved))

		flow, err := testutil.RandomVideoFlow(resolved)
		require.NoError(t, err)
		require.NoError(t, flow.Create())

		snapshot := h.eventually(t, "the flow", func(s []api.DomainInventory) bool {
			return len(s) == 1 && len(s[0].Flows) == 1
		})
		assert.Equal(t, "fast/cam1", snapshot[0].Domain.String())
	})

	t.Run("discovered first", func(t *testing.T) {
		root := t.TempDir()
		stale := filepath.Join(root, "cam1")
		require.NoError(t, os.MkdirAll(stale, 0o755))
		flow, err := testutil.RandomVideoFlow(stale)
		require.NoError(t, err)
		require.NoError(t, flow.Create())

		h := newHarness(t, Options{Areas: []api.Area{{Name: "fast", Path: root, Read: true, Write: true}}})

		// Discovered like any other domain — **not pruned** — and already carrying the name the
		// reconciler is about to materialise it under.
		snapshot := h.eventually(t, "the leftover domain", func(s []api.DomainInventory) bool {
			return len(s) == 1
		})
		assert.Equal(t, "fast/cam1", snapshot[0].Domain.String())

		resolved, err := h.Resolve(api.Domain{Area: "fast", Elements: []string{"cam1"}})
		require.NoError(t, err)
		require.NoError(t, h.Materialise(resolved))

		// One entry, one name, and the flow that was already there.
		snapshot = h.Snapshot()
		require.Len(t, snapshot, 1)
		assert.Equal(t, "fast/cam1", snapshot[0].Domain.String())
		require.Len(t, snapshot[0].Flows, 1)
	})
}

// **RemoveDomain must not evict a materialised domain**, and this is the one correctness-critical
// line in the un-pruning (§10.6). The discoverer only reports directories that currently contain a
// flow, so an unconditional removal would forget a domain the instant its last flow was released —
// while a live session still targeted it.
//
// Driven through the receiver directly rather than by waiting out the discoverer's 7 s rescan: the
// property under test is the union, not the scan interval.
func TestAMaterialisedDomainSurvivesDiscoveryLettingGo(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	h := newHarness(t, Options{Areas: []api.Area{{Name: "fast", Path: root, Read: true, Write: true}}})

	resolved, err := h.Resolve(api.Domain{Area: "fast", Elements: []string{"cam1"}})
	require.NoError(t, err)
	require.NoError(t, h.Materialise(resolved))
	h.AddDomain(resolved) // as a scan that saw its flow would

	require.Len(t, h.Snapshot(), 1)

	// The scan stops seeing it — its last flow was released. The reconciler still holds it.
	h.RemoveDomain(resolved)
	assert.Len(t, h.Snapshot(), 1, "a live session still targets this domain")

	// And it leaves only when both have let go.
	h.Release(resolved)
	assert.Empty(t, h.Snapshot())
}

// The mirror: a domain some *other* actor still has a flow in stays visible after the reconciler
// releases it. It is an ordinary domain that this project happens to have created, which is the
// widening un-pruning is for — a leaked directory is discovered rather than hidden (§10.6).
func TestReleaseDoesNotHideADomainDiscoveryStillReports(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	h := newHarness(t, Options{Areas: []api.Area{{Name: "fast", Path: root, Read: true, Write: true}}})

	resolved, err := h.Resolve(api.Domain{Area: "fast", Elements: []string{"cam1"}})
	require.NoError(t, err)
	require.NoError(t, h.Materialise(resolved))
	h.AddDomain(resolved)

	h.Release(resolved)
	require.Len(t, h.Snapshot(), 1, "discovery still reports it")
	assert.Equal(t, "fast/cam1", h.Snapshot()[0].Domain.String())

	h.RemoveDomain(resolved)
	assert.Empty(t, h.Snapshot())
}

// **An area repointed to a different directory keeps every domain identity on the node**, so
// paths and sessions survive the restart rather than rebuilding (§5.4, §10.6). That is the thing
// the area name buys over path-as-identity, and it is invisible unless it is asserted.
func TestRepointingAnAreaKeepsEveryDomainIdentity(t *testing.T) {
	t.Parallel()

	names := func(t *testing.T, path string) []string {
		t.Helper()

		domain := filepath.Join(path, "cam1")
		require.NoError(t, os.MkdirAll(domain, 0o755))
		flow, err := testutil.RandomVideoFlow(domain)
		require.NoError(t, err)
		require.NoError(t, flow.Create())

		h := newHarness(t, Options{Areas: []api.Area{{Name: "media", Path: path, Read: true}}})
		snapshot := h.eventually(t, "the domain", func(s []api.DomainInventory) bool { return len(s) == 1 })
		return []string{snapshot[0].Domain.String()}
	}

	before := names(t, t.TempDir())
	after := names(t, t.TempDir())
	assert.Equal(t, []string{"media/cam1"}, before)
	assert.Equal(t, before, after, "the directory moved; the identity did not")
}

// --- provenance (§6, §10.6) --------------------------------------------------------------------

// `replicated` is true exactly while one of this agent's own target workers is writing that flow,
// and it is what keeps a label selector from matching this project's own output (§10.7).
//
// It is **pushed from the running workers**, so this test drives it the way the agent's reconcile
// does rather than inferring it from anything on disk.
func TestReplicatedTracksTheRunningTargetWorkers(t *testing.T) {
	h := newHarness(t, Options{})

	flow, err := testutil.RandomVideoFlow(h.domain)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	h.eventually(t, "the flow", func(s []api.DomainInventory) bool {
		_, ok := flowOf(s, h.name, flow.ID())
		return ok
	})

	observed, _ := flowOf(h.Snapshot(), h.name, flow.ID())
	assert.False(t, observed.Replicated, "nothing on this node is writing it")

	h.SetReplicated(map[FlowRef]struct{}{{DomainPath: h.domain, FlowID: flow.ID()}: {}})
	observed, _ = flowOf(h.Snapshot(), h.name, flow.ID())
	assert.True(t, observed.Replicated)

	// A sibling flow in the same domain, written locally, is unaffected — which is the precision
	// the per-flow rule buys over the directory-granular one it replaces (§10.7).
	sibling, err := testutil.RandomVideoFlow(h.domain)
	require.NoError(t, err)
	require.NoError(t, sibling.Create())

	h.eventually(t, "the sibling", func(s []api.DomainInventory) bool {
		_, ok := flowOf(s, h.name, sibling.ID())
		return ok
	})
	observed, _ = flowOf(h.Snapshot(), h.name, sibling.ID())
	assert.False(t, observed.Replicated)

	// **Provenance and production go absent together**, which is what makes the gap safe: the
	// worker stopping is what takes the flag away, and the same stop is what stops the flow
	// advancing (§10.6, §11.1).
	h.SetReplicated(nil)
	observed, _ = flowOf(h.Snapshot(), h.name, flow.ID())
	assert.False(t, observed.Replicated)
}
