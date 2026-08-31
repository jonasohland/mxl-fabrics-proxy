package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The server is always upgraded first, so an older agent must be able to decode a newer
// server's payloads by ignoring what it does not recognise (§13.1). [Selector] is the one
// deliberate exception and has its own tests.
func TestUnknownFieldsAreIgnored(t *testing.T) {
	t.Parallel()

	t.Run("assignment", func(t *testing.T) {
		t.Parallel()
		var assignment Assignment
		require.NoError(t, json.Unmarshal([]byte(`{
			"session_id": "s-1",
			"role": "target",
			"domain": {"area": "fast", "elements": ["ingest"]},
			"bandwidth_budget_bps": 1250000000,
			"interface": {"provider": "verbs", "caps_flags": ["REMOTE_WRITE"], "max_message_size": 1048576, "future_knob": true}
		}`), &assignment))
		assert.Equal(t, RoleTarget, assignment.Role)
		assert.Equal(t, ProviderVerbs, assignment.Interface.Provider)
	})

	t.Run("assignment set", func(t *testing.T) {
		t.Parallel()
		var set AssignmentSet
		require.NoError(t, json.Unmarshal([]byte(`{"node":"edge-01","revision":42,"assignments":[],"hint":"x"}`), &set))
		assert.Equal(t, int64(42), set.Revision)
	})

	t.Run("registration", func(t *testing.T) {
		t.Parallel()
		var reg NodeRegistration
		require.NoError(t, json.Unmarshal([]byte(`{
			"node": "edge-01",
			"instance": "1e1d",
			"capabilities": {
				"fabrics": [{"provider":"tcp","fabric":"dc1","address":"10.0.0.1","numa_node":0}],
				"areas": [{"name":"fast","path":"/dev/shm/mxl","read":true,"write":true,"capacity_bytes":0}]
			},
			"zone": "rack-3"
		}`), &reg))
		require.Len(t, reg.Capabilities.Fabrics, 1)
		assert.Equal(t, "dc1", reg.Capabilities.Fabrics[0].Fabric)
		require.Len(t, reg.Capabilities.Areas, 1)
		assert.Equal(t, "fast", reg.Capabilities.Areas[0].Name)
		assert.True(t, reg.Capabilities.Areas[0].Write)
	})

	t.Run("inventory", func(t *testing.T) {
		t.Parallel()
		var inv InventorySnapshot
		require.NoError(t, json.Unmarshal([]byte(`{
			"node": "studio-a",
			"domains": [{"name":"cameras","flows":[{"id":"abc","flow_def":{},"producing":true,"bitrate":42}]}]
		}`), &inv))
		require.Len(t, inv.Domains[0].Flows, 1)
		assert.True(t, inv.Domains[0].Flows[0].Producing)
	})

	t.Run("status", func(t *testing.T) {
		t.Parallel()
		var status StatusSnapshot
		require.NoError(t, json.Unmarshal([]byte(`{
			"node": "edge-01",
			"sessions": [{"session_id":"s-1","role":"target","state":"ready","epoch":"e1","pid":4242}]
		}`), &status))
		require.Len(t, status.Sessions, 1)
		assert.Equal(t, WorkerReady, status.Sessions[0].State)
	})
}

// A flow definition is transported, not interpreted. The destination worker creates its local
// flow from these bytes and the session identity hashes them (§5.4), so nothing may reorder
// keys or drop what this tree does not model.
func TestFlowDefinitionIsCarriedVerbatim(t *testing.T) {
	t.Parallel()

	// Deliberately not in struct-field order, and with a field no type here knows about.
	const flowDef = `{"format":"urn:x-nmos:format:video","id":"abc","$copyright":"x","some_future_field":{"a":[1,2]},"label":"cam1"}`

	var inv InventorySnapshot
	require.NoError(t, json.Unmarshal([]byte(`{"node":"studio-a","domains":[{"name":"cameras","flows":[{"id":"abc","flow_def":`+flowDef+`}]}]}`), &inv))

	original := inv.Domains[0].Flows[0].Definition
	assert.JSONEq(t, flowDef, string(original))

	// Round-tripping through the API preserves key order and content. Insignificant whitespace
	// is compacted, which is what makes a hash over these bytes stable rather than dependent on
	// how the producer formatted flow_def.json.
	assignment := Assignment{SessionID: "s-1", Role: RoleTarget, FlowDef: original}
	encoded, err := json.Marshal(assignment)
	require.NoError(t, err)

	var decoded Assignment
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, string(original), string(decoded.FlowDef))
	assert.Contains(t, string(decoded.FlowDef), "some_future_field")
}

// An initiator assignment carries a blob and an epoch and nothing that needs interpreting; a
// target assignment carries neither, because the target *produces* the epoch (§5.3).
func TestAssignmentRoleShapes(t *testing.T) {
	t.Parallel()

	target := Assignment{
		SessionID: "s-1",
		Role:      RoleTarget,
		Domain:    Domain{Area: "fast", Elements: []string{"ingest"}},
		FlowDef:   json.RawMessage(`{"id":"abc"}`),
		Interface: InterfaceConfig{Provider: ProviderVerbs, CapFlags: []CapFlag{CapRemoteWrite}},
		Fabric:    "ib-fabric-a",
	}
	encoded, err := json.Marshal(target)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), `"epoch"`)
	assert.NotContains(t, string(encoded), `"target_info"`)
	assert.Contains(t, string(encoded), `"fabric":"ib-fabric-a"`)
	assert.True(t, target.Role.IsTarget())

	// **One domain field, the same shape for both roles** (§10.6). *This supersedes a rendered
	// `domain` string plus `output_domain` elements plus a `root` name that only a target used.*
	assert.Contains(t, string(encoded), `"domain":{"area":"fast","elements":["ingest"]}`)
	assert.NotContains(t, string(encoded), `"root"`)
	assert.NotContains(t, string(encoded), `"output_domain"`)

	initiator := Assignment{
		SessionID:  "s-1",
		Role:       RoleInitiator,
		Epoch:      "sha256:...",
		Domain:     Domain{Area: "media", Elements: []string{"cameras"}},
		FlowID:     "abc",
		TargetInfo: `{"id":"1"}`,
		Peer:       &PeerEndpoint{Node: "edge-01", Address: "10.0.2.4", Service: "24012"},
	}
	encoded, err = json.Marshal(initiator)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"epoch"`)
	assert.NotContains(t, string(encoded), `"flow_def"`)
	assert.Contains(t, string(encoded), `"domain":{"area":"media","elements":["cameras"]}`,
		"the same field, carrying the same kind of value, whichever end this is")
	assert.False(t, initiator.Role.IsTarget())
}

// Path embeds PathStatus, so a request's per-path summary and the full path view agree on
// field names rather than being two shapes describing the same thing.
func TestPathFlattensItsStatus(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(Path{
		PathStatus: PathStatus{ID: "p-1", State: StateActive},
		Requests:   []string{"req-1", "req-2"},
	})
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &raw))
	assert.Contains(t, raw, "id")
	assert.Contains(t, raw, "state")
	assert.Contains(t, raw, "requests")
	assert.NotContains(t, raw, "PathStatus")
}

func TestMilliseconds(t *testing.T) {
	t.Parallel()

	assert.Equal(t, Milliseconds(1500), Millis(1500*time.Millisecond))
	assert.Equal(t, 90*time.Second, Milliseconds(90_000).Duration())

	// 0 is the worker's "wait indefinitely" sentinel and must survive both conversions
	// unchanged — a unit conversion is exactly where a sentinel gets lost (WRS §3, §11.1).
	assert.Equal(t, Milliseconds(0), Millis(0))
	assert.Equal(t, time.Duration(0), Milliseconds(0).Duration())

	var assignment Assignment
	require.NoError(t, json.Unmarshal([]byte(`{"idle_timeout_ms":0}`), &assignment))
	assert.Equal(t, Milliseconds(0), assignment.IdleTimeout)
}

func TestErrorFormatting(t *testing.T) {
	t.Parallel()

	err := &Error{Code: CodeNotReady, Message: "settling"}
	assert.Equal(t, "not_ready: settling", err.Error())
	assert.Equal(t, "not_ready", (&Error{Code: CodeNotReady}).Error())
}

// Node names are validated for fleet-wide uniqueness, not for URL safety (§7.1).
func TestPathHelpersEscapeNodeNames(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "/agent/v1/edge-01/assignments", AssignmentsPath("edge-01"))
	assert.Equal(t, "/agent/v1/rack%2Fedge-01/inventory", InventoryPath("rack/edge-01"))
	assert.Equal(t, "/v1/namespaces/nab/requests/a%20b", RequestPath(RequestID{Namespace: "nab", Name: "a b"}))
	assert.Equal(t, "/v1/nodes/edge-01/domains", NodeDomainsPath("edge-01"))
}
