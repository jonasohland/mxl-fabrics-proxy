package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// EventsCmd shows what happened to a path, a request or a node (§12.1).
//
// A verb of its own rather than a flag on `describe`, and the reason is the same one that keeps
// `get` and `describe` apart: `describe` answers *what is this* and this answers *what happened to
// it*. They are different questions with different shapes — one is a record, the other a list that
// grows — and an operator asks the second one repeatedly while the first is stable.
//
// The two are still joined at the point of use: `describe path` prints the last few entries under
// the status, because the whole reason this exists is that a state and a reason do not explain a
// failure on their own.
type EventsCmd struct {
	Kind string `arg:"" enum:"path,request,node" help:"One of: path, request, node."`
	Name string `arg:"" help:"The path id, request name or node name."`

	// Namespace scopes a request name, because a request's ID is the (namespace, name) pair
	// (§9.3).
	Namespace string `short:"n" help:"Namespace the request is in. Applies to requests; defaults to 'default'."`

	// Since resumes a poll. A sequence number rather than a timestamp: an entry is stamped by
	// whoever emitted it, so a merged view interleaves several clocks and the sequence is the only
	// thing that orders a ring (§12.1).
	Since uint64 `help:"Show only entries after this sequence number."`

	ClientFlags `embed:""`
	OutputFlags `embed:""`
}

func (c *EventsCmd) Run(ctx context.Context) error {
	user, err := c.client()
	if err != nil {
		return err
	}

	var list *api.EventList
	switch c.Kind {
	case "path":
		list, err = user.PathEvents(ctx, c.Name, c.Since)
	case "request":
		list, err = user.RequestEvents(ctx, requestID(c.Namespace, c.Name), c.Since)
	case "node":
		list, err = user.NodeEvents(ctx, c.Name, c.Since)
	default:
		return fmt.Errorf("unknown kind %q", c.Kind)
	}
	if err != nil {
		return err
	}

	return renderAs(c.Output, list, func() {
		out := table()
		defer flush(out)
		printEvents(out, list, 0)
	})
}

// printEvents renders a log. limit caps how many of the most recent entries are shown; zero shows
// everything.
//
// **The newest last**, so a reader's eye ends on what is happening now — the same order a log file
// has, and the opposite of what a "most recent first" list would give. A truncated view says how
// much it left out rather than trailing off, because a log that silently shows the last five reads
// as a log with five entries in it.
func printEvents(out *tabwriter.Writer, list *api.EventList, limit int) {
	if len(list.Events) == 0 {
		fmt.Fprintln(out, "  no events recorded")
		return
	}

	events := list.Events
	if limit > 0 && len(events) > limit {
		fmt.Fprintf(out, "  … %d earlier\n", len(events)-limit)
		events = events[len(events)-limit:]
	}

	// Entries lost to a full ring, which is a different thing from entries an agent never managed
	// to report — that one arrives as an `events_dropped` entry of its own. Both are announced, on
	// the principle that a gap in this log is visible in this log (§12.1).
	if list.Dropped > 0 {
		fmt.Fprintf(out, "  (%d older entries have aged out of this ring)\n", list.Dropped)
	}

	for _, event := range events {
		fmt.Fprintf(out, "  %s\t%s\t%s\t%s\n",
			event.At.Local().Format("15:04:05"),
			event.Severity,
			eventSubject(event),
			eventDetail(event))
	}
}

// eventSubject is the short column: what kind of thing this is, in the vocabulary an operator
// reads rather than the wire's.
func eventSubject(event api.Event) string {
	if event.State != "" {
		return string(event.State)
	}
	return strings.ReplaceAll(string(event.Kind), "_", " ")
}

// eventDetail is the message plus the two things that belong beside it rather than inside it: how
// many times this happened, and whether a worker log came with it.
func eventDetail(event api.Event) string {
	detail := event.Message

	if event.Count > 1 {
		span := ""
		if !event.FirstAt.IsZero() {
			span = " over " + event.At.Sub(event.FirstAt).Round(time.Second).String()
		}
		detail += fmt.Sprintf("  ×%d%s", event.Count, span)
	}
	if event.HasLog {
		detail += "  [log]"
	}
	return detail
}

// LogsCmd fetches the last failing worker's output for a path (§12.2).
//
// Deliberately its own command and not part of `events`: the entry carries a marker that a tail
// exists and this is what goes and gets it, because inlining a few KiB per failure into a list an
// operator or a UI polls would make the cheap read expensive exactly when things are failing.
type LogsCmd struct {
	// Spelled out rather than defaulted, and it costs a word. kong makes a defaulted positional
	// optional, which forbids a required one after it — and more to the point, every other read
	// verb here takes a kind, so `logs <id>` would be the one that does not.
	Kind string `arg:"" enum:"path" help:"Only 'path' for now: a tail is attached to a failure, and failures belong to paths."`
	Name string `arg:"" help:"The path id."`

	ClientFlags `embed:""`
	OutputFlags `embed:""`
}

func (c *LogsCmd) Run(ctx context.Context) error {
	user, err := c.client()
	if err != nil {
		return err
	}

	tail, err := user.PathLogs(ctx, c.Name)
	if err != nil {
		return err
	}

	return renderAs(c.Output, tail, func() { printTail(tail) })
}

func printTail(tail *api.LogTail) {
	out := table()
	defer flush(out)

	fmt.Fprintf(out, "Worker log\t%s\n", tail.Path)
	fmt.Fprintf(out, "  node\t%s\n", tail.Node)
	if tail.Session != "" {
		fmt.Fprintf(out, "  session\t%s %s\n", tail.Session, tail.Role)
	}
	fmt.Fprintf(out, "  captured\t%s\n", tail.At.Local().Format(time.RFC3339))
	if tail.Truncated {
		// Said out loud, because the alternative is an operator reading a partial log as a
		// complete one. The head is what went: a worker's fatal line is its last (§12.2).
		fmt.Fprintln(out, "  truncated\tearlier output was discarded to fit the bound")
	}
	flush(out)

	fmt.Println()
	fmt.Print(tail.Text)
	if !strings.HasSuffix(tail.Text, "\n") {
		fmt.Println()
	}
}

func requestID(namespace, name string) api.RequestID {
	if namespace == "" {
		namespace = api.DefaultNamespace
	}
	return api.RequestID{Namespace: namespace, Name: name}
}
