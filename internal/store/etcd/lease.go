package etcd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/jonasohland/mxl-replicator/internal/store"
)

// GrantLease implements [store.Store].
//
// etcd's TTL is whole seconds, so a duration is rounded **up**. The interface says a backend may
// round the TTL up and that callers should treat it as a floor, and the asymmetry is the point:
// a TTL rounded down is a lease that dies earlier than the caller planned for, and a
// sub-second one rounds to zero, which etcd would take as a lease that has already expired.
//
// etcd applies a floor of its own on top, derived from the election timeout — a cluster with
// default timings grants no less than two seconds. It is well below --lease-ttl, whose default
// is 15s and which §7.1 wants at a small multiple of the heartbeat anyway, so it is worth
// knowing rather than working around.
func (s *Store) GrantLease(ctx context.Context, ttl time.Duration) (store.LeaseID, error) {
	if s.isClosed() {
		return 0, store.ErrClosed
	}
	if ttl <= 0 {
		return 0, errors.New("etcd: lease ttl must be positive")
	}

	seconds := int64(math.Ceil(ttl.Seconds()))
	if seconds < 1 {
		seconds = 1
	}

	resp, err := s.lease.Grant(ctx, seconds)
	if err != nil {
		return 0, translate("grant lease", err)
	}
	if resp.Error != "" {
		// Grant reports some refusals in the response rather than as an RPC error, and a lease
		// id from one of those would be zero — which the interface reserves for "no lease", so
		// it would attach silently to nothing.
		return 0, fmt.Errorf("etcd: grant lease: %s", resp.Error)
	}
	return store.LeaseID(resp.ID), nil
}

// KeepAlive implements [store.Store].
//
// One call, one renewal. clientv3 also offers a KeepAlive that opens a background stream and
// renews the lease until the *client* stops caring, and using it here would quietly invert
// §7.1: the lease exists to expire when the agent stops heartbeating, and a server-side
// renewal loop would hold a departed node's identity — and its observed state — indefinitely.
//
// The failure is the useful half. An agent whose lease expired during a partition learns here
// that its identity is no longer held, rather than reporting into a fleet that has forgotten
// it. clientv3 turns etcd's "TTL -1" answer for an unknown lease into ErrLeaseNotFound, so an
// expired lease and a revoked one are the same outcome, as the interface requires.
func (s *Store) KeepAlive(ctx context.Context, id store.LeaseID) error {
	if s.isClosed() {
		return store.ErrClosed
	}

	_, err := s.lease.KeepAliveOnce(ctx, clientv3.LeaseID(id))
	return translate(fmt.Sprintf("keepalive lease %d", id), err)
}

// RevokeLease implements [store.Store].
//
// etcd deletes every attached key at one revision, which is the property §8.3 depends on: a
// node leaving is a single event in the world, and a watcher must not be able to observe it
// half gone, with its status collected and its inventory still present.
func (s *Store) RevokeLease(ctx context.Context, id store.LeaseID) error {
	if s.isClosed() {
		return store.ErrClosed
	}

	_, err := s.lease.Revoke(ctx, clientv3.LeaseID(id))
	return translate(fmt.Sprintf("revoke lease %d", id), err)
}
