// Package conformance is the shared test suite every [store.Store] backend must pass.
//
// It exists because §8.1's bet — define the interface in etcd's terms, emulate over sqlite — is
// only safe if something checks it. The suite is written against sqlite, which is the backend
// that has to emulate, and must then pass **unchanged** against etcd. If it does not, the
// interface has taken on a property of one backend and the abstraction has leaked, which is
// exactly the failure §8.1 is worried about.
//
// It follows that nothing here may be written to sqlite's behaviour where the interface does
// not require it. In practice that rules out three things, and each of them is a real
// temptation:
//
//   - **Absolute revision numbers.** A backend may start anywhere and may advance for reasons of
//     its own. Every assertion here is relative: revisions increase, or they do not move.
//   - **A shared clock.** Lease expiry is asserted by waiting, not by advancing an injected
//     clock, because there is no injecting etcd's. [Config.LeaseTTL] is the knob that keeps
//     that affordable on a backend with a fast one.
//   - **The whole key space.** Every case works under its own prefix, so the suite is
//     indifferent to whether the store it was handed is genuinely empty.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/store"
)

// Config describes the backend under test.
type Config struct {
	// New opens a store for one test. The backend is responsible for cleaning it up, via
	// t.Cleanup.
	New func(t *testing.T) store.Store

	// LeaseTTL is the shortest lease the backend honours. sqlite can take milliseconds; etcd
	// rounds up to a couple of seconds, so the suite asks rather than assumes.
	LeaseTTL time.Duration

	// ExpiryWait is how long to wait for an expired lease to be collected, on top of the TTL.
	// Expiry is eventual in every backend — a sweeper here, the leader's lease queue in etcd.
	ExpiryWait time.Duration

	// EventWait is how long to wait for a watch event before failing. Defaults to 5s.
	EventWait time.Duration
}

func (c *Config) setDefaults() {
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = time.Second
	}
	if c.ExpiryWait <= 0 {
		c.ExpiryWait = 5 * time.Second
	}
	if c.EventWait <= 0 {
		c.EventWait = 5 * time.Second
	}
}

// Run executes the whole suite against one backend.
func Run(t *testing.T, cfg Config) {
	t.Helper()
	cfg.setDefaults()

	cases := []struct {
		name string
		fn   func(*testing.T, Config)
	}{
		{"GetPutDelete", testGetPutDelete},
		{"KeyMetadata", testKeyMetadata},
		{"RevisionMonotonicity", testRevisionMonotonicity},
		{"NoOpDeleteDoesNotAdvance", testNoOpDeleteDoesNotAdvance},
		{"ListSortedAndScoped", testListSortedAndScoped},
		{"PrefixIsolation", testPrefixIsolation},
		{"CompareAndSwap", testCompareAndSwap},
		{"CompareAndSwapContention", testCompareAndSwapContention},
		{"IfAbsent", testIfAbsent},
		{"WatchFromNow", testWatchFromNow},
		{"WatchDeletes", testWatchDeletes},
		{"WatchResumeFromCursor", testWatchResumeFromCursor},
		{"WatchListHandoffHasNoGap", testWatchListHandoffHasNoGap},
		{"WatchIsPrefixScoped", testWatchIsPrefixScoped},
		{"LeaseKeepAlive", testLeaseKeepAlive},
		{"LeaseExpiryCollectsKeys", testLeaseExpiryCollectsKeys},
		{"LeaseRevoke", testLeaseRevoke},
		{"PutWithDeadLease", testPutWithDeadLease},
		{"PutWithoutLeaseDetaches", testPutWithoutLeaseDetaches},
		{"ClosedStore", testClosedStore},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, cfg) })
	}
}

// prefix gives each case its own corner of the key space, so the suite never depends on the
// store it was handed being empty.
func prefix(t *testing.T) string {
	return "/conformance/" + t.Name() + "/"
}

func testGetPutDelete(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	key := prefix(t) + "a"

	_, err := s.Get(ctx, key)
	require.ErrorIs(t, err, store.ErrNotFound, "get of an absent key")

	_, err = s.Put(ctx, key, []byte("one"))
	require.NoError(t, err)

	kv, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, key, kv.Key)
	assert.Equal(t, []byte("one"), kv.Value)

	_, err = s.Put(ctx, key, []byte("two"))
	require.NoError(t, err)

	kv, err = s.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("two"), kv.Value)

	_, err = s.Delete(ctx, key)
	require.NoError(t, err)

	_, err = s.Get(ctx, key)
	require.ErrorIs(t, err, store.ErrNotFound, "get after delete")
}

func testKeyMetadata(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	key := prefix(t) + "a"

	createRev, err := s.Put(ctx, key, []byte("one"))
	require.NoError(t, err)

	first, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, createRev, first.CreateRevision)
	assert.Equal(t, createRev, first.ModRevision)
	assert.Equal(t, int64(1), first.Version)

	modRev, err := s.Put(ctx, key, []byte("two"))
	require.NoError(t, err)

	second, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, createRev, second.CreateRevision, "create revision survives a rewrite")
	assert.Equal(t, modRev, second.ModRevision)
	assert.Equal(t, int64(2), second.Version)

	// Deleting and rewriting is a new life for the key, not a continuation of the old one.
	_, err = s.Delete(ctx, key)
	require.NoError(t, err)
	recreateRev, err := s.Put(ctx, key, []byte("three"))
	require.NoError(t, err)

	third, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, recreateRev, third.CreateRevision, "create revision resets after a delete")
	assert.Equal(t, int64(1), third.Version, "version resets after a delete")
}

func testRevisionMonotonicity(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	p := prefix(t)

	var last int64
	for i := range 20 {
		// Alternate keys and operations: the revision is store-wide, so it must advance
		// regardless of which key moved.
		key := fmt.Sprintf("%sk%d", p, i%3)
		rev, err := s.Put(ctx, key, []byte(fmt.Sprint(i)))
		require.NoError(t, err)
		assert.Greater(t, rev, last, "put must advance the store revision")
		last = rev

		if i%4 == 3 {
			rev, err := s.Delete(ctx, key)
			require.NoError(t, err)
			assert.Greater(t, rev, last, "delete must advance the store revision")
			last = rev
		}
	}

	// A rewrite with an identical value still counts as a write. This is etcd's behaviour and
	// the interface's: a store that quietly skipped it would give CAS loops and watch cursors
	// different semantics on the two backends.
	key := p + "same"
	first, err := s.Put(ctx, key, []byte("v"))
	require.NoError(t, err)
	second, err := s.Put(ctx, key, []byte("v"))
	require.NoError(t, err)
	assert.Greater(t, second, first, "an unchanged rewrite still advances the revision")
}

func testNoOpDeleteDoesNotAdvance(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	p := prefix(t)

	before, err := s.Put(ctx, p+"a", []byte("v"))
	require.NoError(t, err)

	rev, err := s.Delete(ctx, p+"absent")
	require.NoError(t, err, "deleting an absent key is not an error")
	assert.Equal(t, before, rev, "deleting an absent key advances nothing")

	_, listed, err := s.List(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, before, listed)
}

func testListSortedAndScoped(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	p := prefix(t)

	for _, name := range []string{"c", "a", "b"} {
		_, err := s.Put(ctx, p+name, []byte(name))
		require.NoError(t, err)
	}

	kvs, rev, err := s.List(ctx, p)
	require.NoError(t, err)
	require.Len(t, kvs, 3)
	assert.Equal(t, []string{p + "a", p + "b", p + "c"}, keysOf(kvs), "list is sorted by key")
	assert.Equal(t, []byte("a"), kvs[0].Value)
	assert.Positive(t, rev)

	_, err = s.Delete(ctx, p+"b")
	require.NoError(t, err)

	kvs, after, err := s.List(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, []string{p + "a", p + "c"}, keysOf(kvs))
	assert.Greater(t, after, rev)
}

func testPrefixIsolation(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	p := prefix(t)

	// The sibling is the case that matters: "nodes" shares a string prefix with "node", so a
	// backend matching path components instead of raw bytes, or forgetting the trailing slash,
	// returns one key space when asked for another.
	require.NoError(t, put(ctx, s, p+"node/one", "a"))
	require.NoError(t, put(ctx, s, p+"nodes/one", "b"))
	require.NoError(t, put(ctx, s, p+"nodes/two", "c"))
	require.NoError(t, put(ctx, s, p+"other", "d"))

	kvs, _, err := s.List(ctx, p+"nodes/")
	require.NoError(t, err)
	assert.Equal(t, []string{p + "nodes/one", p + "nodes/two"}, keysOf(kvs))

	kvs, _, err = s.List(ctx, p+"node/")
	require.NoError(t, err)
	assert.Equal(t, []string{p + "node/one"}, keysOf(kvs))

	// Without the trailing slash it is a byte prefix and does match both, which is the
	// documented behaviour rather than a bug — the suite pins it so a backend cannot decide to
	// be helpful about it.
	kvs, _, err = s.List(ctx, p+"node")
	require.NoError(t, err)
	assert.Equal(t, []string{p + "node/one", p + "nodes/one", p + "nodes/two"}, keysOf(kvs))
}

func testCompareAndSwap(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	key := prefix(t) + "a"

	rev, err := s.Put(ctx, key, []byte("one"))
	require.NoError(t, err)

	_, err = s.Put(ctx, key, []byte("stale"), store.IfRevision(rev-1))
	require.ErrorIs(t, err, store.ErrCompareFailed)

	kv, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("one"), kv.Value, "a failed compare writes nothing")

	next, err := s.Put(ctx, key, []byte("two"), store.IfRevision(rev))
	require.NoError(t, err)
	assert.Greater(t, next, rev)

	// The old revision is now stale, which is the whole mechanism.
	_, err = s.Put(ctx, key, []byte("three"), store.IfRevision(rev))
	require.ErrorIs(t, err, store.ErrCompareFailed)

	_, err = s.Delete(ctx, key, store.IfRevision(rev))
	require.ErrorIs(t, err, store.ErrCompareFailed, "delete compares too")

	_, err = s.Delete(ctx, key, store.IfRevision(next))
	require.NoError(t, err)
}

func testCompareAndSwapContention(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	key := prefix(t) + "counter"

	const writers, each = 8, 10

	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				for {
					var (
						value int
						rev   int64
					)
					kv, err := s.Get(ctx, key)
					switch {
					case errors.Is(err, store.ErrNotFound):
					case err != nil:
						assert.NoError(t, err)
						return
					default:
						fmt.Sscanf(string(kv.Value), "%d", &value)
						rev = kv.ModRevision
					}

					next := fmt.Appendf(nil, "%d", value+1)
					opt := store.IfRevision(rev)
					if rev == 0 {
						opt = store.IfRevision(0)
					}
					_, err = s.Put(ctx, key, next, opt)
					if errors.Is(err, store.ErrCompareFailed) {
						continue // lost the race; re-read and try again
					}
					assert.NoError(t, err)
					break
				}
			}
		}()
	}
	wg.Wait()

	kv, err := s.Get(ctx, key)
	require.NoError(t, err)
	var total int
	fmt.Sscanf(string(kv.Value), "%d", &total)
	assert.Equal(t, writers*each, total, "no update may be lost under contention")
	assert.Equal(t, int64(writers*each), kv.Version)
}

func testIfAbsent(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	key := prefix(t) + "a"

	_, err := s.Put(ctx, key, []byte("first"), store.IfAbsent())
	require.NoError(t, err)

	// This is §7.1's fencing: the second claimant loses rather than both proceeding.
	_, err = s.Put(ctx, key, []byte("second"), store.IfAbsent())
	require.ErrorIs(t, err, store.ErrCompareFailed)

	kv, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("first"), kv.Value)

	_, err = s.Delete(ctx, key)
	require.NoError(t, err)

	_, err = s.Put(ctx, key, []byte("third"), store.IfAbsent())
	require.NoError(t, err, "absent again after a delete")
}

func testWatchFromNow(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	p := prefix(t)

	require.NoError(t, put(ctx, s, p+"before", "old"))

	events, err := s.Watch(ctx, p, 0)
	require.NoError(t, err)

	require.NoError(t, put(ctx, s, p+"after", "new"))

	ev := recv(t, cfg, events)
	assert.Equal(t, store.EventPut, ev.Type)
	assert.Equal(t, p+"after", ev.KV.Key, "a watch from now must not replay history")
	assert.Equal(t, []byte("new"), ev.KV.Value)
}

func testWatchDeletes(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	p := prefix(t)

	require.NoError(t, put(ctx, s, p+"a", "v"))

	events, err := s.Watch(ctx, p, 0)
	require.NoError(t, err)

	rev, err := s.Delete(ctx, p+"a")
	require.NoError(t, err)

	ev := recv(t, cfg, events)
	assert.Equal(t, store.EventDelete, ev.Type)
	assert.Equal(t, p+"a", ev.KV.Key)
	assert.Equal(t, rev, ev.KV.ModRevision, "a delete event carries the revision it happened at")
}

// testWatchResumeFromCursor is the missed-revision case: everything happens while nothing is
// watching, and the watch is established afterwards from a cursor the caller kept.
func testWatchResumeFromCursor(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	p := prefix(t)

	require.NoError(t, put(ctx, s, p+"a", "1"))
	_, from, err := s.List(ctx, p)
	require.NoError(t, err)

	require.NoError(t, put(ctx, s, p+"b", "2"))
	require.NoError(t, put(ctx, s, p+"c", "3"))
	_, err = s.Delete(ctx, p+"b")
	require.NoError(t, err)

	events, err := s.Watch(ctx, p, from+1)
	require.NoError(t, err)

	assertSequence(t, cfg, events,
		event{store.EventPut, p + "b"},
		event{store.EventPut, p + "c"},
		event{store.EventDelete, p + "b"},
	)
}

// testWatchListHandoffHasNoGap pins the handoff §9.2's long poll is built on: list, then watch
// from the revision the list reported. A backend that returned a revision inconsistent with its
// own snapshot would either replay a change already in it or, far worse, drop one forever.
func testWatchListHandoffHasNoGap(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	p := prefix(t)

	require.NoError(t, put(ctx, s, p+"a", "in-snapshot"))

	kvs, from, err := s.List(ctx, p)
	require.NoError(t, err)
	require.Len(t, kvs, 1)

	events, err := s.Watch(ctx, p, from+1)
	require.NoError(t, err)

	require.NoError(t, put(ctx, s, p+"b", "after"))

	assertSequence(t, cfg, events, event{store.EventPut, p + "b"})
}

func testWatchIsPrefixScoped(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	p := prefix(t)

	events, err := s.Watch(ctx, p+"wanted/", 0)
	require.NoError(t, err)

	require.NoError(t, put(ctx, s, p+"other/x", "no"))
	require.NoError(t, put(ctx, s, p+"wanted/y", "yes"))

	assertSequence(t, cfg, events, event{store.EventPut, p + "wanted/y"})
}

func testLeaseKeepAlive(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	key := prefix(t) + "leased"

	lease, err := s.GrantLease(ctx, cfg.LeaseTTL)
	require.NoError(t, err)

	_, err = s.Put(ctx, key, []byte("v"), store.WithLease(lease))
	require.NoError(t, err)

	// Hold it past its TTL with heartbeats, the way an agent does (§7.1).
	deadline := time.Now().Add(2 * cfg.LeaseTTL)
	for time.Now().Before(deadline) {
		require.NoError(t, s.KeepAlive(ctx, lease))
		time.Sleep(cfg.LeaseTTL / 4)
	}

	require.NoError(t, s.KeepAlive(ctx, lease))
	_, err = s.Get(ctx, key)
	assert.NoError(t, err, "a lease held by keepalives does not expire")
}

func testLeaseExpiryCollectsKeys(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	p := prefix(t)

	lease, err := s.GrantLease(ctx, cfg.LeaseTTL)
	require.NoError(t, err)

	require.NoError(t, putLeased(ctx, s, p+"inventory", "v", lease))
	require.NoError(t, putLeased(ctx, s, p+"status", "v", lease))
	require.NoError(t, put(ctx, s, p+"registration", "v"))

	events, err := s.Watch(ctx, p, 0)
	require.NoError(t, err)

	// Expiry is eventual in every backend, so this waits rather than asserting immediately.
	require.Eventually(t, func() bool {
		_, err := s.Get(ctx, p+"inventory")
		return errors.Is(err, store.ErrNotFound)
	}, cfg.LeaseTTL+cfg.ExpiryWait, cfg.LeaseTTL/4, "leased keys outlived their lease")

	_, err = s.Get(ctx, p+"status")
	assert.ErrorIs(t, err, store.ErrNotFound, "every key on the lease goes")

	_, err = s.Get(ctx, p+"registration")
	assert.NoError(t, err, "unleased keys are untouched — a registration outlives its agent (§7.1)")

	// The collection must be observable, or a consumer holding a watch never learns the node
	// went away.
	seen := map[string]bool{}
	for range 2 {
		ev := recv(t, cfg, events)
		require.Equal(t, store.EventDelete, ev.Type)
		seen[ev.KV.Key] = true
	}
	assert.Equal(t, map[string]bool{p + "inventory": true, p + "status": true}, seen)

	assert.ErrorIs(t, s.KeepAlive(ctx, lease), store.ErrLeaseNotFound,
		"an expired lease cannot be revived")
}

func testLeaseRevoke(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	p := prefix(t)

	lease, err := s.GrantLease(ctx, time.Hour)
	require.NoError(t, err)
	require.NoError(t, putLeased(ctx, s, p+"a", "v", lease))
	require.NoError(t, putLeased(ctx, s, p+"b", "v", lease))

	require.NoError(t, s.RevokeLease(ctx, lease))

	kvs, _, err := s.List(ctx, p)
	require.NoError(t, err)
	assert.Empty(t, kvs, "revoking takes every attached key with it")

	assert.ErrorIs(t, s.RevokeLease(ctx, lease), store.ErrLeaseNotFound)
	assert.ErrorIs(t, s.KeepAlive(ctx, lease), store.ErrLeaseNotFound)
}

func testPutWithDeadLease(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	key := prefix(t) + "a"

	lease, err := s.GrantLease(ctx, time.Hour)
	require.NoError(t, err)
	require.NoError(t, s.RevokeLease(ctx, lease))

	_, err = s.Put(ctx, key, []byte("v"), store.WithLease(lease))
	require.ErrorIs(t, err, store.ErrLeaseNotFound)

	_, err = s.Get(ctx, key)
	assert.ErrorIs(t, err, store.ErrNotFound, "a refused put writes nothing")
}

// testPutWithoutLeaseDetaches pins the footgun in [store.Store.Put]: observed state is
// rewritten on every report (§9.2), and a rewrite that forgets WithLease turns leased state
// into state that outlives the agent that reported it.
func testPutWithoutLeaseDetaches(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	key := prefix(t) + "a"

	lease, err := s.GrantLease(ctx, time.Hour)
	require.NoError(t, err)
	require.NoError(t, putLeased(ctx, s, key, "leased", lease))

	kv, err := s.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, lease, kv.Lease)

	require.NoError(t, put(ctx, s, key, "rewritten"))

	kv, err = s.Get(ctx, key)
	require.NoError(t, err)
	assert.Zero(t, kv.Lease, "a put without WithLease detaches the lease")

	require.NoError(t, s.RevokeLease(ctx, lease))
	_, err = s.Get(ctx, key)
	assert.NoError(t, err, "the detached key survives its former lease")
}

func testClosedStore(t *testing.T, cfg Config) {
	ctx, s := setup(t, cfg)
	key := prefix(t) + "a"

	require.NoError(t, put(ctx, s, key, "v"))

	events, err := s.Watch(ctx, prefix(t), 0)
	require.NoError(t, err)

	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "Close is idempotent")

	_, err = s.Get(ctx, key)
	assert.ErrorIs(t, err, store.ErrClosed)
	_, err = s.Put(ctx, key, []byte("v"))
	assert.ErrorIs(t, err, store.ErrClosed)

	// An outstanding watch ends rather than hanging.
	select {
	case _, ok := <-events:
		if ok {
			// A terminal error event is permitted, but the channel must then close.
			select {
			case _, ok := <-events:
				assert.False(t, ok, "the channel closes after its terminal event")
			case <-time.After(cfg.EventWait):
				t.Fatal("watch channel did not close after Close")
			}
		}
	case <-time.After(cfg.EventWait):
		t.Fatal("watch did not end when the store closed")
	}
}

// --- helpers ---

func setup(t *testing.T, cfg Config) (context.Context, store.Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, cfg.New(t)
}

func put(ctx context.Context, s store.Store, key, value string) error {
	_, err := s.Put(ctx, key, []byte(value))
	return err
}

func putLeased(ctx context.Context, s store.Store, key, value string, lease store.LeaseID) error {
	_, err := s.Put(ctx, key, []byte(value), store.WithLease(lease))
	return err
}

func keysOf(kvs []store.KV) []string {
	out := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, kv.Key)
	}
	return out
}

type event struct {
	typ store.EventType
	key string
}

func recv(t *testing.T, cfg Config, events <-chan store.Event) store.Event {
	t.Helper()
	select {
	case ev, ok := <-events:
		require.True(t, ok, "watch channel closed unexpectedly")
		require.NoError(t, ev.Err, "watch ended with an error")
		return ev
	case <-time.After(cfg.EventWait):
		t.Fatal("timed out waiting for a watch event")
		return store.Event{}
	}
}

// assertSequence requires exactly these events, in order, and nothing before them.
func assertSequence(t *testing.T, cfg Config, events <-chan store.Event, want ...event) {
	t.Helper()
	for i, w := range want {
		ev := recv(t, cfg, events)
		assert.Equalf(t, w.typ, ev.Type, "event %d type", i)
		assert.Equalf(t, w.key, ev.KV.Key, "event %d key", i)
	}
}
