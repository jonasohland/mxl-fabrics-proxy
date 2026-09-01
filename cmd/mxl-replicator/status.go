package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// StatusCmd is `mxl-replicator status`: is anything wrong?
//
// It is deliberately **not** a list. `get requests` lists requests and `get paths` lists paths;
// this counts them and then names only what is not healthy, because the answer an operator wants
// at 3am is "these two things are broken", not a screen they have to scan.
//
// The three read verbs divide the work: status summarises, get lists, describe explains one thing.
type StatusCmd struct {
	ClientFlags `embed:""`
	OutputFlags `embed:""`
}

// Summary is what status reports, and what `-o json` emits.
type Summary struct {
	// Settling reports that the server has not run its first reconcile (§7.3). It is called out
	// rather than left to be inferred from everything reading WAITING, because the difference
	// between "this server has just started" and "the fleet has stopped" is the whole point.
	Settling bool `json:"settling,omitempty"`

	Nodes    NodeSummary       `json:"nodes"`
	Requests map[api.State]int `json:"requests"`
	Paths    map[api.State]int `json:"paths"`
	Sessions int               `json:"sessions"`

	// Unhealthy names what is not ACTIVE, so status is actionable rather than merely numeric.
	Unhealthy []Unhealthy `json:"unhealthy,omitempty"`
}

type NodeSummary struct {
	Registered int `json:"registered"`

	// Leased is how many are currently held by an agent. Registration is durable and survives
	// the agent being down; only the lease goes away (§7.1).
	Leased int `json:"leased"`

	// NoWritableArea names the nodes that cannot be a replication destination at all — worth
	// surfacing because it is the first thing to check behind an INVALID request (§10.6).
	NoWritableArea []string `json:"no_writable_area,omitempty"`
}

type Unhealthy struct {
	Kind   string    `json:"kind"`
	Name   string    `json:"name"`
	State  api.State `json:"state"`
	Reason string    `json:"reason,omitempty"`
}

func (c *StatusCmd) Run(ctx context.Context) error {
	user, err := c.client()
	if err != nil {
		return err
	}

	nodes, err := user.Nodes(ctx)
	if err != nil {
		return err
	}
	requests, err := user.Requests(ctx, "")
	if err != nil {
		return err
	}
	paths, err := user.Paths(ctx)
	if err != nil {
		return err
	}

	summary := Summary{
		Settling: paths.Settling,
		Requests: map[api.State]int{},
		Paths:    map[api.State]int{},
	}

	for _, node := range nodes {
		summary.Nodes.Registered++
		if node.Live {
			summary.Nodes.Leased++
		}
		if !writable(node.Capabilities.Areas) {
			summary.Nodes.NoWritableArea = append(summary.Nodes.NoWritableArea, node.Name)
		}
		if !node.Live {
			summary.Unhealthy = append(summary.Unhealthy, Unhealthy{
				Kind: "node", Name: node.Name, State: "not leased",
				Reason: "no agent holds this node, its workers may still be running",
			})
		}
	}

	// DISABLED is counted and deliberately not listed (§11). This table is the answer to "is
	// anything wrong", and a request somebody switched off on purpose is not — a fleet with fifteen
	// parked legs would otherwise report fifteen problems every time. It stays visible in the count
	// beside every other state, which is where parked intent belongs: findable, not loud.
	for _, request := range requests {
		summary.Requests[request.Status.State]++
		if request.Status.State != api.StateActive && request.Status.State != api.StateDisabled {
			summary.Unhealthy = append(summary.Unhealthy, Unhealthy{
				Kind: "request", Name: request.Name,
				State: request.Status.State, Reason: request.Status.Reason,
			})
		}
	}

	for _, path := range paths.Paths {
		summary.Paths[path.State]++
		if path.Session != nil {
			summary.Sessions++
		}
	}

	return renderAs(c.Output, summary, func() { printSummary(summary) })
}

func printSummary(summary Summary) {
	if summary.Settling {
		warn("still settling: this is intent, not what is running")
	}

	out := table()
	defer flush(out)

	fmt.Fprintf(out, "nodes\t%d registered, %d leased\n", summary.Nodes.Registered, summary.Nodes.Leased)
	fmt.Fprintf(out, "requests\t%d  %s\n", total(summary.Requests), states(summary.Requests))
	fmt.Fprintf(out, "paths\t%d  %s\n", total(summary.Paths), states(summary.Paths))
	fmt.Fprintf(out, "sessions\t%d running\n", summary.Sessions)

	if len(summary.Nodes.NoWritableArea) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintf(out, "%s: no writable area, cannot be a destination\n",
			strings.Join(summary.Nodes.NoWritableArea, ", "))
	}

	switch {
	case len(summary.Unhealthy) > 0:
		// fall through to the table below
	case total(summary.Requests) == 0:
		// "everything is active" is true of an empty fleet and useless. Say which of the two
		// nothing-is-wrong states this is.
		fmt.Fprintln(out, "\nno replication requests")
		return
	case summary.Requests[api.StateDisabled] == total(summary.Requests):
		fmt.Fprintln(out, "\nevery request is disabled")
		return
	case summary.Requests[api.StateDisabled] > 0:
		// A third nothing-is-wrong state, and it needs its own sentence: "everything is active" is
		// false while some requests are parked, and an operator who reads it that way will not go
		// looking for the leg they turned off in June.
		fmt.Fprintf(out, "\neverything else is active (%d disabled)\n", summary.Requests[api.StateDisabled])
		return
	default:
		fmt.Fprintln(out, "\neverything is active")
		return
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "KIND\tNAME\tSTATE\tREASON")
	for _, item := range summary.Unhealthy {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", item.Kind, item.Name, item.State, item.Reason)
	}
}

func total(counts map[api.State]int) int {
	sum := 0
	for _, count := range counts {
		sum += count
	}
	return sum
}

// states renders a count breakdown worst-first, so the thing to worry about is leftmost.
func states(counts map[api.State]int) string {
	// DISABLED comes after ACTIVE: worst-first is a reading order, and a parked request is the last
	// thing in the line rather than something to worry about (§11).
	order := []api.State{
		api.StateInvalid, api.StateFailed, api.StateDegraded,
		api.StateWaiting, api.StateEstablishing, api.StatePaused, api.StateActive,
		api.StateDisabled,
	}

	var parts []string
	seen := map[api.State]bool{}
	for _, state := range order {
		seen[state] = true
		if counts[state] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[state], state))
		}
	}
	// Anything the vocabulary grew since this list was written still gets counted rather than
	// silently dropped.
	var extra []string
	for state, count := range counts {
		if !seen[state] && count > 0 {
			extra = append(extra, fmt.Sprintf("%d %s", count, state))
		}
	}
	sort.Strings(extra)

	if joined := strings.Join(append(parts, extra...), ", "); joined != "" {
		return "(" + joined + ")"
	}
	return ""
}

// renderAs writes the machine formats verbatim from the API types, so a script written against
// `-o json` is written against the documented API rather than against whichever command produced
// it. Shared by status, get and describe for exactly that reason.
func renderAs(format string, value any, text func()) error {
	switch format {
	case "json":
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	case "yaml":
		return yaml.NewEncoder(os.Stdout).Encode(value)
	default:
		text()
		return nil
	}
}

// writable reports whether any of a node's areas grants writing, which is what makes it a
// replication destination at all (§10.6).
func writable(areas []api.Area) bool {
	for _, area := range areas {
		if area.Write {
			return true
		}
	}
	return false
}

func printRequestTable(requests []api.Request) {
	if len(requests) == 0 {
		fmt.Println("no replication requests")
		return
	}

	// NAMESPACE is a column of its own rather than being folded into the name, because a request's
	// ID is the pair (§9.3) and two requests called `cam1` in two partitions are two rows that
	// would otherwise be indistinguishable.
	out := table("NAMESPACE", "NAME", "STATE", "PATHS", "SOURCES", "DESTINATIONS", "LABELS")
	for _, request := range requests {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			request.NamespaceOrDefault(),
			request.Name,
			request.Status.State,
			pathSummary(request.Status),
			sources(request.Sources),
			destinations(request.Destinations),
			sortedLabels(request.Labels),
		)
	}
	_ = out.Flush()

	// The reason is what says which end is wrong, so it must not be lost to a column width. Printed
	// under the table rather than in it.
	//
	// DISABLED is skipped for the opposite reason to ACTIVE's: not because there is nothing to say
	// but because the row already said it, and the destination column marks which legs are off. A
	// paragraph per parked request under a list is exactly the loudness §11 asks this state not to
	// have.
	for _, request := range requests {
		if request.Status.Reason == "" {
			continue
		}
		if request.Status.State == api.StateActive || request.Status.State == api.StateDisabled {
			continue
		}
		fmt.Printf("\n%s: %s\n", request.ID, request.Status.Reason)
	}
}

// pathSummary is the "1 of 3 active" numerator and denominator (§9.1).
//
// For a PARTIAL request it counts the ACTIVE paths, not the paths in the request's own state:
// PARTIAL is an aggregate and no path is ever in it, so `status.Counts[status.State]` is zero and
// the cell would read "0/12" for a request with eleven paths carrying media (§11).
func pathSummary(status api.RequestStatus) string {
	total := len(status.Paths)
	if total == 0 {
		return "0"
	}
	state := status.State
	if state == api.StatePartial {
		state = api.StateActive
	}
	if count := status.Counts[state]; count != total {
		return fmt.Sprintf("%d/%d", count, total)
	}
	return fmt.Sprintf("%d", total)
}

// sources renders a request's source list for a table cell. A label selector renders its labels,
// which is the line the operator wrote (§9.1).
func sources(list []api.Source) string {
	names := make([]string, 0, len(list))
	for _, src := range list {
		names = append(names, src.Describe())
	}
	return strings.Join(names, ",")
}

// destinations renders the column of `get requests`, marking the parked ones (§9.1).
//
// Marked rather than hidden: a destination that is on file and switched off is the request's own
// text, and a list that showed only the live ones would make a request read as smaller than it is —
// which is exactly the reading that makes somebody write the leg a second time.
func destinations(list []api.Destination) string {
	names := make([]string, 0, len(list))
	for _, dst := range list {
		name := dst.Endpoint()
		if dst.Disabled {
			name += " (off)"
		}
		names = append(names, name)
	}
	return strings.Join(names, ",")
}

func selectorText(selector api.Selector) string {
	switch {
	case selector.GroupHint != nil && selector.GroupHint.Type != "":
		return fmt.Sprintf("group_hint %q (%s)", selector.GroupHint.Name, selector.GroupHint.Type)
	case selector.GroupHint != nil:
		return fmt.Sprintf("group_hint %q", selector.GroupHint.Name)
	case selector.Flow != "":
		return "flow " + selector.Flow
	case selector.All:
		return "all flows"
	default:
		return ""
	}
}
