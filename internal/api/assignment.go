package api

import "encoding/json"

// Role is which end of a session a worker is (§3).
//
// MXL's naming, which runs opposite to the control plane's: the *initiator* is the sending
// side on the source node, and the *target* is the receiving side on the destination node.
// The fabric connection goes source → destination; the information (`target_info`) goes
// destination → source first; and the request is usually authored by neither. Keeping those
// three straight is the main source of bugs in this domain.
//
// [worker.Spec] uses this type rather than defining its own, so there is one spelling of
// "target" from the API through to the worker's config.
type Role string

const (
	// RoleInitiator is the sending worker, on the source node. Reads the local flow and
	// RDMA-writes to the peer. Config key `target: false` (WRS §1).
	RoleInitiator Role = "initiator"

	// RoleTarget is the receiving worker, on the destination node. Binds node:service, creates
	// the local flow from the definition it is given, and is passive. Config key
	// `target: true`.
	RoleTarget Role = "target"
)

// IsTarget reports whether the role maps onto the worker's `target: true`.
func (r Role) IsTarget() bool { return r == RoleTarget }

// AssignmentSet is the response to GET /agent/v1/{node}/assignments: the **complete** set of
// workers this node should be running (§4.1, §9.2).
//
// The agent is a reconciler, not an RPC state machine. It never receives "start this" or "stop
// that" — only a full desired set, which it makes its running processes match. Every operation
// is idempotent and every message carries full state for its scope, which is what makes the
// epoch handling tractable and HA possible at all.
//
// # Empty is a value, not a fallback
//
// The agent's rule is fatal if read literally, because **a failed poll and an empty assignment
// set look identical**. The naive implementation reconciles to zero when it cannot reach the
// server — the control plane going down stops all media, which for live video is exactly
// backwards.
//
// So: the agent acts only on a set it successfully retrieved (§4.2), and "empty" must be
// something it can only *learn*, never *infer*. Two consequences bind this type:
//
//   - The server returns [CodeNotReady] — never a zero-valued AssignmentSet — while it is
//     settling or has no observed state to reconcile against (§7.3, plan §4.2). The agent
//     treats that exactly like a transport failure and skips the reconcile entirely.
//   - A decoder must not be able to manufacture one of these from a failure. In the client,
//     the poll either produces a non-nil set or an error; the two must not be able to collapse
//     into the same call.
//
// This matters beyond a server outage: if the store is restored from an empty backup or has
// its prefix wiped, every agent polls *successfully*, receives an empty set, and correctly
// tears down every worker in the fleet. Fail-static protects against no answer; only the
// server's own not-ready discipline protects against a successful wrong one.
type AssignmentSet struct {
	Node string `json:"node"`

	// Revision is the store revision this set was computed at, and the cursor the agent passes
	// to its next long poll. It advances monotonically; a replica must never serve a set at a
	// revision below the client's cursor, or an agent polling through a load balancer
	// oscillates between two versions and restarts workers on every swing (plan §4.5).
	Revision int64 `json:"revision"`

	Assignments []Assignment `json:"assignments"`
}

// Assignment is one worker the node should be running (§5.3).
//
// Everything here is derived state, recomputed from scratch on every reconcile. The agent must
// *not* diff this object as a whole to decide whether to restart a worker — see §7.3 and the
// note on [Assignment.Fabric]. It keys on session ID, role, and the fields that materially
// affect the worker; an incidental difference such as a re-serialised flow definition or a
// reordered JSON field must never restart a healthy worker.
type Assignment struct {
	SessionID string `json:"session_id"`
	Role      Role   `json:"role"`

	// Epoch is the target incarnation this assignment is for (§5.2).
	//
	// Absent for a target assignment: the target *produces* the epoch. Required for an
	// initiator assignment, and the reconcile rule on the source side is one line — if the
	// epoch I am running for this session differs from the epoch I am assigned, tear down and
	// start a new worker with the new TargetInfo.
	//
	// An initiator is never assigned before an epoch has been reported (§5.3). The ordering is
	// mandatory, not an optimisation: openFlow fails outright if the flow does not exist, and
	// the connect loop waits a long time — indefinitely, before the worker gained a
	// configurable connect timeout — on an unreachable target.
	Epoch string `json:"epoch,omitempty"`

	// Domain is a domain **name**, never a path. The server has no business knowing where a
	// domain lives on disk, and accepting a path here would make the API a remote
	// arbitrary-filesystem-write primitive (§7.2, §13).
	//
	// How the agent resolves it depends on the role, and the asymmetry is the point (§10.6). For
	// an initiator it is an *input* domain, looked up among the domains this agent observes —
	// configured mappings and search-path finds alike — because a source is by definition
	// something the node already has. For a target it is an *output* domain, resolved from
	// [Assignment.Root] and [Assignment.OutputDomain] with no reference to observed state at all.
	//
	// A **string in both roles**, because the two roles' domains are different kinds of thing and
	// only one of them decomposes. An initiator's is an observed name, which for a discovered
	// domain is an absolute path and is opaque; a target's is a rooted element list, and its
	// elements travel in [Assignment.OutputDomain]. What this field carries in both cases is the
	// name — what the agent logs, what the `domain` metric label reports, and what identifies the
	// domain fleet-wide.
	Domain string `json:"domain"`

	// OutputDomain is the element list [Assignment.Domain] renders from, for [RoleTarget] only.
	//
	// The agent resolves the destination directory from *this* rather than by splitting the name,
	// so that nothing outside the CLI's manifest parser ever turns a domain string back into path
	// elements. The security-critical resolver takes structure and never text (§10.6, §13).
	//
	// Redundant with [Assignment.Domain] by construction, not by coincidence: the server derives
	// both from one [Destination.Domain], and [DomainPath] is injective, so they cannot disagree.
	OutputDomain []string `json:"output_domain,omitempty"`

	// Root names which output root [Assignment.Domain] is materialised under, and is meaningful
	// for [RoleTarget] only (§10.6).
	//
	// It must be a root the destination agent advertised in its registration; the agent checks
	// that again itself, and the duplication is the one place in the tree where checking the
	// control plane's work earns its keep (§13).
	//
	// Version skew (§13.1) resolves in the safe direction here. Servers upgrade first, so the
	// case is a new server and an old agent: the old agent ignores this field, falls back to
	// looking the name up among its input mappings, does not find it, and reports a failed
	// session with a reason. Legible, and it fails closed — which is why this needs no protocol
	// bump. The reverse, a new agent against an old server, is already refused outright.
	Root string `json:"root,omitempty"`

	// FlowID is the flow to read, for an initiator. The same ID exists on both nodes after
	// replication — that is the point (§3).
	FlowID string `json:"flow_id"`

	// FlowDef is the source flow's definition, verbatim, for a target: it cannot create its
	// local flow without it (§5.3 step 2). Absent for an initiator, which opens an existing
	// local flow by ID.
	FlowDef json.RawMessage `json:"flow_def,omitempty"`

	// Interface is the negotiated configuration, identical on both ends of the session by
	// necessity — the library performs no negotiation of its own (§10.3).
	Interface InterfaceConfig `json:"interface"`

	// Fabric is the negotiated fabric label (§10.1), and the agent selects its own local bind
	// address by looking up the (Interface.Provider, Fabric) pair among its attachments.
	//
	// It is here because the provider alone is not enough to pick an address: a node can hold
	// two verbs attachments on different InfiniBand fabrics, and binding the wrong one produces
	// a target that comes up perfectly and an initiator that never connects.
	Fabric string `json:"fabric"`

	// TargetInfo is the serialised blob for exactly [Assignment.Epoch], initiator only.
	//
	// Opaque, and treated as such: the only thing done with it locally is to recompute the
	// epoch from it and check that against Epoch before starting anything (§5.3 step 6), which
	// catches a truncated or mismatched blob before it reaches a worker that would silently
	// fail to move data.
	TargetInfo string `json:"target_info,omitempty"`

	// Peer is where the far end is, initiator only.
	//
	// The worker does not need it — the initiator dials the address embedded in TargetInfo, and
	// its own `node`/`service` config keys are its *local* bind (WRS §3). It is carried for
	// diagnostics: when a connect loop spins, the first question is which endpoint it is
	// spinning against, and that is otherwise buried inside an opaque blob.
	Peer *PeerEndpoint `json:"peer,omitempty"`

	// NoNetworkLatencyMeasurement must match on both ends or the target reports garbage latency
	// with no error at all (WRS §5.3). Under the legacy design coordinating that across two
	// hosts was the supervisor's problem; now the server configures both ends of every session
	// from one place, so it is a session-level field rather than per-side config (§5.5).
	NoNetworkLatencyMeasurement bool `json:"no_network_latency_measurement"`

	// SchedPrio is the SCHED_FIFO priority for the transfer loop, or nil to leave scheduling
	// alone. Only ever set for a node that advertised the capability (§10.2).
	SchedPrio *int `json:"sched_prio,omitempty"`

	// IdleTimeout is the worker's no-grain timeout, where 0 means wait indefinitely — the same
	// sentinel the worker uses (WRS §3).
	//
	// Long or infinite is the normal setting, and it is what makes PAUSED a real steady state:
	// with the old hardcoded 10 s, a session whose source simply has no producer would enter a
	// permanent ~13 s restart cycle, and because every target restart changes the epoch, each
	// cycle would cost a report, a recomputed assignment and an initiator restart on another
	// node — forever, per idle flow (§11.1).
	IdleTimeout Milliseconds `json:"idle_timeout_ms"`

	// ConnectTimeout bounds the initiator's connect loop, 0 meaning indefinitely (WRS §3).
	ConnectTimeout Milliseconds `json:"connect_timeout_ms,omitempty"`

	// Labels are the requesting user's labels, applied to this worker's metrics (§12).
	Labels map[string]string `json:"labels,omitempty"`
}

// PeerEndpoint is the far end of a session, for diagnostics.
type PeerEndpoint struct {
	Node    string `json:"node"`
	Address string `json:"address"`
	Service string `json:"service"`
}
