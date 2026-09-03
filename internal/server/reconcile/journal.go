package reconcile

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
	"github.com/jonasohland/mxl-replicator/internal/store"
)

// journal turns two consecutive [Result]s into event-log entries (§12.1).
//
// # Why this is memory and not state
//
// [Compute] is a pure function of one snapshot (§7.3) and must stay that way: a reconciler that
// read its own event log would have history affecting a decision, which is the one thing that
// section forbids outright. So the journal is a **side effect** — it observes what Compute
// concluded and never feeds anything back — and its memory of the previous pass is leader-local.
//
// The alternative, storing the last-emitted state beside each session, was refused for the reason
// §7.3 gives about the readiness record: a field that changes every pass is a feedback loop with a
// store write in it.
//
// # The gap a leader change leaves, and why it is announced
//
// Leader-local memory means a newly elected leader has no baseline. It emits **nothing** for its
// first pass and records [api.EventReconcilerTookOver] on the fleet ring instead.
//
// Emitting every current state as though it had just happened is the tempting alternative and is
// wrong in the way that matters: it fabricates a storm of "path became ACTIVE" on every leader
// change and every server restart, for paths that have been ACTIVE for hours. That is §7.3's
// settling argument one layer up — a server seeing something for the first time must not act as
// though it just happened — and it would make the log least trustworthy exactly when an operator
// reaches for it, since a leader change is often what they are investigating.
type journal struct {
	paths    map[string]pathMemory
	requests map[api.RequestID]requestMemory
	leased   map[string]bool

	// inventory is what each node was last *observed* to hold. Only consulted when
	// [journal.recordInventory] is on.
	inventory map[string]nodeInventory

	// recordInventory turns the flow and domain entries of §12.1 on. Server configuration, on by
	// default, and the one part of this log whose volume is set by the fleet rather than by the
	// control plane.
	recordInventory bool

	// seeded is false until a first pass has established the baseline. It is what makes a leader
	// change a marked gap rather than a fabricated storm.
	seeded bool
}

// nodeInventory is what one node was last observed to hold.
type nodeInventory struct {
	domains map[string]bool
	flows   map[string]bool
}

// pathMemory is the previous pass's view of one path — only the fields a transition is computed
// from, so that an incidental difference cannot produce an entry.
//
// This is §7.3's "already correct" test applied to the log: the agent keys on the config that
// materially affects the worker rather than diffing the assignment object, and for the same
// reason. A journal that diffed whole structures would emit an entry every time a port was
// re-derived or a field reordered.
type pathMemory struct {
	state   api.State
	reason  api.ReasonCode
	session string
	epoch   string
}

type requestMemory struct {
	state api.State
	paths int
}

func newJournal(recordInventory bool) *journal {
	return &journal{
		paths:           map[string]pathMemory{},
		requests:        map[api.RequestID]requestMemory{},
		leased:          map[string]bool{},
		inventory:       map[string]nodeInventory{},
		recordInventory: recordInventory,
	}
}

// entry is one event and the ring it belongs to.
type entry struct {
	key   string
	event api.Event
}

// forget names a ring to delete: a path's log dies with the path (§12.1).
type forget struct{ pathID string }

// diff records what changed since the previous pass and adopts the new state as the baseline.
//
// It returns the entries to record and the paths whose rings should be dropped. Both are batched
// deliberately: the caller flushes once per pass, never once per event, because store revisions
// are not free (§8.1, §12.1).
func (j *journal) diff(fleet *state.Fleet, result *Result, now time.Time) ([]entry, []forget) {
	if !j.seeded {
		// Paths, requests and leases are adopted as the baseline; **inventory deliberately is not**,
		// so the next pass emits a per-node baseline for every node rather than adopting it in
		// silence. Doing both here would make the baseline depend on a race with the settling
		// window — a node whose inventory happened to arrive before the first pass would get one and
		// a node whose arrived after would not, which is the kind of behaviour that reads as a bug
		// in whichever half an operator meets first.
		j.adopt(fleet, result)
		j.seeded = true
		return []entry{{
			key: store.KeyFleetEvents,
			event: api.Event{
				Kind:     api.EventReconcilerTookOver,
				Severity: api.SeverityWarn,
				At:       now,
				Message: fmt.Sprintf(
					"reconciler took over with %d paths and %d requests; transitions before this point were not recorded",
					len(result.Paths), len(result.Requests)),
			},
		}}, nil
	}

	var entries []entry

	for _, path := range result.SortedPaths() {
		entries = append(entries, j.diffPath(path, now)...)
	}

	var dropped []forget
	for id := range j.paths {
		if _, kept := result.Paths[id]; !kept {
			dropped = append(dropped, forget{pathID: id})
		}
	}

	entries = append(entries, j.diffRequests(result, now)...)
	entries = append(entries, j.diffLeases(fleet, now)...)
	entries = append(entries, j.diffInventory(fleet, now)...)

	j.adopt(fleet, result)
	j.adoptInventory(fleet)
	return entries, dropped
}

func (j *journal) diffPath(path api.Path, now time.Time) []entry {
	was, known := j.paths[path.ID]
	is := remember(path)

	var entries []entry

	switch {
	case !known:
		entries = append(entries, entry{
			key: store.PathEventsKey(path.ID),
			event: api.Event{
				Kind: api.EventPathState, Severity: severityFor(path.State), At: now,
				State: path.State, ReasonCode: path.ReasonCode,
				Message: fmt.Sprintf("path created: %s %s → %s",
					path.Source.Flow, path.Source.Node, path.Destination.Node),
			},
		})
	case was.state != is.state || was.reason != is.reason:
		entries = append(entries, entry{
			key:   store.PathEventsKey(path.ID),
			event: stateEvent(path, now),
		})
	}

	// A session that went away without being replaced: its workers were stopped and the path is
	// still here (§11.1's long-idle teardown, a conflict lost, an endpoint that stopped being
	// viable). Otherwise this is invisible — a path simply stops having a session, and nothing in
	// its status says that used to be different.
	//
	// **Only when nothing took its place.** A session ID changing is a *rebuild* — a new epoch, a
	// republished flow definition — and it already reports itself as an establishment below. Saying
	// "withdrawn" for every rebuild would put two entries on every target restart, which is exactly
	// the flapping this log has to stay legible through.
	//
	// **And only while the path survives.** A path that is gone has had its ring deleted in the
	// same pass (§12.1), so an entry about its last session would be written to a key that is about
	// to be removed — work done to produce nothing.
	if was.session != "" && is.session == "" {
		entries = append(entries, entry{
			key: store.PathEventsKey(path.ID),
			event: api.Event{
				// **The severity follows the path, not the withdrawal.** A long-idle teardown is
				// designed behaviour — §11.1's own table has `PAUSED` with no workers as a steady
				// state — so a flat warning would render the system working as intended as a
				// problem. A session withdrawn *into* INVALID or FAILED is a different matter, and
				// reading the severity off the state the path landed in gets both right with one
				// rule.
				Kind: api.EventSessionWithdrawn, Severity: severityFor(path.State), At: now,
				Session: was.session, ReasonCode: path.ReasonCode,
				Message: withdrawalMessage(path),
			},
		})
	}

	// A session change and a state change are two different facts about one pass and both are
	// worth recording: the state says what an operator sees, the session says which worker pair is
	// behind it. Collapsing them would lose the negotiated fabric, which appears nowhere else once
	// the session is replaced.
	if is.session != "" && was.session != is.session {
		entries = append(entries, entry{
			key: store.PathEventsKey(path.ID),
			event: api.Event{
				Kind: api.EventSessionEstablished, Severity: api.SeverityInfo, At: now,
				Session: is.session, Message: sessionMessage(path),
			},
		})
	}

	// An epoch change *within* one session is a target restart (§5.2) and nothing else — the
	// clearest flapping signal there is, and the per-path detail behind
	// `mxl_repl_epoch_transitions_total`. A new session's first epoch is not one, which is why
	// this is guarded on the session being unchanged.
	if was.session == is.session && was.epoch != "" && is.epoch != "" && was.epoch != is.epoch {
		entries = append(entries, entry{
			key: store.PathEventsKey(path.ID),
			event: api.Event{
				Kind: api.EventEpochChanged, Severity: api.SeverityWarn, At: now,
				Session: is.session, Node: path.Destination.Node,
				Message: fmt.Sprintf("target restarted on %s (epoch %s)",
					path.Destination.Node, short(is.epoch)),
			},
		})
	}

	return entries
}

// stateEvent renders one path transition.
//
// A conflict gets a kind of its own rather than being folded into the generic transition, because
// it is the case §7.5 calls otherwise undiagnosable — a path that went ACTIVE → INVALID overnight
// with nobody applying anything — and an operator has to be able to find it without knowing which
// reason codes are conflicts.
func stateEvent(path api.Path, now time.Time) api.Event {
	kind := api.EventPathState
	if isConflict(path.ReasonCode) {
		kind = api.EventConflictLost
	}

	message := fmt.Sprintf("path is %s", path.State)
	if path.Reason != "" {
		message = fmt.Sprintf("path is %s: %s", path.State, path.Reason)
	}

	return api.Event{
		Kind: kind, Severity: severityFor(path.State), At: now,
		State: path.State, ReasonCode: path.ReasonCode, Message: message,
		Session: path.SessionID,
	}
}

func isConflict(code api.ReasonCode) bool {
	switch code {
	case api.ReasonFlowConflict, api.ReasonNamespaceOverlap, api.ReasonLoop:
		return true
	}
	return false
}

// withdrawalMessage says what an operator needs beside "the workers stopped": *why*, which the path
// is already carrying in the state it moved to on the same pass.
//
// Reading it off the path rather than passing a cause down from wherever the withdrawal was decided
// keeps the log and the status from being able to disagree — the same rule §12.1 applies to reason
// codes, one level up.
func withdrawalMessage(path api.Path) string {
	if path.Reason != "" {
		return fmt.Sprintf("session withdrawn, workers stopped: %s", path.Reason)
	}
	return fmt.Sprintf("session withdrawn, workers stopped; path is %s", path.State)
}

func sessionMessage(path api.Path) string {
	if path.Session == nil {
		return "session created"
	}
	provider := string(path.Session.Interface.Provider)
	switch {
	case provider != "" && path.Session.Fabric != "":
		return fmt.Sprintf("session created on %s/%s", provider, path.Session.Fabric)
	case provider != "":
		return fmt.Sprintf("session created on %s", provider)
	}
	return "session created"
}

func (j *journal) diffRequests(result *Result, now time.Time) []entry {
	var entries []entry

	for _, id := range sortedRequestIDs(result.Requests) {
		status := result.Requests[id]
		was, known := j.requests[id]
		is := requestMemory{state: status.State, paths: len(status.Paths)}

		if !known || was.state != is.state {
			entries = append(entries, entry{
				key: store.RequestEventsKey(id.Namespace, id.Name),
				event: api.Event{
					Kind: api.EventRequestState, Severity: severityFor(is.state), At: now,
					State: is.state, ReasonCode: status.ReasonCode,
					Message: requestMessage(status),
				},
			})
		}

		// An expansion that shrank is the failure this kind exists for: a selector quietly
		// matching less than it did looks like nothing at all in a status, because the paths it
		// lost simply are not there any more (§12.1).
		if known && was.paths != is.paths {
			entries = append(entries, entry{
				key: store.RequestEventsKey(id.Namespace, id.Name),
				event: api.Event{
					Kind: api.EventExpansionChanged, Severity: expansionSeverity(was.paths, is.paths),
					At: now,
					Message: fmt.Sprintf("expansion changed: %d paths, was %d",
						is.paths, was.paths),
				},
			})
		}
	}

	return entries
}

func expansionSeverity(was, is int) api.EventSeverity {
	if is < was {
		return api.SeverityWarn
	}
	return api.SeverityInfo
}

func requestMessage(status api.RequestStatus) string {
	if status.Reason != "" {
		return fmt.Sprintf("request is %s: %s", status.State, status.Reason)
	}
	return fmt.Sprintf("request is %s", status.State)
}

// diffLeases records a node going dark.
//
// Only the disappearance: registration is recorded by the handler that accepts it, which knows the
// instance and is the moment it actually happened. An expiring lease has no handler — it is
// observed by whoever notices the key is gone — so it has to be noticed here.
//
// It matters more than it looks. A node losing its lease freezes every path touching it (§4.2)
// rather than converging, which is deliberate and is invisible in a path's own status: the path
// simply stops changing. This is the entry that says why.
func (j *journal) diffLeases(fleet *state.Fleet, now time.Time) []entry {
	var entries []entry

	for node := range j.leased {
		if _, still := fleet.Leases[node]; still {
			continue
		}
		entries = append(entries, entry{
			key: store.NodeEventsKey(node),
			event: api.Event{
				Kind: api.EventNodeLeaseExpired, Severity: api.SeverityWarn, At: now,
				Node:    node,
				Message: "lease expired; paths touching this node are frozen, not torn down",
			},
		})
	}

	return entries
}

// diffInventory records flows and domains entering and leaving a node's inventory (§12.1).
//
// # The rule this feature would otherwise get wrong
//
// **A node that is not leased is skipped entirely.** Its inventory is leased state, so it is gone
// from the snapshot the moment the lease expires — and diffing against that says every flow on the
// node disappeared, at the exact moment nothing happened to any of them. §4.2's closing line is
// the whole of it: *"no observation" is never "nothing there"*, and the mechanism is always
// freezing rather than converging. The node's paths are frozen for the same reason on the same
// pass; this freezes its inventory memory beside them.
//
// # Batched per pass
//
// One entry per node per kind per pass, naming what it can and counting the rest. Fifty entries for
// a restarting node would evict a fifty-entry ring and take the registration entry that explains
// the episode with it, which is the log being emptiest exactly when it is most needed.
func (j *journal) diffInventory(fleet *state.Fleet, now time.Time) []entry {
	if !j.recordInventory {
		return nil
	}

	var entries []entry

	for _, node := range sortedKeys(fleet.Inventory) {
		if _, live := fleet.Leases[node]; !live {
			continue
		}
		is := observed(fleet.Inventory[node].Value)

		was, known := j.inventory[node]
		if !known {
			// First observation of this node under this leader. **Not "everything appeared"** — the
			// node may have been running for days, and reporting its whole inventory as new is the
			// fabricated storm the takeover marker exists to avoid.
			//
			// But not silence either, which is what this originally did and which reads exactly like
			// a node whose flows never appeared. A baseline says where the record starts, and an
			// operator who can see that has the one thing the silence denied them: a reason for the
			// log to begin where it does.
			entries = append(entries, entry{
				key: store.NodeEventsKey(node),
				event: api.Event{
					Kind: api.EventInventoryBaseline, Severity: api.SeverityInfo, At: now, Node: node,
					Message: baselineMessage(is),
				},
			})
			continue
		}

		entries = append(entries,
			inventoryEntries(node, now, "domain", was.domains, is.domains,
				api.EventDomainAppeared, api.EventDomainDisappeared)...)
		entries = append(entries,
			inventoryEntries(node, now, "flow", was.flows, is.flows,
				api.EventFlowAppeared, api.EventFlowDisappeared)...)
	}

	return entries
}

// baselineMessage says what a node was holding when this leader first looked.
func baselineMessage(held nodeInventory) string {
	return fmt.Sprintf("first observed holding %s in %s",
		plural(len(held.flows), "flow"), plural(len(held.domains), "domain"))
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// inventoryEntries renders one node's additions and removals as at most two entries.
func inventoryEntries(node string, now time.Time, noun string, was, is map[string]bool,
	appeared, disappeared api.EventKind,
) []entry {
	added := missing(is, was)
	removed := missing(was, is)

	var entries []entry
	if len(added) > 0 {
		entries = append(entries, entry{
			key: store.NodeEventsKey(node),
			event: api.Event{
				Kind: appeared, Severity: api.SeverityInfo, At: now, Node: node,
				Message: inventoryMessage(noun, "appeared", added),
			},
		})
	}
	if len(removed) > 0 {
		// **Info, not a warning, and that is the whole of the severity policy here.** A producer
		// stopping is an ordinary thing for a fleet to do — it is the same fact `PAUSED` exists to
		// keep out of the fault vocabulary (§11), and rendering it as a warning makes a board full
		// of routine churn read as a board full of problems, which is the argument §11 makes a
		// second time for `DISABLED`.
		//
		// Where a disappearance actually *causes* something, that consequence is recorded where it
		// belongs and carries its own severity: a request whose expansion shrank, a path that went
		// WAITING. This entry is the fact, not the verdict.
		entries = append(entries, entry{
			key: store.NodeEventsKey(node),
			event: api.Event{
				Kind: disappeared, Severity: api.SeverityInfo, At: now, Node: node,
				Message: inventoryMessage(noun, "disappeared", removed),
			},
		})
	}
	return entries
}

// namedInInventoryEntry is how many identities one entry spells out before it starts counting.
//
// Eight, because the point of naming any of them is that an operator can search for the one they
// care about, and the point of stopping is that a fifty-flow node must not put fifty identifiers in
// one message. The count of the remainder is always printed — a truncated list that trails off
// reads as a complete short one.
const namedInInventoryEntry = 8

func inventoryMessage(noun, verb string, names []string) string {
	label := noun
	if len(names) != 1 {
		label = noun + "s"
	}

	shown := names
	suffix := ""
	if len(shown) > namedInInventoryEntry {
		shown = shown[:namedInInventoryEntry]
		suffix = fmt.Sprintf(", and %d more", len(names)-namedInInventoryEntry)
	}
	return fmt.Sprintf("%d %s %s: %s%s", len(names), label, verb, strings.Join(shown, ", "), suffix)
}

// missing returns the keys of a that are absent from b, sorted so that two replicas rendering the
// same change produce the same message.
func missing(a, b map[string]bool) []string {
	var out []string
	for key := range a {
		if !b[key] {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

func (j *journal) adopt(fleet *state.Fleet, result *Result) {
	clear(j.paths)
	for id, path := range result.Paths {
		j.paths[id] = remember(path)
	}

	clear(j.requests)
	for id, status := range result.Requests {
		j.requests[id] = requestMemory{state: status.State, paths: len(status.Paths)}
	}

	clear(j.leased)
	for node := range fleet.Leases {
		j.leased[node] = true
	}
}

// adoptInventory takes the new baseline for every node that is **currently observable**.
//
// A node that is not leased has had its inventory garbage-collected with its lease (§4), so there
// is nothing to adopt — and adopting the absence would be the mistake §4.2 exists to prevent,
// arriving one layer up: on the node's return, everything it holds would read as newly appeared.
// Its previous inventory is therefore carried forward untouched, which is the same freezing the
// reconciler applies to that node's paths.
func (j *journal) adoptInventory(fleet *state.Fleet) {
	if !j.recordInventory {
		return
	}

	for node, entry := range fleet.Inventory {
		if _, live := fleet.Leases[node]; !live {
			continue
		}
		j.inventory[node] = observed(entry.Value)
	}

	// A node whose registration is gone is forgotten outright: it is not coming back under this
	// identity, and holding its inventory would make a re-registered node of the same name report
	// nothing as new.
	for node := range j.inventory {
		if _, registered := fleet.Nodes[node]; !registered {
			delete(j.inventory, node)
		}
	}
}

func observed(snapshot api.InventorySnapshot) nodeInventory {
	held := nodeInventory{domains: map[string]bool{}, flows: map[string]bool{}}
	for _, domain := range snapshot.Domains {
		held.domains[domain.Domain.String()] = true
		for _, flow := range domain.Flows {
			// Keyed by `(domain, flow)`, not by flow ID: the same flow ID can legitimately exist in
			// two domains on one node (§3), and collapsing them would report a flow as still
			// present after it left one of the two.
			held.flows[domain.Domain.String()+"/"+flow.ID] = true
		}
	}
	return held
}

// reset forgets the baseline, so that the next pass declares a gap instead of inventing
// transitions across it. Called when leadership is lost.
func (j *journal) reset() {
	j.seeded = false
	clear(j.paths)
	clear(j.requests)
	clear(j.leased)

	// The inventory memory goes too, so the next leader re-baselines rather than diffing against
	// observations it did not make. Without this a replica that was demoted and promoted again
	// would report changes from the gap as though it had watched them happen — the same claim the
	// takeover marker exists to refuse.
	clear(j.inventory)
}

// sortedRequestIDs orders a request map deterministically.
//
// Not cosmetic: two replicas differing in the order they record a pass would produce two rings
// whose entries appear in different orders for the same fleet, and a poller resuming on a sequence
// number would see different history depending on which one it asked.
func sortedRequestIDs[V any](m map[api.RequestID]V) []api.RequestID {
	out := make([]api.RequestID, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	slices.SortFunc(out, func(a, b api.RequestID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

func remember(path api.Path) pathMemory {
	memory := pathMemory{
		state:   path.State,
		reason:  path.ReasonCode,
		session: path.SessionID,
	}
	if path.Session != nil {
		memory.epoch = path.Session.Epoch
	}
	return memory
}

func severityFor(state api.State) api.EventSeverity {
	switch state {
	case api.StateFailed, api.StateInvalid:
		return api.SeverityError
	case api.StateDegraded, api.StatePartial:
		return api.SeverityWarn
	default:
		return api.SeverityInfo
	}
}

func short(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8] + "…"
}
