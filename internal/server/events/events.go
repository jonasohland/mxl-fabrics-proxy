// Package events records and serves the event log of §12.1.
//
// Everything in the status vocabulary (§11) is level-triggered and last-write-wins: it says what
// is true *now*. An operator debugging a failing path needs what *happened*, and none of it is
// otherwise retained anywhere the control plane can serve — a path that flapped for ten minutes
// and is ACTIVE again reports nothing about the ten minutes.
//
// # One key per object, holding a ring
//
// An object's log is a bounded [api.EventRing] inside a **single value**, rewritten on each
// flush, not one key per entry. That is §9.2's level-triggered discipline arriving a third time:
// an append-only stream needs sequencing, gap detection, compaction and a garbage collector, and
// a ring in one value needs none of them. It reads in one Get, it is bounded by construction, and
// it is deleted by deleting one key.
//
// # What this package must not become
//
// The event log is the first edge-triggered, append-shaped thing in a design that spends §6, §8.3
// and §11.1 removing writers, so two rules are structural rather than stylistic:
//
//   - **One write per reconcile pass, never one per event.** [Recorder.Record] takes a batch, and
//     the reconciler collects a whole pass before flushing. Store revisions are not free: the
//     sqlite backend bounds watch history by revision *count* (§8.1), so a chatty writer can
//     compact out an agent's long-poll cursor.
//   - **Nothing here is ever read by a decision.** Events live outside [store.PrefixSnapshot], so
//     they are invisible to the reconciler's snapshot by construction (§4). A reconciler that read
//     its own event log would have history affecting a decision, which is the one thing §7.3
//     forbids outright.
package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
	"github.com/jonasohland/mxl-replicator/internal/store"
)

// casAttempts bounds the read-modify-write retry on one ring.
//
// A ring has two writers that do not coordinate: the leader, recording what a reconcile pass
// concluded, and whichever replica an agent's event batch landed on (§9.2). Neither is on the
// establishment path and neither is worth a lock, so they resolve by CAS — and by giving up
// quietly, which is the right disposition for a diagnostic write. Blocking a status report to
// record why a status report is interesting would be the wrong trade every time.
const casAttempts = 4

// Options configures a [Recorder].
type Options struct {
	Store  store.Store
	Logger *slog.Logger

	// RingSize is how many entries an object's ring holds. Zero takes
	// [api.DefaultEventRingSize].
	//
	// **A count, never an age.** The overnight failure someone arrives to at 09:00 is the case
	// this log most exists for, and an age bound expires it exactly then (§12.1).
	RingSize int

	// TailBytes caps what [Recorder.PutTail] will store, independent of what an agent chose to
	// capture. Zero takes [api.DefaultLogTailBytes].
	//
	// The two bounds look like one knob spelled twice and are not (§12.2). The agent's buffer is a
	// property of the host, like its port range. This one is a property of the store, and it has
	// to exist on its own: an endpoint that accepts unbounded bytes from a node is a store-filling
	// primitive handed to every member of the fleet.
	TailBytes int

	// Now is the clock, injectable for tests.
	Now func() time.Time

	// Observe is called for each entry actually recorded, and Drop for each one that was not.
	// Both are optional; they exist so the server can count without this package importing
	// Prometheus.
	Observe func(api.EventKind)
	Drop    func(reason string)
}

// Recorder reads and writes event rings.
//
// Safe for concurrent use: it holds no state beyond its configuration, and every write is a CAS
// on the key it touches.
type Recorder struct {
	store     store.Store
	log       *slog.Logger
	ringSize  int
	tailBytes int
	now       func() time.Time
	observe   func(api.EventKind)
	drop      func(string)
}

// New builds a recorder.
func New(opts Options) *Recorder {
	r := &Recorder{
		store:     opts.Store,
		log:       opts.Logger,
		ringSize:  opts.RingSize,
		tailBytes: opts.TailBytes,
		now:       opts.Now,
		observe:   opts.Observe,
		drop:      opts.Drop,
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	if r.ringSize <= 0 {
		r.ringSize = api.DefaultEventRingSize
	}
	if r.tailBytes <= 0 {
		r.tailBytes = api.DefaultLogTailBytes
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r
}

// Record appends entries to one object's ring, coalescing repeats and trimming to the bound.
//
// It is best-effort by design. A failure is logged and returned but no caller is expected to
// abort on it: this is a diagnostic aid, not an audit log (§12.1), and a reconcile that refused
// to finish because it could not write down what it had done would trade media for bookkeeping.
func (r *Recorder) Record(ctx context.Context, key string, batch ...api.Event) error {
	if len(batch) == 0 {
		return nil
	}

	var lastErr error
	for range casAttempts {
		entry, err := state.Get[api.EventRing](ctx, r.store, key)
		if err != nil {
			// A ring that will not decode is not worth failing a reconcile over, and it is not
			// worth preserving either: start a new one rather than losing every future entry to
			// one bad value.
			r.log.Warn("event ring unreadable, starting a new one", "key", key, "error", err)
			entry = state.Entry[api.EventRing]{}
		}

		ring := entry.Value
		for _, event := range batch {
			if event.At.IsZero() {
				event.At = r.now()
			}
			ring.Append(event, r.ringSize)
		}
		dropped := ring.Dropped - entry.Value.Dropped

		_, wrote, err := state.PutJSON(ctx, r.store, key, ring, entry.Prior(), state.WriteOptions{CAS: true})
		switch {
		case errors.Is(err, store.ErrCompareFailed):
			// The other writer got there first. Re-read and re-apply: entries are appended rather
			// than replaced, so losing the race costs a round trip and nothing else.
			lastErr = err
			continue
		case err != nil:
			r.reportDropped(len(batch), "store")
			return fmt.Errorf("record events on %s: %w", key, err)
		}

		if wrote {
			for _, event := range batch {
				if r.observe != nil {
					r.observe(event.Kind)
				}
			}
		}
		if dropped > 0 {
			r.reportDropped(int(dropped), "ring")
		}
		return nil
	}

	r.reportDropped(len(batch), "contention")
	return fmt.Errorf("record events on %s: %w", key, lastErr)
}

func (r *Recorder) reportDropped(n int, reason string) {
	if r.drop == nil {
		return
	}
	for range n {
		r.drop(reason)
	}
}

// RecordPath appends to a path's ring — the workhorse, since the path is the unit of retention
// (§12.1).
func (r *Recorder) RecordPath(ctx context.Context, pathID string, batch ...api.Event) error {
	return r.Record(ctx, store.PathEventsKey(pathID), batch...)
}

// RecordRequest appends to a request's ring, which holds only what is genuinely request-scoped.
func (r *Recorder) RecordRequest(ctx context.Context, id api.RequestID, batch ...api.Event) error {
	return r.Record(ctx, store.RequestEventsKey(id.Namespace, id.Name), batch...)
}

// RecordNode appends to a node's ring — the log that still exists after that node's paths are
// gone, and the one that answers "why did everything on edge-01 re-establish at 12:04".
func (r *Recorder) RecordNode(ctx context.Context, node string, batch ...api.Event) error {
	return r.Record(ctx, store.NodeEventsKey(node), batch...)
}

// Accept records one agent's batch, dropping whatever has already been recorded from that
// instance (§9.2, §12.1).
//
// # De-duplication, and where the cursor lives
//
// Delivery is at-least-once: a batch that was written and whose response was lost arrives again,
// and the agent holds no persistent state to prevent that (§6.1). The cursor lives on the node's
// own ring, keyed by *instance*, so that accepting a batch is the same write as recording it — a
// separate cursor key would double the writes this log is allowed to make — and so that a
// restarted agent, whose counter starts at zero again, is not silently ignored.
//
// # The write order is the one that duplicates rather than loses
//
// Path rings are written first and the node ring, which carries the cursor, last. A failure in
// between leaves the path entries recorded and the cursor un-advanced, so the next delivery
// records them again. The opposite order would advance the cursor over entries that were never
// written, which is the one failure a diagnostic log cannot report on itself.
func (r *Recorder) Accept(ctx context.Context, node, instance string, batch []api.AgentEvent, agentDropped uint64) error {
	nodeKey := store.NodeEventsKey(node)

	ring, err := r.Read(ctx, nodeKey)
	if err != nil {
		return err
	}
	cursor := ring.Accepted[instance]

	byPath := map[string][]api.Event{}
	order := make([]string, 0, len(batch))
	var nodeEvents []api.Event
	var highest uint64

	for _, reported := range batch {
		// A zero sequence is "unsequenced" and is always accepted: it is what a caller that does
		// not number its entries produces, and refusing those would silently drop them.
		if reported.Seq != 0 {
			if reported.Seq <= cursor {
				continue
			}
			highest = max(highest, reported.Seq)
		}

		event := api.Event{
			Kind:       reported.Kind,
			Severity:   reported.Severity,
			At:         reported.At,
			Message:    reported.Message,
			ReasonCode: reported.ReasonCode,
			State:      reported.State,
			Node:       node,
			Session:    reported.Session,
			Role:       reported.Role,
			HasLog:     reported.Log != "" && reported.Path != "",
		}

		if reported.Path == "" {
			nodeEvents = append(nodeEvents, event)
			continue
		}

		key := store.PathEventsKey(reported.Path)
		if _, seen := byPath[key]; !seen {
			order = append(order, key)
		}
		byPath[key] = append(byPath[key], event)

		if reported.Log != "" {
			if err := r.PutTail(ctx, api.LogTail{
				Path: reported.Path, Node: node, Session: reported.Session,
				Role: reported.Role, At: reported.At, Text: reported.Log,
			}); err != nil {
				// The entry is worth recording even when its attachment is not storable, and the
				// marker on it is what tells a reader to try the endpoint.
				r.log.Warn("storing a worker log tail failed",
					"node", node, "path", reported.Path, "error", err)
			}
		}
	}

	// An agent that lost entries to a full queue says so, and it lands as a marker rather than as
	// a silent gap (§12.1). Recorded even when the batch itself was entirely duplicate: the count
	// is about what never arrived, not about what did.
	if agentDropped > 0 {
		nodeEvents = append(nodeEvents, api.Event{
			Kind: api.EventsDropped, Severity: api.SeverityWarn, Node: node,
			Message: fmt.Sprintf("agent dropped %d events before they could be reported", agentDropped),
		})
	}

	var errs []error
	for _, key := range order {
		if err := r.Record(ctx, key, byPath[key]...); err != nil {
			errs = append(errs, err)
		}
	}

	if len(nodeEvents) > 0 || highest > cursor {
		if err := r.recordNodeBatch(ctx, nodeKey, instance, highest, nodeEvents); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recordNodeBatch appends to a node's ring and advances that instance's cursor in the same write.
func (r *Recorder) recordNodeBatch(ctx context.Context, key, instance string, highest uint64, batch []api.Event) error {
	var lastErr error
	for range casAttempts {
		entry, err := state.Get[api.EventRing](ctx, r.store, key)
		if err != nil {
			entry = state.Entry[api.EventRing]{}
		}

		ring := entry.Value
		for _, event := range batch {
			if event.At.IsZero() {
				event.At = r.now()
			}
			ring.Append(event, r.ringSize)
		}
		if highest > ring.Accepted[instance] {
			if ring.Accepted == nil {
				ring.Accepted = map[string]uint64{}
			}
			ring.Accepted[instance] = highest
		}

		_, wrote, err := state.PutJSON(ctx, r.store, key, ring, entry.Prior(), state.WriteOptions{CAS: true})
		switch {
		case errors.Is(err, store.ErrCompareFailed):
			lastErr = err
			continue
		case err != nil:
			r.reportDropped(len(batch), "store")
			return fmt.Errorf("record events on %s: %w", key, err)
		}

		if wrote {
			for _, event := range batch {
				if r.observe != nil {
					r.observe(event.Kind)
				}
			}
		}
		return nil
	}

	r.reportDropped(len(batch), "contention")
	return fmt.Errorf("record events on %s: %w", key, lastErr)
}

// Read returns one ring. A key that does not exist is an empty ring, not an error: an object
// nothing has happened to yet is the ordinary case.
func (r *Recorder) Read(ctx context.Context, key string) (api.EventRing, error) {
	entry, err := state.Get[api.EventRing](ctx, r.store, key)
	if err != nil {
		return api.EventRing{}, fmt.Errorf("read events from %s: %w", key, err)
	}
	return entry.Value, nil
}

// List renders one ring for the API, resumed from cursor.
func (r *Recorder) List(ctx context.Context, key string, cursor uint64) (api.EventList, error) {
	ring, err := r.Read(ctx, key)
	if err != nil {
		return api.EventList{}, err
	}
	return listFrom(ring, cursor), nil
}

// Merge renders several rings as one list, which is how a request is read: its own entries plus
// those of the paths it currently expands onto (§12.1).
//
// Ordered by timestamp, and that is the one place in this design where a wall clock decides
// anything. Sequence numbers are per ring and mean nothing across rings, so there is no better
// key — and this is exactly why §12.1 says a merged view interleaves three clocks and must not be
// read as causality.
func (r *Recorder) Merge(ctx context.Context, keys ...string) (api.EventList, error) {
	var out api.EventList
	for _, key := range keys {
		ring, err := r.Read(ctx, key)
		if err != nil {
			return api.EventList{}, err
		}
		out.Events = append(out.Events, ring.Events...)
		out.Dropped += ring.Dropped
	}

	sortByTime(out.Events)
	return out, nil
}

// Forget deletes an object's ring, and for a path its stored log tail as well.
//
// **A path's log dies with the path** (§12.1). No tombstone and no grace period: retention that
// outlives its object needs a second lifecycle, a TTL and a sweeper, for a question the node log
// still answers.
func (r *Recorder) Forget(ctx context.Context, keys ...string) error {
	var errs []error
	for _, key := range keys {
		if _, err := r.store.Delete(ctx, key); err != nil {
			errs = append(errs, fmt.Errorf("forget %s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

// ForgetPath drops everything retained about one path.
func (r *Recorder) ForgetPath(ctx context.Context, pathID string) error {
	return r.Forget(ctx, store.PathEventsKey(pathID), store.LogKey(pathID))
}

// PutTail stores a worker log tail for one path (§12.2), truncating it to the server's own cap.
//
// Truncation takes the **head**: a worker's fatal line is its last, in both failure shapes — one
// that never comes up and one that dies after hours of healthy transfer.
func (r *Recorder) PutTail(ctx context.Context, tail api.LogTail) error {
	text, truncated := api.TailBytes(tail.Text, r.tailBytes)
	tail.Text = text
	tail.Truncated = tail.Truncated || truncated
	if tail.At.IsZero() {
		tail.At = r.now()
	}

	key := store.LogKey(tail.Path)
	entry, err := state.Get[api.LogTail](ctx, r.store, key)
	if err != nil {
		entry = state.Entry[api.LogTail]{}
	}
	if _, _, err := state.PutJSON(ctx, r.store, key, tail, entry.Prior(), state.WriteOptions{}); err != nil {
		return fmt.Errorf("store log tail for %s: %w", tail.Path, err)
	}
	return nil
}

// Tail returns the stored log tail for a path, and whether there is one.
func (r *Recorder) Tail(ctx context.Context, pathID string) (api.LogTail, bool, error) {
	entry, err := state.Get[api.LogTail](ctx, r.store, store.LogKey(pathID))
	if err != nil {
		return api.LogTail{}, false, fmt.Errorf("read log tail for %s: %w", pathID, err)
	}
	return entry.Value, entry.Found, nil
}

func listFrom(ring api.EventRing, cursor uint64) api.EventList {
	list := api.EventList{
		Events:  ring.Since(cursor),
		Dropped: ring.Dropped,
		Next:    cursor,
	}
	for _, event := range list.Events {
		if event.Seq > list.Next {
			list.Next = event.Seq
		}
	}
	return list
}

func sortByTime(events []api.Event) {
	// Insertion by timestamp, tie-broken on sequence, so a merge of rings whose clocks agree to
	// the millisecond is still deterministic across replicas.
	slices.SortFunc(events, func(a, b api.Event) int {
		switch {
		case a.At.Before(b.At):
			return -1
		case a.At.After(b.At):
			return 1
		case a.Seq < b.Seq:
			return -1
		case a.Seq > b.Seq:
			return 1
		}
		return 0
	})
}
