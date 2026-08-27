package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jonasohland/mxl-replicator/internal/store"
)

// Get reads and decodes one key.
//
// A missing key is not an error: it returns an [Entry] with Found false, which is the same shape
// [Fleet] hands out and can be passed straight to [PutJSON] as a prior. Callers that must
// distinguish "absent" from "unreadable" get that from Found and err respectively.
func Get[T any](ctx context.Context, s store.Store, key string) (Entry[T], error) {
	kv, err := s.Get(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return Entry[T]{}, nil
	}
	if err != nil {
		return Entry[T]{}, err
	}
	return decode[T](*kv)
}

// WriteOptions controls one [PutJSON].
type WriteOptions struct {
	// Lease attaches the key to an agent's liveness lease, so that observed state collects
	// itself when the agent goes away (§8.3).
	//
	// It must be passed on **every** write of a leased key, not only the first: a put with no
	// lease detaches the one the key was holding, which quietly turns observed state into state
	// that outlives its agent.
	Lease store.LeaseID

	// CAS makes the write conditional on the key still being at [Prior.Rev] — or still absent,
	// if the prior was not found.
	//
	// The reconciler uses it on every derived write. It is not concurrency control between
	// reconcile passes (there is only ever one reconciler), it is the guard against a *demoted*
	// leader: one that was partitioned and has not noticed yet is computing from a stale read,
	// so its write loses to the new leader's rather than fighting it (§4.6).
	CAS bool
}

// PutJSON encodes value and writes it, skipping the write entirely when the stored bytes and
// lease already match.
//
// The skip is the point. The store follows etcd in advancing its revision for every write,
// including one that stores a byte-identical value, and every revision wakes every watcher on
// the prefix (§8.1). An unconditional rewrite of an unchanged assignment set would therefore be
// indistinguishable from a real change to an agent's long poll — which is a worker restart on
// the far side of a control plane that had nothing to say.
//
// It reports whether it wrote, and the revision: the store's current revision if it skipped,
// the revision of the write if it did not.
func PutJSON(ctx context.Context, s store.Store, key string, value any, prior Prior, opts WriteOptions) (int64, bool, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0, false, fmt.Errorf("encode %s: %w", key, err)
	}

	if prior.Found && prior.Lease == opts.Lease && bytes.Equal(prior.Raw, encoded) {
		return prior.Rev, false, nil
	}

	var putOpts []store.PutOpt
	if opts.Lease != 0 {
		putOpts = append(putOpts, store.WithLease(opts.Lease))
	}
	if opts.CAS {
		if prior.Found {
			putOpts = append(putOpts, store.IfRevision(prior.Rev))
		} else {
			putOpts = append(putOpts, store.IfAbsent())
		}
	}

	rev, err := s.Put(ctx, key, encoded, putOpts...)
	if err != nil {
		return rev, false, err
	}
	return rev, true, nil
}
