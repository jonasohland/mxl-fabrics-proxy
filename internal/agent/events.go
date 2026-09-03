package agent

import (
	"sync"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// DefaultEventQueue is how many entries an agent buffers between reports.
//
// Generous next to what a healthy node produces — a worker's whole lifecycle is a handful of
// entries — and small next to what a bad minute produces, which is the point: the bound exists to
// keep a flapping node from growing an unbounded queue, not to ration an ordinary one.
const DefaultEventQueue = 256

// eventQueue is what this agent has observed and not yet reported (§12.1).
//
// # Why a queue and not a field on the status snapshot
//
// `status` is a full snapshot compared before sending (§6), and an event folded into one is
// dropped when it repeats and re-sent forever when it does not. Events are a stream; they need a
// stream's semantics, which is what [api.EventBatch] and its own endpoint are for (§9.2).
//
// # Why losing entries is acceptable and must still be announced
//
// The agent holds no persistent state (§6.1), so a restart loses whatever is pending, and a full
// queue drops its oldest. Both are accepted — this is a diagnostic aid, not an audit log — and
// both are counted, so the loss lands as an [api.EventsDropped] marker on the node's log rather
// than as a silent gap. A gap in this log is always visible in this log.
type eventQueue struct {
	limit int

	mu      sync.Mutex
	seq     uint64
	pending []api.AgentEvent
	dropped uint64
}

func newEventQueue(limit int) *eventQueue {
	if limit <= 0 {
		limit = DefaultEventQueue
	}
	return &eventQueue{limit: limit}
}

// push adds one entry, numbering it for the server's de-duplication.
//
// The sequence is per agent *instance* and starts at one: a restarted agent counts from the
// beginning again, which is why the server keys its cursor on the instance rather than the node
// (§12.1).
//
// **The oldest is dropped, not the newest.** A full queue on a failing node is holding the entries
// that describe the failure starting, and the newest ones are more of the same — coalescing on the
// server will fold them anyway. Dropping the head would keep the tail of a crash loop and lose the
// thing that began it.
func (q *eventQueue) push(event api.AgentEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.seq++
	event.Seq = q.seq
	q.pending = append(q.pending, event)

	if over := len(q.pending) - q.limit; over > 0 {
		q.pending = append([]api.AgentEvent(nil), q.pending[over:]...)
		q.dropped += uint64(over)
	}
}

// take removes everything pending, for one report.
//
// The caller must call [eventQueue.restore] if the report fails: delivery is at-least-once and the
// server de-duplicates (§12.1), so re-sending is free and losing a batch to a transport error is
// not.
func (q *eventQueue) take() ([]api.AgentEvent, uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()

	batch, dropped := q.pending, q.dropped
	q.pending, q.dropped = nil, 0
	return batch, dropped
}

// restore puts a failed batch back at the front, ahead of anything queued since.
//
// Order matters here for the same reason the sequence numbers do: the server records what it is
// given in the order it is given, and a batch that jumped ahead of its own predecessors would make
// a ring read backwards.
func (q *eventQueue) restore(batch []api.AgentEvent, dropped uint64) {
	if len(batch) == 0 && dropped == 0 {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.pending = append(batch, q.pending...)
	q.dropped += dropped

	if over := len(q.pending) - q.limit; over > 0 {
		q.pending = append([]api.AgentEvent(nil), q.pending[over:]...)
		q.dropped += uint64(over)
	}
}

func (q *eventQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// emit queues one entry about a path, and wakes the report loop.
//
// Not immediately reported: a worker dying produces a status change as well, and the report loop
// is already woken for that (§6). Sending separately would double the round trips on exactly the
// path that is already busy.
func (a *Agent) emit(event api.AgentEvent) {
	a.events.push(event)
	a.Notify()
}

// wasRejected returns the reason this assignment was refused on the previous pass, if it was.
//
// It exists so a rejection is recorded when it *changes* rather than on every reconcile. An
// assignment this node cannot honour is refused again on every pass for as long as it stands, and
// an entry per pass would fill the ring with one fact repeated — coalescing would fold them, but
// only after they had been sent, which is the cost this avoids.
func (a *Agent) wasRejected(key unitKey) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rejected[key]
}
