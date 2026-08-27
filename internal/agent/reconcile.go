package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sync"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/epoch"
	"github.com/jonasohland/mxl-replicator/internal/worker"
)

// reconcile makes the workers on this node match the assignment set (§4.1, M5g).
//
// Called on exactly one path — a poll that succeeded — and from exactly one goroutine, which is
// what lets it stop and start workers outside the lock without a second pass racing it.
//
// # The already-correct test
//
// The comparison is [worker.Spec.Key], and **never a diff of the assignment object** (§7.3,
// invariant 2). Any incidental difference — a re-derived port, a reordered JSON field, a
// re-serialised flow definition that is semantically identical — would read as a change and
// restart a healthy worker. That bug passes every single-replica test and then flaps the moment
// there are two server replicas or a store round trip in the path, which is to say once it is in
// production. Key hashes the session, the role, the epoch and the configuration that materially
// affects the process, and deliberately excludes the rest.
func (a *Agent) reconcile(ctx context.Context, set *api.AssignmentSet) {
	desired := make(map[unitKey]worker.Spec, len(set.Assignments))
	rejected := map[unitKey]string{}

	for _, assignment := range set.Assignments {
		key := unitKey{Session: assignment.SessionID, Role: assignment.Role}
		spec, err := a.specFor(key, assignment)
		if err != nil {
			// Recorded and reported rather than dropped: an assignment this node cannot honour
			// must reach the operator as a reason on the session, not as a path that sits in
			// ESTABLISHING with nothing to explain it.
			a.log.Error("cannot honour an assignment",
				"session", key.Session, "role", string(key.Role), "error", err)
			rejected[key] = err.Error()
			continue
		}
		desired[key] = spec
	}

	a.mu.Lock()
	running := maps.Clone(a.units)
	a.rejected = rejected
	a.mu.Unlock()

	a.stopUndesired(running, desired)
	a.startMissing(ctx, desired)

	a.Notify()
}

// stopUndesired stops every worker that is no longer wanted, or wanted with different material
// configuration. Removed workers are stopped concurrently, because each costs a signal and a
// grace period and there is no reason to pay them one after another.
func (a *Agent) stopUndesired(running map[unitKey]*unit, desired map[unitKey]worker.Spec) {
	var stopping sync.WaitGroup

	for key, u := range running {
		spec, wanted := desired[key]
		if wanted && u.desired().Key() == spec.Key() {
			continue
		}

		a.mu.Lock()
		delete(a.units, key)
		a.mu.Unlock()

		if !wanted {
			// The session is gone from this node entirely, so its service goes back to the pool.
			// A worker that is merely being *replaced* keeps its port: it is the same session and
			// the same role, and a port that moved for no reason is a difference the peer would
			// have to be told about (§7.4).
			a.cfg.Ports.Release(key.String())
		}

		reason := "no longer assigned"
		if wanted {
			reason = "assigned with different configuration"
		}
		u.log.Info("stopping worker", "reason", reason)

		stopping.Go(u.stop)
	}

	stopping.Wait()
}

// startMissing starts a worker for every desired unit that is not already running.
func (a *Agent) startMissing(ctx context.Context, desired map[unitKey]worker.Spec) {
	for key, spec := range desired {
		a.mu.Lock()
		_, running := a.units[key]
		if !running {
			u := a.newUnit(key, spec)
			a.units[key] = u
			a.mu.Unlock()

			u.log.Info("starting worker", "epoch", spec.Epoch, "service", spec.Service, "provider", spec.Interface.Provider)
			u.start(ctx)
			continue
		}
		a.mu.Unlock()
	}
}

// stopAll tears down every worker this agent is running.
//
// Only on the way out. The agent execs workers as children, so leaving them behind would leave
// ports, memory registrations and flows held by processes nothing supervises — and a restarted
// agent kills and re-establishes everything anyway (§6.1).
func (a *Agent) stopAll() {
	a.mu.Lock()
	running := maps.Clone(a.units)
	clear(a.units)
	a.mu.Unlock()

	var stopping sync.WaitGroup
	for key, u := range running {
		a.cfg.Ports.Release(key.String())
		stopping.Go(u.stop)
	}
	stopping.Wait()
}

// specFor turns an assignment into a worker spec, resolving the three things only this agent
// knows: where the named domain lives, which local address the negotiated fabric means, and
// which service to bind.
func (a *Agent) specFor(key unitKey, assignment api.Assignment) (worker.Spec, error) {
	// A domain **name**, resolved by a strict map lookup over the domains this agent knows. It is
	// never interpreted as a path: that is the single most important invariant in the design, and
	// it is what stops the API being a remote arbitrary-filesystem-write primitive on every node
	// in the fleet (§7.2, §13, invariant 6).
	domainPath, ok := a.cfg.Inventory.Path(assignment.Domain)
	if !ok {
		return worker.Spec{}, fmt.Errorf("domain %q is not mapped on this node", assignment.Domain)
	}

	if assignment.Role == api.RoleTarget && !a.cfg.Inventory.Configured(assignment.Domain) {
		// The server validates this and is the authority. Checking it again costs one map lookup,
		// and it is the invariant above: an agent that trusted the control plane on it would be
		// one compromised or buggy server away from creating flows anywhere a search path reaches.
		return worker.Spec{}, fmt.Errorf("domain %q was discovered rather than configured, and a discovered domain is never a replication destination", assignment.Domain)
	}

	// The provider alone does not identify a local bind address: a node can hold two verbs
	// attachments on different InfiniBand fabrics, and binding the wrong one produces a target
	// that comes up perfectly and an initiator that never connects (§10.1).
	attachment := a.capabilities().FindFabric(assignment.Interface.Provider, assignment.Fabric)
	if attachment == nil {
		return worker.Spec{}, fmt.Errorf("this node advertises no %s attachment on fabric %q",
			assignment.Interface.Provider, assignment.Fabric)
	}

	if assignment.Role == api.RoleInitiator {
		if err := a.verifyEpoch(assignment); err != nil {
			return worker.Spec{}, err
		}
	}

	service, err := a.cfg.Ports.Allocate(key.String(), assignment.Interface.Provider)
	if err != nil {
		return worker.Spec{}, err
	}

	spec := worker.Spec{
		SessionID:                   assignment.SessionID,
		Role:                        assignment.Role,
		Epoch:                       assignment.Epoch,
		DomainPath:                  domainPath,
		FlowID:                      assignment.FlowID,
		FlowDef:                     assignment.FlowDef,
		BindAddress:                 attachment.Address,
		Service:                     service,
		Interface:                   assignment.Interface,
		TargetInfo:                  assignment.TargetInfo,
		NoNetworkLatencyMeasurement: assignment.NoNetworkLatencyMeasurement,
		SchedPrio:                   assignment.SchedPrio,
		IdleTimeout:                 assignment.IdleTimeout.Duration(),
		ConnectTimeout:              assignment.ConnectTimeout.Duration(),
		Labels:                      assignment.Labels,
	}
	if assignment.Role == api.RoleTarget {
		// The worker's target_info key is an output path for this role, chosen by the launcher
		// (WRS §3); the assignment carries none and the spec must not either.
		spec.TargetInfo = ""
		spec.FlowDef = append(json.RawMessage(nil), assignment.FlowDef...)
	}

	if err := spec.Validate(); err != nil {
		a.cfg.Ports.Release(key.String())
		return worker.Spec{}, err
	}
	return spec, nil
}

// verifyEpoch checks that the assigned epoch and the blob agree before anything is started
// (§5.3 step 6).
//
// This is the self-validating half of the content-hash epoch. It catches a mismatched or
// truncated target_info before it reaches a worker that would otherwise present as a healthy
// process transferring nothing — no error, no data, everything upstream reporting fine, which is
// the hardest failure in this system to diagnose (§5.2).
//
// Note what it does *not* check: whether the pair is stale. A pair that agrees with itself but
// describes a target incarnation that has since gone verifies happily, and noticing that is the
// reconcile loop's equality test — the epoch this agent is running against the epoch it was
// assigned — which is [worker.Spec.Key]'s job, not this one's.
func (a *Agent) verifyEpoch(assignment api.Assignment) error {
	info, unknown, err := epoch.Decode(assignment.TargetInfo)
	if err != nil {
		return fmt.Errorf("target info from the peer: %w", err)
	}
	if len(unknown) > 0 {
		a.log.Warn("target info from the peer carries fields this build does not know about",
			"session", assignment.SessionID, "fields", unknown)
	}
	if err := epoch.Verify(assignment.Epoch, info); err != nil {
		return fmt.Errorf("refusing to start an initiator against a target info blob that does not match its epoch: %w", err)
	}
	return nil
}
