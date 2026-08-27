package store

import "net/url"

// The three top-level prefixes, one per layer of the state model (§4).
//
// The split is not cosmetic. Desired state is durable, small and written by users; observed
// state is high-churn, leased, and cheap to rebuild; derived state is a pure function of the
// two and never authoritative. Keeping them apart lets each carry its own compaction and
// backup policy (§8.3), lets a watch be scoped to one layer, and makes "an agent report must
// never mutate desired state" a property you can see in a key rather than a rule someone has
// to remember.
const (
	PrefixDesired  = "/desired/"
	PrefixObserved = "/observed/"
	PrefixDerived  = "/derived/"
)

// Prefixes to list or watch. Each ends in a slash: prefixes match raw bytes, not path
// components, so the trailing slash is what keeps "/desired/node" from matching
// "/desired/nodes/edge-01".
const (
	// PrefixNodes holds node registrations — durable, unleased, and outliving the agent that
	// created them (§7.1).
	PrefixNodes = PrefixDesired + "nodes/"

	// PrefixRequests holds replication requests: durable user intent, never cancelled by the
	// system because a session is failing (§11).
	PrefixRequests = PrefixDesired + "requests/"

	// PrefixLeases holds one key per live agent instance, under that agent's own lease.
	PrefixLeases = PrefixObserved + "leases/"

	// PrefixInventory holds each agent's full domain and flow snapshot, under its lease (§9.2).
	PrefixInventory = PrefixObserved + "inventory/"

	// PrefixStatus holds each agent's full snapshot of the sessions it is actually running,
	// under its lease. Full, not filtered to what was assigned: the server has to be able to
	// see a worker it never assigned in this process lifetime, recognise it by session ID and
	// adopt it, or every server restart glitches media (§7.3).
	PrefixStatus = PrefixObserved + "status/"

	// PrefixSessions holds session records, including the negotiated interface config.
	PrefixSessions = PrefixDerived + "sessions/"

	// PrefixAssignments holds one assignment set per node. Written **only** by the reconciler
	// (§7.3), and the key an agent's long poll watches (§9.2).
	PrefixAssignments = PrefixDerived + "assignments/"
)

// KeyPolicy is operator policy: provider preference order, port ranges, bandwidth budgets.
// A single key, not a prefix.
const KeyPolicy = PrefixDesired + "policy"

// NodeKey is the registration for one node.
func NodeKey(node string) string { return PrefixNodes + escape(node) }

// RequestKey is one replication request.
func RequestKey(id string) string { return PrefixRequests + escape(id) }

// LeaseKey is one agent instance's liveness record. Write it under that agent's lease.
func LeaseKey(node string) string { return PrefixLeases + escape(node) }

// InventoryKey is one node's inventory snapshot. Write it under that agent's lease.
func InventoryKey(node string) string { return PrefixInventory + escape(node) }

// StatusKey is one node's session status snapshot. Write it under that agent's lease.
func StatusKey(node string) string { return PrefixStatus + escape(node) }

// SessionKey is one session record.
func SessionKey(sessionID string) string { return PrefixSessions + escape(sessionID) }

// AssignmentsKey is one node's assignment set.
func AssignmentsKey(node string) string { return PrefixAssignments + escape(node) }

// escape makes an operator-assigned name safe to concatenate into a key.
//
// Node names are validated for fleet-wide uniqueness (§7.1), not for structure — they come
// from a flag, an environment variable or a config file and can contain anything. A name with
// a slash in it would otherwise write outside the prefix it was meant to be in, which is at
// best a key that no list finds and at worst one node's registration landing on another's.
//
// [url.PathEscape] is the same function api.NodePath uses on the same names for the same
// reason, so a node addressable over the agent API is addressable here and the two cannot
// disagree about which node a name refers to.
func escape(name string) string { return url.PathEscape(name) }
