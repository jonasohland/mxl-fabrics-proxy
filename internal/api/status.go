package api

import "time"

// State is the status vocabulary of §11, used for requests, paths and sessions alike.
//
// Each value carries a human-readable reason and the identity of whatever reported it, and
// status is visible at every level: request → path → session → worker.
type State string

const (
	// StateWaiting means the flow is not visible in the system. No workers are running, and it
	// resolves by itself if the flow appears.
	StateWaiting State = "WAITING"

	// StateInvalid needs user action and never resolves by itself. Always carries a reason
	// (§7.2). Rejecting at request time is the point — a bad request must not sit in WAITING
	// looking like it might come good.
	StateInvalid State = "INVALID"

	// StateEstablishing covers the whole setup phase: session created, target assigned, epoch
	// reported, initiator connecting (§5.3).
	//
	// Deliberately not split. The sub-steps are useful in a reason string and in logs, but
	// they are not states an operator makes a different decision about — everything from
	// "session created" to "first grain received" is one condition: coming up.
	StateEstablishing State = "ESTABLISHING"

	// StatePaused means established end to end, but no media is moving: the head index is not
	// advancing.
	//
	// The valuable one. It separates the two questions an operator has to distinguish at 3am —
	// *is the plumbing broken* or *is the source not producing?* — which look identical from a
	// "no media at the destination" alarm and have completely different owners. PAUSED says
	// the fabric connection is fine and nobody is writing on the far end.
	//
	// It is a real steady state, not a transient: it survives because the worker's no-grain
	// timeout is configured long or infinite, and beyond a longer threshold the workers are
	// torn down while the path stays PAUSED (§11.1).
	StatePaused State = "PAUSED"

	// StateActive means media is flowing: the *destination* flow's head index is advancing.
	//
	// Determined from the flow, never from the worker (§11). A worker can report healthy
	// transfers while producing a flow nothing can read; worker metrics stay useful as
	// corroboration and for rate, not as the state signal.
	StateActive State = "ACTIVE"

	// StateDegraded means established but flapping: restart count over a threshold in a
	// window. Classified from restart rate and time-to-death, never from an exit code (§15.1).
	StateDegraded State = "DEGRADED"

	// StateFailed means repeated permanent-looking failure. Still retried, but surfaced
	// loudly.
	StateFailed State = "FAILED"
)

// WorkerState is what an agent reports about one worker it is running (§5.3, §9.2).
//
// Kept distinct from [State] on purpose. [State] is the operator-facing vocabulary and is
// derived by the server, which is the only party that can see both ends of a session and the
// destination flow's liveness. An agent can only report what its own process is doing, and
// "my worker is up" is emphatically not "media is moving" — only the flow can tell you that
// (§11).
type WorkerState string

const (
	// WorkerStarting is spawned but not yet usable: for a target, target-info.json has not
	// appeared; for an initiator, the process is still coming up.
	WorkerStarting WorkerState = "starting"

	// WorkerReady is up and healthy. For a target it additionally means target_info is
	// available and the epoch in the same report is computed from it (§5.3 step 4).
	WorkerReady WorkerState = "ready"

	// WorkerFailed is dead and being restarted with backoff. Restarts and StartedAt in the
	// same report are what the server classifies DEGRADED/FAILED from.
	WorkerFailed WorkerState = "failed"
)

// FlowAddress is (node, domain, flow-id) (§3).
//
// The domain component is required, not decorative: the same flow ID can legitimately exist in
// two domains on one node — which is exactly what a loopback configuration does.
type FlowAddress struct {
	Node   string `json:"node"`
	Domain string `json:"domain"`
	Flow   string `json:"flow"`
}

// PathStatus is the summary of one path: the deduplicated logical edge
// (source flow address) → (destination node, domain) that requests expand onto (§3).
type PathStatus struct {
	// ID is derived deterministically from the path's identity, so it is stable across
	// reconciles, server restarts and leader changes.
	ID string `json:"id"`

	Source      FlowAddress `json:"source"`
	Destination Destination `json:"destination"`

	State      State      `json:"state"`
	Reason     string     `json:"reason,omitempty"`
	ReasonCode ReasonCode `json:"reason_code,omitempty"`

	// SessionID is the session currently realising this path, if any. Empty while WAITING.
	SessionID string `json:"session_id,omitempty"`
}

// Path is the full view of a path, as GET /v1/paths returns it.
type Path struct {
	PathStatus

	// Requests are the IDs of every request that expanded onto this path. This is the
	// refcount: N requests share one path and one session, and the path is torn down when the
	// last of them is cancelled (§3).
	Requests []string `json:"requests"`

	Session *Session `json:"session,omitempty"`
}

// Session is a concrete worker *pair* realising a path (§3). Ephemeral: re-established
// whenever either end restarts.
type Session struct {
	// ID is derived deterministically from the path identity plus the source flow definition's
	// hash (§5.4, §7.3), so a server restart or leader change recomputes the same ID and
	// adopts the running workers rather than orphaning them.
	//
	// The flow-def hash is part of it because a source flow deleted and recreated with a
	// different definition makes the destination's local flow wrong: the session must be
	// rebuilt, not repaired.
	ID string `json:"id"`

	// Epoch identifies the current target-worker incarnation (§5.2). A content hash, so it has
	// no ordering — only equality. Empty until the destination agent has reported one, and an
	// initiator is never assigned before then (§5.3).
	Epoch string `json:"epoch,omitempty"`

	// Fabric is the negotiated fabric label, and Interface the negotiated interface config
	// given to *both* ends (§10.3). Pinned for the session's lifetime: if the fabric goes
	// away the session fails rather than quietly re-negotiating onto a slower provider (§10.4).
	Fabric    string          `json:"fabric"`
	Interface InterfaceConfig `json:"interface"`

	Target    *SessionEndpoint `json:"target,omitempty"`
	Initiator *SessionEndpoint `json:"initiator,omitempty"`
}

// SessionEndpoint is one side of a session, as last reported by the agent running it.
type SessionEndpoint struct {
	Node  string      `json:"node"`
	State WorkerState `json:"state"`

	// Address and Service are the target's bound fabric endpoint. Reported by the agent that
	// bound it, because the agent has ground truth and the server cannot verify a port it
	// hands out (§7.4).
	Address string `json:"address,omitempty"`
	Service string `json:"service,omitempty"`

	// Restarts in the classification window, and when the current instance started. Together
	// they are the DEGRADED/FAILED signal: a worker cycling repeatedly is degraded whatever it
	// returns, and dying in under a second every attempt is a permanent error while dying
	// after minutes of healthy transfer is transient (§15.1).
	Restarts  int       `json:"restarts"`
	StartedAt time.Time `json:"started_at,omitzero"`

	Reason     string     `json:"reason,omitempty"`
	ReasonCode ReasonCode `json:"reason_code,omitempty"`
}

// PathsResponse is GET /v1/paths (§9.1).
type PathsResponse struct {
	// Settling reports that the server has not yet run its first reconcile (§7.3). It says so
	// explicitly rather than reporting everything as WAITING, which would look like a
	// fleet-wide outage to whatever is scraping this.
	Settling bool `json:"settling,omitempty"`

	Paths []Path `json:"paths"`
}

// StatusSnapshot is POST /agent/v1/{node}/status: a **full snapshot** of the sessions the
// agent is actually running (§9.2).
//
// Full, not a delta, and full in a second sense too: it reports every session the agent is
// running, not merely the ones it was assigned. That is what lets a restarted server — which
// has desired state and no observed state — recognise a worker it never assigned in this
// process lifetime by its session ID and adopt it, instead of issuing a fresh assignment and
// glitching media that was fine (§7.3).
type StatusSnapshot struct {
	Node     string          `json:"node"`
	Instance string          `json:"instance"`
	Sessions []SessionStatus `json:"sessions"`
}

// SessionStatus is one running worker as its agent sees it.
type SessionStatus struct {
	SessionID string      `json:"session_id"`
	Role      Role        `json:"role"`
	State     WorkerState `json:"state"`

	// Epoch is the epoch this worker is *running*: for a target, the one computed from its own
	// target_info; for an initiator, the one it was assigned and recomputed from the blob it
	// was given (§5.3 step 6).
	//
	// The initiator's convergence rule is a single equality test against this: if the epoch it
	// is running differs from the epoch it is assigned, tear down and start a new one with the
	// new target_info. No keepalives, no change-detection RPC, no teardown negotiation.
	Epoch string `json:"epoch,omitempty"`

	// TargetInfo is the serialised blob, target role only. It is a set of RDMA
	// memory-registration keys for this specific process's specific mappings, so it is
	// invalidated by any restart of this worker — which is precisely what the epoch tracks.
	TargetInfo string `json:"target_info,omitempty"`

	// Address and Service are what the target actually bound.
	Address string `json:"address,omitempty"`
	Service string `json:"service,omitempty"`

	Restarts  int       `json:"restarts"`
	StartedAt time.Time `json:"started_at,omitzero"`

	Reason     string     `json:"reason,omitempty"`
	ReasonCode ReasonCode `json:"reason_code,omitempty"`
}

// Node is the read view of a registered node, as GET /v1/nodes returns it (§9.1).
type Node struct {
	Name string `json:"name"`

	// Live reports whether an agent instance currently holds this node's liveness lease.
	//
	// An expired lease is *not* proof that the node's workers stopped, and the server must not
	// reassign its sessions on that basis alone (§4.2). Registration is durable and survives
	// the agent being down; only this field goes false.
	Live     bool   `json:"live"`
	Instance string `json:"instance,omitempty"`

	RegisteredAt time.Time `json:"registered_at,omitzero"`
	LastSeen     time.Time `json:"last_seen,omitzero"`

	Capabilities Capabilities    `json:"capabilities"`
	Domains      []DomainMapping `json:"domains"`
}

// NodeList is GET /v1/nodes.
type NodeList struct {
	Nodes []Node `json:"nodes"`
}

// DomainList is GET /v1/nodes/{node}/domains.
type DomainList struct {
	Node    string          `json:"node"`
	Domains []DomainMapping `json:"domains"`
}
