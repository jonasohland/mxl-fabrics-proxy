package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// "Pinned" and "prefer, but this is acceptable" are one mechanism, spelled two ways on the
// wire (§10.4).
func TestProviderPinAcceptsScalarAndArray(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		body string
		want ProviderPin
	}{
		"scalar":  {`"verbs"`, ProviderPin{ProviderVerbs}},
		"array":   {`["verbs","tcp"]`, ProviderPin{ProviderVerbs, ProviderTCP}},
		"null":    {`null`, nil},
		"empty":   {`[]`, ProviderPin{}},
		"unknown": {`"rocks"`, ProviderPin{Provider("rocks")}}, // decodes; Validate rejects
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var pin ProviderPin
			require.NoError(t, json.Unmarshal([]byte(tc.body), &pin))
			assert.Equal(t, tc.want, pin)
		})
	}

	var pin ProviderPin
	assert.Error(t, json.Unmarshal([]byte(`17`), &pin))
}

func TestProviderPinMarshalsBackToWhatItCameFrom(t *testing.T) {
	t.Parallel()

	for _, body := range []string{`"verbs"`, `["verbs","tcp"]`, `null`} {
		var pin ProviderPin
		require.NoError(t, json.Unmarshal([]byte(body), &pin))

		encoded, err := json.Marshal(pin)
		require.NoError(t, err)
		assert.JSONEq(t, body, string(encoded))
	}
}

func TestProviderPinAllows(t *testing.T) {
	t.Parallel()

	// An empty pin allows everything: the server negotiates in its configured order.
	assert.True(t, ProviderPin(nil).Allows(ProviderTCP))

	pin := ProviderPin{ProviderVerbs, ProviderTCP}
	assert.True(t, pin.Allows(ProviderVerbs))
	assert.True(t, pin.Allows(ProviderTCP))

	// The pin is honoured or the request fails — never substituted (§10.4).
	assert.False(t, pin.Allows(ProviderEFA))
	assert.False(t, ProviderPin{ProviderVerbs}.Allows(ProviderTCP))
}

func TestProviderPinValidateRejectsTypos(t *testing.T) {
	t.Parallel()

	require.NoError(t, ProviderPin{ProviderVerbs, ProviderTCP}.Validate())

	err := ProviderPin{Provider("vebrs")}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vebrs")
}

func validSpec() RequestSpec {
	return RequestSpec{
		Name:         "studio-a-cam1-to-edge",
		Source:       Source{Node: "studio-a", Domain: "cameras", Select: Selector{Flow: "5592a23b"}},
		Destinations: []Destination{{Node: "edge-01", Domain: []string{"ingest"}}},
	}
}

func TestRequestSpecValidate(t *testing.T) {
	t.Parallel()

	require.NoError(t, validSpec().Validate())

	for name, mutate := range map[string]func(*RequestSpec){
		"no name":          func(s *RequestSpec) { s.Name = "" },
		"control char":     func(s *RequestSpec) { s.Name = "cam\n1" },
		"overlong name":    func(s *RequestSpec) { s.Name = string(make([]byte, maxNameLength+1)) },
		"no source node":   func(s *RequestSpec) { s.Source.Node = "" },
		"no source domain": func(s *RequestSpec) { s.Source.Domain = "" },
		"no selector":      func(s *RequestSpec) { s.Source.Select = Selector{} },
		"two selectors": func(s *RequestSpec) {
			s.Source.Select.GroupHint = &GroupHintSelector{Name: "Studio A"}
		},
		"no destination node":   func(s *RequestSpec) { s.Destinations[0].Node = "" },
		"no destination domain": func(s *RequestSpec) { s.Destinations[0].Domain = []string{""} },
		// A destination domain is a directory this API is asking a node to create, so the name
		// rule is structural (§10.6). The agent refuses these independently; the server reports
		// ReasonMalformedDomainName for a stored request that somehow carries one.
		"traversing destination domain": func(s *RequestSpec) { s.Destinations[0].Domain = []string{"../etc"} },
		"nested destination domain":     func(s *RequestSpec) { s.Destinations[0].Domain = []string{"a/b"} },
		"absolute destination domain":   func(s *RequestSpec) { s.Destinations[0].Domain = []string{"/etc"} },
		"lookalike destination domain":  func(s *RequestSpec) { s.Destinations[0].Domain = []string{"іngest"} },
		"malformed root":                func(s *RequestSpec) { s.Destinations[0].Root = "../fast" },
		"unknown provider":              func(s *RequestSpec) { s.Provider = ProviderPin{Provider("infiniband")} },
		"no destinations":               func(s *RequestSpec) { s.Destinations = nil },
		"unknown per-destination provider": func(s *RequestSpec) {
			s.Destinations[0].Provider = ProviderPin{Provider("infiniband")}
		},
		// Two entries naming one (node, domain) are the same path written twice, and they can
		// disagree about the root or the provider — at which point there is no answer to pick.
		"duplicate destination": func(s *RequestSpec) {
			s.Destinations = append(s.Destinations, Destination{Node: "edge-01", Domain: []string{"ingest"}, Root: "bulk"})
		},
		"negative teardown": func(s *RequestSpec) {
			negative := Milliseconds(-1)
			s.IdleTeardown = &negative
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spec := validSpec()
			mutate(&spec)
			assert.Error(t, spec.Validate())
		})
	}
}

// The §9.1 request body must decode as written.
func TestRequestSpecDecodesTheDocumentedBody(t *testing.T) {
	t.Parallel()

	const body = `{
	  "name": "cam1-distribution",
	  "source": {
	    "node": "studio-a",
	    "domain": "cameras",
	    "select": { "flow": "5592a23b-0974-45bb-9388-89ea81c42537" }
	  },
	  "destinations": [
	    { "node": "edge-01", "domain": ["ingest"], "root": "fast" },
	    { "node": "edge-02", "domain": ["studio-a", "cam1"], "root": "fast" },
	    { "node": "archive-01", "domain": ["capture"], "provider": "tcp" }
	  ],
	  "provider": "verbs"
	}`

	var spec RequestSpec
	require.NoError(t, json.Unmarshal([]byte(body), &spec))
	require.NoError(t, spec.Validate())

	assert.Equal(t, "cam1-distribution", spec.Name)
	assert.Equal(t, "5592a23b-0974-45bb-9388-89ea81c42537", spec.Source.Select.Flow)
	assert.Equal(t, ProviderPin{ProviderVerbs}, spec.Provider)
	require.Len(t, spec.Destinations, 3)

	// A destination's own pin overrides the request-level one; the others inherit it. Override
	// rather than intersect, or "verbs here, tcp there" would be a pin conflict instead of the
	// ordinary fan-out it is (§10.4).
	assert.Equal(t, ProviderPin{ProviderVerbs}, spec.ProviderFor(spec.Destinations[0]))
	assert.Equal(t, ProviderPin{ProviderTCP}, spec.ProviderFor(spec.Destinations[2]))
	assert.Equal(t, "fast", spec.Destinations[0].Root)

	// A domain is a list of elements on the wire, never a path. `studio-a/cam1` exists only in a
	// manifest, where the CLI splits it (§10.6).
	assert.Equal(t, []string{"studio-a", "cam1"}, spec.Destinations[1].Domain)
	assert.Equal(t, "studio-a/cam1", spec.Destinations[1].DomainName())
	assert.Equal(t, "edge-02/studio-a/cam1", spec.Destinations[1].Endpoint())

	// The root is optional, and omitting it is the common case: it is only needed by a node
	// advertising more than one (§10.6). It must stay off the wire when unset, so a request that
	// named no root is distinguishable from one that named the empty string.
	spec.Destinations[0].Root = ""
	require.NoError(t, spec.Validate())
	encoded, err := json.Marshal(spec.Destinations[0])
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), `"root"`)
}

// Request embeds RequestSpec, so the spec's fields must stay flat on the wire rather than
// nesting under a "spec" key.
func TestRequestFlattensItsSpec(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(Request{ID: "req-1", RequestSpec: validSpec()})
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &raw))

	assert.Contains(t, raw, "id")
	assert.Contains(t, raw, "name")
	assert.Contains(t, raw, "source")
	assert.Contains(t, raw, "destinations")
	assert.NotContains(t, raw, "RequestSpec")
}
