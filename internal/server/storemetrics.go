package server

import (
	"context"
	"errors"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/store"
)

// observedStore times every store operation (§12).
//
// A decorator rather than instrumentation inside the backends, because there are two of them and
// the numbers have to be comparable: sqlite and etcd have very different latency profiles, and
// the reason to measure at all is that the same control plane runs on both (§8.1). Measuring at
// the seam means one implementation and no way for the two to diverge in what they count.
//
// It deliberately does not wrap [store.Store.Watch]'s channel. A watch is long-lived by design
// and its duration is the interval between changes, not a latency — timing it would produce a
// histogram of how quiet the fleet is.
type observedStore struct {
	inner   store.Store
	metrics *controlMetrics
}

var _ store.Store = (*observedStore)(nil)

// observe wraps a store so its operations are timed. The returned store does not own the inner
// one any more than the server does — Close is forwarded, and the caller still owns the original.
func observe(inner store.Store, m *controlMetrics) store.Store {
	return &observedStore{inner: inner, metrics: m}
}

func (s *observedStore) Get(ctx context.Context, key string) (*store.KV, error) {
	defer s.timed("get")()
	kv, err := s.inner.Get(ctx, key)
	s.record("get", err)
	return kv, err
}

func (s *observedStore) List(ctx context.Context, prefix string) ([]store.KV, int64, error) {
	defer s.timed("list")()
	kvs, rev, err := s.inner.List(ctx, prefix)
	s.record("list", err)
	return kvs, rev, err
}

func (s *observedStore) Put(ctx context.Context, key string, value []byte, opts ...store.PutOpt) (int64, error) {
	defer s.timed("put")()
	rev, err := s.inner.Put(ctx, key, value, opts...)
	s.record("put", err)
	return rev, err
}

func (s *observedStore) Delete(ctx context.Context, key string, opts ...store.DelOpt) (int64, error) {
	defer s.timed("delete")()
	rev, err := s.inner.Delete(ctx, key, opts...)
	s.record("delete", err)
	return rev, err
}

func (s *observedStore) Watch(ctx context.Context, prefix string, fromRev int64) (<-chan store.Event, error) {
	defer s.timed("watch")()
	events, err := s.inner.Watch(ctx, prefix, fromRev)
	s.record("watch", err)
	return events, err
}

func (s *observedStore) GrantLease(ctx context.Context, ttl time.Duration) (store.LeaseID, error) {
	defer s.timed("grant_lease")()
	id, err := s.inner.GrantLease(ctx, ttl)
	s.record("grant_lease", err)
	return id, err
}

func (s *observedStore) KeepAlive(ctx context.Context, id store.LeaseID) error {
	defer s.timed("keep_alive")()
	err := s.inner.KeepAlive(ctx, id)
	s.record("keep_alive", err)
	return err
}

func (s *observedStore) RevokeLease(ctx context.Context, id store.LeaseID) error {
	defer s.timed("revoke_lease")()
	err := s.inner.RevokeLease(ctx, id)
	s.record("revoke_lease", err)
	return err
}

func (s *observedStore) Close() error { return s.inner.Close() }

func (s *observedStore) timed(operation string) func() {
	started := time.Now()
	return func() {
		s.metrics.storeDuration.WithLabelValues(operation).Observe(time.Since(started).Seconds())
	}
}

// record counts a failed operation.
//
// [store.ErrNotFound] and [store.ErrCompareFailed] are not failures. Both are ordinary answers
// this control plane asks for on purpose — a CAS that loses is how two replicas serialise, and a
// missing key is how every first read reports "nothing here yet". Counting them would make the
// failure rate a measure of how busy the reconciler is.
func (s *observedStore) record(operation string, err error) {
	switch {
	case err == nil,
		errors.Is(err, store.ErrNotFound),
		errors.Is(err, store.ErrCompareFailed):
		return
	}
	s.metrics.storeFailures.WithLabelValues(operation).Inc()
}
