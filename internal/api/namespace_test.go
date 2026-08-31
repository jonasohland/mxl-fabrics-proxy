package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The zero value is `shared`, and that is the whole of the default: refcounting is the base model
// and forbidding it is the special case a namespace opts into (§9.3).
func TestTheZeroPathPolicyIsShared(t *testing.T) {
	t.Parallel()

	var policy PathPolicy
	assert.False(t, policy.Exclusive())
	assert.False(t, PathsShared.Exclusive())
	assert.True(t, PathsExclusive.Exclusive())

	// And an unset one normalises onto its meaning, so a record written by an auto-create and one
	// written by a document that spelled `shared` compare equal — without which every apply of
	// such a document would look like a change and write (§8.3).
	assert.True(t, Namespace{Name: "nab"}.SameAs(Namespace{Name: "nab", Paths: PathsShared}))
	assert.Equal(t, PathsShared, Namespace{Name: "nab"}.Normalise().Paths)
	assert.False(t, Namespace{Name: "nab"}.SameAs(Namespace{Name: "nab", Paths: PathsExclusive}))
}

// An unknown policy is refused rather than defaulted. Reading it as `shared` would silently
// *widen* what is permitted, which is the wrong direction to fail in for a rule about whether two
// requests may move media over one edge — the same argument [Selector] makes about unknown kinds.
func TestAnUnknownPathPolicyIsRefused(t *testing.T) {
	t.Parallel()

	require.NoError(t, PathPolicy("").Validate())
	require.NoError(t, PathsShared.Validate())
	require.NoError(t, PathsExclusive.Validate())

	err := PathPolicy("mostly").Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown policy")
}

func TestNamespaceNameGrammar(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"nab", "NAB-2026", "show_1", "default"} {
		assert.NoError(t, ValidNamespace(ok), "namespace %q", ok)
	}
	for _, bad := range []string{"", "shows/nab", "nab 2026", "nab.2026", "nab:2026", strings.Repeat("x", 300)} {
		assert.Error(t, ValidNamespace(bad), "namespace %q", bad)
	}
}

// The namespace is a real property on the wire, not a label (§9.3). Omitted, it reads as the
// default — for a spec the write path has not touched yet, which is every manifest on its way in.
func TestRequestNamespaceIsAField(t *testing.T) {
	t.Parallel()

	var spec RequestSpec
	require.NoError(t, json.Unmarshal([]byte(`{
		"name": "cam1",
		"namespace": "nab",
		"source": {"node": "studio-a", "domain": {"name": {"area": "media", "elements": ["cameras"]}}, "select": {"flow": "f-1"}},
		"destinations": [{"node": "edge-01", "domain": {"area": "fast", "elements": ["ingest"]}}]
	}`), &spec))

	assert.Equal(t, "nab", spec.Namespace)
	assert.Equal(t, RequestID{Namespace: "nab", Name: "cam1"}, spec.RequestID())
	assert.Equal(t, "nab/cam1", spec.RequestID().String())
	require.NoError(t, spec.Validate())

	spec.Namespace = ""
	assert.Equal(t, DefaultNamespace, spec.NamespaceOrDefault())
	assert.Equal(t, "default/cam1", spec.RequestID().String())
	require.NoError(t, spec.Validate(), "empty is legal and means the default")

	// Omitted on the way out too, so a request that named no namespace round-trips to the bytes
	// it was written with.
	bare := spec
	bare.Namespace = ""
	encoded, err := json.Marshal(bare)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "namespace")

	spec.Namespace = "shows/nab"
	assert.Error(t, spec.Validate(), "a spelled-out namespace is held to the grammar")
}
