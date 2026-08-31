package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `selector_test.go` re-pointed, because the type is that type's discipline copied deliberately
// (§10.7). Every property asserted there is asserted here for the same reason.

func TestDomainSelectorExactlyOneKind(t *testing.T) {
	t.Parallel()

	name := SelectDomain(Domain{Area: "media", Elements: []string{"cameras"}})
	require.NoError(t, name.Validate())
	assert.Equal(t, DomainSelectorKindName, name.Kind())

	labels := SelectLabels(map[string]string{"role": "cameras"})
	require.NoError(t, labels.Validate())
	assert.Equal(t, DomainSelectorKindLabels, labels.Kind())

	// Neither, and both.
	assert.Error(t, DomainSelector{}.Validate())
	both := DomainSelector{Name: name.Name, Labels: map[string]string{"role": "cameras"}}
	assert.Error(t, both.Validate())
	assert.Empty(t, both.Kind())
}

// The deliberate exception to "never fail on an unrecognised key" (doc.go): an unknown kind is an
// error, because ignoring one silently *widens* the selection to whatever remained — the wrong
// direction to fail in for something that moves uncompressed video.
func TestDomainSelectorRefusesAnUnknownKind(t *testing.T) {
	t.Parallel()

	var s DomainSelector
	err := json.Unmarshal([]byte(`{"regexp": "cam.*"}`), &s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown kind")
	assert.Contains(t, err.Error(), `"regexp"`)

	// And an unknown kind *beside* a known one is still an error, rather than the known one
	// quietly winning.
	err = json.Unmarshal([]byte(`{"labels": {"role": "cameras"}, "regexp": "cam.*"}`), &s)
	assert.ErrorContains(t, err, "unknown kind")
}

// **An empty label map is refused by the validator**, not merely by the manifest's
// scalar-versus-map rule (§10.7). It matches every domain on the node, and it is reachable by
// omission rather than by intent: `domain: {}` and a `domain:` whose keys were all deleted are
// both easy to write.
func TestAnEmptyLabelSelectorIsRefused(t *testing.T) {
	t.Parallel()

	empty := DomainSelector{Labels: map[string]string{}}
	err := empty.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "every domain on the node")

	// Through the decoder too — the syntax is perfectly well-formed, so only the validator refuses
	// it.
	var decoded DomainSelector
	assert.ErrorContains(t, json.Unmarshal([]byte(`{"labels": {}}`), &decoded), "every domain")

	// And [DomainSelector.Matches] never says yes to one, so a record that somehow got past the
	// validator still matches nothing rather than everything.
	assert.False(t, empty.Matches(map[string]string{"role": "cameras"}))
	assert.False(t, DomainSelector{}.Matches(nil))
}

// Validate is reached through **both** Marshal and Unmarshal, because a selector built in Go — by
// the CLI, by a test, by the Kubernetes adapter — bypasses the decoder entirely.
func TestDomainSelectorValidatesInBothDirections(t *testing.T) {
	t.Parallel()

	_, err := json.Marshal(DomainSelector{})
	assert.Error(t, err, "an invalid selector must not reach the wire")

	_, err = json.Marshal(DomainSelector{Labels: map[string]string{}})
	assert.Error(t, err)

	// A valid one round-trips, and the direct form carries the structured value rather than a
	// string — which is what keeps §10.6's "parsed at exactly one boundary" rule intact.
	encoded, err := json.Marshal(SelectDomain(Domain{Area: "media", Elements: []string{"cameras"}}))
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":{"area":"media","elements":["cameras"]}}`, string(encoded))

	var back DomainSelector
	require.NoError(t, json.Unmarshal(encoded, &back))
	assert.Equal(t, "media/cameras", back.String())
}

// **Equality, every key ANDed** (§10.7) — and specifically not a subset test, which is the
// distinction that decides whether a two-key selector means "either" or "both".
func TestLabelMatchingIsEqualityAndAnded(t *testing.T) {
	t.Parallel()

	both := SelectLabels(map[string]string{"role": "cameras", "site": "studio-a"})
	one := SelectLabels(map[string]string{"role": "cameras"})

	// A domain carrying both keys matches the one-key selector...
	assert.True(t, one.Matches(map[string]string{"role": "cameras", "site": "studio-a"}))
	// ...and a domain carrying only one does **not** match the two-key selector. That is the case
	// that tells equality-ANDed apart from a subset test.
	assert.False(t, both.Matches(map[string]string{"role": "cameras"}))
	assert.True(t, both.Matches(map[string]string{"role": "cameras", "site": "studio-a", "extra": "x"}))

	// Values are compared whole and case-sensitively: no wildcard, no value grammar of any kind.
	assert.False(t, one.Matches(map[string]string{"role": "Cameras"}))
	assert.False(t, one.Matches(map[string]string{"role": "camerasx"}))
	assert.False(t, one.Matches(map[string]string{"role": ""}))
	assert.False(t, one.Matches(nil), "a domain with no labels matches nothing")
}

// The direct form is held to the domain grammar, so a selector cannot name something a
// destination could not (§10.6).
func TestANamedDomainSelectorIsHeldToTheGrammar(t *testing.T) {
	t.Parallel()

	for _, domain := range []Domain{
		{Area: "media"},                                  // an area's own directory is not a domain
		{Elements: []string{"cameras"}},                  // names no area
		{Area: "media", Elements: []string{"../etc"}},    // traversal
		{Area: "media", Elements: []string{"a/b"}},       // a separator inside an element
		{Area: "media/inner", Elements: []string{"cam"}}, // a separator inside the area
	} {
		assert.Error(t, SelectDomain(domain).Validate(), "domain %q", domain)
	}
}
