package logs

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tail keeps the end of a worker's output, because a fatal line is always its last — in both
// failure shapes, one that never comes up and one that dies after hours (§12.2).
func TestTheRingKeepsTheEnd(t *testing.T) {
	t.Parallel()

	r := NewRing(64)
	for range 20 {
		r.Add("setup chatter nobody needs")
	}
	r.Add("fatal: unknown error: failed to create flow writer")

	text := r.Text()
	assert.Contains(t, text, "failed to create flow writer")
	assert.LessOrEqual(t, len(text), 64+len("fatal: unknown error: failed to create flow writer")+1)
}

// Bounded in **bytes and not in lines** (§12.2), because a flow definition inside an error message
// is a line the size of a flow definition and a line budget would let one of them evict a whole
// start's history.
func TestTheRingIsBoundedInBytes(t *testing.T) {
	t.Parallel()

	r := NewRing(100)
	for range 500 {
		r.Add("short")
	}

	assert.LessOrEqual(t, len(r.Text()), 110)
	assert.Greater(t, strings.Count(r.Text(), "\n"), 1, "a byte bound still keeps several short lines")
}

// A single line longer than the whole bound is kept rather than dropped. It is almost certainly the
// flow definition in an error message, and half of that diagnostic is worth more than none of it —
// the caller truncates on the way out.
func TestALineLongerThanTheBoundSurvives(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("x", 4096)
	r := NewRing(64)
	r.Add("preamble")
	r.Add(huge)

	require.Contains(t, r.Text(), huge)
}

// A ring nobody wrote to renders as nothing, not as a blank line — an empty tail must be
// distinguishable from a worker that printed one empty line.
func TestAnEmptyRingRendersNothing(t *testing.T) {
	t.Parallel()

	assert.Empty(t, NewRing(64).Text())
}

// A nil ring is usable, so a launcher that was not configured with one does not need a branch at
// every call site.
func TestANilRingIsSafe(t *testing.T) {
	t.Parallel()

	var r *Ring
	assert.NotPanics(t, func() { r.Add("line") })
	assert.Empty(t, r.Text())
}
