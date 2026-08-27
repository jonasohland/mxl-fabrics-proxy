// Package api holds the wire types for both of mxl-replicator's HTTP surfaces: the user API
// under /v1 and the agent API under /agent/v1 (§9).
//
// One package serves both directions on purpose. The server encodes what the agent decodes
// and vice versa, and a type defined twice is a type that drifts. Clients (internal/client,
// the importer, and eventually xpt) speak these types too.
//
// # Rules this package lives by
//
// **Every new field is additive and ignorable by an older agent** (§13.1). The server is
// always upgraded first, so an agent one or more versions behind must be able to decode a
// newer server's payloads by ignoring what it does not know. Concretely: never remove a
// field, never repurpose one, never change a field's type, and never make decoding fail on an
// unrecognised key. The one deliberate exception is [Selector], where rejecting the
// unrecognised is the whole point — see selector.go.
//
// **Stdlib only.** Nothing here imports mxl-utils, the store, or anything else in the tree.
// The wire contract should be readable and testable without dragging in mmap'd flows, and
// the agent converts between mxl-utils' types and these at the edge.
//
// **Flow definitions are carried as verbatim bytes.** [json.RawMessage], never a decoded
// struct that gets re-serialised. Two reasons, both load-bearing: the destination worker
// creates its local flow from these bytes and must reproduce the source definition exactly,
// including fields no Go struct in this tree knows about; and the session identity hashes
// them (§5.4, §7.3), so a re-serialisation that reorders keys would look like a different
// flow and rebuild a healthy session.
//
// **Durations are milliseconds**, as [Milliseconds]. That is the worker's own vocabulary
// (`idle_timeout_ms`, `connect_timeout_ms` — WRS §3), so a value can travel from the server
// through an assignment into a worker config with no unit conversion on the way.
//
// # Vocabulary
//
// The names here follow §3, and the two directions genuinely do run opposite to each other:
// the fabric connection goes source → destination, the information (`target_info`) goes
// destination → source first, and the request is usually authored by neither. [RoleInitiator]
// is the *sending* side and [RoleTarget] is the *receiving* side, which is MXL's naming and
// the reverse of who initiates the request.
package api

// ProtocolVersion is the version of the control-plane wire protocol implemented here.
//
// Version-skew gating is on *this*, not on the build version (plan §4.1). A hard refusal keyed
// on build version turns any HA rolling upgrade of combined-role nodes into a partial outage:
// upgrading a combined instance upgrades both roles at once, so a newer agent reaching an
// older server is unavoidable during the roll. A newer agent than server is a warning and a
// metric; only a differing protocol major is a refusal.
//
// Bump this when a change is *not* additive — which, per the rules above, should be close to
// never.
const ProtocolVersion = 1
