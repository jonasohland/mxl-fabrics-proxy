package api

import "encoding/json"

// InventorySnapshot is POST /agent/v1/{node}/inventory: everything the agent observes in its
// mapped and discovered domains (§6, §9.2).
//
// A **full snapshot**, never a delta. Deltas need sequencing, gap detection and resync paths;
// snapshots need none of that, and at realistic fleet sizes they are small. This is the same
// level-triggered discipline the rest of the system runs on (§4.1).
//
// Inventory replaces what the legacy proxy fetched peer-to-peer over HTTP
// (GET /v1/flows{domain}?id=...) — with the server aggregating a fleet-wide inventory there is
// no agent-to-agent control-plane traffic left at all.
type InventorySnapshot struct {
	Node     string            `json:"node"`
	Instance string            `json:"instance"`
	Domains  []DomainInventory `json:"domains"`
}

// DomainInventory is one domain and the flows currently in it.
type DomainInventory struct {
	// Domain is the fleet-wide identity: `(area, elements)`, assigned by the innermost containing
	// area (§10.6). The local path is deliberately absent — it is agent-local, it is reported once
	// at registration for diagnostics, and it has no business in a per-heartbeat snapshot.
	//
	// **Structured rather than rendered**, so that nothing on the server has to parse a domain
	// string back into its parts: the agent computes the identity once, from its own area table,
	// and the structure travels. `Domain.String()` is what a metric label, a path identity and an
	// error message carry (§10.6).
	Domain Domain `json:"domain"`

	// *There used to be a `Configured` flag here, mirroring the one on a registration's domain
	// mapping.* It is **gone rather than merely descriptive** (§6): it was the security bit before
	// an area carried its own grants, separating an explicitly mapped domain from one a scan
	// found — and with mappings removed there is nothing left for it to distinguish. A domain is a
	// place, and every one this node observes is reported the same way (§10.6).

	Flows []FlowInventory `json:"flows"`
}

// FlowInventory is one observed flow.
type FlowInventory struct {
	ID string `json:"id"`

	// Definition is the flow's definition, carried as the verbatim bytes of flow_def.json.
	//
	// Not optional: the destination worker cannot create its local flow without it (§5.3 step
	// 2). Not decoded and re-encoded either — the destination flow must reproduce the source
	// definition exactly, including fields no struct in this tree knows about, and the session
	// identity hashes these bytes (§5.4).
	//
	// Passing through the API compacts insignificant whitespace and changes nothing else: key
	// order and content survive. That is the property the session-identity hash depends on —
	// the hash is stable regardless of how the producer formatted flow_def.json, and it changes
	// when, and only when, the definition does.
	Definition json.RawMessage `json:"flow_def"`

	// GroupHint is the parsed urn:x-nmos:tag:grouphint/v1.0 tag, absent when the flow has none
	// or it is malformed. This is what a group-hint selector matches against (§9.1) — parsed
	// agent-side, where mxl-utils already does it, so the server never has to know the tag URN.
	GroupHint *GroupHint `json:"group_hint,omitempty"`

	// Replicated reports that **this node's own target worker is writing this flow** — that this
	// project put it here (§6, §10.6).
	//
	// It is the signal the self-amplification guard runs on, and it is what keeps a label selector
	// from matching this project's own output (§10.7). *This supersedes a directory-granular
	// proxy for the same thing — "under an output root" — which was blunt: a domain holding one
	// replicated flow beside nine local ones was entirely invisible as a source.*
	//
	// The agent cannot be wrong about it: it is the process that started the worker. It is derived
	// from **running workers rather than from assignments**, which is what makes §10.6's safety
	// argument hold — provenance and production go absent together, so the window in which a
	// replicated flow reports false is also a window in which it is not advancing, and §11.1's
	// admission rule starts nothing over it.
	//
	// Low-churn by construction — it changes when a target starts or stops — so it costs §6's
	// compare-before-send discipline nothing.
	//
	// Reported to operators as well as consumed by the matcher: it reaches `GET /v1/flows` and
	// `describe domain`, because a selector that silently skips a flow is otherwise undiagnosable
	// (§9.1).
	Replicated bool `json:"replicated,omitempty"`

	// Producing reports whether the flow is being written to, and it is deliberately a coarse,
	// hysteretic boolean rather than a head index (§11.1).
	//
	// Three things about it matter:
	//
	// It drives *admission*: the server holds a path in WAITING and starts no workers at all
	// until the source is actually being produced, so requesting a camera that is not currently
	// live costs nothing. It also drives long-idle teardown, and — on the destination flow —
	// it is the ground truth behind ACTIVE versus PAUSED (§11).
	//
	// It is derived from head-index *deltas across samples*, not from LastWriteTime. The
	// timestamp looks more convenient, but it is TAI nanoseconds and only means anything if the
	// host's TAI offset is configured; a delta needs no clock at all.
	//
	// It must be hysteretic, and a raw head index must never appear in this snapshot. Inventory
	// is a full snapshot written to the store, so a field that changes every frame would make
	// every snapshot differ and turn inventory into a per-heartbeat write stream — trading the
	// churn §11.1 exists to eliminate for a slower version of the same thing. Rate and head
	// index belong in metrics (§12).
	Producing bool `json:"producing"`
}

// FlowEntry is one flow in the fleet-wide inventory, addressed by where it is.
type FlowEntry struct {
	Node   string `json:"node"`
	Domain string `json:"domain"`

	FlowInventory
}

// Address returns the flow's (node, domain, flow-id) address (§3).
func (f FlowEntry) Address() FlowAddress {
	return FlowAddress{Node: f.Node, Domain: f.Domain, Flow: f.ID}
}

// FlowList is GET /v1/flows.
type FlowList struct {
	Flows []FlowEntry `json:"flows"`
}
