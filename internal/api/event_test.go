package api

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func at(second int) time.Time {
	return time.Date(2026, 9, 2, 12, 0, second, 0, time.UTC)
}

func exited(second int, message string) Event {
	return Event{
		Kind:       EventWorkerExited,
		Severity:   SeverityError,
		At:         at(second),
		Message:    message,
		ReasonCode: ReasonWorkerRestarts,
		Session:    "s-1",
		Role:       RoleTarget,
	}
}

// distinct builds entries that must not coalesce with one another.
func distinct(i int) Event {
	return Event{Kind: EventPathState, At: at(i), Session: "s-" + string(rune('0'+i))}
}

// A crash loop must read as *one* thing that is happening repeatedly, not as fifty things.
//
// This is the property the count bound depends on (§12.1): without coalescing, a flapping worker
// fills the ring and evicts the establishment history that explains it, so the log is emptiest
// exactly when it is most needed.
func TestRepeatedEntriesCoalesce(t *testing.T) {
	t.Parallel()

	var ring EventRing
	for i := range 47 {
		ring.Append(exited(i, "worker exited unexpectedly"), DefaultEventRingSize)
	}

	require.Len(t, ring.Events, 1)
	assert.Equal(t, 47, ring.Events[0].Count)
	assert.Equal(t, at(0), ring.Events[0].FirstAt)
	assert.Equal(t, at(46), ring.Events[0].At)
	assert.Zero(t, ring.Dropped, "coalescing must not evict anything")
}

// The merged entry keeps the *latest* message while coalescing on everything else. "exited after
// 1.2s" and "exited after 0.9s" are the same event twice, and a message-sensitive comparison
// would defeat coalescing on exactly the flapping it exists for.
func TestCoalescingIgnoresTheMessageAndKeepsTheNewest(t *testing.T) {
	t.Parallel()

	var ring EventRing
	ring.Append(exited(1, "worker exited unexpectedly after 1.2s"), DefaultEventRingSize)
	ring.Append(exited(2, "worker exited unexpectedly after 0.9s"), DefaultEventRingSize)

	require.Len(t, ring.Events, 1)
	assert.Equal(t, "worker exited unexpectedly after 0.9s", ring.Events[0].Message)
	assert.Equal(t, 2, ring.Events[0].Count)
}

// Entries that differ in anything a reader would act on stay separate — otherwise a worker that
// starts failing for a *new* reason silently increments the old row's count.
func TestDifferentReasonsDoNotCoalesce(t *testing.T) {
	t.Parallel()

	var ring EventRing
	ring.Append(exited(1, "exited"), DefaultEventRingSize)

	other := exited(2, "exited")
	other.ReasonCode = ReasonFabricGone
	ring.Append(other, DefaultEventRingSize)

	assert.Len(t, ring.Events, 2)
}

// A path recovering and failing again is three entries, not one.
//
// This is why [Event.State] is a field rather than something buried in the message: every other
// term in the coalescing key is identical across the three, so without it the ring folds a
// flapping path into one row that says it changed state three times and never says to what.
func TestAStateTransitionCoalescesOnItsState(t *testing.T) {
	t.Parallel()

	var ring EventRing
	for i, state := range []State{StateActive, StateFailed, StateActive} {
		ring.Append(Event{
			Kind: EventPathState, Severity: SeverityInfo, At: at(i), State: state,
			Message: "path is " + string(state),
		}, DefaultEventRingSize)
	}

	require.Len(t, ring.Events, 3)
	assert.Equal(t, StateActive, ring.Events[2].State)
}

// A coalesced entry takes a *new* sequence number even though it stays in place. A poller
// resuming from the old one has to be handed the updated count rather than told nothing changed.
func TestCoalescingAdvancesTheCursor(t *testing.T) {
	t.Parallel()

	var ring EventRing
	ring.Append(exited(1, "exited"), DefaultEventRingSize)
	first := ring.Events[0].Seq

	ring.Append(exited(2, "exited"), DefaultEventRingSize)

	assert.Greater(t, ring.Events[0].Seq, first)
	assert.Len(t, ring.Since(first), 1, "a poller at the old cursor must see the update")
}

// The ring is bounded by count, drops its oldest, and says so — a gap in this log is visible in
// this log (§12.1).
func TestRingIsBoundedAndReportsWhatItDropped(t *testing.T) {
	t.Parallel()

	var ring EventRing
	for i := range 10 {
		ring.Append(distinct(i), 4)
	}

	require.Len(t, ring.Events, 4)
	assert.Equal(t, uint64(6), ring.Dropped)
	assert.Equal(t, "s-6", ring.Events[0].Session, "the oldest survivor is the seventh entry")
}

// Sequence numbers keep rising as entries age out. If they restarted, a poller resuming from an
// old cursor would be handed numbers it had already seen and would skip whatever came after.
func TestSequenceNumbersSurviveEviction(t *testing.T) {
	t.Parallel()

	var ring EventRing
	for i := range 10 {
		ring.Append(distinct(i), 3)
	}

	assert.Equal(t, uint64(11), ring.Next)
	assert.Equal(t, uint64(8), ring.Events[0].Seq)
	assert.Empty(t, ring.Since(10))
}

// A cursor of zero means "everything", so no entry may carry a sequence of zero — otherwise the
// first entry of every ring is filtered out of the first read of it.
func TestSequenceNumbersStartAtOne(t *testing.T) {
	t.Parallel()

	var ring EventRing
	ring.Append(exited(1, "exited"), DefaultEventRingSize)

	assert.Equal(t, uint64(1), ring.Events[0].Seq)
	assert.Len(t, ring.Since(0), 1, "a fresh reader must see the first entry")
}

// The tail keeps the *end* of a worker's output, because a fatal line is always the last one.
func TestTailBytesKeepsTheEnd(t *testing.T) {
	t.Parallel()

	text := "setup line one\nsetup line two\nfatal: could not create flow writer\n"
	cut, truncated := TailBytes(text, 40)

	assert.True(t, truncated)
	assert.Contains(t, cut, "fatal: could not create flow writer")
	assert.NotContains(t, cut, "setup line one")
}

// Truncation cuts at a line boundary. A tail beginning with the back half of a line reads as
// corruption to somebody who does not know the buffer is bounded.
func TestTailBytesCutsOnALineBoundary(t *testing.T) {
	t.Parallel()

	text := "aaaaaaaaaaaaaaaaaaaa\nbbbbbbbbbb\ncccccccccc\n"
	cut, truncated := TailBytes(text, 25)

	require.True(t, truncated)
	assert.False(t, strings.HasPrefix(cut, "a"), "cut mid-line: %q", cut)
	assert.Equal(t, "bbbbbbbbbb\ncccccccccc\n", cut)
}

func TestTailBytesLeavesShortOutputAlone(t *testing.T) {
	t.Parallel()

	cut, truncated := TailBytes("short\n", 4096)
	assert.False(t, truncated)
	assert.Equal(t, "short\n", cut)
}

// Four flows appearing in four consecutive reconcile passes must be four entries.
//
// **Found in a live fleet, not predicted here.** Coalescing ignores the message by design, so these
// folded into `1 flow appeared: …b8d6c502 ×3` — one of the four named and three silently discarded.
// An entry that names what it is about is a different fact every time it is written.
func TestEntriesThatNameThingsDoNotCoalesce(t *testing.T) {
	t.Parallel()

	var ring EventRing
	for i, flow := range []string{"f-1", "f-2", "f-3", "f-4"} {
		ring.Append(Event{
			Kind: EventFlowAppeared, Severity: SeverityInfo, At: at(i), Node: "node1",
			Message: "1 flow appeared: media/mxl0/" + flow,
		}, DefaultEventRingSize)
	}

	require.Len(t, ring.Events, 4)
	for i, flow := range []string{"f-1", "f-2", "f-3", "f-4"} {
		assert.Contains(t, ring.Events[i].Message, flow)
	}
}

// The rule is on the kind, so a reader can ask rather than infer, and whoever adds the next
// identity-bearing kind has one place to put it.
func TestIdentityBearingKindsAreMarked(t *testing.T) {
	t.Parallel()

	for _, kind := range []EventKind{
		EventFlowAppeared, EventFlowDisappeared, EventDomainAppeared, EventDomainDisappeared,
	} {
		assert.False(t, kind.Coalesces(), "%s names what it is about and must not fold", kind)
	}
	for _, kind := range []EventKind{EventWorkerExited, EventPathState, EventEpochChanged} {
		assert.True(t, kind.Coalesces(), "%s is the same fact repeating and should fold", kind)
	}
}
