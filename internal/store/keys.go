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
	PrefixDesired  = PrefixSnapshot + "desired/"
	PrefixObserved = PrefixSnapshot + "observed/"
	PrefixDerived  = PrefixSnapshot + "derived/"
)

// PrefixSnapshot is the root the three state layers live under: what the fleet snapshot lists, and
// what the reconcile loop watches.
//
// **The three layers are nested under one root so that a single range covers exactly the state and
// nothing else.** §7.3 requires the snapshot to be *one* List, because three lists give three
// revisions and a reconcile computed across a skewed snapshot can conclude that a session both
// should and should not exist. Until the event log (§12.1) the only prefix covering all three was
// the empty one — every key in the store — and that is no longer acceptable in either direction:
//
//   - **Reads.** Every user-API read loads the whole snapshot and runs Compute (§7.3). Sized
//     against §14 — a fleet of a thousand paths, each with a bounded ring and a stored log tail —
//     an unscoped list drags megabytes of diagnostics over the wire on every read, to discard them.
//   - **Writes.** A watch on the empty prefix sees the events the reconciler itself just wrote and
//     wakes it for them. It would converge, since the next pass changes nothing, but it would
//     double every pass that recorded anything and put the event log on the establishment path.
//
// Nesting rather than choosing a clever byte range is the point: `/desired/`, `/derived/` and
// `/observed/` share no prefix but their first character, and two of them do not even share that,
// so any range built on their spelling would be an accident waiting for a fourth layer to break
// it. A layer outside this root would be **silently absent from every snapshot**, which is
// indistinguishable from a wiped store and is the one failure §4.2 exists to prevent. A test pins
// the invariant, and a new layer belongs in that test before it belongs in a key.
const PrefixSnapshot = "/state/"

// PrefixEvents is the event log (§12.1): bounded per-object rings, and the worker log tails of
// §12.2.
//
// **A fourth prefix and deliberately not a fourth layer.** It is diagnostics *about* the three
// above rather than state of its own: nothing reconciles against it, no decision reads it, and the
// fleet snapshot excludes it. That exclusion is not tidiness — every user-API read is O(fleet)
// because it loads the whole store and runs Compute (§7.3), so folding a diagnostic log into the
// snapshot would make every unrelated read pay for it.
const PrefixEvents = "/events/"

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

// The four event key spaces (§12.1). Each holds one bounded ring per object in a single value,
// rather than one key per event: an append-only stream would need sequencing, gap detection,
// compaction and a garbage collector, and a ring in one value needs none of them — it reads in one
// Get, it is bounded by construction, and it is deleted by deleting one key.
const (
	// PrefixPathEvents is the event ring for one path, and **the path is the unit of retention**.
	//
	// Not the session: a session is ephemeral (§3), and a session-scoped log would split the story
	// at exactly the boundary under investigation, since a re-establishment is where one session
	// ends and the next begins. A path ID is also stable across server restarts and leader changes
	// (§5.4, §7.3), where a session ID changes whenever the source flow definition does — which is
	// what a log needs from its key.
	PrefixPathEvents = PrefixEvents + "paths/"

	// PrefixRequestEvents is the event ring for one request, keyed `<ns>/<name>` like the request
	// itself (§9.1). It holds only what is genuinely request-scoped — admission refusals, an
	// expansion that changed, a path lost to precedence — rather than a copy of its paths' entries.
	PrefixRequestEvents = PrefixEvents + "requests/"

	// PrefixNodeEvents is the event ring for one node: registration, lease expiry, claims, probe
	// results, start-permit saturation. Cheap by construction, and it is the log that still exists
	// after a node's paths are gone.
	PrefixNodeEvents = PrefixEvents + "nodes/"

	// PrefixLogs is the worker log tail for one path (§12.2), stored apart from the ring so that a
	// UI polling events does not carry a few KiB per failure on every poll.
	PrefixLogs = PrefixEvents + "logs/"
)

// KeyFleetEvents is the control plane's own event ring: what happened to the fleet rather than to
// one object in it — a leader taking over, a settling window closing.
//
// It exists because the entries that explain a **gap** belong to no object. A leader change leaves
// every path's log missing whatever changed during it (§12.1), and writing that marker into a
// thousand path rings would be a store storm at the exact moment the fleet is already churning.
// One write, and every read merges this ring into what it returns — so the marker is visible in
// the log where the gap is, without being stored there.
const KeyFleetEvents = PrefixEvents + "fleet"

// PathEventsKey is one path's event ring.
func PathEventsKey(pathID string) string { return PrefixPathEvents + escape(pathID) }

// RequestEventsKey is one request's event ring, keyed on the `(namespace, name)` pair that is its
// ID — two segments, escaped independently, exactly as [RequestKey] is.
func RequestEventsKey(ns, name string) string {
	return PrefixRequestEvents + escape(ns) + "/" + escape(name)
}

// NodeEventsKey is one node's event ring.
func NodeEventsKey(node string) string { return PrefixNodeEvents + escape(node) }

// LogKey is the last failing worker's log tail for one path.
func LogKey(pathID string) string { return PrefixLogs + escape(pathID) }

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
