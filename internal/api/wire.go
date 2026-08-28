package api

import (
	"fmt"
	"time"
)

// Milliseconds is a duration on the wire, in whole milliseconds.
//
// Milliseconds rather than a Go duration string or a float of seconds because that is the
// worker's unit (WRS §3, `idle_timeout_ms` and `connect_timeout_ms`), so a timeout travels
// from a server flag through an assignment into a worker config without a unit conversion
// anywhere along the way — and a unit conversion is exactly where a "0 means forever"
// sentinel gets lost.
type Milliseconds int64

// Millis converts a Go duration, truncating toward zero.
func Millis(d time.Duration) Milliseconds { return Milliseconds(d / time.Millisecond) }

// Duration converts back.
func (m Milliseconds) Duration() time.Duration { return time.Duration(m) * time.Millisecond }

func (m Milliseconds) String() string { return m.Duration().String() }

// Error is the body of every non-2xx response on both APIs.
//
// Code is machine-readable and stable; Message is for humans and may change freely. Clients
// switch on Code — in particular [CodeNotReady], which the agent must treat exactly like a
// failed poll (§4.2, plan §4.2).
type Error struct {
	Code    ErrorCode         `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ErrorCode identifies a failure class without parsing prose.
type ErrorCode string

const (
	// CodeInvalidRequest is a malformed or semantically impossible request body. For a
	// replication request that is *valid but not yet satisfiable*, the request is accepted and
	// its status is WAITING instead (§7.2) — the split between the two is the point.
	CodeInvalidRequest ErrorCode = "invalid_request"

	// CodeUnauthorized is a missing or wrong bearer token (§13).
	CodeUnauthorized ErrorCode = "unauthorized"

	// CodeNotFound is an unknown request, node or path.
	CodeNotFound ErrorCode = "not_found"

	// CodeNodeClaimed is a second agent claiming a node name whose liveness lease is still
	// held by another instance (§7.1). Loud on purpose: it is a copy-pasted config or an
	// overlapping rollout, and both claimants would otherwise start workers, fight over ports
	// and write duplicates into the destination flow.
	CodeNodeClaimed ErrorCode = "node_claimed"

	// CodeNotReady is the server declining to answer because it has not settled (§7.3) or has
	// no observed state to reconcile against (plan §4.2).
	//
	// This is the most important code in the list. The assignments endpoint must return it
	// rather than an empty [AssignmentSet], and the agent must treat it exactly like a
	// transport failure: skip the reconcile entirely. "Empty" must be a value the agent can
	// only learn, never infer — an empty set that actually means "I don't know yet" tears down
	// every worker in the fleet.
	CodeNotReady ErrorCode = "not_ready"

	// CodeReregister is a report from an agent whose liveness lease is gone — expired during a
	// partition, revoked, or lost with the store's contents.
	//
	// It is not a teardown signal. The agent registers again and keeps its workers running
	// while it does: losing a lease says the fleet has forgotten this node, not that its media
	// should stop (§4.2, §7.1).
	CodeReregister ErrorCode = "reregister"

	// CodeVersionSkew is an agent whose protocol major differs from the server's (§13.1).
	CodeVersionSkew ErrorCode = "version_skew"

	// CodeInternal is anything unclassified.
	CodeInternal ErrorCode = "internal"
)

// ReasonCode explains an INVALID request, or why a path is not progressing (§7.2, §10.3).
//
// Machine-readable because the three negotiation failures are different operator problems and
// a UI should be able to tell them apart without matching on English. The accompanying prose
// carries the specifics (which fabric labels, which providers).
type ReasonCode string

const (
	// --- INVALID: needs user action, never resolves by itself (§7.2) ---

	// ReasonDomainNotMapped is a destination domain that is not an explicitly configured
	// mapping on the destination agent.
	//
	// **No longer emitted (§10.6).** A destination is a name materialised under an output root
	// now, not an input mapping, so the four codes below replace this one. Retained because it
	// is a value older servers produced and a client switching on it should keep working.
	ReasonDomainNotMapped ReasonCode = "domain_not_mapped"

	// ReasonNoOutputRoot is a destination node advertising no output root at all, and therefore
	// not a replication destination (§10.6).
	//
	// This and the three below are what carry the single most important invariant in the design
	// (§13): a destination is always a name inside an operator-configured root, never a path
	// from the API, which is what stops this being a remote arbitrary-filesystem-write primitive
	// on every node in the fleet. It holds regardless of what authentication is configured.
	//
	// Distinct from [ReasonUnknownOutputRoot] because they are different operator problems: this
	// one is a node that was never configured to receive, that one is a typo or a request aimed
	// at the wrong node.
	ReasonNoOutputRoot ReasonCode = "no_output_root"

	// ReasonUnknownOutputRoot is a request naming an output root the destination node does not
	// advertise.
	ReasonUnknownOutputRoot ReasonCode = "unknown_output_root"

	// ReasonAmbiguousOutputRoot is a request naming no root against a node advertising more than
	// one. The candidates are in the accompanying prose; the server never picks one (§10.6).
	ReasonAmbiguousOutputRoot ReasonCode = "ambiguous_output_root"

	// ReasonMalformedDomainName is a destination domain name that is not a single plain path
	// element ([ValidDomainName]).
	//
	// Ordinarily unreachable from the user API, which refuses the same names at POST with a 400.
	// It exists for the reconcile path, which re-validates stored requests: one written directly
	// into the store, or stored before the rule existed, must fail here rather than reaching an
	// agent.
	ReasonMalformedDomainName ReasonCode = "malformed_domain_name"

	// ReasonDomainNameInUse is a destination domain name that already means something else on
	// that node — an input mapping, a discovered domain, or an output domain another request is
	// materialising under a different root.
	//
	// Names are flat per node (§10.6), and this is not tidiness: the assignment, the path
	// identity, the session identity and the `domain` metric label all carry a single string, so
	// two roots holding an "ingest" each would be two paths with one address. Resolved
	// oldest-first like the other conflicts, so a new request never invalidates a path that is
	// probably already carrying media.
	ReasonDomainNameInUse ReasonCode = "domain_name_in_use"

	// ReasonDomainPathInUse is a destination domain whose *name* is free on that node but whose
	// resolved directory is one the node already maps as an input domain.
	//
	// Reachable only because an output root is permitted to be an ancestor of an input mapping
	// (§10.6) — `-m cams=/dev/shm/mxl/cameras` under root `fast=/dev/shm/mxl` collides on the
	// path while the names differ. Kept distinct from [ReasonDomainNameInUse] because the
	// diagnosis and the fix are different: that one is two things called one name, this one is
	// two names for one directory, and fixing it means renaming the output domain or re-mapping
	// the input rather than picking a different root.
	ReasonDomainPathInUse ReasonCode = "domain_path_in_use"

	// ReasonSameEndpoint is a source and destination with the same (node, domain).
	ReasonSameEndpoint ReasonCode = "same_endpoint"

	// ReasonNodeNotRegistered is a source or destination node no agent has ever registered.
	//
	// INVALID rather than WAITING, and the distinction is deliberate: a registration is created
	// by the agent itself and is durable, so an unregistered name is a typo or a node that was
	// never deployed — something only a user can resolve. A *registered* node whose agent is
	// merely down is [ReasonAgentNotLeased], which is WAITING and resolves by itself.
	ReasonNodeNotRegistered ReasonCode = "node_not_registered"

	// ReasonNoSharedFabric is two nodes with no fabric label in common: they may both offer
	// verbs and simply be on different InfiniBand fabrics (§10.1).
	ReasonNoSharedFabric ReasonCode = "no_shared_fabric"

	// ReasonNoSharedProvider is a shared fabric label but no provider offered on it by both.
	ReasonNoSharedProvider ReasonCode = "no_shared_provider"

	// ReasonNoSharedCapability is a viable fabric and provider whose capability intersection
	// contains neither REMOTE_WRITE nor SEND_RECEIVE (§10.3).
	ReasonNoSharedCapability ReasonCode = "no_shared_capability"

	// ReasonPinNotViable is an explicitly pinned provider that is not among the viable pairs.
	// The request fails; the provider is never substituted (§10.4).
	ReasonPinNotViable ReasonCode = "pin_not_viable"

	// ReasonSchedPrioUnavailable is sched_prio requested on a node without the capability.
	ReasonSchedPrioUnavailable ReasonCode = "sched_prio_unavailable"

	// ReasonFlowConflict is a destination (node, domain) that already holds this flow ID from
	// a different source. Two producers into one flow ID corrupts the ring buffer.
	ReasonFlowConflict ReasonCode = "flow_conflict"

	// ReasonLoop is A→B plus B→A for one flow ID. Chains (A→B→C) are fine and useful.
	ReasonLoop ReasonCode = "loop"

	// ReasonNamespaceOverlap is two requests in one namespace expanding onto the same path.
	//
	// Unlike the conflicts above it is not a corruption: the path is refcounted and works
	// perfectly well held twice. It is refused because a namespace is a *partition* — the
	// property a UI relies on to render a request set as a matrix, where clearing one cell
	// stops exactly the paths in it. Two requests sharing an edge breaks that, and the
	// operator's own fix (narrow one selector, or move one request to another namespace) is
	// not something the server can pick for them.
	//
	// Across namespaces sharing stays legal and is the supported way to express fan-in.
	ReasonNamespaceOverlap ReasonCode = "namespace_overlap"

	// --- WAITING: waiting for something that may plausibly appear (§7.2) ---

	// ReasonFlowNotFound is a source flow not currently observed in the source domain. For a
	// group-hint selector it also covers "matched nothing" (§9.1).
	ReasonFlowNotFound ReasonCode = "flow_not_found"

	// ReasonAgentNotLeased is a source or destination agent that is not currently alive.
	ReasonAgentNotLeased ReasonCode = "agent_not_leased"

	// ReasonSourceIdle is a source flow that exists but is not being produced.
	//
	// It accompanies [StatePaused], not [StateWaiting], whether or not workers are running:
	// admission holds a dormant flow before any worker starts, and the long-idle threshold stops
	// the workers of one that has gone quiet, and those are the same condition at two different
	// ages — the source is not sending. WAITING would claim the flow is not visible, which is
	// false, and PAUSED is exactly the distinction an operator needs at 3am: the plumbing is
	// fine, nobody is writing (§11, §11.1).
	ReasonSourceIdle ReasonCode = "source_idle"

	// --- Established, but not moving media (§11) ---

	// ReasonWorkerRestarts is a session flapping: restart count over the threshold in the
	// window. Classified from restart rate and time-to-death, never from an exit code (§15.1).
	ReasonWorkerRestarts ReasonCode = "worker_restarts"

	// ReasonFabricGone is a session whose negotiated fabric is no longer advertised by both
	// ends. The session fails rather than silently re-negotiating onto a slower provider
	// (§10.4).
	ReasonFabricGone ReasonCode = "fabric_gone"
)
