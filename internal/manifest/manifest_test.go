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
  domain: media/cameras
  group_hint: {name: "Studio A:Camera 1"}
destinations:
  - {node: edge-01,    domain: fast/ingest}
  - {node: edge-02,    domain: fast/ingest}
  - {node: archive-01, domain: bulk/capture, provider: tcp}
provider: [verbs, tcp]
labels:
  show: nab

---

name: talkback
source:
  node: studio-a
  domain: media/audio
  flow: 5592a23b-0974-45bb-9388-89ea81c42537
destinations:
  - {node: edge-01, domain: fast/ingest}
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
	assert.Equal(t, "edge-01/fast/ingest", cam1.Destinations[0].Endpoint())
	assert.Equal(t, api.Domain{Area: "fast", Elements: []string{"ingest"}}, cam1.Destinations[0].Domain)

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
source: {node: studio-a, domain: media/cameras, flow: flow-1}
destinations: [{node: edge-01, domain: fast/ingest}]
destinaton: [{node: edge-02, domain: fast/ingest}]
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
			file: "name: a\nsource: {node: s, domain: media/d}\ndestinations: [{node: e, domain: fast/i}]",
			want: "no selector",
		},
		"two selectors": {
			file: "name: a\nsource: {node: s, domain: media/d, flow: f, group_hint: {name: g}}\ndestinations: [{node: e, domain: fast/i}]",
			want: "names both flow and group_hint",
		},
		"group hint with no name": {
			file: "name: a\nsource: {node: s, domain: media/d, group_hint: {type: video}}\ndestinations: [{node: e, domain: fast/i}]",
			want: "group_hint.name is required",
		},
		"no destinations": {
			file: "name: a\nsource: {node: s, domain: media/d, flow: f}",
			want: "at least one destination",
		},
		// The same (node, domain) twice is one path written twice, and the entries can disagree
		// about the root or the provider.
		"duplicate destination": {
			file: "name: a\nsource: {node: s, domain: media/d, flow: f}\ndestinations: [{node: e, domain: fast/i}, {node: e, domain: fast/i}]",
			want: "both name e/fast/i",
		},
		// A destination domain is a directory this API asks a node to create, so the name rule is
		// structural and refused before anything reaches the server (§10.6).
		"destination domain is a path": {
			file: "name: a\nsource: {node: s, domain: media/d, flow: f}\ndestinations: [{node: e, domain: /dev/shm/x}]",
			want: "destinations[0].domain",
		},
		"no name": {
			file: "source: {node: s, domain: media/d, flow: f}\ndestinations: [{node: e, domain: fast/i}]",
			want: "name is required",
		},
		"unknown provider": {
			file: "name: a\nsource: {node: s, domain: media/d, flow: f}\ndestinations: [{node: e, domain: fast/i}]\nprovider: infiniband",
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
source: {node: s, domain: media/d, flow: f}
destinations: [{node: e, domain: fast/i}]
---
name: bad
source: {node: s, domain: media/d}
destinations: [{node: e, domain: fast/i}]
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
source: {node: s, domain: media/d, flow: f}
destinations: [{node: e, domain: fast/i}]
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
	return "name: " + name + "\nsource: {node: s, domain: media/d, flow: f}\ndestinations: [{node: " + dstNode + ", domain: fast/i}]\n"
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

	assert.Equal(t, []string{"first", "second", "fourth"}, docNames(docs),
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

// --- hierarchical domains (§10.6) -------------------------------------------------------------

// `fast/a/b` in the file, `(area, elements)` on the wire. **This is the only parser of a domain
// string in the tree**, which is what lets the wire type, the server and the agent's resolver all
// take structure and never text.
func TestADomainStringSplitsIntoAreaAndElements(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1
source: {node: studio-a, domain: media/cameras, flow: flow-1}
destinations:
  - {node: edge-01, domain: fast/studio-a/cam1}
  - {node: edge-02, domain: bulk/ingest}
`
	docs, err := Parse(strings.NewReader(file), "m.yaml")
	require.NoError(t, err)
	specs, err := Specs(docs)
	require.NoError(t, err)
	require.Len(t, specs, 1)

	assert.Equal(t, api.Domain{Area: "fast", Elements: []string{"studio-a", "cam1"}}, specs[0].Destinations[0].Domain)
	assert.Equal(t, api.Domain{Area: "bulk", Elements: []string{"ingest"}}, specs[0].Destinations[1].Domain,
		"a flat domain is a one-element list inside its area")

	// And it renders back to what was written, because the split is injective — neither an area
	// name nor an element can contain a separator.
	assert.Equal(t, "fast/studio-a/cam1", specs[0].Destinations[0].DomainName())

	// *There used to be a separate `root:` key beside the elements.* It is gone, and a file that
	// still carries one is refused as an unknown key rather than quietly ignored (§10.6).
	_, err = Parse(strings.NewReader("name: a\nsource: {node: s, domain: media/d, flow: f}\ndestinations: [{node: e, domain: fast/i, root: fast}]"), "m.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "root")
}

// ParseDomain is the **one** parser of a domain string in the tree, so every shape that would
// otherwise have to be handled — or silently absorbed — by a second one is refused here (§10.6).
func TestParseDomainRejections(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ domain, want string }{
		// An absolute path is what a raw filesystem path looks like, and accepting one is the
		// thing the whole design exists to prevent.
		"absolute":      {"/dev/shm/mxl/ingest", "is absolute"},
		"trailing":      {"fast/studio-a/", "ends with a separator"},
		"empty element": {"fast/studio-a//cam1", "contains an empty element"},
		// **An area's own directory is not a domain**, so a bare area name names nothing (§10.6).
		"area only":      {"fast", "names only the area"},
		"traversal":      {"fast/studio-a/../../etc", "element 2"},
		"dot":            {"fast/studio-a/./cam1", "element 2"},
		"too deep":       {"fast/a/b/c/d/e/f/g/h/i", "at most 8"},
		"bad area":       {"fa st/cam1", "area"},
		"bad character":  {"fast/studio a/cam1", "element 1"},
		"hidden element": {"fast/studio-a/.hidden", "element 2"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseDomain(tc.domain)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	for _, domain := range []string{"fast/ingest", "fast/studio-a/cam1", "fast/a/b/c/d/e/f/g/h"} {
		t.Run("ok/"+domain, func(t *testing.T) {
			t.Parallel()
			parsed, err := ParseDomain(domain)
			require.NoError(t, err)

			// The first segment is the area and the rest are the elements, and the rendering is
			// injective — which is what lets the rest of the system carry the string.
			assert.Equal(t, domain, parsed.String())
			assert.Equal(t, "fast", parsed.Area)
			assert.Equal(t, strings.Split(domain, "/")[1:], parsed.Elements)
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
	assert.Equal(t, []string{"cam1-distribution", "talkback"}, docNames(docs))

	// The same documents are not a valid *apply*, and say so rather than being half-accepted.
	_, err = Specs(docs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "names.yaml: source names no selector")

	// A drifted document — one whose destinations no longer describe anything real — still
	// deletes, because delete never looks at them.
	docs, err = Parse(strings.NewReader(
		"name: cam1\nsource: {node: gone, domain: gone, flow: f}\ndestinations: [{node: gone, domain: gone}]\n"), "old.yaml")
	require.NoError(t, err)
	assert.Equal(t, []string{"cam1"}, docNames(docs))
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

// --- namespaces (§9.3) --------------------------------------------------------------------

// The namespace is a real property on both sides now. *This supersedes it being a field in the
// file and a reserved label on the wire*, which left two spellings to reconcile and a
// disagreement between them to be refused rather than resolved.
func TestNamespaceIsARealField(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1
namespace: nab
source: {node: studio-a, domain: media/cameras, flow: f-1}
destinations: [{node: edge-01, domain: fast/ingest}]
labels: {show: gala}
`
	docs, err := Parse(strings.NewReader(file), "-")
	require.NoError(t, err)
	specs, err := Specs(docs)
	require.NoError(t, err)
	require.Len(t, specs, 1)

	assert.Equal(t, "nab", specs[0].Namespace)
	assert.Equal(t, map[string]string{"show": "gala"}, specs[0].Labels,
		"nothing is folded into the labels any more")
	assert.Equal(t, api.RequestID{Namespace: "nab", Name: "cam1"}, specs[0].RequestID())
}

// Naming no namespace leaves the spec's field empty and reads as the default. The server writes
// it out on the POST; inventing it here would only mean the file and the wire disagreed about
// what was sent.
func TestNoNamespaceReadsAsTheDefault(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1
source: {node: studio-a, domain: media/cameras, flow: f-1}
destinations: [{node: edge-01, domain: fast/ingest}]
`
	docs, err := Parse(strings.NewReader(file), "-")
	require.NoError(t, err)
	specs, err := Specs(docs)
	require.NoError(t, err)

	assert.Empty(t, specs[0].Labels, "nothing is invented here")
	assert.Equal(t, api.DefaultNamespace, specs[0].NamespaceOrDefault())
}

// `namespace` under `labels:` is an ordinary user label again and means nothing to the server.
// It rides into metrics like any other — where the reserved-name check drops it, since the
// namespace is a metric dimension of its own (§12) — and it does not decide which partition the
// request is in.
func TestTheLabelSpellingIsJustALabelNow(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1
source: {node: studio-a, domain: media/cameras, flow: f-1}
destinations: [{node: edge-01, domain: fast/ingest}]
labels: {namespace: archive}
`
	docs, err := Parse(strings.NewReader(file), "-")
	require.NoError(t, err)
	specs, err := Specs(docs)
	require.NoError(t, err)

	assert.Equal(t, api.DefaultNamespace, specs[0].NamespaceOrDefault(),
		"a label does not put a request in a namespace")
	assert.Equal(t, "archive", specs[0].Labels["namespace"])
}

// The name is checked where the file is read, so `apply --dry-run` on a bad file fails without a
// server — and by the same function the server uses, so the two cannot drift.
func TestNamespaceNameIsValidatedInTheFile(t *testing.T) {
	t.Parallel()

	for _, ns := range []string{"shows/nab", "nab 2026"} {
		file := fmt.Sprintf(`
name: cam1
namespace: %s
source: {node: studio-a, domain: media/cameras, flow: f-1}
destinations: [{node: edge-01, domain: fast/ingest}]
`, ns)
		err := parseAndConvert(strings.NewReader(file), "-")
		require.Error(t, err, "namespace %q", ns)
		assert.Contains(t, err.Error(), "namespace")
	}
}

// --- kind (§9.1) ----------------------------------------------------------------------------

// A document with no `kind:` is a request. That is what every manifest written before kinds
// existed is, and it is what the overwhelming majority of documents will always be.
func TestKindDefaultsToRequest(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1
source: {node: studio-a, domain: media/cameras, flow: f-1}
destinations: [{node: edge-01, domain: fast/ingest}]
`
	docs, err := Parse(strings.NewReader(file), "-")
	require.NoError(t, err)
	require.Len(t, docs, 1)
	assert.Equal(t, KindRequest, docs[0].Kind)
	require.NotNil(t, docs[0].Request)
}

func TestNamespaceDocument(t *testing.T) {
	t.Parallel()

	const file = `
kind: namespace
name: nab
paths: exclusive
description: the NAB show floor
`
	docs, err := Parse(strings.NewReader(file), "-")
	require.NoError(t, err)
	require.Len(t, docs, 1)
	require.Equal(t, KindNamespace, docs[0].Kind)

	spec, err := docs[0].Namespace.Spec()
	require.NoError(t, err)
	assert.Equal(t, api.Namespace{Name: "nab", Paths: api.PathsExclusive, Description: "the NAB show floor"}, spec)
	assert.True(t, spec.Paths.Exclusive())
}

// A namespace document with no `paths:` is stored as `shared` rather than as empty, so that a
// record written by an auto-create and one written by a document that said nothing compare equal
// and a re-apply writes nothing.
func TestNamespaceDocumentDefaultsToShared(t *testing.T) {
	t.Parallel()

	docs, err := Parse(strings.NewReader("kind: namespace\nname: nab\n"), "-")
	require.NoError(t, err)

	spec, err := docs[0].Namespace.Spec()
	require.NoError(t, err)
	assert.Equal(t, api.PathsShared, spec.Paths)
	assert.False(t, spec.Paths.Exclusive())
}

// An unrecognised kind is an error, on the same reasoning as an unrecognised key: the file was
// written against a different binary and nothing in it can be trusted to mean what it says.
func TestUnknownKindIsRefused(t *testing.T) {
	t.Parallel()

	_, err := Parse(strings.NewReader("kind: transport\nname: x\n"), "-")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown kind")
	assert.Contains(t, err.Error(), "namespace")
}

// Strictness follows the kind. `paths:` is meaningful on a namespace and meaningless on a
// request, so it has to be an error on the second — which is only possible because the kind is
// read before the body is decoded.
func TestKeysAreStrictPerKind(t *testing.T) {
	t.Parallel()

	_, err := Parse(strings.NewReader("name: cam1\npaths: exclusive\n"), "-")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "paths")

	_, err = Parse(strings.NewReader("kind: namespace\nname: nab\nsource: {node: a}\n"), "-")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source")
}

// Namespaces are applied before requests whatever order the file is in. The end state does not
// depend on it; the intermediate state does — a request landing ahead of the document that makes
// its namespace exclusive is admitted and then invalidated, which reads as the apply having
// broken something.
func TestApplyOrdersNamespacesFirst(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1
namespace: nab
source: {node: studio-a, domain: media/cameras, flow: f-1}
destinations: [{node: edge-01, domain: fast/ingest}]
---
kind: namespace
name: nab
paths: exclusive
`
	docs, err := Parse(strings.NewReader(file), "-")
	require.NoError(t, err)
	require.Len(t, docs, 2)
	assert.Equal(t, KindRequest, docs[0].Kind, "file order is preserved by Parse")

	ordered := SortForApply(docs)
	assert.Equal(t, KindNamespace, ordered[0].Kind)
	assert.Equal(t, KindRequest, ordered[1].Kind)

	// And delete goes the other way, because a namespace delete is refused while a request
	// references it.
	reversed := SortForDelete(docs)
	assert.Equal(t, KindRequest, reversed[0].Kind)
	assert.Equal(t, KindNamespace, reversed[1].Kind)
}

// Two requests of one name in two namespaces are two objects, not a duplicate. That is the whole
// point of scoping names to the namespace (§9.3).
func TestOneNameInTwoNamespacesIsNotADuplicate(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1
namespace: nab
source: {node: studio-a, domain: media/cameras, flow: f-1}
destinations: [{node: edge-01, domain: fast/ingest}]
---
name: cam1
namespace: archive
source: {node: studio-a, domain: media/cameras, flow: f-1}
destinations: [{node: edge-02, domain: fast/ingest}]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "m.yaml")
	require.NoError(t, os.WriteFile(path, []byte(file), 0o600))

	docs, err := Load([]string{path})
	require.NoError(t, err)
	assert.Len(t, docs, 2)
}

// docNames is what the old Names() helper did, kept here because several tests read a file for
// nothing but the names in it — which is exactly what `delete -f` does.
func docNames(docs []Document) []string {
	names := make([]string, 0, len(docs))
	for _, doc := range docs {
		names = append(names, doc.Name())
	}
	return names
}

// --- domain labels (§10.7) ---------------------------------------------------------------------

// **A scalar `domain:` is a name and a map is a label set**, which is the whole of the
// disambiguation and needs no marker key: YAML already tells the two apart (§9.1).
func TestSourceDomainIsAScalarOrAMap(t *testing.T) {
	t.Parallel()

	const file = `
name: by-name
source: {node: studio-a, domain: media/cameras, flow: f-1}
destinations: [{node: edge-01, domain: fast/ingest}]
---
name: by-label
source:
  node: studio-a
  domain: {role: cameras, site: studio-a}
  flow: f-1
destinations: [{node: edge-01, domain: fast/ingest}]
`
	docs, err := Parse(strings.NewReader(file), "-")
	require.NoError(t, err)
	specs, err := Specs(docs)
	require.NoError(t, err)
	require.Len(t, specs, 2)

	// The scalar goes through [ParseDomain] — the one parser of a domain string in the tree — and
	// lands as the same structured value a destination carries (§10.6).
	assert.Equal(t, api.DomainSelectorKindName, specs[0].Source.Domain.Kind())
	assert.Equal(t, api.Domain{Area: "media", Elements: []string{"cameras"}}, *specs[0].Source.Domain.Name)

	assert.Equal(t, api.DomainSelectorKindLabels, specs[1].Source.Domain.Kind())
	assert.Equal(t, map[string]string{"role": "cameras", "site": "studio-a"}, specs[1].Source.Domain.Labels)
}

// `domain: {}` is a label selector with no keys, not a third thing needing a rule of its own — and
// it is refused, because an empty map matches every domain on the node and is reachable by
// omission rather than by intent (§9.1, §10.7).
func TestAnEmptySourceDomainMapIsRefused(t *testing.T) {
	t.Parallel()

	const file = `
name: wide
source: {node: studio-a, domain: {}, flow: f-1}
destinations: [{node: edge-01, domain: fast/ingest}]
`
	err := parseAndConvert(strings.NewReader(file), "-")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "every domain on the node")
}

func TestDomainDocument(t *testing.T) {
	t.Parallel()

	const file = `
kind: domain
node: studio-a
domain: media/cameras
labels: {role: cameras, name: cameras}
`
	docs, err := Parse(strings.NewReader(file), "-")
	require.NoError(t, err)
	require.Len(t, docs, 1)
	require.Equal(t, KindDomain, docs[0].Kind)
	assert.Equal(t, "studio-a:media/cameras", docs[0].Name(),
		"a domain name means nothing without the node it is on")

	node, write, err := docs[0].Domain.Write()
	require.NoError(t, err)
	assert.Equal(t, "studio-a", node)
	assert.Equal(t, api.Domain{Area: "media", Elements: []string{"cameras"}}, write.Domain)

	// **An apply**, always: the manifest is the desired set, and it owns the keys it declares
	// (§9.1). A patch is what the `label` verb sends.
	assert.Equal(t, "apply", write.Kind())
	assert.Equal(t, map[string]string{"role": "cameras", "name": "cameras"}, write.Apply)

	// A document declaring no labels is an apply of nothing — which removes whatever it declared
	// last time — rather than a document that does nothing.
	docs, err = Parse(strings.NewReader("kind: domain\nnode: studio-a\ndomain: media/cameras\n"), "-")
	require.NoError(t, err)
	_, write, err = docs[0].Domain.Write()
	require.NoError(t, err)
	assert.Equal(t, "apply", write.Kind())
	assert.NotNil(t, write.Apply)
	assert.Empty(t, write.Apply)
}

// Domains are applied after namespaces and **before requests**, whatever order the file is in: a
// request landing ahead of the labels its selector matches expands to nothing and then to
// something, which reads as the apply having broken and then fixed itself (§9.1).
func TestApplyOrdersDomainsBetweenNamespacesAndRequests(t *testing.T) {
	t.Parallel()

	const file = `
name: cam1
source: {node: studio-a, domain: {role: cameras}, flow: f-1}
destinations: [{node: edge-01, domain: fast/ingest}]
---
kind: domain
node: studio-a
domain: media/cameras
labels: {role: cameras}
---
kind: namespace
name: nab
`
	docs, err := Parse(strings.NewReader(file), "-")
	require.NoError(t, err)

	ordered := SortForApply(docs)
	assert.Equal(t, []Kind{KindNamespace, KindDomain, KindRequest},
		[]Kind{ordered[0].Kind, ordered[1].Kind, ordered[2].Kind})

	// And delete goes the other way, so a request is cancelled before the namespace it lives in.
	reversed := SortForDelete(docs)
	assert.Equal(t, []Kind{KindRequest, KindDomain, KindNamespace},
		[]Kind{reversed[0].Kind, reversed[1].Kind, reversed[2].Kind})
}
