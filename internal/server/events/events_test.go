package events

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/store"
	"github.com/jonasohland/mxl-replicator/internal/store/sqlite"
)

func testRecorder(t *testing.T, opts Options) (*Recorder, store.Store) {
	t.Helper()

	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "store.db"), sqlite.Options{
		PollInterval: 20 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, s.Close()) })

	opts.Store = s
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) }
	}
	return New(opts), s
}

func exited(message string) api.Event {
	return api.Event{
		Kind: api.EventWorkerExited, Severity: api.SeverityError,
		ReasonCode: api.ReasonWorkerRestarts, Session: "s-1", Role: api.RoleTarget,
		Message: message,
	}
}

// A crash loop must reach the store as one entry with a count. Coalescing is not cosmetic here:
// it is what keeps a flapping worker from evicting the establishment history that explains it.
func TestACrashLoopIsOneEntry(t *testing.T) {
	t.Parallel()

	r, _ := testRecorder(t, Options{})
	ctx := t.Context()

	for range 47 {
		require.NoError(t, r.RecordPath(ctx, "p-1", exited("worker exited unexpectedly")))
	}

	ring, err := r.Read(ctx, store.PathEventsKey("p-1"))
	require.NoError(t, err)
	require.Len(t, ring.Events, 1)
	assert.Equal(t, 47, ring.Events[0].Count)
	assert.Zero(t, ring.Dropped)
}

// A path's log dies with the path, and so does its tail (§12.1, §12.2).
func TestForgetPathDropsTheRingAndTheTail(t *testing.T) {
	t.Parallel()

	r, _ := testRecorder(t, Options{})
	ctx := t.Context()

	require.NoError(t, r.RecordPath(ctx, "p-1", exited("gone")))
	require.NoError(t, r.PutTail(ctx, api.LogTail{Path: "p-1", Node: "edge-01", Text: "fatal: nope\n"}))

	require.NoError(t, r.ForgetPath(ctx, "p-1"))

	ring, err := r.Read(ctx, store.PathEventsKey("p-1"))
	require.NoError(t, err)
	assert.Empty(t, ring.Events)

	_, found, err := r.Tail(ctx, "p-1")
	require.NoError(t, err)
	assert.False(t, found)
}

// Delivery from an agent is at-least-once (§9.2), so a batch whose response was lost arrives
// again. Without de-duplication that lands as a second copy of everything the agent has just
// reported — or, worse, silently inflates the count of an entry that coalesces.
func TestARedeliveredBatchIsRecordedOnce(t *testing.T) {
	t.Parallel()

	r, _ := testRecorder(t, Options{})
	ctx := t.Context()

	batch := []api.AgentEvent{
		{Seq: 1, Path: "p-1", Kind: api.EventWorkerExited, Severity: api.SeverityError, Message: "died"},
		{Seq: 2, Kind: api.EventNodeRegistered, Severity: api.SeverityInfo, Message: "instance inst-a registered"},
	}

	require.NoError(t, r.Accept(ctx, "edge-01", "inst-a", batch, 0))
	require.NoError(t, r.Accept(ctx, "edge-01", "inst-a", batch, 0))

	pathRing, err := r.Read(ctx, store.PathEventsKey("p-1"))
	require.NoError(t, err)
	assert.Len(t, pathRing.Events, 1)
	assert.Zero(t, pathRing.Events[0].Count, "a redelivery must not read as a second occurrence")

	nodeRing, err := r.Read(ctx, store.NodeEventsKey("edge-01"))
	require.NoError(t, err)
	assert.Len(t, nodeRing.Events, 1)
}

// A restarted agent counts from one again, so a cursor carried across instances would silently
// discard everything the new instance reports — which is exactly the moment (§6.1's restart)
// that an operator is trying to understand.
func TestANewInstanceIsNotFilteredByTheOldCursor(t *testing.T) {
	t.Parallel()

	r, _ := testRecorder(t, Options{})
	ctx := t.Context()

	old := []api.AgentEvent{{Seq: 9, Kind: api.EventNodeRegistered, Severity: api.SeverityInfo, Message: "first"}}
	require.NoError(t, r.Accept(ctx, "edge-01", "inst-a", old, 0))

	fresh := []api.AgentEvent{{Seq: 1, Kind: api.EventNodeRegistered, Severity: api.SeverityInfo, Message: "second"}}
	require.NoError(t, r.Accept(ctx, "edge-01", "inst-b", fresh, 0))

	ring, err := r.Read(ctx, store.NodeEventsKey("edge-01"))
	require.NoError(t, err)
	require.Len(t, ring.Events, 1)

	// One entry rather than two, and that is coalescing rather than the cursor: repeated
	// registrations are the same fact happening again, so they fold into a count with the latest
	// message — which is what "this agent has restarted twice" should look like. What the cursor
	// must not do is discard the second one, and the count is what proves it did not.
	assert.Equal(t, 2, ring.Events[0].Count)
	assert.Equal(t, "second", ring.Events[0].Message)
}

// An agent that lost entries to a full queue says so, and it lands as a marker rather than as a
// silent gap: a gap in this log is always visible in this log (§12.1).
func TestADroppedBatchIsAnnounced(t *testing.T) {
	t.Parallel()

	r, _ := testRecorder(t, Options{})
	ctx := t.Context()

	require.NoError(t, r.Accept(ctx, "edge-01", "inst-a", nil, 12))

	ring, err := r.Read(ctx, store.NodeEventsKey("edge-01"))
	require.NoError(t, err)
	require.Len(t, ring.Events, 1)
	assert.Equal(t, api.EventsDropped, ring.Events[0].Kind)
	assert.Contains(t, ring.Events[0].Message, "12")
}

// A tail carried on an agent event reaches its own key, and the entry carries only the marker —
// so that a UI polling the ring does not carry a few KiB per failure (§12.2).
func TestALogTailIsStoredApartFromTheEntry(t *testing.T) {
	t.Parallel()

	r, _ := testRecorder(t, Options{})
	ctx := t.Context()

	require.NoError(t, r.Accept(ctx, "edge-01", "inst-a", []api.AgentEvent{{
		Seq: 1, Path: "p-1", Kind: api.EventWorkerExited, Severity: api.SeverityError,
		Message: "worker exited", Session: "s-1", Role: api.RoleTarget,
		Log: "[12:47:22.102] [error] fatal: unknown error: failed to create flow writer\n",
	}}, 0))

	ring, err := r.Read(ctx, store.PathEventsKey("p-1"))
	require.NoError(t, err)
	require.Len(t, ring.Events, 1)
	assert.True(t, ring.Events[0].HasLog)
	assert.NotContains(t, ring.Events[0].Message, "fatal:", "the tail must not be inlined")

	tail, found, err := r.Tail(ctx, "p-1")
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, tail.Text, "failed to create flow writer")
	assert.Equal(t, "edge-01", tail.Node)
}

// The server caps what it stores independently of what an agent chose to capture, because an
// endpoint that accepts unbounded bytes from a node is a store-filling primitive handed to every
// member of the fleet (§12.2). Truncation takes the head, so the fatal line survives.
func TestAnOversizedTailIsTruncatedAtTheHead(t *testing.T) {
	t.Parallel()

	r, _ := testRecorder(t, Options{TailBytes: 128})
	ctx := t.Context()

	text := strings.Repeat("setup chatter that nobody needs\n", 40) +
		"fatal: unknown error: failed to create flow writer\n"
	require.NoError(t, r.PutTail(ctx, api.LogTail{Path: "p-1", Node: "edge-01", Text: text}))

	tail, found, err := r.Tail(ctx, "p-1")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, tail.Truncated)
	assert.LessOrEqual(t, len(tail.Text), 128)
	assert.Contains(t, tail.Text, "failed to create flow writer")
}

// The merged view is how a request is read: its own entries plus those of the paths it currently
// expands onto (§12.1).
func TestMergeOrdersByTime(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	r, _ := testRecorder(t, Options{})
	ctx := t.Context()

	require.NoError(t, r.RecordPath(ctx, "p-2", api.Event{
		Kind: api.EventPathState, At: base.Add(2 * time.Second), Message: "second",
	}))
	require.NoError(t, r.RecordPath(ctx, "p-1", api.Event{
		Kind: api.EventPathState, At: base, Message: "first",
	}))

	merged, err := r.Merge(ctx, store.PathEventsKey("p-1"), store.PathEventsKey("p-2"))
	require.NoError(t, err)
	require.Len(t, merged.Events, 2)
	assert.Equal(t, "first", merged.Events[0].Message)
	assert.Equal(t, "second", merged.Events[1].Message)
}

// Reading a ring that has never been written is an empty log, not an error: an object nothing has
// happened to yet is the ordinary case, not a 404.
func TestAnUnwrittenRingReadsEmpty(t *testing.T) {
	t.Parallel()

	r, _ := testRecorder(t, Options{})

	list, err := r.List(context.Background(), store.PathEventsKey("nothing-here"), 0)
	require.NoError(t, err)
	assert.Empty(t, list.Events)
	assert.Zero(t, list.Next)
}
