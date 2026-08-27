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
