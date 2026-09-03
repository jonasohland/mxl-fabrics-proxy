package server

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/store"
)

// An agent's batch reaches the path's log, and the tail it carried reaches the endpoint that
// serves it — which is the whole of §12.2's promise: the sentence explaining a failure is readable
// from the control plane by someone with no shell access to the node.
func TestAnAgentBatchReachesThePathLog(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register("edge-01", "inst-a")

	body := api.EventBatch{
		Node:     "edge-01",
		Instance: "inst-a",
		Events: []api.AgentEvent{{
			Seq: 1, Path: "p-1", Kind: api.EventWorkerExited, Severity: api.SeverityError,
			Message: "worker exited unexpectedly after 812ms", Session: "s-1", Role: api.RoleTarget,
			Log: "[12:47:22.102] [error] fatal: unknown error: failed to create flow writer\n",
		}},
	}
	require.Equal(t, http.StatusNoContent, h.do(http.MethodPost, api.EventsPath("edge-01"), body).status)

	var list api.EventList
	got := h.do(http.MethodGet, api.PathEventsPath("p-1"), nil)
	require.Equal(t, http.StatusOK, got.status)
	got.decode(t, &list)

	require.Len(t, list.Events, 1)
	assert.Equal(t, api.EventWorkerExited, list.Events[0].Kind)
	assert.Equal(t, "edge-01", list.Events[0].Node)
	assert.True(t, list.Events[0].HasLog)
	assert.NotZero(t, list.Next, "the response carries the cursor to resume from")

	var tail api.LogTail
	logs := h.do(http.MethodGet, api.PathLogsPath("p-1"), nil)
	require.Equal(t, http.StatusOK, logs.status)
	logs.decode(t, &tail)
	assert.Contains(t, tail.Text, "failed to create flow writer")
}

// A path nothing has failed on has no tail, and that is a 404 rather than an empty body — the
// difference between "no worker has failed here" and "a worker failed and printed nothing".
func TestAPathWithNoFailureHasNoLog(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	got := h.do(http.MethodGet, api.PathLogsPath("p-nothing"), nil)

	assert.Equal(t, http.StatusNotFound, got.status)
	assert.Equal(t, api.CodeNotFound, got.apiError(t).Code)
}

// An object nothing has happened to reads as an empty log rather than an error: a path that has
// just been created is the ordinary case, not a missing resource.
func TestAnUnwrittenLogReadsEmpty(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	var list api.EventList
	got := h.do(http.MethodGet, api.PathEventsPath("p-nothing"), nil)
	require.Equal(t, http.StatusOK, got.status)
	got.decode(t, &list)
	assert.Empty(t, list.Events)
}

// The cursor is what a poller resumes from, and it is a sequence number rather than a timestamp
// (§12.1).
func TestTheCursorReturnsOnlyWhatIsNew(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register("edge-01", "inst-a")

	post := func(seq uint64, message string) {
		body := api.EventBatch{Node: "edge-01", Instance: "inst-a", Events: []api.AgentEvent{{
			Seq: seq, Path: "p-1", Kind: api.EventWorkerExited, Severity: api.SeverityError,
			// Distinct sessions, so the two do not coalesce into one entry — this test is about
			// the cursor, and coalescing is tested where it belongs.
			Message: message, Session: message,
		}}}
		require.Equal(t, http.StatusNoContent,
			h.do(http.MethodPost, api.EventsPath("edge-01"), body).status)
	}

	post(1, "first")
	var first api.EventList
	h.do(http.MethodGet, api.PathEventsPath("p-1"), nil).decode(t, &first)
	require.Len(t, first.Events, 1)

	post(2, "second")
	var next api.EventList
	h.do(http.MethodGet, api.PathEventsPath("p-1")+"?since="+strconv.FormatUint(first.Next, 10), nil).decode(t, &next)

	require.Len(t, next.Events, 1)
	assert.Equal(t, "second", next.Events[0].Message)
}

// A registration lands on the node's log at the moment it happens, which is the entry that answers
// "why did every path on this node re-establish at 12:04" (§12.1).
func TestRegistrationIsRecordedOnTheNodeLog(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register("edge-01", "inst-a")

	var list api.EventList
	got := h.do(http.MethodGet, api.NodeEventsPath("edge-01"), nil)
	require.Equal(t, http.StatusOK, got.status)
	got.decode(t, &list)

	require.Len(t, list.Events, 1)
	assert.Equal(t, api.EventNodeRegistered, list.Events[0].Kind)
	assert.Contains(t, list.Events[0].Message, "inst-a")
}

// The takeover marker explains a gap, so it must only appear on objects that could have had one.
//
// Merging the fleet ring unconditionally puts "transitions before this point were not recorded" at
// the top of every log — including objects created *after* the takeover, whose logs are in fact
// complete. Telling an operator to distrust a complete log is worse than saying nothing.
func TestTheTakeoverMarkerStaysOutOfLogsItCannotExplain(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register("edge-01", "inst-a")

	// A takeover recorded before this path has any history of its own.
	require.NoError(t, h.server.events.Record(t.Context(), store.KeyFleetEvents, api.Event{
		Kind: api.EventReconcilerTookOver, Severity: api.SeverityWarn,
		At:      time.Now().Add(-time.Hour),
		Message: "reconciler took over",
	}))

	body := api.EventBatch{Node: "edge-01", Instance: "inst-a", Events: []api.AgentEvent{{
		Seq: 1, Path: "p-1", Kind: api.EventWorkerExited, Severity: api.SeverityError,
		At: time.Now(), Message: "died",
	}}}
	require.Equal(t, http.StatusNoContent, h.do(http.MethodPost, api.EventsPath("edge-01"), body).status)

	var list api.EventList
	h.do(http.MethodGet, api.PathEventsPath("p-1"), nil).decode(t, &list)

	require.Len(t, list.Events, 1)
	assert.Equal(t, api.EventWorkerExited, list.Events[0].Kind)

	// A takeover *within* the object's lifetime is merged, because that one really did lose
	// transitions this path would otherwise have recorded.
	require.NoError(t, h.server.events.Record(t.Context(), store.KeyFleetEvents, api.Event{
		Kind: api.EventReconcilerTookOver, Severity: api.SeverityWarn,
		At:      time.Now().Add(time.Second),
		Message: "reconciler took over",
	}))

	var after api.EventList
	h.do(http.MethodGet, api.PathEventsPath("p-1"), nil).decode(t, &after)
	require.Len(t, after.Events, 2)
	assert.Equal(t, api.EventReconcilerTookOver, after.Events[1].Kind)
}

// **The event log must stay out of the fleet snapshot** (§4, §12.1).
//
// Every user-API read loads the whole snapshot and reconciles it, so a diagnostic log inside that
// key space would make every unrelated read carry it — and a watch on it would wake the reconciler
// with its own writes. The prefix split is what prevents both, and this is the test that the keys
// actually land where that argument assumes.
func TestEventKeysAreOutsideTheFleetSnapshot(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register("edge-01", "inst-a")

	kvs, _, err := h.store.List(t.Context(), store.PrefixSnapshot)
	require.NoError(t, err)
	require.NotEmpty(t, kvs, "the registration should be in the snapshot")

	for _, kv := range kvs {
		assert.NotContains(t, kv.Key, store.PrefixEvents,
			"an event key landed inside the fleet snapshot: %s", kv.Key)
	}

	events, _, err := h.store.List(t.Context(), store.PrefixEvents)
	require.NoError(t, err)
	assert.NotEmpty(t, events, "the registration event should exist, just not in the snapshot")
}
