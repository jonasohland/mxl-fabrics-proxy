package etcd

import (
	"context"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/jonasohland/mxl-replicator/internal/store"
)

// Watch implements [store.Store].
//
// It does not return until etcd has confirmed the watcher exists. That is not politeness about
// error reporting, it is the contract: [clientv3.Watcher.Watch] creates the server-side watcher
// asynchronously, and a "from now" watcher starts at whatever revision was current when the
// *server* processed the create request. A caller that watched and then immediately wrote —
// which is what every reconciler and every test does — could otherwise have its own write land
// first and never be told about it. Waiting for the created-notification closes that window
// completely: anything the caller does after Watch returns happens at a revision the watcher
// already covers.
//
// The cost of that wait is that a Watch against an unreachable cluster blocks rather than
// failing: clientv3 retries the create indefinitely, and this call returns when it succeeds or
// when ctx ends, whichever comes first. That is the right shape for a level-triggered consumer,
// which has nothing to do without a store anyway and wants to resume the moment there is one —
// but it means **the caller's context is the only bound**, so give one a deadline wherever a
// hung watch would be worse than a failed one (§9.2's long poll, above all).
func (s *Store) Watch(ctx context.Context, prefix string, fromRev int64) (<-chan store.Event, error) {
	if s.isClosed() {
		return nil, store.ErrClosed
	}

	opts := []clientv3.OpOption{clientv3.WithPrefix(), clientv3.WithCreatedNotify()}
	if fromRev > 0 {
		// Zero or negative is "from now", which is clientv3's meaning for an unset revision:
		// the server starts the watcher after its current revision.
		opts = append(opts, clientv3.WithRev(fromRev))
	}

	// The watch outlives this call, so it gets its own cancellable context. The forwarder owns
	// the cancel from here on and calls it however it exits, which is what releases the
	// server-side watcher.
	wctx, cancel := context.WithCancel(ctx)
	wch := s.watcher.Watch(wctx, prefix, opts...)

	var created clientv3.WatchResponse
	select {
	case resp, ok := <-wch:
		if !ok {
			cancel()
			return terminated(s.endReason(ctx)), nil
		}
		if err := resp.Err(); err != nil {
			// A watch from a revision etcd has already compacted is refused at creation, which
			// is where the caller finds out. The recovery is the documented one: re-list, and
			// watch again from the revision the list reports.
			cancel()
			return terminated(translate("watch "+prefix, err)), nil
		}
		created = resp
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	case <-s.done:
		cancel()
		return nil, store.ErrClosed
	}

	out := make(chan store.Event, watchBuffer)
	go s.forward(wctx, cancel, wch, out, created)
	return out, nil
}

// forward pumps etcd's watch responses onto the store's event channel until one of them ends
// the watch.
//
// created is the response that established the watcher. etcd sends it on its own, so it carries
// no events in practice — but it is forwarded rather than discarded, because "in practice" is
// not a property of this package and a discarded event here would be an event lost forever.
func (s *Store) forward(
	ctx context.Context,
	cancel context.CancelFunc,
	wch clientv3.WatchChan,
	out chan store.Event,
	created clientv3.WatchResponse,
) {
	defer close(out)
	defer cancel()

	for _, ev := range created.Events {
		if !emit(ctx, out, convert(ev)) {
			return
		}
	}

	for {
		select {
		case <-s.done:
			terminate(out, store.ErrClosed)
			return

		case resp, ok := <-wch:
			if !ok {
				if err := s.endReason(ctx); err != nil {
					terminate(out, err)
				}
				return
			}
			if err := resp.Err(); err != nil {
				if s.isClosed() {
					// The stream was torn down by our own Close. Say so, rather than handing
					// back a transport error the caller cannot act on.
					err = store.ErrClosed
				} else {
					err = translate("watch", err)
				}
				terminate(out, err)
				return
			}
			for _, ev := range resp.Events {
				if !emit(ctx, out, convert(ev)) {
					return
				}
			}
		}
	}
}

// endReason explains a watch stream that closed without an error of its own, or returns nil if
// the caller already knows: its own context ending is not news.
//
// Anything else — a client closed out from under a [New] store, say — is reported as
// [store.ErrClosed], because from the caller's side that is what happened: this store will not
// serve it again.
func (s *Store) endReason(ctx context.Context) error {
	if ctx.Err() != nil && !s.isClosed() {
		return nil
	}
	return store.ErrClosed
}

// convert turns one etcd event into one store event.
func convert(ev *clientv3.Event) store.Event {
	out := store.Event{KV: fromPB(ev.Kv)}
	if ev.Type == clientv3.EventTypeDelete {
		out.Type = store.EventDelete
		// etcd reports a deletion with the key and the revision it happened at and nothing
		// else; the empty value it fills in is an artefact, not data, and the interface says
		// Value is nil for a delete.
		out.KV.Value = nil
		return out
	}
	out.Type = store.EventPut
	return out
}

// emit delivers one event, reporting whether the watch should carry on.
func emit(ctx context.Context, out chan<- store.Event, ev store.Event) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// terminate makes a best-effort delivery of a final error.
//
// Best-effort because the channel closing immediately afterwards is the signal that matters —
// [store.Event.Err] only says *why* — and a shutdown must not block on a consumer that has
// already walked away from its watch.
func terminate(out chan<- store.Event, err error) {
	select {
	case out <- store.Event{Err: err}:
	default:
	}
}

// terminated is a watch that ended before it began: one error, then closed.
func terminated(err error) <-chan store.Event {
	out := make(chan store.Event, 1)
	if err != nil {
		out <- store.Event{Err: err}
	}
	close(out)
	return out
}
