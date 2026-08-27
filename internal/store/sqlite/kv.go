package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jonasohland/mxl-replicator/internal/store"
)

const kvColumns = `key, value, create_revision, mod_revision, version, lease`

// Get implements [store.Store].
func (s *Store) Get(ctx context.Context, key string) (*store.KV, error) {
	if s.isClosed() {
		return nil, store.ErrClosed
	}

	row := s.db.QueryRowContext(ctx, `SELECT `+kvColumns+` FROM kv WHERE key = ?`, key)
	kv, err := scanKV(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, store.ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("sqlite: get %s: %w", key, err)
	}
	return &kv, nil
}

// List implements [store.Store].
//
// The rows and the revision are read in one read transaction, deliberately. Reading them
// separately would leave a window in which a write lands between the two, and the caller would
// get a snapshot paired with a revision that is either ahead of it — so a watch from
// revision+1 misses that write forever — or behind it, so the write is delivered twice. The
// first of those is a silent lost update in the agent long poll (§9.2); WAL gives a consistent
// read snapshot for free, so there is no reason to take the risk.
func (s *Store) List(ctx context.Context, prefix string) ([]store.KV, int64, error) {
	if s.isClosed() {
		return nil, 0, store.ErrClosed
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, fmt.Errorf("sqlite: begin read: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only

	var rows *sql.Rows
	if end, bounded := prefixEnd(prefix); bounded {
		rows, err = tx.QueryContext(ctx,
			`SELECT `+kvColumns+` FROM kv WHERE key >= ? AND key < ? ORDER BY key`, prefix, end)
	} else {
		rows, err = tx.QueryContext(ctx, `SELECT `+kvColumns+` FROM kv ORDER BY key`)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("sqlite: list %s: %w", prefix, err)
	}
	defer rows.Close()

	var out []store.KV
	for rows.Next() {
		kv, err := scanKV(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("sqlite: list %s: %w", prefix, err)
		}
		out = append(out, kv)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("sqlite: list %s: %w", prefix, err)
	}

	var rev int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM meta WHERE id = 1`).Scan(&rev); err != nil {
		return nil, 0, fmt.Errorf("sqlite: read revision: %w", err)
	}
	return out, rev, nil
}

// Put implements [store.Store].
func (s *Store) Put(ctx context.Context, key string, value []byte, opts ...store.PutOpt) (int64, error) {
	cfg, err := store.NewPutConfig(opts)
	if err != nil {
		return 0, err
	}
	if value == nil {
		value = []byte{}
	}

	return s.write(ctx, func(w *writeTx) error {
		cur, err := w.lookup(key)
		if err != nil {
			return err
		}
		if cfg.IfAbsent && cur != nil {
			return store.ErrCompareFailed
		}
		if cfg.HasIfRevision && modRevision(cur) != cfg.IfRevision {
			return store.ErrCompareFailed
		}
		if cfg.Lease != 0 {
			alive, err := w.leaseAlive(cfg.Lease, nowMS(s.now()))
			if err != nil {
				return err
			}
			if !alive {
				return store.ErrLeaseNotFound
			}
		}

		rev := w.revision()
		kv := store.KV{
			Key:            key,
			Value:          value,
			CreateRevision: rev,
			ModRevision:    rev,
			Version:        1,
			Lease:          cfg.Lease,
		}
		if cur != nil {
			kv.CreateRevision, kv.Version = cur.CreateRevision, cur.Version+1
		}
		return w.putKV(kv)
	})
}

// Delete implements [store.Store].
func (s *Store) Delete(ctx context.Context, key string, opts ...store.DelOpt) (int64, error) {
	cfg, err := store.NewDelConfig(opts)
	if err != nil {
		return 0, err
	}

	return s.write(ctx, func(w *writeTx) error {
		cur, err := w.lookup(key)
		if err != nil {
			return err
		}
		if cfg.HasIfRevision && modRevision(cur) != cfg.IfRevision {
			return store.ErrCompareFailed
		}
		if cur == nil {
			// A no-op, and specifically not an error: a reconciler that has computed "this key
			// should not exist" is right whether or not it already does.
			return nil
		}
		return w.deleteKV(key)
	})
}

// modRevision is the value a compare is made against: an absent key compares as revision 0,
// matching etcd, which is what makes IfRevision(0) mean "must not exist".
func modRevision(kv *store.KV) int64 {
	if kv == nil {
		return 0
	}
	return kv.ModRevision
}

// lookup reads a key inside the transaction, returning nil if it is absent.
func (w *writeTx) lookup(key string) (*store.KV, error) {
	row := w.tx.QueryRowContext(w.ctx, `SELECT `+kvColumns+` FROM kv WHERE key = ?`, key)
	kv, err := scanKV(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("sqlite: read %s: %w", key, err)
	}
	return &kv, nil
}

func (w *writeTx) putKV(kv store.KV) error {
	_, err := w.tx.ExecContext(w.ctx, `
		INSERT INTO kv (key, value, create_revision, mod_revision, version, lease)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value           = excluded.value,
			create_revision = excluded.create_revision,
			mod_revision    = excluded.mod_revision,
			version         = excluded.version,
			lease           = excluded.lease`,
		kv.Key, kv.Value, kv.CreateRevision, kv.ModRevision, kv.Version, int64(kv.Lease))
	if err != nil {
		return fmt.Errorf("sqlite: put %s: %w", kv.Key, err)
	}
	return w.appendEvent(store.EventPut, kv)
}

func (w *writeTx) deleteKV(key string) error {
	if _, err := w.tx.ExecContext(w.ctx, `DELETE FROM kv WHERE key = ?`, key); err != nil {
		return fmt.Errorf("sqlite: delete %s: %w", key, err)
	}
	// A delete event carries the key and the revision it happened at, and nothing else — there
	// is no value to report and the previous one is not the caller's business (etcd requires an
	// explicit option for it, and nothing in this project wants it).
	return w.appendEvent(store.EventDelete, store.KV{Key: key, ModRevision: w.revision()})
}

func (w *writeTx) appendEvent(typ store.EventType, kv store.KV) error {
	rev := w.revision()
	seq := w.seq
	w.seq++

	_, err := w.tx.ExecContext(w.ctx, `
		INSERT INTO events (revision, seq, key, type, value, create_revision, mod_revision, version, lease)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rev, seq, kv.Key, int(typ), kv.Value, kv.CreateRevision, kv.ModRevision, kv.Version, int64(kv.Lease))
	if err != nil {
		return fmt.Errorf("sqlite: append event for %s: %w", kv.Key, err)
	}
	return nil
}

// scanner is satisfied by both [sql.Row] and [sql.Rows].
type scanner interface {
	Scan(dest ...any) error
}

func scanKV(src scanner) (store.KV, error) {
	var (
		kv    store.KV
		value []byte
		lease int64
	)
	err := src.Scan(&kv.Key, &value, &kv.CreateRevision, &kv.ModRevision, &kv.Version, &lease)
	if err != nil {
		return store.KV{}, err
	}
	if value == nil {
		value = []byte{}
	}
	kv.Value, kv.Lease = value, store.LeaseID(lease)
	return kv, nil
}
