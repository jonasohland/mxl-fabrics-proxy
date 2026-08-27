package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/store"
	"github.com/jonasohland/mxl-replicator/internal/store/conformance"
)

// TestConformance is the point of the suite: the same cases that will be pointed at etcd.
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Config{
		New: func(t *testing.T) store.Store {
			return open(t, Options{
				// Fast enough that lease expiry is a test rather than a wait, and still far
				// above anything the suite's own operations take.
				SweepInterval: 20 * time.Millisecond,
			})
		},
		LeaseTTL:   200 * time.Millisecond,
		ExpiryWait: 5 * time.Second,
	})
}

func open(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "store.db"), opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// fakeClock drives lease expiry without sleeping through it. The sweeper's *ticker* is still
// real time — only the expiry comparison is faked — so tests advance the clock and then wait
// for a tick.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{at: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func TestLeaseExpiryIsDrivenByTheClock(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	s := open(t, Options{Now: clock.Now, SweepInterval: 10 * time.Millisecond})
	ctx := context.Background()

	lease, err := s.GrantLease(ctx, time.Hour)
	require.NoError(t, err)
	_, err = s.Put(ctx, "/k", []byte("v"), store.WithLease(lease))
	require.NoError(t, err)

	// Well inside the TTL: several sweeps go by and nothing is collected.
	time.Sleep(50 * time.Millisecond)
	_, err = s.Get(ctx, "/k")
	require.NoError(t, err)

	clock.Advance(2 * time.Hour)
	require.Eventually(t, func() bool {
		_, err := s.Get(ctx, "/k")
		return err == store.ErrNotFound
	}, 5*time.Second, 10*time.Millisecond)
}

// TestKeepAliveRefusesAnExpiredLeaseBeforeItIsSwept pins the §7.1 fencing property that the
// sweeper's periodicity would otherwise put a hole in: between a lease expiring and being
// collected, a late heartbeat must not bring it back. Sweeping is disabled here so the window
// is the whole test rather than a few milliseconds of it.
func TestKeepAliveRefusesAnExpiredLeaseBeforeItIsSwept(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	s := open(t, Options{Now: clock.Now, SweepInterval: time.Hour})
	ctx := context.Background()

	lease, err := s.GrantLease(ctx, time.Minute)
	require.NoError(t, err)
	_, err = s.Put(ctx, "/k", []byte("v"), store.WithLease(lease))
	require.NoError(t, err)

	require.NoError(t, s.KeepAlive(ctx, lease))

	clock.Advance(2 * time.Minute)

	assert.ErrorIs(t, s.KeepAlive(ctx, lease), store.ErrLeaseNotFound)

	// Still present, because nothing has swept yet — and a put against the dead lease is
	// refused all the same.
	_, err = s.Get(ctx, "/k")
	assert.NoError(t, err)

	_, err = s.Put(ctx, "/other", []byte("v"), store.WithLease(lease))
	assert.ErrorIs(t, err, store.ErrLeaseNotFound)
}

// TestWatchDeliversARevisionLargerThanOneBatch is the regression test for the cursor.
//
// A lease revocation deletes every attached key at a single revision, so with more keys than
// watchBatch one revision spans several queries. A cursor tracking only the revision would skip
// the remainder of it on the next pass — silently, and only for large fan-outs, which is to say
// only on the biggest nodes in the fleet.
func TestWatchDeliversARevisionLargerThanOneBatch(t *testing.T) {
	t.Parallel()

	s := open(t, Options{SweepInterval: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const keys = watchBatch + watchBatch/2

	lease, err := s.GrantLease(ctx, time.Hour)
	require.NoError(t, err)
	for i := range keys {
		_, err := s.Put(ctx, fmt.Sprintf("/leased/%04d", i), []byte("v"), store.WithLease(lease))
		require.NoError(t, err)
	}

	events, err := s.Watch(ctx, "/leased/", 0)
	require.NoError(t, err)

	require.NoError(t, s.RevokeLease(ctx, lease))

	seen := make(map[string]bool, keys)
	var revision int64
	for range keys {
		select {
		case ev := <-events:
			require.NoError(t, ev.Err)
			require.Equal(t, store.EventDelete, ev.Type)
			seen[ev.KV.Key] = true
			if revision == 0 {
				revision = ev.KV.ModRevision
			}
			assert.Equal(t, revision, ev.KV.ModRevision, "one revocation is one revision")
		case <-ctx.Done():
			t.Fatalf("timed out after %d of %d delete events", len(seen), keys)
		}
	}
	assert.Len(t, seen, keys)
}

func TestCompactionEndsAStaleWatch(t *testing.T) {
	t.Parallel()

	s := open(t, Options{SweepInterval: 10 * time.Millisecond, HistoryRevisions: 5})
	ctx := context.Background()

	var last int64
	for i := range 40 {
		rev, err := s.Put(ctx, fmt.Sprintf("/k%d", i), []byte("v"))
		require.NoError(t, err)
		last = rev
	}

	// Wait for the sweeper to move compact_revision past the start of history.
	require.Eventually(t, func() bool {
		_, compacted, err := s.revisions(ctx)
		return err == nil && compacted > 0
	}, 5*time.Second, 10*time.Millisecond)

	events, err := s.Watch(ctx, "/", 1)
	require.NoError(t, err)

	select {
	case ev := <-events:
		assert.ErrorIs(t, ev.Err, store.ErrCompacted)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the compaction error")
	}

	// A cursor inside the retained window still works, which is what makes the bound safe: a
	// live consumer holds a revision only between consecutive polls.
	events, err = s.Watch(ctx, "/", last)
	require.NoError(t, err)
	_, err = s.Put(ctx, "/fresh", []byte("v"))
	require.NoError(t, err)

	select {
	case ev := <-events:
		require.NoError(t, ev.Err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event on a live cursor")
	}
}

func TestStateSurvivesReopen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")

	first, err := Open(ctx, path, Options{})
	require.NoError(t, err)
	rev, err := first.Put(ctx, "/desired/requests/r1", []byte(`{"name":"a"}`))
	require.NoError(t, err)
	require.NoError(t, first.Close())

	second, err := Open(ctx, path, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })

	kv, err := second.Get(ctx, "/desired/requests/r1")
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"name":"a"}`), kv.Value)

	// The revision counter continues rather than restarting, or a cursor an agent held across
	// the restart would silently point into the future.
	next, err := second.Put(ctx, "/desired/requests/r2", []byte(`{"name":"b"}`))
	require.NoError(t, err)
	assert.Greater(t, next, rev)
}

func TestOpenCreatesParentDirectories(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "var", "lib", "mxl-replicator", "store.db")
	s, err := Open(context.Background(), path, Options{})
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

func TestConcurrentWritesAreSerialised(t *testing.T) {
	t.Parallel()

	s := open(t, Options{})
	ctx := context.Background()

	const writers, each = 8, 25

	var wg sync.WaitGroup
	revisions := make(chan int64, writers*each)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				rev, err := s.Put(ctx, fmt.Sprintf("/w%d/k%d", w, i), []byte("v"))
				assert.NoError(t, err)
				revisions <- rev
			}
		}()
	}
	wg.Wait()
	close(revisions)

	// Every write got its own revision: none shared, none skipped.
	seen := make(map[int64]bool, writers*each)
	for rev := range revisions {
		assert.False(t, seen[rev], "revision %d handed out twice", rev)
		seen[rev] = true
	}
	assert.Len(t, seen, writers*each)

	kvs, rev, err := s.List(ctx, "/")
	require.NoError(t, err)
	assert.Len(t, kvs, writers*each)
	assert.True(t, seen[rev], "the final revision is the last write's")
}

func TestPrefixEnd(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		prefix  string
		end     string
		bounded bool
	}{
		{"/desired/nodes/", "/desired/nodes0", true}, // '/' + 1 == '0'
		{"a", "b", true},
		{"az", "a{", true},
		{"", "", false},
		{"\xff", "", false},
		{"a\xff", "b", true},
	} {
		end, bounded := prefixEnd(tc.prefix)
		assert.Equal(t, tc.bounded, bounded, "prefix %q", tc.prefix)
		if tc.bounded {
			assert.Equal(t, tc.end, end, "prefix %q", tc.prefix)
		}
	}
}

func TestGrantLeaseRejectsNonPositiveTTL(t *testing.T) {
	t.Parallel()

	s := open(t, Options{})
	_, err := s.GrantLease(context.Background(), 0)
	assert.Error(t, err)
}

func TestRevokeConsumesARevisionWithNoKeysAttached(t *testing.T) {
	t.Parallel()

	s := open(t, Options{SweepInterval: time.Hour})
	ctx := context.Background()

	lease, err := s.GrantLease(ctx, time.Hour)
	require.NoError(t, err)

	_, before, err := s.List(ctx, "/")
	require.NoError(t, err)

	require.NoError(t, s.RevokeLease(ctx, lease))

	_, after, err := s.List(ctx, "/")
	require.NoError(t, err)
	assert.Greater(t, after, before, "a revocation is observable even with nothing attached")
}
