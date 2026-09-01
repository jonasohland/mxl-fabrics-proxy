package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rate control off is the sentinel a rate of zero carries, and it has to be the *permissive*
// reading: a zero that admitted nothing would be a configuration typo that silently stops every
// flow on the node (§6.3).
func TestARateOfZeroAdmitsEverything(t *testing.T) {
	gate := newStartGate(0, 0)

	waited := false
	for range 100 {
		require.True(t, gate.admit(t.Context(), func() { waited = true }))
	}

	assert.False(t, waited, "nothing should have queued")
	_, delayed, _ := gate.stats()
	assert.Zero(t, delayed)
}

// The burst is the operationally meaningful number: it is how many workers may go into setup at
// the same instant, which is the quantity the exhaustion this exists for is measured against
// (§6.3).
func TestTheBurstIsWhatMayStartAtOnce(t *testing.T) {
	// 50/s, so the first start past the burst waits ~20 ms — long enough to measure, short enough
	// not to slow the suite down.
	gate := newStartGate(50, 2)

	started := time.Now()
	require.True(t, gate.admit(t.Context(), nil))
	require.True(t, gate.admit(t.Context(), nil))
	assert.Less(t, time.Since(started), 10*time.Millisecond, "the burst must not be paced")

	queued := 0
	started = time.Now()
	require.True(t, gate.admit(t.Context(), func() { queued++ }))
	assert.GreaterOrEqual(t, time.Since(started), 10*time.Millisecond)
	assert.Equal(t, 1, queued, "onWait is called on a start that waits, and only on one")

	waiting, delayed, waited := gate.stats()
	assert.Zero(t, waiting, "nothing is queued once every admit has returned")
	assert.EqualValues(t, 1, delayed)
	assert.Positive(t, waited)
}

// A worker withdrawn while it is queued must leave, promptly, without its permit. Reconcile stops
// workers synchronously (§6), so a wait that ignored cancellation would hold up every other stop
// on the node behind a permit nobody is going to use.
func TestAQueuedStartIsCancellable(t *testing.T) {
	// 5/s, so a permit is 200 ms away: long enough that a cancellation lands while the wait is
	// genuinely blocked, short enough to measure what happens to the permit afterwards.
	gate := newStartGate(5, 1)
	require.True(t, gate.admit(t.Context(), nil))

	ctx, cancel := context.WithCancel(t.Context())
	admitted := make(chan bool, 1)
	go func() { admitted <- gate.admit(ctx, nil) }()

	waiting := func() int {
		count, _, _ := gate.stats()
		return count
	}
	require.Eventually(t, func() bool { return waiting() == 1 }, time.Second, time.Millisecond)

	cancel()
	cancelled := time.Now()
	select {
	case ok := <-admitted:
		assert.False(t, ok, "a cancelled wait is not an admission")
	case <-time.After(2 * time.Second):
		t.Fatal("a queued start ignored its cancelled context")
	}

	assert.Zero(t, waiting())

	// And it left its reservation behind rather than taking it: the next start waits out the one
	// permit that was pending, not two. A worker withdrawn while queued must not spend a permit on
	// its way out, or a fleet-wide withdrawal would pace the re-establishment that follows it.
	assert.True(t, gate.admit(t.Context(), nil))
	assert.Less(t, time.Since(cancelled), 300*time.Millisecond,
		"the cancelled wait consumed a permit nothing used")
}

// A burst below one would admit nothing, ever. It is refused at parse time; this is the second
// guard, because a node that comes up healthy with every session stuck in `starting` is a bad way
// to find out (§6.3).
func TestABurstBelowOneIsClamped(t *testing.T) {
	gate := newStartGate(100, 0)

	started := time.Now()
	assert.True(t, gate.admit(t.Context(), nil))
	assert.Less(t, time.Since(started), time.Second)
}
