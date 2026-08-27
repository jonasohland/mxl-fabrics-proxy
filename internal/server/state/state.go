// Package state is what the server stores and how it reads it back.
//
// The three layers of §4 are kept apart here as deliberately as they are in the key space:
//
//   - **Desired** — node registrations and replication requests. Durable, small, low-churn,
//     written by users and by an agent's own registration. Never mutated by an agent report.
//   - **Observed** — leases, inventory and session status. High-churn, leased so it collects
//     itself, and never trusted to survive a server restart.
//   - **Derived** — sessions and assignments, plus the reconciler's own record. A pure function
//     of the two above, recomputed rather than remembered, and written only by the leader.
//
// [Fleet] is one consistent read of all three, and it is the input every decision in the server
// is made from: the reconciler computes assignments from it, the user API renders status from
// it, and request validation runs against it. Taking it in a single [store.Store.List] is not
// an incidental choice — three separate lists would give three revisions, and a reconcile
// computed across a skewed snapshot can conclude that a session both should and should not
// exist.
package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/store"
)

// NodeRecord is a node registration: durable desired state saying this node exists and what it
// can do (§7.1).
//
// Deliberately separate from [LeaseRecord]. A registration outlives the agent process that
// created it — the node still exists while its agent is being upgraded — and merging the two
// would make a restarting agent look like a node that had never been configured.
type NodeRecord struct {
	Node         string              `json:"node"`
	Capabilities api.Capabilities    `json:"capabilities"`
	Domains      []api.DomainMapping `json:"domains"`

	// RegisteredAt is when this node first registered, preserved across re-registrations so it
	// reads as "known since" rather than "restarted at".
	RegisteredAt time.Time `json:"registered_at"`

	// UpdatedAt is when the advertised capabilities last changed.
	UpdatedAt time.Time `json:"updated_at"`
}

// Domain returns the node's mapping for a domain name.
func (n NodeRecord) Domain(name string) (api.DomainMapping, bool) {
	for _, mapping := range n.Domains {
		if mapping.Name == name {
			return mapping, true
		}
	}
	return api.DomainMapping{}, false
}

// LeaseRecord is the liveness lease: observed state saying an agent instance currently holds
// this node's identity (§7.1).
//
// Written once, under the lease itself, and then only kept alive — a heartbeat renews the TTL
// and writes nothing. That is why there is no LastSeen field: a heartbeat that rewrote this key
// would advance the store revision several times a minute per node forever, waking every
// watcher and turning liveness into the highest-volume writer in the system, in exchange for a
// timestamp the lease's own existence already bounds to within its TTL (§8.3).
type LeaseRecord struct {
	Node     string        `json:"node"`
	Instance string        `json:"instance"`
	Lease    store.LeaseID `json:"lease"`

	// Versions is what this agent instance reported at registration, including the worker's mxl
	// and libfabric versions. The mxl pair is the load-bearing one: target_info is produced by
	// one node's mxl-fabrics and consumed by another's, so a pair straddling a version boundary
	// is a compatibility concern neither agent can detect alone (§10.2).
	Versions api.Versions `json:"versions"`

	AcquiredAt time.Time `json:"acquired_at"`
}

// RequestRecord is durable user intent (§3, §9.1).
//
// A request is never cancelled because its session is failing. Only a user removes one.
type RequestRecord struct {
	ID        string          `json:"id"`
	Spec      api.RequestSpec `json:"spec"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// PathIdentity is the deduplicated logical edge requests expand onto: one source flow address
// to one destination (node, domain) (§3).
//
// N requests naming the same edge produce one path, one session and one worker pair; the path
// goes away when the last of them is cancelled.
type PathIdentity struct {
	Source      api.FlowAddress `json:"source"`
	Destination api.Destination `json:"destination"`
}

// SessionRecord is the one piece of derived state that must be *stable* rather than merely
// recomputable, and is therefore stored (§7.3).
//
// Two things live here and nowhere else:
//
// The **negotiated interface config**, pinned for the session's lifetime. If the fabric a
// session is using goes away the session fails with a reason; it does not quietly re-negotiate
// onto a slower provider, because a re-negotiation at 3am with no operator action is the same
// silent downgrade §10.4 forbids, just harder to notice.
//
// The **creation time**, which is what makes "how long has this been coming up" answerable.
//
// The ID itself is not stored so much as *reproduced*: it is derived deterministically from the
// path identity and the source flow definition (§5.4), so a server restart or a leader change
// recomputes the same ID and adopts the running workers rather than orphaning them.
type SessionRecord struct {
	ID   string       `json:"id"`
	Path PathIdentity `json:"path"`

	// FlowDefHash is the source definition this session was built for. It is part of the session
	// ID, so a source flow deleted and recreated with a different definition produces a
	// different session: the destination's local flow is now wrong and must be rebuilt, not
	// repaired.
	FlowDefHash string `json:"flow_def_hash"`

	Fabric    string              `json:"fabric"`
	Interface api.InterfaceConfig `json:"interface"`

	CreatedAt time.Time `json:"created_at"`
}

// ReconcilerRecord is what the leader publishes about the reconcile loop (see
// [store.KeyReconciler]).
type ReconcilerRecord struct {
	// Leader identifies the replica currently running the reconciler. Diagnostics only —
	// leadership is held by the elector, not by this key.
	Leader string `json:"leader"`

	// Settled reports that the settling window has passed and at least one reconcile has
	// completed, so the assignment sets in the store mean what they say (§7.3).
	//
	// Every replica reads this before serving an assignment set, including the ones that are not
	// the leader. It is the fleet-wide answer to "is 'no assignments' a fact or an absence".
	Settled   bool      `json:"settled"`
	SettledAt time.Time `json:"settled_at"`

	// There is deliberately no "last reconciled at" or "last reconciled revision" here. This key
	// is written through the same compare-before-write path as everything else, so a field that
	// changed on every pass would be a store write on every pass — which wakes every watcher,
	// including the reconciler's own, which reconciles again. A heartbeat with a feedback loop
	// attached is not a diagnostic worth having (§8.3).
}

// Entry is one decoded key: its value, the bytes it was stored as, and the metadata a
// compare-and-swap or a compare-before-write needs.
//
// Raw is kept because the server writes what it reads: comparing the bytes it is about to write
// against the bytes already there is how it avoids waking every watcher with an unchanged
// rewrite (§7.3). The store deliberately does not do that for it — a backend that silently
// dropped no-op writes would behave differently from etcd, which counts writes rather than
// changes.
type Entry[T any] struct {
	Found bool
	Value T
	Raw   []byte
	Rev   int64
	Lease store.LeaseID
}

// Prior is the untyped view of an [Entry] that [PutJSON] compares against.
type Prior struct {
	Found bool
	Raw   []byte
	Rev   int64
	Lease store.LeaseID
}

// Prior returns this entry as a [Prior].
func (e Entry[T]) Prior() Prior {
	return Prior{Found: e.Found, Raw: e.Raw, Rev: e.Rev, Lease: e.Lease}
}

// Malformed is a key that could not be decoded.
//
// Collected rather than returned as an error: one unreadable key — a hand-edited value, a
// record written by a version that got something wrong — must not wedge the reconciler for the
// whole fleet. The server logs these; everything else carries on without them.
type Malformed struct {
	Key string
	Err error
}

// Fleet is one consistent read of the whole store.
type Fleet struct {
	// Revision is the store revision this snapshot was taken at, and the cursor a watch resumes
	// from. Every map below is as of exactly this revision.
	Revision int64

	Nodes     map[string]Entry[NodeRecord]
	Leases    map[string]Entry[LeaseRecord]
	Inventory map[string]Entry[api.InventorySnapshot]
	Status    map[string]Entry[api.StatusSnapshot]

	Requests map[string]Entry[RequestRecord]

	Sessions    map[string]Entry[SessionRecord]
	Assignments map[string]Entry[api.AssignmentSet]
	Reconciler  Entry[ReconcilerRecord]

	Malformed []Malformed
}

// Load reads the entire store into a [Fleet].
//
// One List of the empty prefix, which is every key in this deployment's key space. That is a
// deliberate trade: it costs a full read on every reconcile, and it buys a single revision
// across all three layers. At the fleet sizes in view (§14) the read is small, and the
// alternative — a list per prefix — produces a snapshot whose parts are from different points
// in time, which is exactly how a reconciler talks itself into tearing down a session that a
// newer part of the same snapshot would have kept.
//
// Keys outside the three layers are ignored rather than reported: leader election writes under
// this key space too, and a future addition must not read as corruption to an older server.
func Load(ctx context.Context, s store.Store) (*Fleet, error) {
	kvs, revision, err := s.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("load fleet: %w", err)
	}

	fleet := &Fleet{
		Revision:    revision,
		Nodes:       map[string]Entry[NodeRecord]{},
		Leases:      map[string]Entry[LeaseRecord]{},
		Inventory:   map[string]Entry[api.InventorySnapshot]{},
		Status:      map[string]Entry[api.StatusSnapshot]{},
		Requests:    map[string]Entry[RequestRecord]{},
		Sessions:    map[string]Entry[SessionRecord]{},
		Assignments: map[string]Entry[api.AssignmentSet]{},
	}

	for _, kv := range kvs {
		switch {
		case strings.HasPrefix(kv.Key, store.PrefixNodes):
			decodeInto(fleet, kv, fleet.Nodes, func(r NodeRecord) string { return r.Node })
		case strings.HasPrefix(kv.Key, store.PrefixRequests):
			decodeInto(fleet, kv, fleet.Requests, func(r RequestRecord) string { return r.ID })
		case strings.HasPrefix(kv.Key, store.PrefixLeases):
			decodeInto(fleet, kv, fleet.Leases, func(r LeaseRecord) string { return r.Node })
		case strings.HasPrefix(kv.Key, store.PrefixInventory):
			decodeInto(fleet, kv, fleet.Inventory, func(r api.InventorySnapshot) string { return r.Node })
		case strings.HasPrefix(kv.Key, store.PrefixStatus):
			decodeInto(fleet, kv, fleet.Status, func(r api.StatusSnapshot) string { return r.Node })
		case strings.HasPrefix(kv.Key, store.PrefixSessions):
			decodeInto(fleet, kv, fleet.Sessions, func(r SessionRecord) string { return r.ID })
		case strings.HasPrefix(kv.Key, store.PrefixAssignments):
			decodeInto(fleet, kv, fleet.Assignments, func(r api.AssignmentSet) string { return r.Node })
		case kv.Key == store.KeyReconciler:
			entry, err := decode[ReconcilerRecord](kv)
			if err != nil {
				fleet.Malformed = append(fleet.Malformed, Malformed{Key: kv.Key, Err: err})
				continue
			}
			fleet.Reconciler = entry
		}
	}

	return fleet, nil
}

// decodeInto decodes one key into a map, taking the map key from the record itself rather than
// from the store key.
//
// The store key holds an escaped node name or request ID (see keys.go); the record holds the
// unescaped original. Reading the name back out of the record means there is exactly one
// unescaping in the system — none — and no way for a key and its contents to disagree about
// which node they describe.
func decodeInto[T any](fleet *Fleet, kv store.KV, into map[string]Entry[T], name func(T) string) {
	entry, err := decode[T](kv)
	if err != nil {
		fleet.Malformed = append(fleet.Malformed, Malformed{Key: kv.Key, Err: err})
		return
	}
	key := name(entry.Value)
	if key == "" {
		fleet.Malformed = append(fleet.Malformed, Malformed{Key: kv.Key, Err: fmt.Errorf("record carries no name")})
		return
	}
	into[key] = entry
}

func decode[T any](kv store.KV) (Entry[T], error) {
	var value T
	if err := json.Unmarshal(kv.Value, &value); err != nil {
		return Entry[T]{}, err
	}
	return Entry[T]{
		Found: true,
		Value: value,
		Raw:   kv.Value,
		Rev:   kv.ModRevision,
		Lease: kv.Lease,
	}, nil
}

// Live reports whether an agent instance currently holds this node's identity.
//
// An expired lease is **not** proof that the node's workers stopped, and nothing in the server
// may treat it as such: the media is still flowing until something says otherwise (§4.2).
func (f *Fleet) Live(node string) bool {
	_, ok := f.Leases[node]
	return ok
}

// LiveNodes returns the names of every node currently holding a lease.
func (f *Fleet) LiveNodes() []string {
	nodes := make([]string, 0, len(f.Leases))
	for node := range f.Leases {
		nodes = append(nodes, node)
	}
	return nodes
}

// Flow returns one observed flow, if the node is reporting inventory and has it.
//
// A node that is not reporting inventory returns false here, and callers must not read that as
// "the flow is gone": it means "no observation", which for a node whose agent is down is the
// only honest answer (§4.2).
func (f *Fleet) Flow(node, domain, id string) (api.FlowInventory, bool) {
	for _, flow := range f.Flows(node, domain) {
		if flow.ID == id {
			return flow, true
		}
	}
	return api.FlowInventory{}, false
}

// Flows returns every flow observed in one node's domain.
func (f *Fleet) Flows(node, domain string) []api.FlowInventory {
	entry, ok := f.Inventory[node]
	if !ok {
		return nil
	}
	for _, d := range entry.Value.Domains {
		if d.Name == domain {
			return d.Flows
		}
	}
	return nil
}

// SessionStatus returns what a node last reported about one session, by role.
func (f *Fleet) SessionStatus(node, sessionID string, role api.Role) (api.SessionStatus, bool) {
	entry, ok := f.Status[node]
	if !ok {
		return api.SessionStatus{}, false
	}
	for _, session := range entry.Value.Sessions {
		if session.SessionID == sessionID && session.Role == role {
			return session, true
		}
	}
	return api.SessionStatus{}, false
}

// pathIDTag and sessionIDTag domain-separate the two identity hashes, so a path ID can never
// collide with a session ID and neither can be confused for the other in a key or a log line.
const (
	pathIDTag    = "mxl-replicator/path/v1"
	sessionIDTag = "mxl-replicator/session/v1"
	flowDefTag   = "mxl-replicator/flowdef/v1"
)

// idLength is how many hex characters an ID carries. 32 is 128 bits, which is far past any
// collision concern for a fleet's worth of paths and keeps the IDs short enough to read in a
// log line and type into a URL.
const idLength = 32

// ID derives the path's stable identifier.
//
// Deterministic from the identity alone, which is what makes it survive a server restart and a
// leader change: a recomputed ID that differed would orphan the workers realising it (§7.3).
//
// Note what is *not* in it — the flow definition. A path is "this flow, from here to there",
// and it stays the same path when the producer republishes the flow with a different
// definition. The session underneath it is what changes; see [SessionID].
func (p PathIdentity) ID() string {
	return hashID(pathIDTag,
		p.Source.Node, p.Source.Domain, p.Source.Flow,
		p.Destination.Node, p.Destination.Domain)
}

// SessionID derives a session's stable identifier from the path it realises and the source flow
// definition it was built for (§5.4, §7.3).
//
// The flow-definition hash is in it deliberately. mxl-utils' Flow.IsValid() exists because a
// flow can be deleted and recreated under the same ID with a different definition; when that
// happens the destination's local flow is wrong in a way no amount of reconnecting fixes. Since
// the ID changes, the old session is torn down and a new one is built, which is the only
// correct outcome.
func SessionID(path PathIdentity, flowDefHash string) string {
	return hashID(sessionIDTag,
		path.Source.Node, path.Source.Domain, path.Source.Flow,
		path.Destination.Node, path.Destination.Domain,
		flowDefHash)
}

// FlowDefHash hashes a flow definition as it travels on the wire.
//
// Whitespace is normalised and nothing else: the definition is carried as verbatim bytes
// precisely so that key order and unmodelled fields survive (§2a), and canonicalising further
// here would put this hash at odds with the bytes the destination worker actually creates its
// flow from. Compacting is safe because passing through the API compacts too, so the hash is
// the same whether a definition came straight off disk or through a round trip.
func FlowDefHash(def json.RawMessage) string {
	sum := sha256.New()
	sum.Write([]byte(flowDefTag))
	sum.Write(compact(def))
	return hex.EncodeToString(sum.Sum(nil))[:idLength]
}

func compact(raw json.RawMessage) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		// Not valid JSON — hash what is there. A definition that cannot be parsed is a problem
		// the destination worker will report far more clearly than this function could, and
		// hashing the raw bytes keeps the identity stable in the meantime.
		return raw
	}
	return buf.Bytes()
}

// hashID hashes length-prefixed fields, so that no rearrangement of the field boundaries can
// produce the same digest as a different identity.
func hashID(tag string, fields ...string) string {
	sum := sha256.New()
	sum.Write([]byte(tag))
	for _, field := range fields {
		var length [8]byte
		n := uint64(len(field))
		for i := range length {
			length[i] = byte(n >> (8 * (7 - i)))
		}
		sum.Write(length[:])
		sum.Write([]byte(field))
	}
	return hex.EncodeToString(sum.Sum(nil))[:idLength]
}
