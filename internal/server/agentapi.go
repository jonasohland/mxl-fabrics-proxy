package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
	"github.com/jonasohland/mxl-replicator/internal/store"
)

// handleRegister is POST /agent/v1/register (§7.1, M4a).
//
// Two separate concepts land here and must not be merged:
//
//   - The **registration** is durable desired state: this node exists, here is what it can do.
//     It survives the agent being down, because the node still exists while its agent is being
//     upgraded.
//   - The **liveness lease** is observed state with a TTL: an agent instance currently holds
//     this node's identity. Everything that agent reports is written under it, so a node that
//     stops heartbeating stops being visible without anything having to clean up after it.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	registration, ok := decodeBody[api.NodeRegistration](w, r)
	if !ok {
		return
	}

	if err := validateRegistration(registration); err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}
	if err := s.checkProtocol(registration); err != nil {
		s.logger.Error("refusing an agent this server cannot serve",
			"node", registration.Node,
			"agent_protocol", registration.Capabilities.Versions.Protocol,
			"server_protocol", api.ProtocolVersion,
			"agent_version", registration.Capabilities.Versions.Replicator)
		writeError(w, http.StatusBadRequest, api.CodeVersionSkew, err.Error())
		return
	}

	ctx := r.Context()
	node := registration.Node

	// shm's fabric label is derived from the node name, and the server canonicalises what it
	// stores so that the derivation cannot disagree between two agents (§10.1). Both sides use
	// api.SHMFabric, so an agent that labelled it correctly sees no change here.
	canonicaliseSHM(&registration)

	lease, err := s.store.GrantLease(ctx, s.leaseTTL)
	if err != nil {
		storeError(w, s.logger, "grant lease", err)
		return
	}

	record := state.LeaseRecord{
		Node:       node,
		Instance:   registration.Instance,
		Lease:      lease,
		Versions:   registration.Capabilities.Versions,
		AcquiredAt: s.now(),
	}

	if err := s.claim(ctx, record); err != nil {
		// Whatever happened, the lease we just granted is not going to be used.
		_ = s.store.RevokeLease(ctx, lease)

		var claimed *claimedError
		if errors.As(err, &claimed) {
			// Loud on purpose. Two agents claiming one node name is a copy-pasted config or an
			// overlapping rollout, and it is nasty: both receive the same assignments, both start
			// workers, they fight over ports and write duplicates into the destination flow
			// (§7.1).
			s.logger.Error("rejecting a second claimant for a node name",
				"node", node, "claimant", registration.Instance, "holder", claimed.holder)
			writeError(w, http.StatusConflict, api.CodeNodeClaimed,
				"node "+node+" is already claimed by another agent instance",
				"holder", claimed.holder)
			return
		}
		storeError(w, s.logger, "claim node", err)
		return
	}

	if err := s.writeRegistration(ctx, registration); err != nil {
		storeError(w, s.logger, "write registration", err)
		return
	}

	if registration.Capabilities.Versions.Protocol < api.ProtocolVersion {
		// Tolerated by design: the server is always upgraded first, so an agent one or more
		// versions behind is the expected state during a roll (§13.1).
		s.logger.Warn("agent is behind this server's protocol version",
			"node", node,
			"agent_protocol", registration.Capabilities.Versions.Protocol,
			"server_protocol", api.ProtocolVersion)
	}

	s.logger.Info("node registered",
		"node", node,
		"instance", registration.Instance,
		"fabrics", len(registration.Capabilities.Fabrics),
		"domains", len(registration.Domains),
		"mxl", registration.Capabilities.Versions.MXL)

	writeJSON(w, http.StatusOK, api.RegistrationResponse{
		Lease:             strconv.FormatInt(int64(lease), 10),
		TTL:               api.Millis(s.leaseTTL),
		HeartbeatInterval: api.Millis(s.heartbeat),
		Server:            s.versions(),
	})
}

// claimedError is a node name whose lease is held by a different instance.
type claimedError struct{ holder string }

func (e *claimedError) Error() string { return "node claimed by instance " + e.holder }

// claim makes the lease key exclusive (§7.1).
//
// The primitive is a create-or-fail: the first claimant wins the IfAbsent, and a second one
// loses it while the first lease holds. A *re-registration by the same instance* is not a second
// claimant — an agent told to re-register comes back with the same instance UUID — so it takes
// its own key over and the stale lease is revoked rather than left to expire.
func (s *Server) claim(ctx context.Context, record state.LeaseRecord) error {
	key := store.LeaseKey(record.Node)

	_, _, err := state.PutJSON(ctx, s.store, key, record, state.Prior{},
		state.WriteOptions{Lease: record.Lease, CAS: true})
	if !errors.Is(err, store.ErrCompareFailed) {
		return err
	}

	existing, err := state.Get[state.LeaseRecord](ctx, s.store, key)
	if err != nil {
		return err
	}
	if !existing.Found {
		// It went away between the failed create and the read — an expiry landing in the
		// window. One more attempt, and if that loses too the caller retries by re-registering.
		_, _, err = state.PutJSON(ctx, s.store, key, record, state.Prior{},
			state.WriteOptions{Lease: record.Lease, CAS: true})
		return err
	}
	if existing.Value.Instance != record.Instance {
		return &claimedError{holder: existing.Value.Instance}
	}

	if _, _, err := state.PutJSON(ctx, s.store, key, record, existing.Prior(),
		state.WriteOptions{Lease: record.Lease, CAS: true}); err != nil {
		return err
	}
	if existing.Value.Lease != 0 && existing.Value.Lease != record.Lease {
		if err := s.store.RevokeLease(ctx, existing.Value.Lease); err != nil && !errors.Is(err, store.ErrLeaseNotFound) {
			return err
		}
	}
	return nil
}

// writeRegistration stores the durable half, preserving when the node was first seen.
func (s *Server) writeRegistration(ctx context.Context, registration api.NodeRegistration) error {
	key := store.NodeKey(registration.Node)

	existing, err := state.Get[state.NodeRecord](ctx, s.store, key)
	if err != nil {
		return err
	}

	record := state.NodeRecord{
		Node:         registration.Node,
		Capabilities: registration.Capabilities,
		Domains:      registration.Domains,
		RegisteredAt: s.now(),
		UpdatedAt:    s.now(),
	}
	if existing.Found {
		record.RegisteredAt = existing.Value.RegisteredAt
		// UpdatedAt would otherwise change on every re-registration and defeat the
		// compare-before-write: an agent restarting would rewrite a registration that is
		// identical, waking every watcher for nothing (§8.3).
		record.UpdatedAt = existing.Value.UpdatedAt
		candidate := record
		candidate.UpdatedAt = s.now()
		if !sameCapabilities(existing.Value, record) {
			record = candidate
		}
	}

	_, _, err = state.PutJSON(ctx, s.store, key, record, existing.Prior(), state.WriteOptions{})
	return err
}

// handleHeartbeat is POST /agent/v1/{node}/heartbeat: renew the lease, and nothing else.
//
// Deliberately not a write. A heartbeat that rewrote its lease record would advance the store
// revision several times a minute per node forever, waking every watcher — including the
// reconciler's — and making liveness the highest-volume writer in the system (§8.3).
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("node")

	beat, ok := decodeBody[api.Heartbeat](w, r)
	if !ok {
		return
	}
	if beat.Node != "" && beat.Node != node {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, "body names node "+beat.Node+" but the path names "+node)
		return
	}

	held, ok := s.heldBy(w, r.Context(), node, beat.Instance)
	if !ok {
		return
	}

	if err := s.store.KeepAlive(r.Context(), held.Lease); err != nil {
		if errors.Is(err, store.ErrLeaseNotFound) {
			// The lease expired between the read and the renewal. Re-register; keep the workers
			// running while you do (§4.2).
			writeJSON(w, http.StatusOK, api.HeartbeatResponse{Reregister: true})
			return
		}
		storeError(w, s.logger, "keepalive", err)
		return
	}

	writeJSON(w, http.StatusOK, api.HeartbeatResponse{})
}

// heldBy checks that this instance holds the node's lease, and writes the response if not.
//
// Every agent report goes through here. Without it, a stale agent instance — one that lost the
// race for a name, or came back after a partition — could keep overwriting the winner's observed
// state with its own view of the world.
func (s *Server) heldBy(w http.ResponseWriter, ctx context.Context, node, instance string) (state.LeaseRecord, bool) {
	record, err := state.Get[state.LeaseRecord](ctx, s.store, store.LeaseKey(node))
	if err != nil {
		storeError(w, s.logger, "read lease", err)
		return state.LeaseRecord{}, false
	}

	switch {
	case !record.Found:
		writeError(w, http.StatusConflict, api.CodeReregister, "node "+node+" holds no lease; register again")
		return state.LeaseRecord{}, false
	case instance == "":
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, "instance is required")
		return state.LeaseRecord{}, false
	case record.Value.Instance != instance:
		s.logger.Warn("rejecting a report from an instance that does not hold the node's lease",
			"node", node, "reporter", instance, "holder", record.Value.Instance)
		writeError(w, http.StatusConflict, api.CodeNodeClaimed,
			"node "+node+" is held by another agent instance", "holder", record.Value.Instance)
		return state.LeaseRecord{}, false
	}

	return record.Value, true
}

// handleInventory is POST /agent/v1/{node}/inventory (M4b).
//
// A **full snapshot**, written rather than merged. Deltas need sequencing, gap detection and
// resync paths; snapshots need none of that, and at realistic fleet sizes they are small (§9.2).
func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) {
	snapshot, ok := decodeBody[api.InventorySnapshot](w, r)
	if !ok {
		return
	}
	s.ingest(w, r, snapshot.Node, snapshot.Instance, store.InventoryKey, snapshot)
}

// handleStatus is POST /agent/v1/{node}/status (M4b).
//
// Also a full snapshot, and full in a second sense: it reports every session the agent is
// running, not merely the ones it was assigned. That is what lets a restarted server — which has
// desired state and no observed state — recognise a worker it never assigned in this process
// lifetime by its session ID and adopt it, rather than issuing a fresh assignment and glitching
// media that was fine (§7.3).
//
// It is also the path an epoch arrives on: a target reporting a new incarnation lands here, the
// write wakes the reconciler, and the peer's initiator assignment changes (§5.2).
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	snapshot, ok := decodeBody[api.StatusSnapshot](w, r)
	if !ok {
		return
	}
	s.ingest(w, r, snapshot.Node, snapshot.Instance, store.StatusKey, snapshot)
}

// ingest writes one observed-state snapshot under the reporting agent's lease.
func (s *Server) ingest(w http.ResponseWriter, r *http.Request, node, instance string, key func(string) string, snapshot any) {
	path := r.PathValue("node")
	if node != "" && node != path {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, "body names node "+node+" but the path names "+path)
		return
	}

	ctx := r.Context()
	held, ok := s.heldBy(w, ctx, path, instance)
	if !ok {
		return
	}

	existing, err := state.Get[map[string]any](ctx, s.store, key(path))
	if err != nil {
		storeError(w, s.logger, "read snapshot", err)
		return
	}

	// The lease goes on **every** write, not just the first: a put with no lease detaches the
	// one the key was holding, which quietly turns observed state into state that outlives its
	// agent (§9.2).
	if _, _, err := state.PutJSON(ctx, s.store, key(path), snapshot, existing.Prior(), state.WriteOptions{Lease: held.Lease}); err != nil {
		storeError(w, s.logger, "write snapshot", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleAssignments is GET /agent/v1/{node}/assignments?rev=&wait= (M4c, §9.2).
//
// A long poll with a revision cursor, not a server push. That is what makes it
// proxy-transparent: an SSE or gRPC stream lands on one replica, and state written by another
// has to be watched and fanned out to reach it, which is exactly the sticky-session requirement
// HA is meant to avoid (§8.2). It is also trivially resumable, and it degrades to plain polling
// behind a proxy that buffers.
//
// # Empty is a value, not a fallback
//
// The rule this endpoint exists to uphold: **it must never answer an empty assignment set that
// actually means "I don't know yet"**. A failed poll and an empty set look identical to an
// agent, and an agent that reconciles to zero tears down every worker it is running. So while
// the reconciler has not settled — a fresh server, a leader change, or a store restored from an
// empty backup — this returns [api.CodeNotReady] and the agent skips its reconcile entirely
// (§4.2, plan §4.2).
func (s *Server) handleAssignments(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("node")
	ctx := r.Context()

	cursor, err := queryInt64(r, api.QueryRevision)
	if err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}
	wait := queryDuration(r, api.QueryWait, s.maxLongPoll, s.maxLongPoll)

	settled, err := s.settled(ctx)
	if err != nil {
		storeError(w, s.logger, "read reconciler state", err)
		return
	}
	if !settled {
		writeError(w, http.StatusServiceUnavailable, api.CodeNotReady,
			"the reconciler has not settled; this is not an empty assignment set")
		return
	}

	registered, err := state.Get[state.NodeRecord](ctx, s.store, store.NodeKey(node))
	if err != nil {
		storeError(w, s.logger, "read registration", err)
		return
	}
	if !registered.Found {
		writeError(w, http.StatusConflict, api.CodeReregister, "node "+node+" is not registered")
		return
	}

	set, revision, err := s.pollAssignments(ctx, node, cursor, wait)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		storeError(w, s.logger, "read assignments", err)
		return
	}
	if revision < 0 {
		// The store's view is behind the cursor this agent already holds. Serving it would walk
		// the agent backwards through two assignment versions and restart workers on every
		// swing, so say "not ready" instead and let it poll again (plan §4.5).
		writeError(w, http.StatusServiceUnavailable, api.CodeNotReady,
			"this replica's view is behind the requested revision")
		return
	}

	set.Node = node
	set.Revision = revision
	if set.Assignments == nil {
		// `[]`, not `null`. Both decode to a zero-length slice and the agent distinguishes "got a
		// set" from "got no answer" at the HTTP layer rather than by nil-ness — but this is the
		// one field in the system where confusing empty with absent stops every worker in the
		// fleet, so it is spelled unambiguously on the wire too.
		set.Assignments = []api.Assignment{}
	}
	writeJSON(w, http.StatusOK, set)
}

// pollAssignments reads a node's set, waiting for it to move past the cursor.
//
// It returns a negative revision when the store's own revision is behind the caller's cursor,
// which is the HA hazard above rather than an error.
func (s *Server) pollAssignments(ctx context.Context, node string, cursor int64, wait time.Duration) (api.AssignmentSet, int64, error) {
	key := store.AssignmentsKey(node)

	read := func() (api.AssignmentSet, int64, bool, error) {
		kvs, revision, err := s.store.List(ctx, key)
		if err != nil {
			return api.AssignmentSet{}, 0, false, err
		}
		if revision < cursor {
			return api.AssignmentSet{}, -1, true, nil
		}

		var set api.AssignmentSet
		var modified int64
		if len(kvs) > 0 {
			if err := json.Unmarshal(kvs[0].Value, &set); err != nil {
				return api.AssignmentSet{}, 0, false, err
			}
			modified = kvs[0].ModRevision
		}
		// A first poll (no cursor) is answered immediately; after that, only a set that has
		// actually changed ends the wait. An absent key is a legitimate "nothing to do" — the
		// settled check above is what makes that safe to say.
		return set, revision, cursor == 0 || modified > cursor, nil
	}

	set, revision, ready, err := read()
	if err != nil || ready {
		return set, revision, err
	}

	// Nothing new. Hold the request open until it changes or the wait expires; a wait that
	// expires returns the current set, so an agent behind a buffering proxy degrades to plain
	// polling rather than hanging.
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	events, err := s.store.Watch(waitCtx, key, revision+1)
	if err != nil {
		return set, revision, err
	}

	for {
		select {
		case <-waitCtx.Done():
			set, revision, _, err := read()
			return set, revision, err
		case event, ok := <-events:
			if !ok || event.Err != nil {
				set, revision, _, err := read()
				return set, revision, err
			}
			set, revision, ready, err := read()
			if err != nil || ready {
				return set, revision, err
			}
		}
	}
}

// settled reports whether the fleet's reconciler has completed a pass.
func (s *Server) settled(ctx context.Context) (bool, error) {
	record, err := state.Get[state.ReconcilerRecord](ctx, s.store, store.KeyReconciler)
	if err != nil {
		return false, err
	}
	return record.Found && record.Value.Settled, nil
}
