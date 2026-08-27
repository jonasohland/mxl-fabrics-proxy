// Package sqlite implements [store.Store] over a local sqlite database.
//
// This is the single-node backend (§2). It is written first, before etcd, on purpose: sqlite
// is the whole interface minus everything etcd gives away for free, so building it first
// proves the emulation §8.1 promises is actually small before an etcd backend can hide a leaky
// abstraction behind features it happens to have. The conformance suite is written here and
// must then pass unchanged against etcd; if it does not, the interface is wrong.
//
// # The emulation
//
// Three things etcd has and sqlite does not, and what each costs here:
//
//   - **Revisions.** One counter in a single-row table, read and bumped inside the same
//     transaction as the write it belongs to. Store-wide, not per-key.
//   - **Watch.** An append-only history table written by every mutating transaction, which
//     watchers read forward through from a cursor. History is bounded — see the compaction
//     note on [Options.HistoryRevisions] — so a watch resumed from far enough back gets
//     [store.ErrCompacted] rather than a silently incomplete stream.
//   - **Leases.** An expiry column and a sweeper goroutine. Expiry is therefore *eventual*: a
//     key outlives its lease by up to [Options.SweepInterval]. [store.Store.KeepAlive] does not
//     wait for the sweeper though — it refuses a lease whose expiry has passed, so a lease the
//     server considers dead can never be resurrected by a late heartbeat, which is what §7.1's
//     fencing needs.
//
// # Concurrency
//
// The database runs in WAL mode, so reads are concurrent, and every write transaction is
// serialised behind one process-local mutex. That is not a limitation being worked around:
// this backend is single-node by definition (§8.2), the write rate is a handful of small
// values per agent heartbeat (§8.3), and serialising in Go rather than discovering
// SQLITE_BUSY halfway through a transaction removes a whole class of retry logic that would
// otherwise have to be right.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	// The cgo-free sqlite driver. Cgo-free matters here: the deployment is a container that
	// already carries libmxl, libmxl-fabrics, libfabric and libspdlog (WRS §9), and a cgo build
	// of the control plane would add a second toolchain constraint to an image that is awkward
	// enough already.
	_ "modernc.org/sqlite"

	"github.com/jonasohland/mxl-replicator/internal/store"
)

// Defaults for [Options].
const (
	DefaultSweepInterval    = time.Second
	DefaultPollInterval     = time.Second
	DefaultHistoryRevisions = 20_000
)

// watchBatch is how many events one watch query fetches. A watcher that finds a full batch
// loops again immediately rather than waiting, so this bounds memory per poll, not throughput.
const watchBatch = 512

// watchBuffer is the per-watch channel buffer. Enough to absorb a lease revocation taking out
// one agent's whole observed state without stalling the writer's notification.
const watchBuffer = 64

// Options configures a [Store]. The zero value is usable; every field has a default.
type Options struct {
	// Now is the clock, defaulting to [time.Now]. Injectable so lease expiry can be tested
	// without sleeping through it.
	Now func() time.Time

	// SweepInterval is how often expired leases are collected and history is compacted.
	// It is the upper bound on how long a key outlives the lease it was written under.
	SweepInterval time.Duration

	// PollInterval is the watch backstop. Watches are woken directly by the transaction that
	// commits, so this is not the latency anything normally sees — it exists so that a missed
	// wakeup degrades to polling rather than to a hang, which is the same trade the agent's
	// long poll makes against a buffering proxy (§9.2).
	PollInterval time.Duration

	// HistoryRevisions is how many revisions of watch history to keep. Older events are
	// discarded and watches resuming from before the cut get [store.ErrCompacted].
	//
	// History has to be bounded by something: events carry the value written, a target_info
	// blob is 1–2 KB, and a fabric outage writes one per worker restart (§8.3) — so an unbounded
	// table is a slow leak in exactly the deployment that can least afford one. The default is
	// far above any live cursor: agents hold a revision only between consecutive long polls,
	// and the recovery from losing one is a re-list.
	HistoryRevisions int64

	// Logger receives sweeper failures, which have nowhere else to go — nothing is waiting on
	// the sweeper's return value. Defaults to [slog.Default].
	Logger *slog.Logger
}

func (o *Options) setDefaults() {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.SweepInterval <= 0 {
		o.SweepInterval = DefaultSweepInterval
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.HistoryRevisions <= 0 {
		o.HistoryRevisions = DefaultHistoryRevisions
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Store is a [store.Store] backed by a sqlite database.
type Store struct {
	db   *sql.DB
	opts Options

	// writeMu serialises write transactions. See the package comment.
	writeMu sync.Mutex

	// notify wakes watchers the instant a transaction commits.
	notify *notifier

	closeOnce sync.Once
	done      chan struct{}
	sweepDone chan struct{}

	mu     sync.RWMutex
	closed bool
}

var _ store.Store = (*Store)(nil)

// Open opens or creates the database at path.
//
// Parent directories are created, so a fresh deployment does not have to arrange
// /var/lib/mxl-replicator to exist before the server starts.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	opts.setDefaults()

	if path == "" {
		return nil, errors.New("sqlite: no database path")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("sqlite: create %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}

	s := &Store{
		db:        db,
		opts:      opts,
		notify:    newNotifier(),
		done:      make(chan struct{}),
		sweepDone: make(chan struct{}),
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}

	go s.sweepLoop()
	return s, nil
}

// dsn builds the connection string. The pragmas are per-connection, which is why they live
// here rather than in migrate: database/sql opens connections whenever it feels like it.
func dsn(path string) string {
	q := url.Values{}
	// WAL: concurrent readers against a writer, which is what makes the watch backstop poll
	// and the reconciler's reads coexist with heartbeat writes.
	q.Add("_pragma", "journal_mode(wal)")
	// A backstop only. Writes are already serialised in Go; this covers the checkpointer and
	// anything holding the file briefly (a backup, a stray sqlite3 shell).
	q.Add("_pragma", "busy_timeout(10000)")
	// NORMAL under WAL: durable across process crash, may lose the last transactions on power
	// loss. Correct for this store — observed state is rebuilt from agent reports within a
	// heartbeat (§4), and desired state is written by a user who is still holding the response.
	q.Add("_pragma", "synchronous(normal)")
	return "file:" + path + "?" + q.Encode()
}

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	id               INTEGER PRIMARY KEY CHECK (id = 1),
	revision         INTEGER NOT NULL,
	compact_revision INTEGER NOT NULL
);

INSERT OR IGNORE INTO meta (id, revision, compact_revision) VALUES (1, 0, 0);

CREATE TABLE IF NOT EXISTS kv (
	key             TEXT PRIMARY KEY,
	value           BLOB NOT NULL,
	create_revision INTEGER NOT NULL,
	mod_revision    INTEGER NOT NULL,
	version         INTEGER NOT NULL,
	lease           INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS kv_lease ON kv (lease) WHERE lease <> 0;

CREATE TABLE IF NOT EXISTS events (
	revision        INTEGER NOT NULL,
	seq             INTEGER NOT NULL,
	key             TEXT NOT NULL,
	type            INTEGER NOT NULL,
	value           BLOB,
	create_revision INTEGER NOT NULL,
	mod_revision    INTEGER NOT NULL,
	version         INTEGER NOT NULL,
	lease           INTEGER NOT NULL,
	PRIMARY KEY (revision, seq)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS leases (
	id         INTEGER PRIMARY KEY,
	ttl_ms     INTEGER NOT NULL,
	expires_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS leases_expires ON leases (expires_ms);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
	}
	return nil
}

// Close ends every watch, stops the sweeper and closes the database.
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		close(s.done)
		<-s.sweepDone
		err = s.db.Close()
	})
	return err
}

func (s *Store) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

func (s *Store) now() time.Time { return s.opts.Now() }

func nowMS(t time.Time) int64 { return t.UnixMilli() }

// writeTx is one write transaction, and the only thing that may allocate a revision.
//
// A revision is allocated lazily, by the first mutation that actually happens. That is what
// makes a delete of an absent key, or a CAS that fails its compare, cost nothing: no revision
// is consumed, no watcher is woken, and callers polling a cursor see no change where there was
// none.
type writeTx struct {
	ctx context.Context
	tx  *sql.Tx

	// cur is the store revision as it was when this transaction began.
	cur int64

	// next is the revision allocated to this transaction, or 0 if it has not mutated anything.
	next int64

	// seq orders events within one revision. A lease revocation deletes every attached key at
	// a single revision, so revision alone does not order them.
	seq int64
}

// revision allocates this transaction's revision, or returns the one already allocated.
func (w *writeTx) revision() int64 {
	if w.next == 0 {
		w.next = w.cur + 1
	}
	return w.next
}

// write runs fn inside a write transaction and returns the resulting store revision.
//
// On error the transaction is rolled back and the store's *current* revision is returned,
// which is what a CAS retry loop wants to see next after [store.ErrCompareFailed].
func (s *Store) write(ctx context.Context, fn func(*writeTx) error) (int64, error) {
	if s.isClosed() {
		return 0, store.ErrClosed
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sqlite: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	var cur int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM meta WHERE id = 1`).Scan(&cur); err != nil {
		return 0, fmt.Errorf("sqlite: read revision: %w", err)
	}

	w := &writeTx{ctx: ctx, tx: tx, cur: cur}
	if err := fn(w); err != nil {
		return cur, err
	}

	if w.next == 0 {
		// Nothing happened. Commit anyway so any reads fn made are released cleanly.
		if err := tx.Commit(); err != nil {
			return cur, fmt.Errorf("sqlite: commit: %w", err)
		}
		return cur, nil
	}

	if _, err := tx.ExecContext(ctx, `UPDATE meta SET revision = ? WHERE id = 1`, w.next); err != nil {
		return cur, fmt.Errorf("sqlite: bump revision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return cur, fmt.Errorf("sqlite: commit: %w", err)
	}

	// Only after the commit is durable, or a watcher could read ahead of what it can see.
	s.notify.wake()
	return w.next, nil
}

// notifier is a broadcast wakeup: watchers take the current channel, do their query, then wait
// on it. Taking the channel *before* querying is what makes it race-free — a commit landing in
// between closes the channel the watcher is about to wait on, so the wait returns immediately
// rather than sleeping through a change that already happened.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func newNotifier() *notifier { return &notifier{ch: make(chan struct{})} }

func (n *notifier) waiter() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *notifier) wake() {
	n.mu.Lock()
	defer n.mu.Unlock()
	close(n.ch)
	n.ch = make(chan struct{})
}

// prefixEnd returns the exclusive upper bound of the key range covered by prefix, and whether
// there is one at all. An empty prefix, or one that is all 0xff bytes, is unbounded above.
func prefixEnd(prefix string) (string, bool) {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			end := make([]byte, i+1)
			copy(end, b)
			end[i]++
			return string(end), true
		}
	}
	return "", false
}

// randomLeaseID returns a non-zero random lease id. Random rather than sequential so that a
// lease id from a previous database cannot be mistaken for a live one by a client that held it
// across a restore.
func randomLeaseID() store.LeaseID {
	var buf [8]byte
	for {
		rand.Read(buf[:])
		// Positive: the column is a signed INTEGER and a negative id would round-trip fine but
		// read badly in logs and in etcd, where lease ids are positive int64.
		id := int64(binary.BigEndian.Uint64(buf[:]) &^ (1 << 63))
		if id != 0 {
			return store.LeaseID(id)
		}
	}
}
