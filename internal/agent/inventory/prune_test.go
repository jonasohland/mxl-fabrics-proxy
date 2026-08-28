package inventory

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonasohland/mxl-utils/pkg/mxl"
	"github.com/jonasohland/mxl-utils/pkg/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// recorder is a [mxl.DomainReceiver] that remembers what reached it.
type recorder struct {
	added   []string
	removed []string
}

func (r *recorder) AddDomain(path string)    { r.added = append(r.added, path) }
func (r *recorder) RemoveDomain(path string) { r.removed = append(r.removed, path) }

// The filter itself, without the discoverer: both directions, and the one path inside a root that
// must still be reported.
func TestPruneHidesRootsFromDiscovery(t *testing.T) {
	root := "/dev/shm/mxl/replicated"
	mapping := "/dev/shm/mxl/cameras"

	rec := &recorder{}
	p := &prune{
		roots: []string{root},
		keep:  map[string]string{mapping: "cams"},
		recv:  []mxl.DomainReceiver{rec},
		log:   discard(),
	}

	for _, path := range []string{
		root,                         // flows at the top level would make the root itself a domain
		root + "/cam1",               // an output domain, or a foreign one inside the write area
		root + "/studio-a/cam1",      // hierarchical, so the scan recurses to it
		root + "/",                   // uncleaned
		mapping + "/../replicated/x", // uncleaned, and inside the root once resolved
	} {
		p.AddDomain(path)
		p.RemoveDomain(path)
	}
	assert.Empty(t, rec.added, "nothing inside a root is reported")
	assert.Empty(t, rec.removed, "and nothing inside one is withdrawn either")

	for _, path := range []string{
		mapping,                         // a configured mapping inside a root: the 10h layout
		"/dev/shm/mxl/other",            // a sibling of the root
		"/dev/shm/mxl/replicated-other", // a string prefix of the root, but not inside it
		"/dev/shm/mxl",                  // the root's parent
	} {
		rec.added, rec.removed = nil, nil
		p.AddDomain(path)
		p.RemoveDomain(path)
		assert.Equal(t, []string{filepath.Clean(path)}, rec.added, path)
		assert.Equal(t, []string{filepath.Clean(path)}, rec.removed, path)
	}
}

// The filter is not in the way on a node that has no roots, so a source-only node runs the code
// path it always did.
func TestReceiversAreUnwrappedWithoutRoots(t *testing.T) {
	plain := rootedWith(t, Options{Domains: []Domain{{Name: "cameras", Path: t.TempDir()}}})
	assert.Len(t, plain.receivers(nil), 2, "the inventory and the watcher, directly")

	withRoot := rootedWith(t, Options{OutputRoots: []Root{{Name: "fast", Path: t.TempDir()}}})
	recv := withRoot.receivers(nil)
	require.Len(t, recv, 1)
	assert.IsType(t, &prune{}, recv[0])
}

func rootedWith(t *testing.T, opts Options) *Inventory {
	t.Helper()
	opts.Logger = discard()
	inv, err := New(opts)
	require.NoError(t, err)
	return inv
}

// **A root is written, not read.** With a search path above a root, a domain some other actor
// created inside it is not reported — while one outside the root still is (§10.6).
func TestDiscoveryIsPrunedInsideAnOutputRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "replicated")
	inside := filepath.Join(root, "foreign")
	outside := filepath.Join(base, "cameras")

	for _, dir := range []string{inside, outside} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
		flow, err := testutil.RandomVideoFlow(dir)
		require.NoError(t, err)
		require.NoError(t, flow.Create())
	}

	h := newHarness(t, Options{
		SearchPaths: []string{base},
		OutputRoots: []Root{{Name: "local", Path: root}},
	})

	snapshot := h.eventually(t, "the domain outside the root", func(s []api.DomainInventory) bool {
		return len(s) == 1 && len(s[0].Flows) == 1
	})
	assert.Equal(t, outside, snapshot[0].Name)

	// Give the pruned one every chance to show up late before concluding it will not.
	time.Sleep(50 * time.Millisecond)
	for _, domain := range h.Snapshot() {
		assert.NotEqual(t, inside, domain.Name, "a domain inside a root is never discovered")
	}

	// And it is not resolvable as a source either, so it cannot be replicated *from* by naming the
	// path discovery would have given it.
	_, ok := h.Input(inside)
	assert.False(t, ok)
}

// The naming race the pruning exists to remove: `<root>/cam1` left holding a flow by a SIGKILLed
// worker is discovered before the reconciler materialises it, and [Inventory.AddDomain] returns
// early for a path it already knows — so without pruning the domain would keep its *path* name
// through materialisation, and the server, which matches a session's destination by name, would
// leave the path in ESTABLISHING with nothing explaining why.
func TestAMaterialisedDomainOutranksAStaleDiscovery(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "replicated")
	stale := filepath.Join(root, "cam1")

	require.NoError(t, os.MkdirAll(stale, 0o755))
	flow, err := testutil.RandomVideoFlow(stale)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	h := newHarness(t, Options{
		SearchPaths: []string{base},
		OutputRoots: []Root{{Name: "local", Path: root}},
	})

	time.Sleep(50 * time.Millisecond)
	require.Empty(t, h.Snapshot(), "the stale directory is inside a root, so the scan ignores it")

	resolved, err := h.Output("local", []string{"cam1"})
	require.NoError(t, err)
	require.NoError(t, h.Materialise("cam1", resolved))

	// Named by the request, not by its path — and carrying the flow that was already there, which
	// is what the watcher's own ReadDir on AddDomain delivers.
	snapshot := h.eventually(t, "the materialised domain", func(s []api.DomainInventory) bool {
		return len(s) == 1 && len(s[0].Flows) == 1
	})
	assert.Equal(t, "cam1", snapshot[0].Name)
	assert.True(t, snapshot[0].Configured)

	// It resolves as a source under its short name, which is what makes a chain A→B→C work.
	path, ok := h.Input("cam1")
	assert.True(t, ok)
	assert.Equal(t, resolved, path)
}

// The removal half, and the one that is easy to leave out. The discoverer only reports directories
// that currently contain a flow, so an unfiltered RemoveDomain would withdraw a materialised domain
// the moment its last flow was released — forgetting a domain a live session still targets, and,
// with the add side pruned, never bringing it back.
//
// Slow by construction: mxl-utils' discoverer rescans on a fixed 7 s period, and the point here is
// that a full scan cycle passes *after* the flow is gone without the domain being withdrawn.
func TestAMaterialisedDomainSurvivesItsLastFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out a full 7s discoverer scan cycle")
	}

	base := t.TempDir()
	root := filepath.Join(base, "replicated")

	h := newHarness(t, Options{
		SearchPaths: []string{base},
		OutputRoots: []Root{{Name: "local", Path: root}},
	})

	resolved, err := h.Output("local", []string{"cam1"})
	require.NoError(t, err)
	require.NoError(t, h.Materialise("cam1", resolved))

	flow, err := testutil.RandomVideoFlow(resolved)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	h.eventually(t, "the flow", func(s []api.DomainInventory) bool {
		return len(s) == 1 && len(s[0].Flows) == 1
	})

	require.NoError(t, os.RemoveAll(filepath.Join(resolved, flow.ID()+".mxl-flow")))
	time.Sleep(8 * time.Second)

	snapshot := h.Snapshot()
	require.Len(t, snapshot, 1, "the domain is still observed; only Release may withdraw it")
	assert.Equal(t, "cam1", snapshot[0].Name)
	assert.Empty(t, snapshot[0].Flows)

	// Release is the only thing that withdraws it, and it does.
	h.Release("cam1", resolved)
	assert.Empty(t, h.Snapshot())
}
