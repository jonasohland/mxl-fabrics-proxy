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

// PrefixElection is where leader election puts its keys (§8.2).
//
// Not one of the three layers: it is machinery rather than state, nothing reads it as part of a
// fleet snapshot, and it is named here only so that the snapshot loader and the elector cannot
// disagree about which keys are which. On etcd it sits under the deployment's own key prefix,
// alongside everything else this store writes.
const PrefixElection = "/election/"

// Prefixes to list or watch. Each ends in a slash: prefixes match raw bytes, not path
// components, so the trailing slash is what keeps "/desired/node" from matching
// "/desired/nodes/edge-01".
const (
	// PrefixNodes holds node registrations — durable, unleased, and outliving the agent that
	// created them (§7.1).
	PrefixNodes = PrefixDesired + "nodes/"

	// PrefixNamespaces holds namespace records: the partitions requests are named within, and
	// what carries whether requests inside one may share a path (§9.3).
	//
	// Desired state, so it is not leased, and it is inside the reconciler's single List("") for
	// free (§7.3) — which is what lets the overlap rule consult a namespace's policy with no new
	// signalling and no second read.
	PrefixNamespaces = PrefixDesired + "namespaces/"

	// PrefixRequests holds replication requests: durable user intent, never cancelled by the
	// system because a session is failing (§11).
	//
	// **Two levels**, `<ns>/<name>`, because a request's ID is the pair (§9.1). A flat key space
	// with a rendered ID in it would work for lookup and fail for listing: "every request in
	// namespace nab" is a prefix scan here and a full scan with a filter there.
	PrefixRequests = PrefixDesired + "requests/"

	// PrefixDomains holds the operator's labels on a node's domains (§10.7).
	//
	// Under `/desired/`, so it is **not leased** — a label is durable user intent about a node,
	// written by a user rather than by the agent (§4) — and it is inside the reconciler's single
	// List("") for free (§7.3). That is what makes a relabel move a request's expansion through
	// the ordinary reconcile with no new signalling.
	PrefixDomains = PrefixDesired + "domains/"

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

// KeyReconciler is what the reconciler publishes about itself: which replica is leading, and
// whether it has settled and acted at least once (§7.3).
//
// Every replica serves the agent API, but only the leader reconciles, so a follower cannot tell
// from its own state whether the assignment sets in the store are meaningful yet. It reads this
// key instead. That also makes a *wiped store* say so: with no reconciler record, no replica
// will serve an assignment set, so agents get a not-ready answer and skip the reconcile
// entirely rather than being handed an empty set that means "I don't know" (plan §4.2).
const KeyReconciler = PrefixDerived + "reconciler"

// NodeKey is the registration for one node.
func NodeKey(node string) string { return PrefixNodes + escape(node) }

// NamespaceKey is one namespace record.
func NamespaceKey(ns string) string { return PrefixNamespaces + escape(ns) }

// NamespaceRequestsPrefix is every request in one namespace, for a scoped list.
func NamespaceRequestsPrefix(ns string) string { return PrefixRequests + escape(ns) + "/" }

// RequestKey is one replication request, keyed on the `(namespace, name)` pair that is its ID.
//
// Two segments, each escaped independently, so the separator between them is the only unescaped
// slash in the key and a prefix scan splits exactly where it is meant to.
func RequestKey(ns, name string) string { return NamespaceRequestsPrefix(ns) + escape(name) }

// DomainLabelsPrefix is every label record for one node, for a scoped list.
func DomainLabelsPrefix(node string) string { return PrefixDomains + escape(node) + "/" }

// DomainLabelsKey is the label record for one `(node, domain)`.
//
// **The domain name contains `/`**, and [escape] is what keeps that from splitting the key: it is
// [url.PathEscape], which percent-encodes a separator (`fast/ingest` → `fast%2Fingest`), so the
// record stays one key segment and the two-level prefix scan splits where it is meant to. The
// property belongs to the standard library's escaping mode rather than to anything in this tree,
// which is why it is pinned by a test rather than left to this comment.
func DomainLabelsKey(node, domain string) string { return DomainLabelsPrefix(node) + escape(domain) }

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
