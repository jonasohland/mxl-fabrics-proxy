package server

import (
	"context"
	"net/http"
	"slices"
	"strconv"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/store"
)

// This file serves the event log (§12.1) and the worker log tails of §12.2.
//
// # These are the only user-API reads that do not run Compute
//
// Every other handler here goes through [Server.view], which loads the whole fleet and reconciles
// it — O(fleet) rather than O(response), and deliberately so, because what an operator is shown
// and what the fleet is being told to do must not drift (§7.3).
//
// An event read is one Get on one key. That is not an inconsistency in the rule but the rule not
// applying: a ring is a record of what already happened, so there is nothing to recompute and
// nothing that could drift. It matters practically as well — the event log is the endpoint a UI
// polls fastest, and it is polled hardest exactly when the fleet is least healthy.
//
// # Every read merges the fleet ring
//
// A leader change leaves a gap in every object's log (§12.1), and the marker explaining it is
// written once, to [store.KeyFleetEvents], rather than into a thousand rings. Merging it in on the
// way out is what puts the marker in the log where the gap is without storing it there.

// handlePathEvents is GET /v1/paths/{id}/events.
func (s *Server) handlePathEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.serveEvents(w, r, store.PathEventsKey(id))
}

// handleRequestEvents is GET /v1/namespaces/{ns}/requests/{name}/events.
//
// A request's view is its own ring merged with those of the paths it **currently** expands onto
// (§12.1). Its own ring holds only what is genuinely request-scoped — an admission refusal, an
// expansion that changed, a path lost to precedence — and the case that proves it needs one is a
// request expanding onto nothing at all, where there is no path for "why is this WAITING" to be
// asked of.
//
// This is the one read here that pays for a fleet load, and it is not avoidable: which paths a
// request expands onto is derived, so there is nowhere else to learn it from.
func (s *Server) handleRequestEvents(w http.ResponseWriter, r *http.Request) {
	id := api.RequestID{Namespace: r.PathValue("ns"), Name: r.PathValue("name")}

	view, ok := s.loadView(w, r)
	if !ok {
		return
	}
	status, found := view.result.Requests[id]
	if !found {
		writeError(w, http.StatusNotFound, api.CodeNotFound, "no request "+id.String())
		return
	}

	keys := []string{store.RequestEventsKey(id.Namespace, id.Name)}
	for _, path := range status.Paths {
		keys = append(keys, store.PathEventsKey(path.ID))
	}
	s.serveMerged(w, r, keys...)
}

// handleNodeEvents is GET /v1/nodes/{node}/events.
//
// Answered for a node with no registration, like the domain read is (§9.1): a node's log outlives
// its paths and, after a deregistration, is the only place left that says what happened.
func (s *Server) handleNodeEvents(w http.ResponseWriter, r *http.Request) {
	s.serveEvents(w, r, store.NodeEventsKey(r.PathValue("node")))
}

// handlePathLogs is GET /v1/paths/{id}/logs (§12.2).
//
// A separate endpoint from the events read rather than a field on it. The event carries a marker
// that a tail exists; fetching it is a deliberate act, because inlining a few KiB per failure into
// a ring a UI polls would make the cheap read expensive exactly when things are failing.
func (s *Server) handlePathLogs(w http.ResponseWriter, r *http.Request) {
	tail, found, err := s.events.Tail(r.Context(), r.PathValue("id"))
	if err != nil {
		storeError(w, s.logger, "read log tail", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, api.CodeNotFound,
			"no worker log has been captured for this path")
		return
	}
	writeJSON(w, http.StatusOK, tail)
}

func (s *Server) serveEvents(w http.ResponseWriter, r *http.Request, key string) {
	s.serveMerged(w, r, key)
}

func (s *Server) serveMerged(w http.ResponseWriter, r *http.Request, keys ...string) {
	since, err := cursor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}

	list, err := s.readEvents(r.Context(), since, keys...)
	if err != nil {
		storeError(w, s.logger, "read events", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// readEvents merges the requested rings with the fleet ring and filters to what is new.
//
// The cursor is applied **after** the merge rather than per ring, because a merged list has one
// ordering and the caller resumes from one number. Within a single ring that is exactly the
// sequence; across rings it is not, which is why [api.EventList.Next] is the maximum rather than a
// position, and why a caller polling a merged view can be handed an entry twice if two rings
// happen to disagree about ordering. Duplicates in a diagnostic view are cheap; missed entries
// are not, so the bias is deliberate.
func (s *Server) readEvents(ctx context.Context, since uint64, keys ...string) (api.EventList, error) {
	own, err := s.events.Merge(ctx, keys...)
	if err != nil {
		return api.EventList{}, err
	}

	// **The fleet ring is context for an object's history, never a substitute for it**, and it is
	// merged in so that a leader change's gap marker appears in the log where the gap is (§12.1).
	// Two rules bound that, and both exist because the marker makes a claim — *transitions before
	// this point were not recorded* — that is only true of some objects:
	//
	//   - **An object with nothing of its own reads as empty.** Otherwise a path whose log died
	//     with it comes back holding the control plane's entries, which renders a deleted object
	//     as one that still exists.
	//   - **Only fleet entries inside the object's own lifetime are merged.** A takeover that
	//     happened before this path existed cannot have lost any of *its* transitions, and showing
	//     it there tells an operator to distrust a log that is in fact complete.
	merged := own
	if len(own.Events) > 0 {
		fleet, err := s.events.Read(ctx, store.KeyFleetEvents)
		if err != nil {
			return api.EventList{}, err
		}
		merged = withFleetContext(own, fleet)
	}

	list := api.EventList{Events: make([]api.Event, 0, len(merged.Events)), Dropped: merged.Dropped, Next: since}
	for _, event := range merged.Events {
		if event.Seq <= since {
			continue
		}
		list.Events = append(list.Events, event)
		if event.Seq > list.Next {
			list.Next = event.Seq
		}
	}
	return list, nil
}

// withFleetContext folds the fleet ring's entries into an object's own, keeping only those that
// fall inside the object's lifetime.
//
// "Inside the lifetime" is taken from the oldest entry the object still holds rather than from a
// creation time it does not record. That is deliberately conservative in the safe direction: a ring
// that has dropped its oldest entries starts later than the object did, so a marker from the gap
// between the two is hidden. It would have explained a gap that is already reported — by
// [api.EventRing.Dropped], on the same read.
func withFleetContext(own api.EventList, fleet api.EventRing) api.EventList {
	if len(own.Events) == 0 || len(fleet.Events) == 0 {
		return own
	}

	from := own.Events[0].At
	var context []api.Event
	for _, event := range fleet.Events {
		if !event.At.Before(from) {
			context = append(context, event)
		}
	}
	if len(context) == 0 {
		return own
	}

	// A fresh slice rather than appending onto the caller's: the merge above builds its result with
	// append, so the backing array can have spare capacity and writing into it would scribble on a
	// ring another read is holding.
	merged := own
	merged.Events = make([]api.Event, 0, len(own.Events)+len(context))
	merged.Events = append(merged.Events, own.Events...)
	merged.Events = append(merged.Events, context...)

	slices.SortStableFunc(merged.Events, func(a, b api.Event) int { return a.At.Compare(b.At) })
	return merged
}

func cursor(r *http.Request) (uint64, error) {
	raw := r.URL.Query().Get(api.QuerySince)
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseUint(raw, 10, 64)
}

// handleAgentEvents is POST /agent/v1/{node}/events (§9.2, §12.1).
//
// The one agent report that is a stream rather than a snapshot. Three properties, and each one is
// the reason a field on `status` would not have worked:
//
//   - **Delivery is at-least-once.** A batch that was written and whose response was lost arrives
//     again, and the agent holds no persistent state to prevent that (§6.1). The per-agent sequence
//     number is what lets the server drop what it has already recorded.
//   - **A dropped batch announces itself.** An agent whose bounded queue overflowed reports the
//     count, and it lands as an [api.EventsDropped] marker on the node's log rather than as a
//     silent gap: a gap in this log is always visible in this log.
//   - **It is accepted by any replica.** Nothing here is leader-only, because a diagnostic write
//     that had to find the leader would be lost whenever an agent's request landed elsewhere —
//     which, behind a plain load balancer, is most of the time (§8.2).
func (s *Server) handleAgentEvents(w http.ResponseWriter, r *http.Request) {
	batch, ok := decodeBody[api.EventBatch](w, r)
	if !ok {
		return
	}

	node := r.PathValue("node")
	if batch.Node != "" && batch.Node != node {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest,
			"body names node "+batch.Node+" but the path names "+node)
		return
	}

	ctx := r.Context()
	if _, ok := s.heldBy(w, ctx, node, batch.Instance); !ok {
		return
	}

	if err := s.events.Accept(ctx, node, batch.Instance, batch.Events, batch.Dropped); err != nil {
		storeError(w, s.logger, "record events", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
