package api

import "net/url"

// The two APIs live under distinct path prefixes (§9).
//
// Split because they have different auth, different clients, different rate profiles and
// different compatibility guarantees — and because an operator must be able to expose one at
// an ingress without exposing the other. Of the two the *agent* API is the privileged one:
// anything that can call it can claim to be a node, inject fabricated inventory, and read
// other nodes' target_info, which is a set of RDMA rkeys (§13).
const (
	// UserPrefix is the operator-facing API.
	UserPrefix = "/v1"

	// AgentPrefix is the agent-facing API.
	AgentPrefix = "/agent/v1"
)

// User API paths.
const (
	PathRequests = UserPrefix + "/requests"
	PathNodes    = UserPrefix + "/nodes"
	PathFlows    = UserPrefix + "/flows"
	PathPaths    = UserPrefix + "/paths"
)

// Agent API paths that take no node name.
const (
	PathRegister = AgentPrefix + "/register"
)

// HeaderOutcome reports what POST /v1/requests did, or would do under a dry run.
//
// A header rather than a body field, because it describes the *operation* and not the resource:
// [Request] is what the request now is, and it is byte-identical whether the write happened or
// was skipped as a no-op.
//
// The client cannot work this out for itself. The response echoes the spec that was sent, so
// comparing the two says only that the server agreed — not whether anything changed. And the
// status code cannot carry it either: skipping an unchanged write is still a 200, because the
// request does exist and is as asked for.
const HeaderOutcome = "X-Mxl-Outcome"

// The values [HeaderOutcome] takes.
const (
	// OutcomeCreated: the request did not exist.
	OutcomeCreated = "created"

	// OutcomeUpdated: it existed and the spec differed, so it was rewritten.
	OutcomeUpdated = "updated"

	// OutcomeUnchanged: it existed with exactly this spec, and **nothing was written**
	// (invariant 13). Distinguished from `updated` because a controller re-applying on every
	// resync must be able to see that it is not churning the store, and because an operator
	// re-running a file wants to see that nothing moved.
	OutcomeUnchanged = "unchanged"
)

// QueryDryRun asks POST /v1/requests to validate, reconcile and report what *would* happen
// without writing anything (plan M8d).
//
// Nearly free, because the accept path already builds a candidate fleet and reconciles it in
// order to reject INVALID — the only difference is skipping the store write. It is what lets
// `apply --dry-run` report real outcomes, including conflicts across requests, rather than
// diffing specs and guessing.
const QueryDryRun = "dry_run"

// Query parameters on the assignment long poll (§9.2).
const (
	// QueryRevision is the agent's cursor. The server holds the request until the revision
	// advances past it, or until the wait expires.
	QueryRevision = "rev"

	// QueryWait is how long the agent is willing to be held, in seconds. The server caps it:
	// it must stay below any intermediate proxy's idle timeout, because long polling has to
	// survive a dumb proxy — degrading to plain polling is acceptable, hanging is not.
	QueryWait = "wait"
)

// RequestPath is /v1/requests/{id}.
func RequestPath(id string) string { return PathRequests + "/" + url.PathEscape(id) }

// NodePath is /v1/nodes/{node}.
func NodePath(node string) string { return PathNodes + "/" + url.PathEscape(node) }

// NodeDomainsPath is /v1/nodes/{node}/domains.
func NodeDomainsPath(node string) string { return NodePath(node) + "/domains" }

// Per-node agent API paths.
//
// The node name is escaped because it is operator-assigned free-form text: it is validated for
// uniqueness (§7.1), not for URL safety, and a name with a slash in it must address the node it
// names rather than a route that does not exist.
func agentNodePath(node string) string { return AgentPrefix + "/" + url.PathEscape(node) }

// HeartbeatPath is /agent/v1/{node}/heartbeat.
func HeartbeatPath(node string) string { return agentNodePath(node) + "/heartbeat" }

// InventoryPath is /agent/v1/{node}/inventory.
func InventoryPath(node string) string { return agentNodePath(node) + "/inventory" }

// StatusPath is /agent/v1/{node}/status.
func StatusPath(node string) string { return agentNodePath(node) + "/status" }

// AssignmentsPath is /agent/v1/{node}/assignments.
func AssignmentsPath(node string) string { return agentNodePath(node) + "/assignments" }
