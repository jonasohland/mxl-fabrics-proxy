package api

import (
	"strings"
	"time"
)

// This file is the event log's wire and storage vocabulary (§12.1).
//
// Everything in [State] is level-triggered and last-write-wins: it says what is true *now*. An
// operator debugging a failing path needs what *happened*, and none of it is otherwise retained
// anywhere the control plane can serve — a path that flapped for ten minutes and is ACTIVE again
// reports nothing about the ten minutes.
//
// Three properties shape every type here, and each one is load-bearing rather than stylistic:
//
//   - **An object's log is one bounded ring in one value**, not one key per event (§12.1). That is
//     §9.2's level-triggered discipline a third time: an append-only stream needs sequencing, gap
//     detection, compaction and a garbage collector, and a ring in one value needs none of them.
//   - **Bounded by count, never by age.** The overnight failure someone arrives to at 09:00 is the
//     case this log most exists for, and an age bound expires it exactly then.
//   - **Repeats coalesce.** Forty-seven identical worker exits are one entry that says *flapping*,
//     which is both what an operator needs to read and what keeps a count bound honest — without
//     it, a crash loop evicts the establishment history that explains it.
//
// And one property that is a warning rather than a design: this is a **diagnostic aid, not an
// audit log**. The agent's queue is memory and its restart loses whatever is pending (§6.1), a
// ring drops its oldest, and a leader change leaves a marked gap. Every loss announces itself, but
// nothing here is a record of what happened — it is the best account processes with bounded memory
// can give of it.

// EventSeverity is how loudly an entry should read. Three values, matching the severities an
// operator already sees in a log line, and deliberately not the [State] vocabulary: a state is
// what something *is*, a severity is how much a reader should care that it changed.
type EventSeverity string

const (
	// SeverityInfo is the ordinary progress of a healthy object: a session established, a first
	// grain received, a node registering.
	SeverityInfo EventSeverity = "info"

	// SeverityWarn is something that will probably resolve by itself but explains a symptom: an
	// epoch change, a node's lease expiring, a gap in this log.
	//
	// **Designed behaviour is never a warning, however inconvenient it is.** A producer stopping, a
	// long-idle teardown, a start waiting for a permit — each of those is the system doing exactly
	// what §11.1 or §6.3 says it should, and rendering them as warnings makes a board of ordinary
	// fleet activity read as a board of problems. That is the mistake §11 avoids twice over, once
	// by giving an idle source `PAUSED` instead of a fault and once by giving a parked leg
	// `DISABLED`, and the same rule governs here.
	//
	// Where such a thing does cause a problem, the *consequence* carries the severity, on the
	// object it happened to.
	SeverityWarn EventSeverity = "warn"

	// SeverityError is a failure, and is what the log is read for.
	SeverityError EventSeverity = "error"
)

// EventKind is the closed vocabulary of what an entry says.
//
// Closed, and paired with a [ReasonCode] from the *existing* vocabulary rather than a second one
// (§12.1). Free text is unqueryable, untranslatable and impossible to coalesce on — [Event.Message]
// is the human rendering of a kind and its fields, never the thing a reader has to match against.
type EventKind string

const (
	// --- Path-anchored (§12.1: the path is the unit of retention) ---

	// EventPathState is a path moving between the states of §11. The workhorse of the log.
	EventPathState EventKind = "path_state_changed"

	// EventSessionEstablished is a session reaching ready on both ends, carrying the negotiated
	// fabric and interface — which is the one moment the negotiation result is worth recording,
	// since [Session] holds only the current one.
	EventSessionEstablished EventKind = "session_established"

	// EventEpochChanged is a target restarting (§5.2). An excellent flapping signal, and the
	// reason `mxl_repl_epoch_transitions_total` exists; here it is the per-path detail behind
	// that counter.
	EventEpochChanged EventKind = "epoch_changed"

	// EventWorkerExited is a worker dying when the agent did not stop it. Carries the log tail
	// when it is the transition into FAILED (§12.2).
	EventWorkerExited EventKind = "worker_exited"

	// EventWorkerStartQueued is a start waiting for a permit from the token bucket (§6.3).
	//
	// Worth its own kind because a queued start is otherwise indistinguishable from one that
	// launched and is coming up slowly, which is exactly the distinction an operator watching a
	// slow recovery needs.
	EventWorkerStartQueued EventKind = "worker_start_queued"

	// EventAssignmentRejected is an assignment the agent could not honour: a domain it does not
	// observe, an area it does not grant write on, a blob that fails verification (§6).
	//
	// Reported rather than dropped, for the reason §6 gives — dropping it leaves a path in
	// ESTABLISHING with nothing anywhere to explain why.
	EventAssignmentRejected EventKind = "assignment_rejected"

	// EventSessionWithdrawn is a session taken away while its path stays: a long-idle teardown
	// (§11.1), a conflict lost, an endpoint that stopped being viable.
	//
	// Two things it deliberately does not cover, both because there would be nothing to write it
	// to or nothing new to say:
	//
	//   - **A request deleted.** That removes the path as well, and a path's log dies with the path
	//     (§12.1) — so the entry would be written to a key being deleted in the same pass.
	//   - **A rebuild.** A new epoch or a republished flow definition *replaces* the session, and
	//     that reports itself as [EventSessionEstablished]. Recording a withdrawal beside it would
	//     put two entries on every target restart, which is the flapping this log has to stay
	//     legible through.
	//
	// What is left is the case that is otherwise invisible: the path stops having a session, and
	// nothing in its status says that used to be different.
	EventSessionWithdrawn EventKind = "session_withdrawn"

	// EventConflictLost is a path losing to another under §7.5's precedence, naming the winner.
	//
	// The case this exists for is the one §7.5 calls otherwise undiagnosable: a path that went
	// ACTIVE → INVALID overnight with nobody applying anything.
	EventConflictLost EventKind = "path_conflict_lost"

	// --- Request-anchored: what is genuinely request-scoped and has no path to live on ---

	// EventRequestState is a request's aggregate moving, including into the two aggregate-only
	// words (§11).
	EventRequestState EventKind = "request_state_changed"

	// There is deliberately **no `request_rejected`**, and the reason is structural rather than an
	// omission. A refusal at `POST` happens before anything is written (§7.2): the handler computes
	// against a candidate fleet, finds the request structurally invalid and returns 400 without
	// creating it — so there is no request, and therefore no ring for the entry to live in. Every
	// refusal that *is* recordable has a request behind it, and that one arrives as
	// [EventRequestState] carrying INVALID and the code that refused it, which is the same
	// information under a kind that can actually be emitted.
	//
	// EventExpansionChanged is a selector matching a different set than it did: three flows and
	// now two.
	//
	// The case that proves the request needs a log of its own is a request expanding onto
	// **nothing** — there is no path, and "why is this WAITING" has nowhere else to be asked.
	EventExpansionChanged EventKind = "expansion_changed"

	// --- Node-anchored: cheap by construction, and it outlives the paths ---

	// EventNodeRegistered is an agent registering or re-registering, carrying the instance.
	//
	// This is the entry that answers "why did every path on edge-01 re-establish at 12:04" in one
	// line rather than in fifty identical path entries (§12.1).
	EventNodeRegistered EventKind = "node_registered"

	// There is deliberately **no `interfaces_probed`**, for the same structural reason there is no
	// `request_rejected`. The probe's result is what the agent puts in its registration body
	// (§10.5), so the server already holds every number such an entry could carry and records them
	// on this one — a second entry would be the same fact written twice, one store write apart. And
	// a probe that *fails* cannot be recorded at all: the agent has no lease, so it is not
	// registered, so it has nothing to report through. That failure surfaces as a node that never
	// appears rather than as an entry.
	//
	// EventInventoryBaseline is a leader saying where a node's inventory record *starts*: what that
	// node was holding the first time this leader saw it.
	//
	// It exists because the alternative to announcing a baseline is announcing nothing, and the
	// silence is indistinguishable from a node whose flows never appeared. A leader cannot honestly
	// report a first observation as an appearance — the flows may have been there for days, and
	// saying they just arrived is the fabricated-storm mistake [EventReconcilerTookOver] exists to
	// avoid — but it can say *this is where the record begins*, which is the same honesty in a form
	// an operator can read.
	//
	// **It coalesces, deliberately**, unlike the four kinds below. What it carries is counts rather
	// than identities, so folding loses nothing an operator cannot get from the inventory itself —
	// and the resulting count is a signal in its own right: a node re-baselined three times in two
	// minutes is leader churn, which §8.2 calls otherwise invisible.
	EventInventoryBaseline EventKind = "inventory_baseline"

	// EventFlowAppeared and EventFlowDisappeared are flows entering and leaving a node's inventory
	// (§6), recorded on that node's ring when the server is configured to (§12.1).
	//
	// **Optional, and on by default**, because this is the one part of the log whose volume is set
	// by the fleet rather than by the control plane: a node's flows are whatever its producers are
	// doing, and a deployment where that churns is a deployment that should be able to turn this
	// off without losing the rest of the log.
	//
	// **Batched per pass, never per flow.** A node restarting takes fifty flows away and brings
	// fifty back (§14), and one entry each would evict a fifty-entry ring twice over — losing the
	// registration entry that explains the whole episode. One entry per pass names what it can and
	// counts the rest, the same shape [RequestStatus] already uses for excluded flows.
	EventFlowAppeared    EventKind = "flow_appeared"
	EventFlowDisappeared EventKind = "flow_disappeared"

	// EventDomainAppeared and EventDomainDisappeared are the same for domains, which are far lower
	// churn: a domain exists while it holds flows or a session targets it (§10.6), so it moves when
	// an operator or the reconciler does something rather than when a producer does.
	EventDomainAppeared    EventKind = "domain_appeared"
	EventDomainDisappeared EventKind = "domain_disappeared"

	// EventNodeLeaseExpired is a node going dark. Its paths are frozen rather than converged
	// (§4.2), which is worth being able to see.
	EventNodeLeaseExpired EventKind = "node_lease_expired"

	// EventNodeClaimed is a second instance claiming a node name held by another (§7.1). Neither
	// stops a worker, and the second claimant keeps asking.
	EventNodeClaimed EventKind = "node_claimed"

	// --- Markers: a gap in this log is always visible in this log ---

	// EventReconcilerTookOver is a newly elected leader declaring that it has no baseline, so its
	// first pass emitted no state transitions (§12.1).
	//
	// The alternative — emitting every current state as though it had just happened — fabricates a
	// storm on every leader change and every server restart. That is §7.3's settling argument one
	// layer up: a server seeing something for the first time must not act as though it just
	// happened.
	EventReconcilerTookOver EventKind = "reconciler_took_over"

	// EventsDropped is entries lost before they could be recorded: an agent's bounded queue
	// overflowing, or an agent restart discarding what was pending (§6.1).
	//
	// Distinct from [EventRing.Dropped], which is history aged out of a full ring. Both are
	// expected in a bad hour and only this one means something was never seen at all.
	EventsDropped EventKind = "events_dropped"
)

// Coalesces reports whether two adjacent entries of this kind may be folded into one.
//
// **False for every kind whose identity lives in its message**, and that is the whole of the rule.
// [Event.coalescesWith] deliberately ignores the message, because "exited after 1.2s" and "exited
// after 0.9s" are one worker failing twice — but an entry that *names* what it is about is a
// different fact each time it is written, and folding two of them keeps the newest message and
// silently discards the other's contents.
//
// It was found in a live fleet rather than predicted here: four flows appearing in four consecutive
// reconcile passes rendered as `1 flow appeared: …b8d6c502 ×3`, naming one of the four and losing
// the other three. The same trap caught path transitions earlier, where the fix was to add the
// state to the coalescing key; this is that fix generalised, because piling every identity-bearing
// field into the key does not scale and gets forgotten by whoever adds the next kind.
func (k EventKind) Coalesces() bool {
	switch k {
	case EventFlowAppeared, EventFlowDisappeared, EventDomainAppeared, EventDomainDisappeared:
		return false
	}
	return true
}

// EventKinds is the whole vocabulary, for validation and for a metric that must be able to report
// a zero — same reason [States] is exported.
func EventKinds() []EventKind {
	return []EventKind{
		EventPathState, EventSessionEstablished, EventEpochChanged, EventWorkerExited,
		EventWorkerStartQueued, EventAssignmentRejected, EventSessionWithdrawn, EventConflictLost,
		EventRequestState, EventExpansionChanged,
		EventNodeRegistered, EventNodeLeaseExpired, EventNodeClaimed,
		EventInventoryBaseline,
		EventFlowAppeared, EventFlowDisappeared, EventDomainAppeared, EventDomainDisappeared,
		EventReconcilerTookOver, EventsDropped,
	}
}

// Event is one entry in an object's ring.
type Event struct {
	// Seq orders the ring and is what a poller resumes from. Assigned server-side, monotonic
	// within one ring and meaningless across rings.
	//
	// **This, not the timestamp, is the ordering.** An entry is stamped by whoever emitted it, so
	// a request's merged view interleaves the clocks of two agents and a leader. TAI correctness
	// is a deployment assumption (§11.1), but it is an assumption about offsets rather than about
	// ordering across hosts, and a log that implied otherwise would invite an operator to read
	// causality out of two nodes' timestamps.
	Seq uint64 `json:"seq"`

	Kind     EventKind     `json:"kind"`
	Severity EventSeverity `json:"severity"`

	// At is when this entry last happened, and FirstAt when the run of coalesced entries behind
	// it started. FirstAt is set only when Count is above one, so an ordinary entry carries one
	// timestamp and says one thing.
	At      time.Time `json:"at"`
	FirstAt time.Time `json:"first_at,omitzero"`

	// Count is how many occurrences this entry stands for, omitted when it is one. `×47 over 6m`
	// is the rendering, and it is more legible than forty-seven rows as well as cheaper (§12.1).
	Count int `json:"count,omitempty"`

	// Message is the human rendering. A reader matches on Kind and ReasonCode; this is for eyes.
	Message string `json:"message"`

	// ReasonCode is §7.2's vocabulary, never a second one. An entry about a path going INVALID
	// carries the code that path is reporting, so the log and the status cannot disagree.
	ReasonCode ReasonCode `json:"reason_code,omitempty"`

	// State is what the object moved *to*, on the kinds that describe a transition.
	//
	// A field rather than something a reader digs out of [Event.Message], and the reason is
	// coalescing: without it, ACTIVE → FAILED → ACTIVE is three entries whose coalescing key is
	// identical, so the ring would fold a recovering path into one row that says it changed state
	// three times and never says to what. A renderer also gets its state badge without parsing
	// English.
	State State `json:"state,omitempty"`

	// Node, Session and Role locate the entry inside its object. Session is a *field* precisely
	// because a session is not a log of its own: a session-scoped log would split the story at the
	// re-establishment being investigated (§12.1).
	Node    string `json:"node,omitempty"`
	Session string `json:"session,omitempty"`
	Role    Role   `json:"role,omitempty"`

	// Request is the `<ns>/<name>` that caused a path entry, where one request is responsible. A
	// path is shared by N requests (§3), so this is empty as often as not.
	Request string `json:"request,omitempty"`

	// HasLog reports that a worker log tail was stored with this entry and can be fetched from the
	// path's `logs` endpoint (§12.2).
	//
	// A marker rather than the tail itself: inlining a few KiB per failure into a ring that a UI
	// polls would make the cheap read expensive exactly when things are failing, which is when it
	// is read most.
	HasLog bool `json:"has_log,omitempty"`
}

// coalescesWith reports whether e is another occurrence of the same thing as other.
//
// **The message is deliberately not part of the test.** "exited after 1.2s" and "exited after
// 0.9s" are the same event happening twice, and a message-sensitive comparison would defeat
// coalescing on exactly the flapping this feature exists to compress. The merged entry keeps the
// *latest* message, so the detail on screen is the most recent one.
func (e Event) coalescesWith(other Event) bool {
	if !e.Kind.Coalesces() {
		return false
	}
	return e.Kind == other.Kind &&
		e.Severity == other.Severity &&
		e.ReasonCode == other.ReasonCode &&
		e.State == other.State &&
		e.Node == other.Node &&
		e.Session == other.Session &&
		e.Role == other.Role &&
		e.Request == other.Request
}

// DefaultEventRingSize is how many entries an object's ring holds (§12.1).
//
// Generous, because nothing that repeats can fill it: coalescing means fifty entries is fifty
// *distinct* things, and an object that has genuinely done fifty distinct things has a history
// worth reading. The bound is on count and not on age, so a path that failed last week still
// holds the ring saying so.
const DefaultEventRingSize = 50

// EventRing is an object's whole log: a bounded, coalescing ring stored as one value (§12.1).
//
// The zero value is a valid empty ring, which matters because the read path renders a missing key
// as an empty log rather than as an error — an object nothing has happened to yet is the ordinary
// case, not a 404.
type EventRing struct {
	// Events, oldest first.
	Events []Event `json:"events"`

	// Next is the sequence number the next entry takes. Kept on the ring rather than derived from
	// the last entry, so that sequence numbers keep rising as entries age out and a poller
	// resuming from an old cursor cannot be handed a lower number it has already seen.
	Next uint64 `json:"next"`

	// Accepted is the highest [AgentEvent.Seq] recorded from each agent instance, on a node's ring
	// only.
	//
	// Delivery from an agent is at-least-once (§9.2): a batch that was written and whose response
	// was lost arrives again, and the agent holds no persistent state to prevent that (§6.1). This
	// is what makes the duplicate a no-op. It lives on the node's ring rather than in a key of its
	// own so that accepting a batch is the same write as recording it — a separate cursor key
	// would double the writes this log is allowed to make.
	//
	// Keyed by instance rather than by node: a restarted agent starts counting from zero again, so
	// a cursor carried across instances would silently discard everything the new one reports.
	Accepted map[string]uint64 `json:"accepted,omitempty"`

	// Dropped counts entries evicted by the bound over this ring's life. A reader that sees it
	// rise knows history was lost here, which is the same discipline as [EventsDropped] one level
	// down: a gap in this log is visible in this log.
	Dropped uint64 `json:"dropped,omitempty"`
}

// Append adds an entry, coalescing it into the last one when it is another occurrence of the same
// thing, and trims the ring to limit.
//
// It reports whether the ring changed, which is what lets a caller skip a store write — the same
// no-write-if-unchanged discipline as §6's compare-before-send, and it is what keeps the event log
// from becoming the writer that breaks §8.3.
func (r *EventRing) Append(e Event, limit int) bool {
	if limit <= 0 {
		limit = DefaultEventRingSize
	}
	// **Sequence numbers start at one, and that is load-bearing rather than aesthetic.** A cursor
	// of zero means "everything this ring still holds", and a reader resumes from entries *above*
	// its cursor — so a zero-numbered entry is indistinguishable from one already seen and is
	// filtered out of the first read of every ring.
	if r.Next == 0 {
		r.Next = 1
	}

	if n := len(r.Events); n > 0 && r.Events[n-1].coalescesWith(e) {
		last := &r.Events[n-1]
		if last.Count == 0 {
			last.Count = 1
		}
		if last.FirstAt.IsZero() {
			last.FirstAt = last.At
		}
		last.Count++
		last.At = e.At
		last.Message = e.Message
		last.HasLog = last.HasLog || e.HasLog
		// A new sequence number even though the entry stayed in place: a poller resuming from the
		// old one must be handed the updated count rather than told nothing changed.
		last.Seq = r.Next
		r.Next++
		return true
	}

	e.Seq = r.Next
	r.Next++
	r.Events = append(r.Events, e)

	if over := len(r.Events) - limit; over > 0 {
		r.Events = append([]Event(nil), r.Events[over:]...)
		r.Dropped += uint64(over)
	}
	return true
}

// Since returns the entries with a sequence number above cursor, for a poller resuming.
//
// A cursor from a ring whose history has since been dropped simply gets everything the ring still
// holds — the [EventRing.Dropped] count is how a reader learns it missed something, rather than an
// error that would make an ordinary catch-up look like a failure.
func (r *EventRing) Since(cursor uint64) []Event {
	out := make([]Event, 0, len(r.Events))
	for _, e := range r.Events {
		if e.Seq > cursor {
			out = append(out, e)
		}
	}
	return out
}

// EventList is what the read endpoints return (§9.1).
type EventList struct {
	Events []Event `json:"events"`

	// Dropped is [EventRing.Dropped], carried onto the wire so a reader can tell a short log from
	// a truncated one.
	Dropped uint64 `json:"dropped,omitempty"`

	// Next is the cursor to resume from: the highest sequence number in this response, or the
	// caller's own cursor when nothing is new.
	Next uint64 `json:"next"`
}

// AgentEvent is one entry on its way from an agent to the server (§9.2, §12.1).
//
// Agents never write the store (§4), so what an agent observes — why a worker exited, that a start
// is queued behind a permit, that an assignment could not be honoured — reaches the log through
// the agent API. It is a separate endpoint from `status` rather than a field on it, because
// `status` is a compared-before-send snapshot (§6) and an event folded into one is dropped when it
// repeats and re-sent forever when it does not.
type AgentEvent struct {
	// Seq is the agent's own monotonic counter, and it exists for de-duplication: delivery is
	// at-least-once, so a batch that was written and whose response was lost arrives again. The
	// server drops what it has already recorded from this instance rather than the agent trying
	// to guarantee exactly-once, which it cannot — it holds no persistent state (§6.1).
	Seq uint64 `json:"seq"`

	// Path is the path ID this entry belongs to. Empty means the node's own log, which is where
	// anything not attributable to one path goes.
	Path string `json:"path,omitempty"`

	Kind       EventKind     `json:"kind"`
	Severity   EventSeverity `json:"severity"`
	At         time.Time     `json:"at"`
	Message    string        `json:"message"`
	ReasonCode ReasonCode    `json:"reason_code,omitempty"`
	State      State         `json:"state,omitempty"`
	Session    string        `json:"session,omitempty"`
	Role       Role          `json:"role,omitempty"`

	// Log is the worker's log tail, present only on the entry carrying a transition into FAILED
	// (§12.2) — one tail per crash loop, not one per restart.
	Log string `json:"log,omitempty"`
}

// EventBatch is POST /agent/v1/{node}/events.
type EventBatch struct {
	Node     string       `json:"node"`
	Instance string       `json:"instance"`
	Events   []AgentEvent `json:"events"`

	// Dropped is how many entries this agent lost to a full queue since its last successful
	// report. The server records it as an [EventsDropped] marker on the node's log rather than
	// silently accepting a gap.
	Dropped uint64 `json:"dropped,omitempty"`
}

// DefaultLogTailBytes is how much worker output the agent keeps per start, and the default cap on
// what the server will store (§12.2).
//
// Bounded in **bytes and not in lines**, because a flow definition inside an error message is a
// line the size of a flow definition (§15) and a line budget would let one of them evict a whole
// start's history.
const DefaultLogTailBytes = 8 << 10

// LogTail is the last failing worker's output for a path (§12.2), as
// GET /v1/paths/{id}/logs returns it.
//
// Stored under its own key and fetched by its own endpoint, so the ring a UI polls stays small
// while the heavy read is deliberate.
type LogTail struct {
	Path    string `json:"path"`
	Node    string `json:"node"`
	Session string `json:"session,omitempty"`
	Role    Role   `json:"role,omitempty"`

	// At is when the worker this came from died.
	At time.Time `json:"at"`

	// Truncated reports that output was discarded from the **head** to fit the bound. The head is
	// what goes: a worker's fatal line is its last, in both failure shapes — one that never comes
	// up and one that dies after hours (§12.2).
	Truncated bool `json:"truncated,omitempty"`

	Text string `json:"text"`
}

// TailBytes truncates s to the last n bytes, reporting whether anything was discarded.
//
// It cuts at a line boundary when there is one in the discarded portion, so the first line of a
// tail is a whole line rather than the back half of one — a partial line reads as corruption to
// somebody who does not know the buffer is bounded.
func TailBytes(s string, n int) (string, bool) {
	if n <= 0 || len(s) <= n {
		return s, false
	}
	cut := s[len(s)-n:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i+1 < len(cut) {
		cut = cut[i+1:]
	}
	return cut, true
}
