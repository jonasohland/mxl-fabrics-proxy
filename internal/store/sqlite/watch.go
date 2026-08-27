package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/store"
)

// Watch implements [store.Store].
func (s *Store) Watch(ctx context.Context, prefix string, fromRev int64) (<-chan store.Event, error) {
	if s.isClosed() {
		return nil, store.ErrClosed
	}

	cur, compacted, err := s.revisions(ctx)
	if err != nil {
		return nil, err
	}

	from := fromRev
	if from <= 0 {
		// "From now": the next revision to be written. Anything already committed is the
		// caller's to read with List, and delivering it here would duplicate it.
		from = cur + 1
	}

	out := make(chan store.Event, watchBuffer)
	if from <= compacted {
		go func() {
			defer close(out)
			emit(ctx, s.done, out, store.Event{Err: store.ErrCompacted})
		}()
		return out, nil
	}

	go s.watchLoop(ctx, prefix, cursor{revision: from - 1, seq: math.MaxInt64}, out)
	return out, nil
}

// cursor is the position of a watch in the history: the last event it delivered.
//
// It is a (revision, seq) *pair* rather than a revision, and that is load-bearing rather than
// tidy. One revision can hold many events — a lease expiring takes out every key attached to it
// at once — so a batch boundary can fall inside a revision. A revision-only cursor advanced to
// that revision would then ask for `revision > it` on the next pass and skip the rest of it: a
// silently dropped delete, which reads downstream as a node that never went away.
type cursor struct {
	revision int64
	seq      int64
}

// revisions reads the current and compacted revisions together.
func (s *Store) revisions(ctx context.Context) (current, compacted int64, err error) {
	row := s.db.QueryRowContext(ctx, `SELECT revision, compact_revision FROM meta WHERE id = 1`)
	if err := row.Scan(&current, &compacted); err != nil {
		return 0, 0, fmt.Errorf("sqlite: read revisions: %w", err)
	}
	return current, compacted, nil
}

// watchLoop reads history forward from a cursor, waiting between passes.
//
// The wakeup is taken *before* the query, not after it. A commit that lands while the query is
// in flight closes the channel this pass is about to wait on, so the wait returns immediately
// and the next pass sees the change. Taking it afterwards would lose exactly those wakeups —
// rarely, under load, and presenting as an agent that sat out a whole poll interval before
// noticing a new epoch (§6.1).
func (s *Store) watchLoop(ctx context.Context, prefix string, cur cursor, out chan store.Event) {
	defer close(out)

	for {
		wake := s.notify.waiter()

		events, next, err := s.readEvents(ctx, prefix, cur)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			emit(ctx, s.done, out, store.Event{Err: err})
			return
		}

		for _, ev := range events {
			if !emit(ctx, s.done, out, ev) {
				return
			}
		}
		cur = next

		if len(events) == watchBatch {
			// A full batch means there is probably more waiting. Do not sleep on it.
			continue
		}

		select {
		case <-wake:
		case <-time.After(s.opts.PollInterval):
		case <-ctx.Done():
			return
		case <-s.done:
			emit(ctx, s.done, out, store.Event{Err: store.ErrClosed})
			return
		}
	}
}

// readEvents returns up to watchBatch events after cur, in revision then sequence order, along
// with the cursor to resume from.
//
// The compaction check shares the read transaction with the query. A sweeper that compacts past
// the cursor between the two would otherwise leave the watcher silently skipping the discarded
// range, which is the one failure this whole mechanism must not have: a missed delete leaves a
// consumer believing a node is still there.
func (s *Store) readEvents(ctx context.Context, prefix string, cur cursor) ([]store.Event, cursor, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, cur, fmt.Errorf("sqlite: begin read: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only

	var compacted int64
	if err := tx.QueryRowContext(ctx, `SELECT compact_revision FROM meta WHERE id = 1`).Scan(&compacted); err != nil {
		return nil, cur, fmt.Errorf("sqlite: read revisions: %w", err)
	}
	if cur.revision < compacted {
		return nil, cur, store.ErrCompacted
	}

	const columns = `revision, seq, key, type, value, create_revision, mod_revision, version, lease`

	// The pair comparison, spelled out so it can use the (revision, seq) primary key.
	const after = `(revision > :rev OR (revision = :rev AND seq > :seq))`

	var rows *sql.Rows
	if end, bounded := prefixEnd(prefix); bounded {
		rows, err = tx.QueryContext(ctx, `
			SELECT `+columns+` FROM events
			WHERE `+after+` AND key >= :lo AND key < :hi
			ORDER BY revision, seq LIMIT :limit`,
			sql.Named("rev", cur.revision), sql.Named("seq", cur.seq),
			sql.Named("lo", prefix), sql.Named("hi", end), sql.Named("limit", watchBatch))
	} else {
		rows, err = tx.QueryContext(ctx, `
			SELECT `+columns+` FROM events
			WHERE `+after+`
			ORDER BY revision, seq LIMIT :limit`,
			sql.Named("rev", cur.revision), sql.Named("seq", cur.seq),
			sql.Named("limit", watchBatch))
	}
	if err != nil {
		return nil, cur, fmt.Errorf("sqlite: read events: %w", err)
	}
	defer rows.Close()

	out := make([]store.Event, 0, watchBatch)
	next := cur
	for rows.Next() {
		var (
			ev    store.Event
			pos   cursor
			typ   int
			value []byte
			lease int64
		)
		err := rows.Scan(&pos.revision, &pos.seq, &ev.KV.Key, &typ, &value,
			&ev.KV.CreateRevision, &ev.KV.ModRevision, &ev.KV.Version, &lease)
		if err != nil {
			return nil, cur, fmt.Errorf("sqlite: read events: %w", err)
		}
		ev.Type = store.EventType(typ)
		ev.KV.Lease = store.LeaseID(lease)
		if ev.Type == store.EventPut {
			if value == nil {
				value = []byte{}
			}
			ev.KV.Value = value
		}
		out = append(out, ev)
		next = pos
	}
	if err := rows.Err(); err != nil {
		return nil, cur, fmt.Errorf("sqlite: read events: %w", err)
	}
	return out, next, nil
}

// emit delivers one event, reporting whether the watch should carry on.
func emit(ctx context.Context, done <-chan struct{}, out chan<- store.Event, ev store.Event) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	case <-done:
		return false
	}
}
