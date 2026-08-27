package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectorDecodesOneKind(t *testing.T) {
	t.Parallel()

	var flow Selector
	require.NoError(t, json.Unmarshal([]byte(`{"flow":"5592a23b-0974-45bb-9388-89ea81c42537"}`), &flow))
	assert.Equal(t, SelectorKindFlow, flow.Kind())
	assert.Equal(t, "5592a23b-0974-45bb-9388-89ea81c42537", flow.Flow)

	var hint Selector
	require.NoError(t, json.Unmarshal([]byte(`{"group_hint":{"name":"Studio A:Camera 1","type":"video"}}`), &hint))
	assert.Equal(t, SelectorKindGroupHint, hint.Kind())
	require.NotNil(t, hint.GroupHint)
	assert.Equal(t, "Studio A:Camera 1", hint.GroupHint.Name)
	assert.Equal(t, "video", hint.GroupHint.Type)
}

// The rejection is the invariant: without it, "adding a selector kind cannot change the
// meaning of an existing request" is not a real property (§9.1).
func TestSelectorRejectsAnythingButExactlyOneKind(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"two kinds":     `{"flow":"abc","group_hint":{"name":"Studio A"}}`,
		"no kinds":      `{}`,
		"null":          `null`,
		"empty flow":    `{"flow":""}`,
		"unknown kind":  `{"label":"cam1"}`,
		"known+unknown": `{"flow":"abc","label":"cam1"}`,
		"empty name":    `{"group_hint":{"type":"video"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var s Selector
			assert.Error(t, json.Unmarshal([]byte(body), &s))
		})
	}
}

// An unknown kind must name itself, because the operator-facing failure is "your client is
// newer than this server" and that is only obvious if the error says which kind it choked on.
func TestSelectorUnknownKindErrorNamesIt(t *testing.T) {
	t.Parallel()

	var s Selector
	err := json.Unmarshal([]byte(`{"regexp":"cam.*"}`), &s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"regexp"`)
	assert.Contains(t, err.Error(), "group_hint")
}

// A Selector built in Go bypasses the decoder entirely — the importer and the Kubernetes
// adapter both do that — so an invalid one must not be able to reach the wire either.
func TestSelectorMarshalValidates(t *testing.T) {
	t.Parallel()

	_, err := json.Marshal(Selector{Flow: "abc", GroupHint: &GroupHintSelector{Name: "Studio A"}})
	assert.Error(t, err)

	_, err = json.Marshal(Selector{})
	assert.Error(t, err)

	encoded, err := json.Marshal(Selector{Flow: "abc"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"flow":"abc"}`, string(encoded))
}

func TestSelectorRoundTrips(t *testing.T) {
	t.Parallel()

	for _, original := range []Selector{
		{Flow: "5592a23b-0974-45bb-9388-89ea81c42537"},
		{GroupHint: &GroupHintSelector{Name: "Studio A:Camera 1", Type: "video"}},
		{GroupHint: &GroupHintSelector{Name: "Studio A:Camera 1"}},
	} {
		encoded, err := json.Marshal(original)
		require.NoError(t, err)

		var decoded Selector
		require.NoError(t, json.Unmarshal(encoded, &decoded))
		assert.Equal(t, original, decoded)
	}
}

func TestGroupHintSelectorMatches(t *testing.T) {
	t.Parallel()

	observed := GroupHint{Name: "Studio A:Camera 1", Type: "video"}

	// Omitting the type selects every flow sharing the name — how a camera's video and audio
	// are replicated together (§9.1).
	assert.True(t, GroupHintSelector{Name: "Studio A:Camera 1"}.Matches(observed))
	assert.True(t, GroupHintSelector{Name: "Studio A:Camera 1", Type: "video"}.Matches(observed))

	assert.False(t, GroupHintSelector{Name: "Studio A:Camera 1", Type: "audio"}.Matches(observed))
	assert.False(t, GroupHintSelector{Name: "Studio A:Camera 2"}.Matches(observed))

	// The name is matched exactly, case included: two group names differing only in case are
	// two group names.
	assert.False(t, GroupHintSelector{Name: "studio a:camera 1"}.Matches(observed))
}
