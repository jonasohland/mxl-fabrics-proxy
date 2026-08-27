package store

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound is a [Store.Get] for a key that does not exist.
	//
	// [Store.Delete] does not return it: deleting a key that is not there is a no-op, because a
	// reconciler that has just computed "this key should not exist" is right either way.
	ErrNotFound = errors.New("store: key not found")

	// ErrCompareFailed is a CAS that lost — [IfRevision] against a key that has since been
	// written, or [IfAbsent] against a key that now exists.
	//
	// It is an ordinary outcome, not a fault. The caller re-reads and decides again; §7.1's
	// registration fencing is exactly this loop, and so is any write that must not clobber a
	// concurrent one.
	ErrCompareFailed = errors.New("store: compare failed")

	// ErrLeaseNotFound is a lease that has expired, been revoked, or never existed.
	//
	// Returned by [Store.KeepAlive] and [Store.RevokeLease], and by [Store.Put] with
	// [WithLease]. For an agent this is the signal that its liveness lease is gone and its
	// observed state went with it — it must re-register rather than keep reporting (§7.1,
	// api.CodeReregister).
	ErrLeaseNotFound = errors.New("store: lease not found")

	// ErrCompacted is a watch started from a revision whose history has been discarded.
	//
	// The recovery is always the same and always available: [Store.List] the prefix to get a
	// current snapshot and a fresh revision, then watch from there. Nothing is lost, because
	// every consumer in this system is level-triggered (§4.1) — the events are a wakeup and a
	// cursor, not a log that has to be replayed in full.
	ErrCompacted = errors.New("store: revision compacted")

	// ErrClosed is any operation on a store that has been closed.
	ErrClosed = errors.New("store: closed")
)

// LeaseID identifies a TTL lease. Never zero for a real lease; the zero value means "no lease".
type LeaseID int64

// KV is one key and its value, with the revision metadata that makes CAS and watch resumption
// possible.
type KV struct {
	Key   string
	Value []byte

	// CreateRevision is the revision at which this key was last created — set when the key came
	// into existence and unchanged by subsequent writes, so it resets if the key is deleted and
	// written again.
	CreateRevision int64

	// ModRevision is the revision of the write that produced this value. It is the value to
	// pass to [IfRevision] for a compare-and-swap.
	ModRevision int64

	// Version counts writes since the key was created, starting at 1.
	Version int64

	// Lease is the lease this key is attached to, or 0 for an unleased key.
	Lease LeaseID
}

// EventType is what happened to a key.
type EventType int

const (
	// EventPut is a key created or updated. The event carries the value written.
	EventPut EventType = iota

	// EventDelete is a key removed, whether explicitly or by its lease expiring. The event
	// carries the key and the revision of the deletion; Value is nil.
	EventDelete
)

func (t EventType) String() string {
	switch t {
	case EventPut:
		return "put"
	case EventDelete:
		return "delete"
	default:
		return "unknown"
	}
}

// Event is one change to one key, delivered by [Store.Watch].
type Event struct {
	Type EventType
	KV   KV

	// Err ends the watch. When it is non-nil this is the **last** event on the channel, the
	// other fields are meaningless, and the channel is closed immediately afterwards.
	//
	// A watch that ends for any reason other than the caller's own context is not fatal to the
	// caller: re-list the prefix and watch again from the revision the list reports. The only
	// error worth branching on is [ErrCompacted], and its recovery is the same thing.
	Err error
}

// Store is a revisioned key-value store.
//
// Revisions are **store-wide, not per-key**: one monotonic counter that every mutating
// operation advances by exactly one, so a revision names a point in the store's history rather
// than a version of some particular key. That is what lets [Store.List] hand back a cursor a
// [Store.Watch] can resume from, and it is why the agent long-poll can be a single integer on
// a query string (§9.2).
//
// All methods are safe for concurrent use.
type Store interface {
	// Get returns one key, or [ErrNotFound].
	Get(ctx context.Context, key string) (*KV, error)

	// List returns every key under prefix, sorted by key, together with the store revision the
	// snapshot was taken at.
	//
	// That revision is the whole point of the second return value: `List` then `Watch` from
	// `revision + 1` sees every change after the snapshot and none of the ones already in it,
	// with no window in between. Building a cursor any other way — the highest ModRevision in
	// the result, say — is wrong, because a revision that touched a key outside the prefix, or
	// deleted one inside it, leaves no ModRevision behind to find.
	//
	// prefix matches raw bytes, exactly as etcd's does: it is a string prefix, not a path
	// component. Pass the trailing slash. "/desired/node" would otherwise match
	// "/desired/nodes/edge-01", which is a different part of the key space (see keys.go).
	// The empty prefix matches every key.
	List(ctx context.Context, prefix string) ([]KV, int64, error)

	// Put writes a value and returns the revision of the write.
	//
	// Every successful Put advances the store revision, **including one that writes a value
	// byte-identical to what was already there**. This follows etcd, where the revision counts
	// writes rather than changes, and it is left that way deliberately rather than
	// short-circuited here: a store that silently dropped no-op writes would give CAS loops and
	// watch cursors different behaviour on the two backends, which is exactly the divergence the
	// conformance suite exists to prevent. The consequence for callers is real though — an
	// unconditional rewrite of an unchanged assignment set wakes every watcher on it, so the
	// reconciler should compare before it writes (§7.3).
	//
	// A Put with no [WithLease] **detaches** any lease the key was holding. Observed state is
	// rewritten on every report (§9.2), so the lease has to be passed every time; forgetting it
	// once turns leased state into state that outlives its agent.
	//
	// On [ErrCompareFailed] the returned revision is the store's current revision, which is the
	// value a retry loop wants next.
	Put(ctx context.Context, key string, value []byte, opts ...PutOpt) (int64, error)

	// Delete removes a key and returns the revision of the deletion.
	//
	// Deleting a key that does not exist is a no-op: it returns the current revision, advances
	// nothing, and emits no event.
	Delete(ctx context.Context, key string, opts ...DelOpt) (int64, error)

	// Watch delivers changes to keys under prefix until ctx ends or the store closes.
	//
	// fromRev is inclusive. Zero or negative means "from now": only changes made after the call
	// returns. A positive fromRev replays history from that revision, and returns a channel
	// carrying a single [ErrCompacted] event if that history is gone.
	//
	// Events arrive in revision order, and within a revision in the order the keys were
	// written. One revision can produce several events — a lease expiring takes out everything
	// attached to it at a single revision — so a consumer that reconciles per event will
	// reconcile more than once for it. That is harmless and is what level-triggered means.
	//
	// The channel is closed when the watch ends. Closure alone does not say why: check ctx, and
	// see [Event.Err] for the rest.
	Watch(ctx context.Context, prefix string, fromRev int64) (<-chan Event, error)

	// GrantLease creates a lease with the given TTL.
	//
	// Backends may round the TTL up to something they can honour, so treat it as a floor. Keys
	// attached to the lease with [WithLease] are deleted when it expires or is revoked.
	GrantLease(ctx context.Context, ttl time.Duration) (LeaseID, error)

	// KeepAlive resets a lease's TTL, or returns [ErrLeaseNotFound] if it has already gone.
	//
	// The failure is the useful half: an agent whose lease expired during a partition learns
	// here that its node identity is no longer held and that its observed state has been
	// collected, rather than carrying on reporting into a fleet that has forgotten it (§7.1).
	KeepAlive(ctx context.Context, id LeaseID) error

	// RevokeLease drops a lease and every key attached to it, at one revision.
	RevokeLease(ctx context.Context, id LeaseID) error

	// Close releases the store. Outstanding watches are ended and further calls return
	// [ErrClosed]. Idempotent.
	Close() error
}

// PutOpt conditions or annotates a [Store.Put].
type PutOpt interface{ applyPut(*PutConfig) }

// DelOpt conditions a [Store.Delete].
type DelOpt interface{ applyDel(*DelConfig) }

// PutConfig is the resolved set of [PutOpt]s. Backends read it; callers build it with the
// option constructors.
type PutConfig struct {
	// Lease to attach, or 0 for none.
	Lease LeaseID

	// IfRevision requires the key's current ModRevision to equal this value, and IfAbsent
	// requires the key not to exist. They are mutually exclusive; setting both is a
	// programming error and backends reject it.
	IfRevision    int64
	HasIfRevision bool
	IfAbsent      bool
}

// DelConfig is the resolved set of [DelOpt]s.
type DelConfig struct {
	IfRevision    int64
	HasIfRevision bool
}

// NewPutConfig resolves options into a [PutConfig].
func NewPutConfig(opts []PutOpt) (PutConfig, error) {
	var cfg PutConfig
	for _, opt := range opts {
		opt.applyPut(&cfg)
	}
	if cfg.IfAbsent && cfg.HasIfRevision {
		return cfg, errors.New("store: IfAbsent and IfRevision are mutually exclusive")
	}
	return cfg, nil
}

// NewDelConfig resolves options into a [DelConfig].
func NewDelConfig(opts []DelOpt) (DelConfig, error) {
	var cfg DelConfig
	for _, opt := range opts {
		opt.applyDel(&cfg)
	}
	return cfg, nil
}

// WithLease attaches the written key to a lease, so that it is deleted when the lease expires.
//
// This is how observed state garbage-collects itself (§8.3): the agent's inventory and status
// keys are written under its liveness lease, so a node that stops heartbeating stops being
// visible without anything having to notice and clean up after it.
func WithLease(id LeaseID) PutOpt { return leaseOpt(id) }

// IfRevision makes the operation a compare-and-swap against the key's current [KV.ModRevision],
// failing with [ErrCompareFailed] if the key has been written since it was read.
//
// Passing 0 asserts the key does not exist, matching etcd, where an absent key compares as
// revision 0 — but prefer [IfAbsent], which says so.
func IfRevision(rev int64) interface {
	PutOpt
	DelOpt
} {
	return ifRevisionOpt(rev)
}

// IfAbsent makes a put succeed only if the key does not yet exist.
//
// The create-or-fail primitive behind fencing: two agents claiming one node name resolve by
// one of them losing this race, loudly, rather than both proceeding (§7.1).
func IfAbsent() PutOpt { return ifAbsentOpt{} }

type leaseOpt LeaseID

func (o leaseOpt) applyPut(cfg *PutConfig) { cfg.Lease = LeaseID(o) }

type ifRevisionOpt int64

func (o ifRevisionOpt) applyPut(cfg *PutConfig) {
	cfg.IfRevision, cfg.HasIfRevision = int64(o), true
}

func (o ifRevisionOpt) applyDel(cfg *DelConfig) {
	cfg.IfRevision, cfg.HasIfRevision = int64(o), true
}

type ifAbsentOpt struct{}

func (ifAbsentOpt) applyPut(cfg *PutConfig) { cfg.IfAbsent = true }
