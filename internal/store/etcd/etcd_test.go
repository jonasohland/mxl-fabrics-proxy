package etcd

import (
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/jonasohland/mxl-replicator/internal/store"
	"github.com/jonasohland/mxl-replicator/internal/store/conformance"
)

// TestConformance is the deliverable of M3: the same suite, unchanged, that the sqlite backend
// passes. If it needed a single case relaxed or a single backend-specific branch, the interface
// would have taken on a property of one backend and §8.1's bet would have failed.
func TestConformance(t *testing.T) {
	endpoints := startEtcd(t)

	conformance.Run(t, conformance.Config{
		New: func(t *testing.T) store.Store {
			return newStore(t, endpoints, DefaultPrefix)
		},
		// Seconds rather than sqlite's milliseconds. etcd's TTL granularity is one second and
		// its revocation pass is periodic on top of that, so this is the floor rather than a
		// margin — which is exactly what Config.LeaseTTL exists to let a backend say.
		LeaseTTL:   2 * time.Second,
		ExpiryWait: 10 * time.Second,
	})
}

// TestPrefixNamespacesStoredKeys pins what --store-etcd-prefix is for: one etcd cluster hosting
// more than one deployment, or sharing space with something else.
//
// The assertion that matters is the one made with the raw client. Filtering reads by prefix
// would satisfy every other check here while still writing into the neighbour's key space, and
// the failure mode of getting that wrong is two fleets quietly reconciling each other's nodes.
func TestPrefixNamespacesStoredKeys(t *testing.T) {
	endpoints := startEtcd(t)
	ctx := t.Context()

	blue := newStore(t, endpoints, "/blue")
	green := newStore(t, endpoints, "/green")

	_, err := blue.Put(ctx, store.NodeKey("edge-01"), []byte("blue"))
	require.NoError(t, err)
	_, err = green.Put(ctx, store.NodeKey("edge-01"), []byte("green"))
	require.NoError(t, err)

	kv, err := blue.Get(ctx, store.NodeKey("edge-01"))
	require.NoError(t, err)
	assert.Equal(t, []byte("blue"), kv.Value)
	assert.Equal(t, store.NodeKey("edge-01"), kv.Key, "keys come back without the namespace")

	kvs, _, err := green.List(ctx, store.PrefixNodes)
	require.NoError(t, err)
	require.Len(t, kvs, 1, "a list sees only its own deployment")
	assert.Equal(t, []byte("green"), kvs[0].Value)

	raw := rawClient(t, endpoints)
	resp, err := raw.Get(ctx, "/blue"+store.NodeKey("edge-01"))
	require.NoError(t, err)
	require.Len(t, resp.Kvs, 1, "the prefix is part of the stored key, not a read-side filter")
	assert.Equal(t, []byte("blue"), resp.Kvs[0].Value)

	// And a watch is scoped the same way, with the namespace stripped back off on the way out.
	events, err := blue.Watch(ctx, store.PrefixNodes, 0)
	require.NoError(t, err)

	_, err = green.Put(ctx, store.NodeKey("edge-02"), []byte("green"))
	require.NoError(t, err)
	_, err = blue.Put(ctx, store.NodeKey("edge-02"), []byte("blue"))
	require.NoError(t, err)

	ev := recv(t, events)
	assert.Equal(t, store.EventPut, ev.Type)
	assert.Equal(t, store.NodeKey("edge-02"), ev.KV.Key)
	assert.Equal(t, []byte("blue"), ev.KV.Value)
}

// TestWatchIsEstablishedBeforeReturn is the one etcd behaviour this package had to work around
// rather than inherit. clientv3 creates the server-side watcher asynchronously, so without the
// created-notification handshake in Watch a write issued immediately afterwards can land at a
// revision the watcher was created past — and is then never delivered, rather than delivered
// late. The loop is because that race is a race: it reproduces in a fraction of attempts.
func TestWatchIsEstablishedBeforeReturn(t *testing.T) {
	endpoints := startEtcd(t)
	ctx := t.Context()

	s := newStore(t, endpoints, DefaultPrefix)

	for i := range 25 {
		prefix := fmt.Sprintf("/derived/watch-race/%d/", i)

		events, err := s.Watch(ctx, prefix, 0)
		require.NoError(t, err)

		_, err = s.Put(ctx, prefix+"a", []byte("v"))
		require.NoError(t, err)

		ev := recv(t, events)
		require.Equal(t, prefix+"a", ev.KV.Key, "attempt %d: the write raced watch creation", i)
	}
}

// TestWatchFromCompactedRevision covers the one case the conformance suite cannot construct on
// its own: it has no way to compact a backend's history, since compaction is a cluster-wide
// operator action in etcd and an internal sweeper in sqlite.
func TestWatchFromCompactedRevision(t *testing.T) {
	endpoints := startEtcd(t)
	ctx := t.Context()

	s := newStore(t, endpoints, DefaultPrefix)
	prefix := "/derived/compacted/"

	first, err := s.Put(ctx, prefix+"a", []byte("1"))
	require.NoError(t, err)
	latest, err := s.Put(ctx, prefix+"b", []byte("2"))
	require.NoError(t, err)

	// Compaction is deliberately not something this backend does to a cluster it may be
	// sharing, so the test does what an operator's --auto-compaction-retention would.
	raw := rawClient(t, endpoints)
	_, err = raw.Compact(ctx, latest)
	require.NoError(t, err)

	events, err := s.Watch(ctx, prefix, first)
	require.NoError(t, err)

	ev := recv(t, events)
	assert.ErrorIs(t, ev.Err, store.ErrCompacted)
	assertClosed(t, events)

	// The documented recovery, and the point of reporting it rather than silently skipping the
	// discarded range: re-list for a snapshot and a fresh revision, then watch from there.
	kvs, rev, err := s.List(ctx, prefix)
	require.NoError(t, err)
	require.Len(t, kvs, 2)

	events, err = s.Watch(ctx, prefix, rev+1)
	require.NoError(t, err)

	_, err = s.Put(ctx, prefix+"c", []byte("3"))
	require.NoError(t, err)

	ev = recv(t, events)
	require.NoError(t, ev.Err)
	assert.Equal(t, prefix+"c", ev.KV.Key)
}

// TestGrantLeaseRoundsTTLUp pins the direction of the rounding. etcd's TTL is whole seconds, and
// rounding the other way would hand back a lease shorter than the caller asked for — or, under
// a second, one that was already expired when it was granted.
//
// The assertion is a floor rather than an equality because it is not the only rounding in play:
// etcd enforces a cluster minimum of its own, derived from the election timeout, so a
// single-node cluster with default timings will not grant less than two seconds however little
// is asked for. That is the same promise from the other side — the interface says a TTL may be
// rounded up and must be treated as a floor — so the exact case is asserted separately, above
// any plausible cluster minimum, to pin that nothing here rounds up more than it has to.
func TestGrantLeaseRoundsTTLUp(t *testing.T) {
	endpoints := startEtcd(t)
	ctx := t.Context()

	s := newStore(t, endpoints, DefaultPrefix)
	raw := rawClient(t, endpoints)

	grantedTTL := func(ttl time.Duration) int64 {
		t.Helper()
		id, err := s.GrantLease(ctx, ttl)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.RevokeLease(ctx, id) })

		resp, err := raw.TimeToLive(ctx, clientv3.LeaseID(id))
		require.NoError(t, err)
		return resp.GrantedTTL
	}

	for _, tc := range []struct {
		ttl     time.Duration
		atLeast int64
	}{
		{100 * time.Millisecond, 1},
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{2500 * time.Millisecond, 3},
	} {
		assert.GreaterOrEqualf(t, grantedTTL(tc.ttl), tc.atLeast, "ttl %s", tc.ttl)
	}

	assert.Equal(t, int64(15), grantedTTL(15*time.Second), "a whole-second ttl is passed through")

	_, err := s.GrantLease(ctx, 0)
	assert.Error(t, err, "a zero ttl is a lease that is already dead")
}

// TestKeepAliveDoesNotRenewInTheBackground is §7.1's fencing, stated as a property of this
// backend: clientv3 also offers a KeepAlive that renews on a background stream until the client
// stops caring, and reaching for it here would mean a node's identity — and its observed state
// — outliving the agent that stopped heartbeating.
func TestKeepAliveDoesNotRenewInTheBackground(t *testing.T) {
	endpoints := startEtcd(t)
	ctx := t.Context()

	s := newStore(t, endpoints, DefaultPrefix)
	key := store.LeaseKey("edge-01")

	lease, err := s.GrantLease(ctx, time.Second)
	require.NoError(t, err)
	_, err = s.Put(ctx, key, []byte("v"), store.WithLease(lease))
	require.NoError(t, err)

	require.NoError(t, s.KeepAlive(ctx, lease), "one call, one renewal")

	// Then nothing. A background renewer would keep this alive indefinitely.
	require.Eventually(t, func() bool {
		_, err := s.Get(ctx, key)
		return errors.Is(err, store.ErrNotFound)
	}, 15*time.Second, 200*time.Millisecond, "a lease nobody renewed did not expire")

	assert.ErrorIs(t, s.KeepAlive(ctx, lease), store.ErrLeaseNotFound)
}

// TestOpenRejectsUnreachableCluster pins the probe. clientv3.New does not dial, so without it a
// server pointed at a dead etcd would log a successful start and fail on its first request.
func TestOpenRejectsUnreachableCluster(t *testing.T) {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))

	_, err := Open(t.Context(), Options{
		Endpoints:   []string{endpoint},
		DialTimeout: time.Second,
		Logger:      slog.New(slog.DiscardHandler),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), endpoint, "the error names what could not be reached")

	_, err = Open(t.Context(), Options{Logger: slog.New(slog.DiscardHandler)})
	assert.Error(t, err, "no endpoints is a configuration error, not a connection attempt")
}

// TestNewDoesNotCloseTheClient covers the seam §8.2's leader election needs: election and
// storage share one connection, so closing the store must not take the connection with it.
func TestNewDoesNotCloseTheClient(t *testing.T) {
	endpoints := startEtcd(t)
	ctx := t.Context()

	cli := rawClient(t, endpoints)
	s := New(cli, Options{Prefix: "/shared", Logger: slog.New(slog.DiscardHandler)})

	_, err := s.Put(ctx, store.NodeKey("edge-01"), []byte("v"))
	require.NoError(t, err)

	events, err := s.Watch(ctx, store.PrefixNodes, 0)
	require.NoError(t, err)

	require.NoError(t, s.Close())

	_, err = s.Get(ctx, store.NodeKey("edge-01"))
	assert.ErrorIs(t, err, store.ErrClosed, "the store is closed even though the client is not")
	assertClosed(t, events)

	// The client is still usable, which is the whole point.
	resp, err := cli.Get(ctx, "/shared"+store.NodeKey("edge-01"))
	require.NoError(t, err)
	assert.Len(t, resp.Kvs, 1)
}

// TestPrefixDefaultsAndTrimming: "/mxl-replicator" and "/mxl-replicator/" have to name one key
// space, because the store's own keys are absolute and a trailing slash would otherwise produce
// "//desired/..." — a different key, invisible to every list made the other way.
func TestPrefixDefaultsAndTrimming(t *testing.T) {
	endpoints := startEtcd(t)
	ctx := t.Context()

	slashed := newStore(t, endpoints, DefaultPrefix+"/")
	assert.Equal(t, DefaultPrefix, slashed.Prefix())

	_, err := slashed.Put(ctx, store.NodeKey("edge-01"), []byte("v"))
	require.NoError(t, err)

	plain := newStore(t, endpoints, DefaultPrefix)
	kv, err := plain.Get(ctx, store.NodeKey("edge-01"))
	require.NoError(t, err)
	assert.Equal(t, []byte("v"), kv.Value)

	// An unset prefix is the default, not the cluster root: getting that backwards would put a
	// deployment's nodes and requests over the root of a cluster it may be sharing.
	defaulted := newStore(t, endpoints, "")
	assert.Equal(t, DefaultPrefix, defaulted.Prefix())

	// The root is still reachable, but only by asking for it.
	root := newStore(t, endpoints, "/")
	assert.Empty(t, root.Prefix())

	_, err = root.Put(ctx, "/rooted", []byte("v"))
	require.NoError(t, err)

	resp, err := rawClient(t, endpoints).Get(ctx, "/rooted")
	require.NoError(t, err)
	assert.Len(t, resp.Kvs, 1)
}

// --- helpers ---

// rawClient is an unnamespaced client, for the assertions that have to look at what is actually
// stored and for the operations this backend deliberately does not offer.
func rawClient(t *testing.T, endpoints []string) *clientv3.Client {
	t.Helper()

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
		Logger:      newZapLogger(slog.New(slog.DiscardHandler)),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })

	return cli
}

func recv(t *testing.T, events <-chan store.Event) store.Event {
	t.Helper()

	select {
	case ev, ok := <-events:
		require.True(t, ok, "watch channel closed unexpectedly")
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a watch event")
		return store.Event{}
	}
}

func assertClosed(t *testing.T, events <-chan store.Event) {
	t.Helper()

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			require.Error(t, ev.Err, "unexpected event %+v before the channel closed", ev.KV)
		case <-time.After(10 * time.Second):
			t.Fatal("watch channel did not close")
		}
	}
}
