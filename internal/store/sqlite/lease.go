package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/store"
)

// grantAttempts bounds the retry on a lease id collision. With 63 random bits a single retry is
// already improbable; the loop exists so that an exhausted random source fails loudly instead
// of spinning.
const grantAttempts = 8

// GrantLease implements [store.Store].
//
// Granting does not advance the store revision. A lease is not a key, nothing watches one, and
// bumping the revision here would wake every watcher in the fleet each time an agent registers.
func (s *Store) GrantLease(ctx context.Context, ttl time.Duration) (store.LeaseID, error) {
	if s.isClosed() {
		return 0, store.ErrClosed
	}
	if ttl <= 0 {
		return 0, errors.New("sqlite: lease ttl must be positive")
	}

	ttlMS := ttl.Milliseconds()
	if ttlMS < 1 {
		// Sub-millisecond TTLs round up rather than to zero. A zero TTL is a lease that is
		// already expired, which is never what the caller meant.
		ttlMS = 1
	}
	expires := nowMS(s.now()) + ttlMS

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	for range grantAttempts {
		id := randomLeaseID()
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO leases (id, ttl_ms, expires_ms) VALUES (?, ?, ?)`, int64(id), ttlMS, expires)
		switch {
		case err == nil:
			return id, nil
		case isConstraintErr(err):
			continue
		default:
			return 0, fmt.Errorf("sqlite: grant lease: %w", err)
		}
	}
	return 0, errors.New("sqlite: grant lease: no free lease id")
}

// KeepAlive implements [store.Store].
//
// A lease whose expiry has already passed is refused even though the sweeper may not have
// collected it yet. Sweeping is periodic, so the alternative would be a window in which a late
// heartbeat resurrects a lease the server has already stopped counting — and node identity is
// held by exactly this lease (§7.1), so that window is one in which two agents could both
// believe they hold one node name.
func (s *Store) KeepAlive(ctx context.Context, id store.LeaseID) error {
	if s.isClosed() {
		return store.ErrClosed
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	now := nowMS(s.now())
	res, err := s.db.ExecContext(ctx, `
		UPDATE leases SET expires_ms = :now + ttl_ms
		WHERE id = :id AND expires_ms > :now`,
		sql.Named("now", now), sql.Named("id", int64(id)))
	if err != nil {
		return fmt.Errorf("sqlite: keepalive %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: keepalive %d: %w", id, err)
	}
	if n == 0 {
		return store.ErrLeaseNotFound
	}
	return nil
}

// RevokeLease implements [store.Store].
func (s *Store) RevokeLease(ctx context.Context, id store.LeaseID) error {
	_, err := s.write(ctx, func(w *writeTx) error {
		alive, err := w.leaseAlive(id, nowMS(s.now()))
		if err != nil {
			return err
		}
		if !alive {
			return store.ErrLeaseNotFound
		}
		return w.dropLease(id)
	})
	return err
}

// leaseAlive reports whether a lease exists and has not expired.
func (w *writeTx) leaseAlive(id store.LeaseID, now int64) (bool, error) {
	var expires int64
	err := w.tx.QueryRowContext(w.ctx,
		`SELECT expires_ms FROM leases WHERE id = ?`, int64(id)).Scan(&expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("sqlite: read lease %d: %w", id, err)
	}
	return expires > now, nil
}

// dropLease deletes a lease and every key attached to it, at one revision.
//
// One revision for the whole set, not one each: a lease going away is a single event in the
// world — a node left — and splitting it across revisions would let a watcher observe a node
// half gone, with its status collected and its inventory still present.
func (w *writeTx) dropLease(id store.LeaseID) error {
	rows, err := w.tx.QueryContext(w.ctx, `SELECT key FROM kv WHERE lease = ? ORDER BY key`, int64(id))
	if err != nil {
		return fmt.Errorf("sqlite: read lease %d keys: %w", id, err)
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return fmt.Errorf("sqlite: read lease %d keys: %w", id, err)
		}
		keys = append(keys, key)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("sqlite: read lease %d keys: %w", id, err)
	}

	for _, key := range keys {
		if err := w.deleteKV(key); err != nil {
			return err
		}
	}

	if _, err := w.tx.ExecContext(w.ctx, `DELETE FROM leases WHERE id = ?`, int64(id)); err != nil {
		return fmt.Errorf("sqlite: delete lease %d: %w", id, err)
	}

	// A lease with no keys attached still has to consume a revision, or revoking it would be
	// invisible to a watcher and to anything using the revision as a cursor.
	w.revision()
	return nil
}

// sweepLoop collects expired leases and compacts history.
func (s *Store) sweepLoop() {
	defer close(s.sweepDone)

	ticker := time.NewTicker(s.opts.SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}

		// Bounded per pass: the sweeper must not be the thing that holds the write lock while
		// an agent is trying to heartbeat.
		ctx, cancel := context.WithTimeout(context.Background(), s.opts.SweepInterval)
		if err := s.sweepExpired(ctx); err != nil && !errors.Is(err, store.ErrClosed) {
			s.opts.Logger.Warn("store: sweeping expired leases failed", "error", err)
		}
		if err := s.compact(ctx); err != nil && !errors.Is(err, store.ErrClosed) {
			s.opts.Logger.Warn("store: compacting history failed", "error", err)
		}
		cancel()
	}
}

// sweepExpired revokes every lease whose expiry has passed, each at its own revision.
func (s *Store) sweepExpired(ctx context.Context) error {
	now := nowMS(s.now())

	rows, err := s.db.QueryContext(ctx, `SELECT id FROM leases WHERE expires_ms <= ?`, now)
	if err != nil {
		return fmt.Errorf("sqlite: read expired leases: %w", err)
	}
	var expired []store.LeaseID
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("sqlite: read expired leases: %w", err)
		}
		expired = append(expired, store.LeaseID(id))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("sqlite: read expired leases: %w", err)
	}

	for _, id := range expired {
		// Re-checked inside the transaction against the same clock, so a keepalive that landed
		// between the query above and here wins rather than losing to a stale read.
		_, err := s.write(ctx, func(w *writeTx) error {
			alive, err := w.leaseAlive(id, nowMS(s.now()))
			if err != nil || alive {
				return err
			}
			return w.dropLease(id)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// compact discards watch history older than [Options.HistoryRevisions].
//
// It moves compact_revision first and deletes afterwards, both in one transaction, so a watcher
// can never observe a window where the events are gone but the store still claims to have them.
func (s *Store) compact(ctx context.Context) error {
	if s.isClosed() {
		return store.ErrClosed
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin compact: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	var current, compacted int64
	err = tx.QueryRowContext(ctx, `SELECT revision, compact_revision FROM meta WHERE id = 1`).
		Scan(&current, &compacted)
	if err != nil {
		return fmt.Errorf("sqlite: read revisions: %w", err)
	}

	target := current - s.opts.HistoryRevisions
	if target <= compacted {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `UPDATE meta SET compact_revision = ? WHERE id = 1`, target); err != nil {
		return fmt.Errorf("sqlite: set compact revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE revision <= ?`, target); err != nil {
		return fmt.Errorf("sqlite: delete events: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit compact: %w", err)
	}
	return nil
}

// isConstraintErr reports whether err is a uniqueness violation, which here means only one
// thing: a lease id that collided with a live one. Matched on the message because the driver
// surfaces the sqlite result code as text.
func isConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}
