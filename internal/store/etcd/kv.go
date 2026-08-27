package etcd

import (
	"context"
	"errors"
	"fmt"

	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/jonasohland/mxl-replicator/internal/store"
)

// Get implements [store.Store].
func (s *Store) Get(ctx context.Context, key string) (*store.KV, error) {
	if s.isClosed() {
		return nil, store.ErrClosed
	}

	resp, err := s.kv.Get(ctx, key)
	if err != nil {
		return nil, translate("get "+key, err)
	}
	if len(resp.Kvs) == 0 {
		return nil, store.ErrNotFound
	}

	kv := fromPB(resp.Kvs[0])
	return &kv, nil
}

// List implements [store.Store].
//
// The revision returned is the one etcd served the range at, which is what makes the
// list-then-watch handoff of §9.2 gapless: it is a property of the read, not of the keys in it,
// so it is still right when the revision that moved touched a key outside the prefix or removed
// one inside it.
//
// The read is linearizable, which is clientv3's default and is left that way on purpose. A
// serializable read is served from whichever replica answered and can be arbitrarily stale, and
// a cursor taken from a stale read is §4.5's oscillating agent.
func (s *Store) List(ctx context.Context, prefix string) ([]store.KV, int64, error) {
	if s.isClosed() {
		return nil, 0, store.ErrClosed
	}

	resp, err := s.kv.Get(ctx, prefix,
		clientv3.WithPrefix(),
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	if err != nil {
		return nil, 0, translate("list "+prefix, err)
	}

	var out []store.KV
	for _, kv := range resp.Kvs {
		out = append(out, fromPB(kv))
	}
	return out, resp.Header.Revision, nil
}

// Put implements [store.Store].
//
// A conditional put is a transaction rather than a put with a comparison attached, because etcd
// has no such thing: compare-and-swap is the txn, and the returned header revision is what the
// interface promises in both outcomes — the revision of the write on success, and the store's
// current revision on [store.ErrCompareFailed], which is the value a retry loop wants next.
func (s *Store) Put(ctx context.Context, key string, value []byte, opts ...store.PutOpt) (int64, error) {
	cfg, err := store.NewPutConfig(opts)
	if err != nil {
		return 0, err
	}
	if s.isClosed() {
		return 0, store.ErrClosed
	}
	if value == nil {
		value = []byte{}
	}

	var putOpts []clientv3.OpOption
	if cfg.Lease != 0 {
		putOpts = append(putOpts, clientv3.WithLease(clientv3.LeaseID(cfg.Lease)))
	}

	if !cfg.IfAbsent && !cfg.HasIfRevision {
		resp, err := s.kv.Put(ctx, key, string(value), putOpts...)
		if err != nil {
			return 0, translate("put "+key, err)
		}
		return resp.Header.Revision, nil
	}

	resp, err := s.kv.Txn(ctx).
		If(compare(key, cfg.IfAbsent, cfg.IfRevision)).
		Then(clientv3.OpPut(key, string(value), putOpts...)).
		Commit()
	if err != nil {
		return 0, translate("put "+key, err)
	}
	if !resp.Succeeded {
		return resp.Header.Revision, store.ErrCompareFailed
	}
	return resp.Header.Revision, nil
}

// Delete implements [store.Store].
//
// Deleting a key that is not there advances nothing: etcd only allocates a revision for a
// transaction that actually changed something, so the response carries the unchanged current
// revision. That is the same behaviour the sqlite backend goes out of its way to produce, and
// the reason is the same — a reconciler polling a cursor must not see churn it generated with
// its own no-ops.
func (s *Store) Delete(ctx context.Context, key string, opts ...store.DelOpt) (int64, error) {
	cfg, err := store.NewDelConfig(opts)
	if err != nil {
		return 0, err
	}
	if s.isClosed() {
		return 0, store.ErrClosed
	}

	if !cfg.HasIfRevision {
		resp, err := s.kv.Delete(ctx, key)
		if err != nil {
			return 0, translate("delete "+key, err)
		}
		return resp.Header.Revision, nil
	}

	resp, err := s.kv.Txn(ctx).
		If(compare(key, false, cfg.IfRevision)).
		Then(clientv3.OpDelete(key)).
		Commit()
	if err != nil {
		return 0, translate("delete "+key, err)
	}
	if !resp.Succeeded {
		return resp.Header.Revision, store.ErrCompareFailed
	}
	return resp.Header.Revision, nil
}

// compare builds the guard for a conditional operation.
//
// [store.IfAbsent] is a create-revision comparison rather than a mod-revision one even though
// both read as "revision 0 means absent": create-revision is 0 only for a key that does not
// exist, while mod-revision compares equal to 0 for an absent key *and* would have to be
// reasoned about again if etcd ever grew a key whose mod-revision could be reset.
func compare(key string, ifAbsent bool, rev int64) clientv3.Cmp {
	if ifAbsent {
		return clientv3.Compare(clientv3.CreateRevision(key), "=", 0)
	}
	return clientv3.Compare(clientv3.ModRevision(key), "=", rev)
}

// fromPB converts etcd's key-value into the store's.
func fromPB(kv *mvccpb.KeyValue) store.KV {
	value := kv.Value
	if value == nil {
		// An empty value round-trips through protobuf as nil. The sqlite backend hands back an
		// empty slice, and a backend difference visible to `x == nil` is exactly the kind the
		// conformance suite cannot see and a caller can.
		value = []byte{}
	}
	return store.KV{
		Key:            string(kv.Key),
		Value:          value,
		CreateRevision: kv.CreateRevision,
		ModRevision:    kv.ModRevision,
		Version:        kv.Version,
		Lease:          store.LeaseID(kv.Lease),
	}
}

// translate maps etcd's errors onto the interface's, and wraps everything else with the
// operation that produced it.
//
// Only two of etcd's errors have a meaning the caller is expected to branch on. Everything else
// — no leader, unreachable endpoint, exceeded quota — is an infrastructure failure that the
// caller can only report and retry, so it keeps etcd's own wording rather than being flattened
// into something less informative.
func translate(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, rpctypes.ErrLeaseNotFound):
		return store.ErrLeaseNotFound
	case errors.Is(err, rpctypes.ErrCompacted):
		return store.ErrCompacted
	}
	return fmt.Errorf("etcd: %s: %w", op, err)
}
