package agent

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// find returns the first reported entry of a kind.
func find(events []api.AgentEvent, kind api.EventKind) (api.AgentEvent, bool) {
	for _, event := range events {
		if event.Kind == kind {
			return event, true
		}
	}
	return api.AgentEvent{}, false
}

func count(events []api.AgentEvent, kind api.EventKind) int {
	n := 0
	for _, event := range events {
		if event.Kind == kind {
			n++
		}
	}
	return n
}

// The point of §12.2 in one test: the sentence that explains a failure reaches the server with
// the failure, so an operator with no shell access to the node can read it.
func TestAFailingWorkerPushesItsLogTail(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.server.assign("edge-01", targetAssignment("s1"))
	h.run()

	h.eventually("the target", func() bool {
		return h.launcher.Find("s1", api.RoleTarget) != nil
	})

	worker := h.launcher.Find("s1", api.RoleTarget)
	worker.SetLogTail("[12:47:22.101] [error] [flow.cpp:244] Failed to create flow : permission denied\n" +
		"[12:47:22.102] [error] fatal: unknown error: failed to create flow writer\n")
	worker.Die(errors.New("exited"))

	h.eventually("the exit to be reported with its log", func() bool {
		event, ok := find(h.server.reportedEvents(), api.EventWorkerExited)
		return ok && event.Log != ""
	})

	event, ok := find(h.server.reportedEvents(), api.EventWorkerExited)
	require.True(t, ok)
	assert.Equal(t, "p-s1", event.Path, "entries are anchored on the path, not the session")
	assert.Equal(t, "s1", event.Session)
	assert.Equal(t, api.RoleTarget, event.Role)
	assert.Equal(t, api.SeverityError, event.Severity)
	assert.Contains(t, event.Log, "failed to create flow writer")
	assert.NotZero(t, event.Seq, "entries are numbered so the server can de-duplicate a retry")
}

// **One tail per crash loop, not one per restart** (§12.2). Forty-seven copies of one fatal line
// is not evidence, it is volume — and it is a few KiB of it, per restart, per failing path.
func TestACrashLoopPushesOneTail(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.server.assign("edge-01", targetAssignment("s1"))
	h.run()

	for attempt := 1; attempt <= 3; attempt++ {
		h.eventually("a worker to kill", func() bool {
			return h.launcher.Find("s1", api.RoleTarget) != nil
		})
		worker := h.launcher.Find("s1", api.RoleTarget)
		worker.SetLogTail("fatal: unknown error: failed to create flow writer\n")
		worker.Die(errors.New("exited"))

		h.eventually("the death to be reported", func() bool {
			return count(h.server.reportedEvents(), api.EventWorkerExited) >= attempt
		})
	}

	events := h.server.reportedEvents()
	assert.GreaterOrEqual(t, count(events, api.EventWorkerExited), 3)

	withLogs := 0
	for _, event := range events {
		if event.Log != "" {
			withLogs++
		}
	}
	assert.Equal(t, 1, withLogs, "a crash loop must push its log once, not once per restart")
}

// A queue that has overflowed says so, so that a gap in this log is visible in this log (§12.1).
func TestAnOverflowingQueueDropsItsOldestAndCountsIt(t *testing.T) {
	t.Parallel()

	q := newEventQueue(3)
	for i := range 10 {
		q.push(api.AgentEvent{Kind: api.EventWorkerExited, Message: string(rune('a' + i))})
	}

	batch, dropped := q.take()
	require.Len(t, batch, 3)
	assert.Equal(t, uint64(7), dropped)
	assert.Equal(t, "h", batch[0].Message, "the oldest go, so what survives is the newest")
	assert.Equal(t, uint64(8), batch[0].Seq, "numbering does not restart when entries are dropped")
}

// Delivery is at-least-once and the server de-duplicates, so a failed report must put its batch
// back rather than drop it — losing a batch to a transport error loses the entries that explain
// whatever is going wrong.
func TestAFailedReportKeepsItsBatch(t *testing.T) {
	t.Parallel()

	q := newEventQueue(16)
	q.push(api.AgentEvent{Kind: api.EventWorkerExited, Message: "first"})
	q.push(api.AgentEvent{Kind: api.EventWorkerExited, Message: "second"})

	batch, dropped := q.take()
	require.Len(t, batch, 2)
	assert.Zero(t, q.len())

	q.restore(batch, dropped)
	q.push(api.AgentEvent{Kind: api.EventWorkerExited, Message: "third"})

	again, _ := q.take()
	require.Len(t, again, 3)
	assert.Equal(t, "first", again[0].Message, "a restored batch goes back in front of what came after it")
	assert.Equal(t, "third", again[2].Message)
}

// An assignment this node cannot honour reaches the path's own log, not only the status.
//
// The status carries the reason for as long as the assignment stands; the entry survives it being
// withdrawn, which is exactly the case where the reason would otherwise vanish before anyone read
// it (§6).
func TestAnUnhonourableAssignmentIsRecordedOnItsPath(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	bad := targetAssignment("s1")
	bad.Domain = api.Domain{Area: "nowhere", Elements: []string{"ingest"}}
	h.server.assign("edge-01", bad)
	h.run()

	h.eventually("the rejection to be reported", func() bool {
		_, ok := find(h.server.reportedEvents(), api.EventAssignmentRejected)
		return ok
	})

	event, _ := find(h.server.reportedEvents(), api.EventAssignmentRejected)
	assert.Equal(t, "p-s1", event.Path)
	assert.Equal(t, api.SeverityError, event.Severity)
	assert.Contains(t, event.Message, "nowhere")
}

// A healthy fleet produces no entries at all, which is what keeps the event log from becoming
// this design's loudest writer (§8.3, §12.1).
func TestAHealthyWorkerReportsNoEvents(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.server.assign("edge-01", targetAssignment("s1"))
	h.run()

	h.eventually("the target to come up", func() bool {
		status, ok := h.server.lastStatus("s1", api.RoleTarget)
		return ok && status.State == api.WorkerReady
	})

	assert.Empty(t, h.server.reportedEvents(),
		"a worker that came up without waiting or failing has nothing to say")
}
