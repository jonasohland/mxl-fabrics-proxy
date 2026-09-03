package logs

import (
	"strings"
	"sync"
)

// DefaultRingBytes is how much of a worker's output a [Ring] keeps by default (§12.2).
const DefaultRingBytes = 8 << 10

// Ring keeps the tail of one worker start's output, so that the line explaining a failure can be
// pushed to the server with the failure (§12.2).
//
// # Bytes, not lines
//
// A flow definition inside an error message is a line the size of a flow definition (§15), and a
// line budget would let one of them evict a whole start's history. The scanner feeding this is
// already sized for that case.
//
// # The tail, not the head
//
// A worker's fatal line is its last, in both failure shapes — one that never comes up, and one
// that dies after hours of healthy transfer. The cost is real and is accepted: a run with
// `FI_LOG_LEVEL=debug` puts libfabric's own diagnostics through the same stream and can push the
// setup lines out of the window. Those lines are reproducible; a fatal is not.
//
// # Everything, not only what parses
//
// The ring takes raw lines, before [Parse]. A tail holding only what the parser understood would
// omit whatever a library on the link line printed on its way out, which is precisely the case
// where the parser is least likely to be right and the output most likely to matter.
type Ring struct {
	limit int

	mu    sync.Mutex
	lines []string
	bytes int
}

// NewRing builds a ring holding at most limit bytes. Zero or negative takes [DefaultRingBytes].
func NewRing(limit int) *Ring {
	if limit <= 0 {
		limit = DefaultRingBytes
	}
	return &Ring{limit: limit}
}

// Add appends one line, evicting from the front until the ring is within its bound.
//
// A single line longer than the whole bound is kept rather than dropped: it is almost certainly
// the flow definition in an error message, and half of that diagnostic is worth more than none of
// it. [Ring.Text] is what the caller truncates.
func (r *Ring) Add(line string) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.lines = append(r.lines, line)
	r.bytes += len(line) + 1

	for r.bytes > r.limit && len(r.lines) > 1 {
		r.bytes -= len(r.lines[0]) + 1
		r.lines = r.lines[1:]
	}
}

// Text returns the retained output, oldest line first.
func (r *Ring) Text() string {
	if r == nil {
		return ""
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.lines) == 0 {
		return ""
	}
	return strings.Join(r.lines, "\n") + "\n"
}
