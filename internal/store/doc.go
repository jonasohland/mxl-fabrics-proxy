// Package store is the server's persistence layer: a revisioned key-value store with
// compare-and-swap, prefix watches and TTL leases (§8).
//
// # Why the interface is in etcd's terms
//
// Two backends are required — etcd for HA, sqlite for a single node (§2) — and supporting both
// behind one interface is the classic place a project like this gets stuck. etcd gives you
// revisions, CAS, watch and leases; sqlite gives you transactions and none of the other three.
// Design to sqlite and HA becomes impossible; design to etcd and the sqlite backend must
// emulate.
//
// §8.1 settles it: **define the interface in etcd's terms and emulate over sqlite.** The
// emulation is genuinely small — a monotonic revision column bumped in the writing
// transaction, a history table the watch reads forward through, and an expiry column with a
// sweeper — and the alternative, a domain-level interface with two hand-written
// implementations, duplicates the reconciler's consistency logic twice over. The agent
// long-poll (§9.2) wants a revision cursor either way.
//
// The [conformance] suite is what keeps the abstraction honest: it is written against sqlite
// and must pass **unchanged** against etcd. If it does not, the interface is wrong, and that
// is precisely the leak §8.1 is worried about.
//
// # The key space
//
// Desired, observed and derived state (§4) live under separate prefixes, so that the three can
// carry different compaction and backup policy (§8.3) and so that a watch can be scoped to one of
// them — and all three sit under one root, so that a single range covers exactly the state:
//
//	/state/desired/nodes/<node>           registration: attachments, versions, areas, port range
//	/state/desired/requests/<ns>/<name>   replication requests
//	/state/desired/policy                 operator policy: provider order, budgets
//	/state/observed/leases/<node>         leased — instance uuid, agent version
//	/state/observed/inventory/<node>      leased — full snapshot (§9.2)
//	/state/observed/status/<node>         leased — full snapshot of sessions actually running
//	/state/derived/sessions/<session-id>  session record: negotiated interface config
//	/state/derived/assignments/<node>     written only by the reconciler
//	/events/paths/<path-id>               bounded event ring (§12.1)
//	/events/logs/<path-id>                the last failing worker's log tail (§12.2)
//
// The nesting is load-bearing rather than decorative — see [PrefixSnapshot]. The event log is
// outside it on purpose: it is diagnostics *about* the three layers rather than state of its own,
// nothing reconciles against it, and a snapshot that carried it would make every user-API read pay
// for a log it ignores.
//
// Observed state is written **under a lease**, so an agent that goes away garbage-collects
// itself (§8.3) and the server never has to distinguish "this node reported nothing" from
// "this node is gone". Desired state is never leased: a registration outlives the agent that
// made it (§7.1), and a request is durable user intent (§11).
//
// keys.go holds the constructors. Use them rather than formatting keys by hand — they escape
// the node name, which is operator-assigned free-form text and would otherwise be able to
// climb out of its prefix.
//
// # What this package does not do
//
// It stores bytes. It does not know what a request or an assignment is, and nothing here
// imports [github.com/jonasohland/mxl-replicator/internal/api]. Serialisation, validation and
// the meaning of any particular key belong to the server.
package store
