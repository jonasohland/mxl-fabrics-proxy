// Package etcd implements [store.Store] over an etcd v3 cluster.
//
// This is the HA backend (§8.2). Where the sqlite backend has to emulate three things etcd
// has — revisions, prefix watches and TTL leases — this one mostly hands them straight
// through, which is the point: §8.1 bet that an interface written in etcd's terms would make
// the etcd backend nearly free, and the [conformance] suite passing here unchanged is the
// evidence that the bet paid off.
//
// # What still needed care
//
// Four things, each of which would be a silent correctness bug rather than a compile error:
//
//   - **A watch is not established until etcd says so.** [clientv3.Watcher.Watch] returns a
//     channel immediately and creates the server-side watcher asynchronously, so a caller that
//     watched and then wrote could have its own write land before the watcher existed — and a
//     "from now" watcher starts at the revision current when the *server* processed the create,
//     so that write would be missed rather than delayed. [Store.Watch] therefore asks for a
//     created-notification and does not return until it arrives.
//   - **KeepAlive is single-shot.** clientv3's KeepAlive is a background stream that renews a
//     lease until the *client* stops caring, which is precisely backwards for §7.1: the lease
//     must die when the agent stops heartbeating. [Store.KeepAlive] uses KeepAliveOnce, so one
//     agent heartbeat is one renewal and a silent agent expires.
//   - **Lease TTLs are whole seconds.** They are rounded *up*, never down, because the interface
//     says a backend may only lengthen a TTL — and a sub-second TTL rounded down is a lease that
//     was already dead when it was granted.
//   - **Every key is namespaced.** Options.Prefix exists so one etcd cluster can host several
//     replicator deployments, or share space with something else entirely, so it has to apply to
//     the stored key rather than to a filter on the way out.
//
// # What is deliberately not here
//
// **Compaction.** The sqlite backend bounds its own watch history because nothing else would;
// etcd's history is bounded by the cluster's own `--auto-compaction-retention`, which is an
// operator setting on a resource this store may be sharing with other tenants. Compacting from
// here would discard history cluster-wide, including watches this process knows nothing about.
// [store.ErrCompacted] is handled either way — it is reported when etcd reports it, and the
// recovery is the same re-list as on sqlite.
//
// **Leader election.** §8.2 gives the reconciler to one elected replica, but that is a server
// concern and it needs the concurrency package rather than this interface. [New] exists for it:
// it takes an already-open client so election and storage share one connection, one set of
// credentials and one set of keepalives.
package etcd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/namespace"

	"github.com/jonasohland/mxl-replicator/internal/store"
)

// Defaults for [Options].
const (
	// DefaultPrefix matches the --store-etcd-prefix default.
	DefaultPrefix = "/mxl-replicator"

	// DefaultDialTimeout bounds both the initial connection and the reachability probe [Open]
	// makes before it reports success.
	DefaultDialTimeout = 5 * time.Second
)

// gRPC keepalive settings for the client connection.
//
// Not a tuning knob: without them a connection black-holed by a NAT timeout or a silently
// dropped route stays "up" from the client's point of view until something times out at the
// application layer. §7.1's node identity and §8.2's leader election both ride on lease
// renewals over this connection, so the failure has to be detected in seconds.
const (
	dialKeepAliveTime    = 30 * time.Second
	dialKeepAliveTimeout = 10 * time.Second
)

// watchBuffer is the per-watch channel buffer, matching the sqlite backend: enough to absorb a
// lease revocation taking out one agent's whole observed state without stalling the forwarder.
const watchBuffer = 64

// probeKey is read by [Open] to prove the cluster is reachable. It is never written, and it
// lives under the configured prefix so the probe also proves the namespace is usable.
const probeKey = "/.reachable"

// Options configures a [Store]. Endpoints is required; everything else has a default.
type Options struct {
	// Endpoints are the etcd client URLs, e.g. http://etcd-0:2379.
	Endpoints []string

	// Prefix is prepended to every key this store reads or writes, so one etcd cluster can host
	// several deployments. A trailing slash is trimmed, because the store's own keys already
	// begin with one (see keys.go).
	//
	// Empty means [DefaultPrefix], **not** "the whole key space": an unset field is far more
	// likely to be an oversight than a decision, and the consequence of guessing wrong that way
	// is a deployment writing its nodes and requests over a shared cluster's root. Pass "/" to
	// ask for the root deliberately.
	Prefix string

	// DialTimeout bounds the initial connection and the reachability probe.
	DialTimeout time.Duration

	// TLS, Username and Password are the cluster's authentication, if it has any.
	TLS      *tls.Config
	Username string
	Password string

	// Logger receives the etcd client's own diagnostics — endpoint failures, reconnects, TLS
	// errors — bridged out of zap. Defaults to [slog.Default].
	Logger *slog.Logger
}

// resolve applies the defaults. It is **not** idempotent — "/" resolves to the empty prefix, and
// the empty prefix resolves to [DefaultPrefix] — so it runs exactly once, in [New], and every
// other constructor goes through there.
func (o *Options) resolve() {
	if o.Prefix == "" {
		o.Prefix = DefaultPrefix
	}
	// "/mxl-replicator/" and "/mxl-replicator" must name the same key space: the store's keys
	// are absolute ("/desired/nodes/x"), so a trailing slash here would produce "//desired".
	// The same rule turns an explicit "/" into the root.
	o.Prefix = strings.TrimRight(o.Prefix, "/")

	if o.DialTimeout <= 0 {
		o.DialTimeout = DefaultDialTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Store is a [store.Store] backed by an etcd v3 cluster.
type Store struct {
	cli *clientv3.Client

	// owns records whether Close should close the client. False for a [New] store, whose
	// client is shared with the caller's leader election (§8.2).
	owns bool

	opts Options

	// The namespaced views. The client's own KV/Watcher/Lease are deliberately left alone: a
	// caller that handed us a client via [New] is still using it for election keys of its own
	// and must not find them silently rewritten.
	kv      clientv3.KV
	lease   clientv3.Lease
	watcher clientv3.Watcher

	closeOnce sync.Once
	done      chan struct{}

	mu     sync.RWMutex
	closed bool
}

var _ store.Store = (*Store)(nil)

// Open connects to a cluster and returns a store that owns the connection.
func Open(ctx context.Context, opts Options) (*Store, error) {
	opts.resolve()

	if len(opts.Endpoints) == 0 {
		return nil, errors.New("etcd: no endpoints")
	}

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:            opts.Endpoints,
		DialTimeout:          opts.DialTimeout,
		DialKeepAliveTime:    dialKeepAliveTime,
		DialKeepAliveTimeout: dialKeepAliveTimeout,
		// Keepalives on an idle connection too. The connection is idle exactly when nothing is
		// changing, which is the steady state (§8.3) — and the state it must not be silently
		// dead in.
		PermitWithoutStream: true,
		TLS:                 opts.TLS,
		Username:            opts.Username,
		Password:            opts.Password,
		Logger:              newZapLogger(opts.Logger),
	})
	if err != nil {
		return nil, fmt.Errorf("etcd: connect to %s: %w", strings.Join(opts.Endpoints, ","), err)
	}

	s := wrap(cli, opts)
	s.owns = true

	// clientv3.New does not wait for a connection, so without this an unreachable cluster
	// would be reported as a started server that fails on its first request — long after the
	// operator was told everything was fine. One bounded linearizable read of an absent key
	// proves quorum, credentials and TLS all at once, and costs nothing when they are right.
	probeCtx, cancel := context.WithTimeout(ctx, opts.DialTimeout)
	defer cancel()
	if _, err := s.kv.Get(probeCtx, probeKey); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("etcd: %s unreachable: %w", strings.Join(opts.Endpoints, ","), err)
	}

	return s, nil
}

// New wraps an already-open client, which it does not take ownership of: [Store.Close] ends
// this store's watches and refuses further calls, but leaves the connection open.
//
// This is the seam §8.2's leader election needs. The elected-leader machinery is a server
// concern and speaks to etcd directly, and running it on a second connection would mean a
// second set of credentials, a second set of keepalives, and a partition that could take out
// storage while leaving election intact — or the reverse.
//
// The caller keeps its own unnamespaced view of the client. Use Options.Prefix on the election
// keys too, or they land outside the deployment's key space.
func New(cli *clientv3.Client, opts Options) *Store {
	opts.resolve()
	return wrap(cli, opts)
}

// wrap builds the store around already-resolved options.
func wrap(cli *clientv3.Client, opts Options) *Store {
	return &Store{
		cli:     cli,
		opts:    opts,
		kv:      namespace.NewKV(cli.KV, opts.Prefix),
		lease:   namespace.NewLease(cli.Lease, opts.Prefix),
		watcher: namespace.NewWatcher(cli.Watcher, opts.Prefix),
		done:    make(chan struct{}),
	}
}

// Client returns the underlying client, for the parts of the server that need etcd itself
// rather than the store abstraction — leader election, above all.
func (s *Store) Client() *clientv3.Client { return s.cli }

// Prefix returns the resolved key prefix, after defaulting and trimming.
func (s *Store) Prefix() string { return s.opts.Prefix }

// Close ends every watch and, if this store opened the connection, closes it. Idempotent.
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		// Before the client goes, so a watch forwarder that is about to see its stream torn
		// down can report [store.ErrClosed] rather than a transport error the caller cannot
		// act on.
		close(s.done)

		if s.owns {
			err = s.cli.Close()
			// A cancelled client context is what a clean shutdown looks like from inside
			// clientv3, not a failure to report to the caller.
			if errors.Is(err, context.Canceled) {
				err = nil
			}
		}
	})
	return err
}

func (s *Store) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}
