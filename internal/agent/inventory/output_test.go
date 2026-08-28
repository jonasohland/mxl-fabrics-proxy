package inventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonasohland/mxl-utils/pkg/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

func rooted(t *testing.T, roots ...Root) *Inventory {
	t.Helper()

	inv, err := New(Options{OutputRoots: roots, Logger: discard()})
	require.NoError(t, err)
	return inv
}

func TestOutputResolvesUnderTheNamedRoot(t *testing.T) {
	fast, bulk := t.TempDir(), t.TempDir()
	inv := rooted(t, Root{Name: "fast", Path: fast}, Root{Name: "bulk", Path: bulk})

	path, err := inv.Output("fast", []string{"ingest"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(fast, "ingest"), path)

	// The same domain name under a different root is a different directory. The server is what
	// stops both existing at once — names are flat per node (§10.6) — and this resolver
	// deliberately does not: it answers for the assignment it was given.
	path, err = inv.Output("bulk", []string{"ingest"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(bulk, "ingest"), path)
}

// Nothing about a root has to exist on disk for it to resolve: the directory is created when a
// target assignment is accepted (§10.6, 10d), and the resolver is a pure function of two names.
func TestOutputDoesNotRequireTheRootToExist(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not", "created", "yet")
	inv := rooted(t, Root{Name: "fast", Path: root})

	path, err := inv.Output("fast", []string{"ingest"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "ingest"), path)
}

func TestOutputRefusesAnUnadvertisedRoot(t *testing.T) {
	inv := rooted(t, Root{Name: "fast", Path: t.TempDir()})

	_, err := inv.Output("bulk", []string{"ingest"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no output root "bulk"`)
	assert.Contains(t, err.Error(), `"fast"`, "the error names what this node does advertise")

	// An assignment from a server that has not been told about roots at all. It fails for the
	// same reason and says so differently, because "you named nothing" and "you named the wrong
	// thing" are different operator problems.
	_, err = inv.Output("", []string{"ingest"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no output root named")
}

// A node with no roots configured is not a replication destination at all (§10.6). It is the
// default, and the message has to say that rather than looking like a typo.
func TestOutputRefusesEverythingWithoutRoots(t *testing.T) {
	inv := rooted(t)

	_, err := inv.Output("", []string{"ingest"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a replication destination")

	_, err = inv.Output("fast", []string{"ingest"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a replication destination")
}

// The character-set half of the invariant. Every one of these is refused, and the traversal cases
// are refused *twice* — see TestContainmentHoldsWithoutTheCharacterSet.
func TestOutputRefusesEveryNameThatIsNotAPlainElement(t *testing.T) {
	root := t.TempDir()
	inv := rooted(t, Root{Name: "fast", Path: root})

	for _, name := range []string{
		"",
		".",
		"..",
		"../etc",
		"../../etc/cron.d",
		"a/b",
		"a/../../b",
		"/etc",
		"/",
		`a\b`,          // a separator on another platform, and not a name anyone means here
		"ingest\x00sh", // NUL, which truncates the name for anything that hands it to C
		"ingest sh",
		"ingest\n",
		".hidden",
		"-flag",
		"ingést",     // non-ASCII
		"ingest​",    // zero-width space
		"іngest",     // Cyrillic і, a lookalike for a name an operator may hold
		"ingest⁄etc", // fraction slash, a lookalike for a separator
		strings.Repeat("a", api.MaxDomainNameLen+1),
	} {
		t.Run(strings.ToValidUTF8(name, "?"), func(t *testing.T) {
			path, err := inv.Output("fast", []string{name})
			require.Error(t, err, "resolved to %q", path)
			assert.Empty(t, path)
		})
	}

	// And the shapes that are fine, so the rule is not simply "refuse everything".
	for _, name := range []string{"ingest", "ingest-2", "ingest_2", "a.b", "A1", strings.Repeat("a", api.MaxDomainNameLen)} {
		t.Run("ok/"+name, func(t *testing.T) {
			path, err := inv.Output("fast", []string{name})
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(root, name), path)
		})
	}
}

// The containment half, exercised on its own so that neither check is the only thing holding the
// invariant up (§10.6). Nothing escapes the root here even with the character set bypassed.
//
// The two checks are not equals, and it is worth being exact about which does what. The character
// set is the primary: elements that pass it contain no separator and no `..`, so joining them onto
// a root produces exactly the path the request named. Containment is the backstop — it is what
// still holds if that rule is ever loosened, and it is the check that would have to fail for this
// API to become a write primitive. Neither is redundant and only one of them is sufficient.
func TestContainmentHoldsWithoutTheCharacterSet(t *testing.T) {
	root := "/dev/shm/mxl"

	// Every one of these escapes the root, or resolves to something other than the elements as
	// given. The equality formulation catches all of them: if Join cleaned anything away, the two
	// strings differ.
	for _, elements := range [][]string{
		{""},
		{"."},
		{".."},
		{"../etc"},
		{"../mxl-evil"}, // the sibling-prefix case a HasPrefix spelling gets wrong
		{"../../../etc/cron.d"},
		{"a/../../b"},
		{"nested/../.."},
		{"/"},
		{"./"},
		{"studio-a", ".."},
		{"studio-a", "../../etc"},
		{"..", "studio-a"},

		// Absolute-looking, and now refused *here* as well. The direct-child spelling this
		// replaced let it through — `/etc` re-rooted to `<root>/etc`, inside the root but not the
		// domain the request named — and only the character-set check caught it. Joining against
		// the elements as given makes it a containment failure too, which is a strictly stronger
		// check than the one it replaced.
		{"/etc"},
	} {
		t.Run(strings.Join(elements, "|"), func(t *testing.T) {
			path, err := contain(root, elements)
			require.Error(t, err, "resolved to %q", path)
		})
	}

	// What containment deliberately does *not* catch: an element containing a separator stays
	// inside the root, so it passes here and is [api.ValidDomainElements]'s to refuse. That is the
	// division of labour, and it is why both checks exist —
	// TestOutputRefusesEveryNameThatIsNotAPlainElement is where the other half is pinned.
	path, err := contain(root, []string{"a/b"})
	require.NoError(t, err)
	assert.Equal(t, "/dev/shm/mxl/a/b", path)

	// And the shapes that are simply correct, flat and nested.
	path, err = contain(root, []string{"ingest"})
	require.NoError(t, err)
	assert.Equal(t, "/dev/shm/mxl/ingest", path)

	path, err = contain(root, []string{"studio-a", "cam1"})
	require.NoError(t, err)
	assert.Equal(t, "/dev/shm/mxl/studio-a/cam1", path)
}

// The sharpest property of the input/output split: the destination resolver is a pure function of
// this agent's configuration and the assignment, with no dependence on what happens to be on disk
// (§10.6). A control plane that could steer a destination by first getting something discovered
// would be a control plane that could steer it anywhere.
func TestOutputConsultsNoObservedState(t *testing.T) {
	root := t.TempDir()
	search := t.TempDir()
	found := filepath.Join(search, "nested", "domain")
	require.NoError(t, os.MkdirAll(found, 0o755))

	// Empty: nothing observed at all.
	empty := rooted(t, Root{Name: "fast", Path: root})
	before, err := empty.Output("fast", []string{"ingest"})
	require.NoError(t, err)

	// Full: the same root configuration, plus a configured domain and a discovered one carrying a
	// flow.
	flow, err := testutil.RandomVideoFlow(found)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	full := newHarness(t, Options{
		Domains:     []Domain{{Name: "cameras", Path: t.TempDir()}},
		SearchPaths: []string{search},
		OutputRoots: []Root{{Name: "fast", Path: root}},
	})

	full.eventually(t, "the discovered domain", func(s []api.DomainInventory) bool {
		return len(s) == 2
	})

	after, err := full.Output("fast", []string{"ingest"})
	require.NoError(t, err)
	assert.Equal(t, before, after)

	// A discovered domain is a source and never a destination, and its name is a path, so it fails
	// the character-set rule as well as everything else (§7.2, §13).
	_, err = full.Output("fast", []string{found})
	require.Error(t, err)

	// An input mapping's name resolves under the root like any other name rather than resolving to
	// the mapping. Names are flat per node, but that conflict is the server's to reject (§10.6,
	// 10e): this resolver never silently redirects a destination somewhere it was not sent.
	path, err := full.Output("fast", []string{"cameras"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "cameras"), path)
}

// Roots are created at startup so that only the leaf MkdirAll for a domain is ever on the
// establishment path (§6.1, §10.6).
func TestCreateRootsPreCreatesTheDirectories(t *testing.T) {
	base := t.TempDir()
	fast := filepath.Join(base, "does", "not", "exist", "yet")
	inv := rooted(t, Root{Name: "fast", Path: fast})

	require.NoError(t, inv.CreateRoots())

	info, err := os.Stat(fast)
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	// The domain itself is not created here: it is materialised when a target assignment for it
	// is accepted, and forgotten when the last path targeting it goes (§10.6).
	_, err = os.Stat(filepath.Join(fast, "ingest"))
	assert.ErrorIs(t, err, os.ErrNotExist)

	// Idempotent: an agent restart runs this again over directories that already exist.
	require.NoError(t, inv.CreateRoots())
}

// A root that cannot be created is reported at startup rather than as a target worker dying at
// assignment time, where it would look like a fabric problem.
func TestCreateRootsReportsWhatItCouldNotCreate(t *testing.T) {
	base := t.TempDir()
	blocked := filepath.Join(base, "file")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))

	inv := rooted(t, Root{Name: "fast", Path: filepath.Join(blocked, "under-a-file")})

	err := inv.CreateRoots()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `output root "fast"`)
}

// The same rule the resolver is built from, callable before there is an [Inventory] — so an
// operator who overlapped a root with a search path hears about it when the process starts
// (§6.2, §10.6).
func TestValidateRootsIsTheSameRuleWithoutAnInventory(t *testing.T) {
	base := t.TempDir()
	nested := filepath.Join(base, "nested")

	require.NoError(t, ValidateRoots(
		[]Root{{Name: "fast", Path: base}},
		[]Domain{{Name: "cameras", Path: base + "-other"}},
		nil,
	))

	err := ValidateRoots(
		[]Root{{Name: "fast", Path: base + "/"}}, // uncleaned, as a flag value arrives
		nil,
		[]string{nested},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlaps search path")
}

func TestRootsAreWhatRegistrationAdvertises(t *testing.T) {
	fast, bulk := t.TempDir(), t.TempDir()
	inv := rooted(t, Root{Name: "bulk", Path: bulk + "/"}, Root{Name: "fast", Path: fast})

	assert.Equal(t, []Root{{Name: "bulk", Path: bulk}, {Name: "fast", Path: fast}}, inv.Roots(),
		"ordered by name, and paths cleaned")
}

func TestRootConfigurationIsRefusedWhenItIsAmbiguous(t *testing.T) {
	base := t.TempDir()
	nested := filepath.Join(base, "nested")
	sibling := t.TempDir()

	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{
			name: "relative path",
			opts: Options{OutputRoots: []Root{{Name: "fast", Path: "relative/path"}}},
			want: "is not absolute",
		},
		{
			name: "filesystem root",
			opts: Options{OutputRoots: []Root{{Name: "fast", Path: "/"}}},
			want: "the filesystem root is not an output root",
		},
		{
			name: "empty name",
			opts: Options{OutputRoots: []Root{{Name: "", Path: base}}},
			want: "empty",
		},
		{
			name: "name that is not a plain element",
			opts: Options{OutputRoots: []Root{{Name: "../fast", Path: base}}},
			want: "output root name",
		},
		{
			name: "same name twice",
			opts: Options{OutputRoots: []Root{{Name: "fast", Path: base}, {Name: "fast", Path: sibling}}},
			want: "declared twice",
		},
		{
			name: "same path twice",
			opts: Options{OutputRoots: []Root{{Name: "fast", Path: base}, {Name: "bulk", Path: base}}},
			want: "an output root twice",
		},
		{
			name: "root inside another root",
			opts: Options{OutputRoots: []Root{{Name: "fast", Path: base}, {Name: "bulk", Path: nested}}},
			want: "overlap",
		},
		{
			// A root that *is* a domain directory would put output domains inside a domain, and
			// no per-request check can undo that. Distinct from a root above one, which is
			// permitted — see TestARootMayBeTheParentOfAnInputDomain.
			name: "root that is an input mapping",
			opts: Options{
				Domains:     []Domain{{Name: "cameras", Path: base}},
				OutputRoots: []Root{{Name: "fast", Path: base}},
			},
			want: `overlaps domain "cameras"`,
		},
		{
			name: "root under an input mapping",
			opts: Options{
				Domains:     []Domain{{Name: "cameras", Path: base}},
				OutputRoots: []Root{{Name: "fast", Path: nested}},
			},
			want: `overlaps domain "cameras"`,
		},
		{
			name: "root under a search path",
			opts: Options{
				SearchPaths: []string{base},
				OutputRoots: []Root{{Name: "fast", Path: nested}},
			},
			want: "overlaps search path",
		},
		{
			name: "root over a search path",
			opts: Options{
				SearchPaths: []string{nested},
				OutputRoots: []Root{{Name: "fast", Path: base}},
			},
			want: "overlaps search path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Logger = discard()
			_, err := New(tc.opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	// A sibling directory whose path is a string prefix of the root's is not an overlap. This is
	// the same boundary the resolver is careful about, on the configuration side.
	_, err := New(Options{
		Domains:     []Domain{{Name: "cameras", Path: base + "-other"}},
		OutputRoots: []Root{{Name: "fast", Path: base}},
		Logger:      discard(),
	})
	require.NoError(t, err)
}

// **A root may be the parent of an input domain.** One directory holding this node's domains,
// some read and some written, is a reasonable layout and refusing it outright would be refusing a
// legal arrangement to prevent one nameable case — which [Inventory.Output] refuses precisely.
func TestARootMayBeTheParentOfAnInputDomain(t *testing.T) {
	base := t.TempDir()
	cameras := filepath.Join(base, "cameras")

	// The mapping's *name* deliberately differs from its directory's basename, which is what makes
	// this the case the server's name check cannot see.
	inv, err := New(Options{
		Domains:     []Domain{{Name: "cams", Path: cameras}},
		OutputRoots: []Root{{Name: "fast", Path: base}},
	})
	require.NoError(t, err, "a root above an input domain is a legal layout")

	// A sibling under the same root is fine.
	resolved, err := inv.Output("fast", []string{"ingest"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(base, "ingest"), resolved)

	// The input domain's own directory is not. Refused by *path*, so the name it is requested
	// under does not matter: this is the collision the containment permission leaves open, and
	// without it one directory would be an input domain under one name and an output domain under
	// another.
	_, err = inv.Output("fast", []string{"cameras"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `input domain "cams"`)
	assert.Contains(t, err.Error(), "never written to")

	// And the name check still catches the ordinary case, where the names collide too.
	inv, err = New(Options{
		Domains:     []Domain{{Name: "cameras", Path: cameras}},
		OutputRoots: []Root{{Name: "fast", Path: base}},
	})
	require.NoError(t, err)
	_, err = inv.Output("fast", []string{"cameras"})
	assert.Error(t, err)
}
