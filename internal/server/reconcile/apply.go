package reconcile

import (
	"context"
	"fmt"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
	"github.com/jonasohland/mxl-replicator/internal/store"
)

// Changes counts what one [Apply] actually wrote. Zero across the board is the steady state and
// the normal outcome: a reconcile that changes nothing must touch nothing, or every pass wakes
// every agent's long poll (§7.3).
type Changes struct {
	SessionsWritten    int
	SessionsDeleted    int
	AssignmentsWritten int
	AssignmentsDeleted int
}

// Any reports whether anything was written.
func (c Changes) Any() bool {
	return c.SessionsWritten+c.SessionsDeleted+c.AssignmentsWritten+c.AssignmentsDeleted > 0
}

// Apply makes the derived key space match a computed result.
//
// It is the **only** writer of /derived/ (§7.3), and it runs only on the elected leader. Every
// write is a compare-and-swap against the revision the snapshot was read at, which is the guard
// against a leader that has been demoted and not noticed: it is computing from a stale read, so
// its writes lose to the new leader's rather than fighting them (§4.6). A lost CAS surfaces as
// [store.ErrCompareFailed]; the caller reloads and tries again, or discovers it is no longer the
// leader.
func Apply(ctx context.Context, s store.Store, fleet *state.Fleet, result *Result) (Changes, error) {
	var changes Changes

	for _, id := range sortedKeys(result.Sessions) {
		record := result.Sessions[id]
		prior := fleet.Sessions[id].Prior()

		_, wrote, err := state.PutJSON(ctx, s, store.SessionKey(id), record, prior, state.WriteOptions{CAS: true})
		if err != nil {
			return changes, fmt.Errorf("write session %s: %w", id, err)
		}
		if wrote {
			changes.SessionsWritten++
		}
	}

	for _, id := range sortedKeys(fleet.Sessions) {
		if _, keep := result.Sessions[id]; keep {
			continue
		}
		if _, err := s.Delete(ctx, store.SessionKey(id), store.IfRevision(fleet.Sessions[id].Rev)); err != nil {
			return changes, fmt.Errorf("delete session %s: %w", id, err)
		}
		changes.SessionsDeleted++
	}

	for _, node := range sortedKeys(result.Assignments) {
		set := result.Assignments[node]

		// The revision on the wire is the store revision the set was *served* at, not one the
		// reconciler knows or should invent: it is the agent's poll cursor. Storing a zero here
		// keeps the compare-before-write above honest — a set that is otherwise unchanged must
		// not look different because the store moved on for unrelated reasons.
		set.Revision = 0

		prior := fleet.Assignments[node].Prior()
		_, wrote, err := state.PutJSON(ctx, s, store.AssignmentsKey(node), set, prior, state.WriteOptions{CAS: true})
		if err != nil {
			return changes, fmt.Errorf("write assignments for %s: %w", node, err)
		}
		if wrote {
			changes.AssignmentsWritten++
		}
	}

	for _, node := range sortedKeys(fleet.Assignments) {
		if _, keep := result.Assignments[node]; keep {
			continue
		}
		// A node with an assignment set and no registration: it was deregistered, or the key
		// outlived whatever wrote it. Its agent, if any, is not being served anything anyway —
		// the agent API answers from registrations.
		if _, err := s.Delete(ctx, store.AssignmentsKey(node), store.IfRevision(fleet.Assignments[node].Rev)); err != nil {
			return changes, fmt.Errorf("delete assignments for %s: %w", node, err)
		}
		changes.AssignmentsDeleted++
	}

	return changes, nil
}

// AssignmentsFor returns a node's assignment set from a result, or an empty one.
//
// Empty is a real answer here — but only because the caller has already established that the
// reconciler has settled. Serving this without that check is the fleet-wide outage of plan §4.2.
func (r *Result) AssignmentsFor(node string) api.AssignmentSet {
	if set, ok := r.Assignments[node]; ok {
		return set
	}
	return api.AssignmentSet{Node: node}
}
