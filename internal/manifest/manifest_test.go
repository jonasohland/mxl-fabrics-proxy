package manifest

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// parseAndConvert is Parse followed by Specs, which is what `apply` does. Both stages can reject a
// document and the tests below do not care which did.
func parseAndConvert(r io.Reader, origin string) error {
	docs, err := Parse(r, origin)
	if err != nil {
		return err
	}
	_, err = Specs(docs)
	return err
}

// The worked example from the plan (M8b) must decode to exactly the requests it describes. It is
// also the README's example and `loopback.yaml`'s shape, so this is the format's contract.
func TestTheDocumentedManifestDecodes(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1-distribution
source:
  node: studio-a
  domain: cameras
  group_hint: {name: "Studio A:Camera 1"}
destinations:
  - {node: edge-01,    domain: ingest,  root: fast}
  - {node: edge-02,    domain: ingest,  root: fast}
  - {node: archive-01, domain: capture, provider: tcp}
provider: [verbs, tcp]
labels:
  show: nab

---

name: talkback
source:
  node: studio-a
  domain: audio
  flow: 5592a23b-0974-45bb-9388-89ea81c42537
destinations:
  - {node: edge-01, domain: ingest, root: fast}
idle_teardown_ms: 0
`

	docs, err := Parse(strings.NewReader(file), "studio-a.yaml")
	require.NoError(t, err)
	specs, err := Specs(docs)
	require.NoError(t, err)
	require.Len(t, specs, 2)

	cam1 := specs[0]
	assert.Equal(t, "cam1-distribution", cam1.Name)
	assert.Equal(t, "studio-a", cam1.Source.Node)

	// The selector is flattened in the file and a tagged union on the wire (§9.1). Omitting
	// `type` selects every flow sharing the name, which is how a camera's video and audio are
	// replicated together.
	require.NotNil(t, cam1.Source.Select.GroupHint)
	assert.Equal(t, "Studio A:Camera 1", cam1.Source.Select.GroupHint.Name)
	assert.Empty(t, cam1.Source.Select.GroupHint.Type)

	require.Len(t, cam1.Destinations, 3)
	assert.Equal(t, "edge-01/ingest", cam1.Destinations[0].Endpoint())
	assert.Equal(t, "fast", cam1.Destinations[0].Root)

	// A bare string and a list are the same mechanism, spelled two ways (§10.4), and a
	// destination's own pin overrides the document-level one.
	assert.Equal(t, api.ProviderPin{api.ProviderVerbs, api.ProviderTCP}, cam1.Provider)
	assert.Equal(t, api.ProviderPin{api.ProviderTCP}, cam1.ProviderFor(cam1.Destinations[2]))
	assert.Equal(t, api.ProviderPin{api.ProviderVerbs, api.ProviderTCP}, cam1.ProviderFor(cam1.Destinations[0]))

	assert.Equal(t, map[string]string{"show": "nab"}, cam1.Labels)

	talkback := specs[1]
	assert.Equal(t, "5592a23b-0974-45bb-9388-89ea81c42537", talkback.Source.Select.Flow)
	require.NotNil(t, talkback.IdleTeardown)
	assert.Zero(t, talkback.IdleTeardown.Duration(), "0 disables teardown: keep a bursty feed hot")
}

// A typo'd key must be an error naming the key. A declarative file whose misspelled field
// silently does nothing is the failure this format exists to prevent — the opposite call from
// §5.2's unknown-field rule, and for a reason that does not carry over: this file is written by a
// human against this binary, not by an independently-versioned upstream.
func TestAnUnknownKeyIsAnError(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1
source: {node: studio-a, domain: cameras, flow: flow-1}
destinations: [{node: edge-01, domain: ingest}]
destinaton: [{node: edge-02, domain: ingest}]
`
	_, err := Parse(strings.NewReader(file), "typo.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "destinaton")
	assert.Contains(t, err.Error(), "typo.yaml")
}

func TestDocumentRejections(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		file string
		want string
	}{
		"no selector": {
			file: "name: a\nsource: {node: s, domain: d}\ndestinations: [{node: e, domain: i}]",
			want: "no selector",
		},
		"two selectors": {
			file: "name: a\nsource: {node: s, domain: d, flow: f, group_hint: {name: g}}\ndestinations: [{node: e, domain: i}]",
			want: "names both flow and group_hint",
		},
		"group hint with no name": {
			file: "name: a\nsource: {node: s, domain: d, group_hint: {type: video}}\ndestinations: [{node: e, domain: i}]",
			want: "group_hint.name is required",
		},
		"no destinations": {
			file: "name: a\nsource: {node: s, domain: d, flow: f}",
			want: "at least one destination",
		},
		// The same (node, domain) twice is one path written twice, and the entries can disagree
		// about the root or the provider.
		"duplicate destination": {
			file: "name: a\nsource: {node: s, domain: d, flow: f}\ndestinations: [{node: e, domain: i}, {node: e, domain: i, root: bulk}]",
			want: "both name e/i",
		},
		// A destination domain is a directory this API asks a node to create, so the name rule is
		// structural and refused before anything reaches the server (§10.6).
		"destination domain is a path": {
			file: "name: a\nsource: {node: s, domain: d, flow: f}\ndestinations: [{node: e, domain: /dev/shm/x}]",
			want: "destinations[0].domain",
		},
		"no name": {
			file: "source: {node: s, domain: d, flow: f}\ndestinations: [{node: e, domain: i}]",
			want: "name is required",
		},
		"unknown provider": {
			file: "name: a\nsource: {node: s, domain: d, flow: f}\ndestinations: [{node: e, domain: i}]\nprovider: infiniband",
			want: "unknown provider",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := parseAndConvert(strings.NewReader(tc.file), "m.yaml")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// Which document failed has to be in the message: a file with eight requests in it is otherwise
// a search.
func TestErrorsNameTheDocument(t *testing.T) {
	t.Parallel()

	const file = `
name: good
source: {node: s, domain: d, flow: f}
destinations: [{node: e, domain: i}]
---
name: bad
source: {node: s, domain: d}
destinations: [{node: e, domain: i}]
`
	err := parseAndConvert(strings.NewReader(file), "studio-a.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "studio-a.yaml document 2")
}

// A trailing `---` is a habit, not a mistake.
func TestEmptyDocumentsAreSkipped(t *testing.T) {
	t.Parallel()

	const file = `---
name: a
source: {node: s, domain: d, flow: f}
destinations: [{node: e, domain: i}]
---
`
	docs, err := Parse(strings.NewReader(file), "m.yaml")
	require.NoError(t, err)
	require.Len(t, docs, 1)
}

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

func doc(name, dstNode string) string {
	return "name: " + name + "\nsource: {node: s, domain: d, flow: f}\ndestinations: [{node: " + dstNode + ", domain: i}]\n"
}

// -f takes a file, a directory or "-", and is repeatable. A directory is flat and sorted: a
// manifest directory is a set of files an operator maintains, and recursing would sweep up
// whatever else lives under it.
func TestLoadAcceptsFilesAndDirectories(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write(t, dir, "b.yaml", doc("second", "e2"))
	write(t, dir, "a.yml", doc("first", "e1"))
	write(t, dir, "notes.txt", "ignored, not a manifest")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o755))
	write(t, filepath.Join(dir, "nested"), "c.yaml", doc("third", "e3"))

	loose := t.TempDir()
	extra := write(t, loose, "extra.yaml", doc("fourth", "e4"))

	docs, err := Load([]string{dir, extra})
	require.NoError(t, err)

	assert.Equal(t, []string{"first", "second", "fourth"}, Names(docs),
		"sorted within a directory, in argument order across them, and not recursive")
}

// Two documents naming one request means the second silently replaces the first, which is never
// what was meant — and it is exactly what a careless copy-paste produces.
func TestLoadRejectsDuplicateNames(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write(t, dir, "a.yaml", doc("cam1", "e1"))
	write(t, dir, "b.yaml", doc("cam1", "e2"))

	_, err := Load([]string{dir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cam1")
}

func TestLoadRequiresAPath(t *testing.T) {
	t.Parallel()

	_, err := Load(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-f")
}

// --- hierarchical output domains (§10.6) ------------------------------------------------------

// `a/b` in the file, elements on the wire. **This is the only parser of a domain string in the
// tree**, which is what lets the wire type, the server and the agent's resolver all take structure
// and never text.
func TestADomainPathSplitsIntoElements(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1
source: {node: studio-a, domain: cameras, flow: flow-1}
destinations:
  - {node: edge-01, domain: studio-a/cam1, root: fast}
  - {node: edge-02, domain: ingest}
`
	docs, err := Parse(strings.NewReader(file), "m.yaml")
	require.NoError(t, err)
	specs, err := Specs(docs)
	require.NoError(t, err)
	require.Len(t, specs, 1)

	assert.Equal(t, []string{"studio-a", "cam1"}, specs[0].Destinations[0].Domain)
	assert.Equal(t, []string{"ingest"}, specs[0].Destinations[1].Domain, "a flat name is a one-element list")

	// And it renders back to what was written, because the split is injective — no element can
	// contain a separator.
	assert.Equal(t, "studio-a/cam1", specs[0].Destinations[0].DomainName())
}

func TestDomainPathRejections(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ domain, want string }{
		// An absolute path is what a raw filesystem path looks like, and accepting one is the
		// thing the whole design exists to prevent. It is also how a *discovered* domain is named,
		// so refusing it is what keeps a requested domain and a discovered one from colliding.
		"absolute":       {"/dev/shm/mxl/ingest", "is absolute"},
		"trailing":       {"studio-a/", "ends with a separator"},
		"empty element":  {"studio-a//cam1", "contains an empty element"},
		"traversal":      {"studio-a/../../etc", "element 2"},
		"dot":            {"studio-a/./cam1", "element 2"},
		"too deep":       {"a/b/c/d/e/f/g/h/i", "at most 8"},
		"bad character":  {"studio a/cam1", "element 1"},
		"hidden element": {"studio-a/.hidden", "element 2"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseDomain(tc.domain)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	for _, domain := range []string{"ingest", "studio-a/cam1", "a/b/c/d/e/f/g/h"} {
		t.Run("ok/"+domain, func(t *testing.T) {
			t.Parallel()
			elements, err := ParseDomain(domain)
			require.NoError(t, err)
			assert.Equal(t, domain, strings.Join(elements, "/"))
		})
	}
}

// **A document carrying nothing but a name is a complete instruction to delete.**
//
// `apply` needs every document to be a valid request; `delete` needs only what the file *named*.
// Requiring the rest would break "delete what this file created" exactly when it is most wanted —
// when the file has drifted from what is deployed, or was only ever written to record the names.
func TestANameIsEnoughToIdentifyARequest(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1-distribution
---
name: talkback
`
	docs, err := Parse(strings.NewReader(file), "names.yaml")
	require.NoError(t, err)
	assert.Equal(t, []string{"cam1-distribution", "talkback"}, Names(docs))

	// The same documents are not a valid *apply*, and say so rather than being half-accepted.
	_, err = Specs(docs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "names.yaml: source names no selector")

	// A drifted document — one whose destinations no longer describe anything real — still
	// deletes, because delete never looks at them.
	docs, err = Parse(strings.NewReader(
		"name: cam1\nsource: {node: gone, domain: gone, flow: f}\ndestinations: [{node: gone, domain: gone}]\n"), "old.yaml")
	require.NoError(t, err)
	assert.Equal(t, []string{"cam1"}, Names(docs))
}

// Two things are still checked on every document, whichever verb asked for it.
func TestEveryDocumentIsCheckedForNameAndUnknownKeys(t *testing.T) {
	t.Parallel()

	// A document that identifies nothing.
	_, err := Parse(strings.NewReader("labels: {show: nab}\n"), "m.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")

	// A typo is a typo in a file you are deleting from too — and it may mean the name you meant is
	// somewhere this did not look.
	_, err = Parse(strings.NewReader("name: cam1\nnaem: cam2\n"), "m.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "naem")

	// And the duplicate-name rule holds for a name-only file, where it means the file disagrees
	// with itself about what there is to remove.
	dir := t.TempDir()
	write(t, dir, "a.yaml", "name: cam1\n---\nname: cam1\n")
	_, err = Load([]string{dir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cam1")
}

// --- namespaces (§7b) ---------------------------------------------------------------------

// The namespace is a field of its own in the file and a label on the wire. The field exists
// because burying the one key the server acts on in a free-text map hides it; the label is what
// `--prune` selects on, which is the whole reason a namespace needs no second mechanism.
func TestNamespaceBecomesALabel(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1
namespace: nab
source: {node: studio-a, domain: cameras, flow: f-1}
destinations: [{node: edge-01, domain: ingest}]
labels: {show: gala}
`
	docs, err := Parse(strings.NewReader(file), "-")
	require.NoError(t, err)
	specs, err := Specs(docs)
	require.NoError(t, err)
	require.Len(t, specs, 1)

	assert.Equal(t, map[string]string{"show": "gala", api.LabelNamespace: "nab"}, specs[0].Labels)
	assert.Equal(t, "nab", specs[0].Namespace())
}

// Naming no namespace leaves the spec alone. The server writes the default in on the POST, so
// filling it here as well would only mean the file and the wire disagreed about what was sent.
func TestNoNamespaceLeavesTheLabelsUntouched(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1
source: {node: studio-a, domain: cameras, flow: f-1}
destinations: [{node: edge-01, domain: ingest}]
`
	docs, err := Parse(strings.NewReader(file), "-")
	require.NoError(t, err)
	specs, err := Specs(docs)
	require.NoError(t, err)

	assert.Empty(t, specs[0].Labels, "nothing is invented here")
	assert.Equal(t, api.DefaultNamespace, specs[0].Namespace(), "but a reader still gets an answer")
}

// A manifest written before the field existed spelled it under labels. It still works and still
// means the same thing.
func TestTheLabelSpellingStillWorks(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1
source: {node: studio-a, domain: cameras, flow: f-1}
destinations: [{node: edge-01, domain: ingest}]
labels: {namespace: archive}
`
	docs, err := Parse(strings.NewReader(file), "-")
	require.NoError(t, err)
	specs, err := Specs(docs)
	require.NoError(t, err)
	assert.Equal(t, "archive", specs[0].Namespace())
}

// Both spellings agreeing is fine; disagreeing is refused rather than resolved. There is no
// defensible winner, and the value decides which requests may not overlap and what --prune
// catches.
func TestBothNamespaceSpellingsMustAgree(t *testing.T) {
	t.Parallel()

	const doc = `
name: cam1
namespace: %s
source: {node: studio-a, domain: cameras, flow: f-1}
destinations: [{node: edge-01, domain: ingest}]
labels: {namespace: archive}
`
	require.NoError(t, parseAndConvert(strings.NewReader(fmt.Sprintf(doc, "archive")), "-"),
		"the same value twice is redundant, not wrong")

	err := parseAndConvert(strings.NewReader(fmt.Sprintf(doc, "nab")), "-")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `namespace is "nab" but labels.namespace is "archive"`)
	assert.Contains(t, err.Error(), `namespace is "nab" but labels.namespace is "archive"`)
}

// The name is checked where the file is read, so `apply --dry-run` on a bad file fails without a
// server — and by the same function the server uses, so the two cannot drift.
func TestNamespaceNameIsValidatedInTheFile(t *testing.T) {
	t.Parallel()

	for _, ns := range []string{"shows/nab", "nab 2026", `""`} {
		file := fmt.Sprintf(`
name: cam1
namespace: %s
source: {node: studio-a, domain: cameras, flow: f-1}
destinations: [{node: edge-01, domain: ingest}]
`, ns)
		err := parseAndConvert(strings.NewReader(file), "-")
		require.Error(t, err, "namespace %q", ns)
		assert.Contains(t, err.Error(), "namespace")
	}
}
