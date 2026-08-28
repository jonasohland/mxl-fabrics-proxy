package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

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

	// NoOutputRoot counts nodes that cannot be a replication destination at all — worth
	// surfacing because it is the first thing to check behind an INVALID request (§10.6).
	NoOutputRoot []string `json:"no_output_root,omitempty"`
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
	requests, err := user.Requests(ctx)
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
		if len(node.Capabilities.OutputRoots) == 0 {
			summary.Nodes.NoOutputRoot = append(summary.Nodes.NoOutputRoot, node.Name)
		}
		if !node.Live {
			summary.Unhealthy = append(summary.Unhealthy, Unhealthy{
				Kind: "node", Name: node.Name, State: "not leased",
				Reason: "no agent holds this node; its workers may still be running",
			})
		}
	}

	for _, request := range requests {
		summary.Requests[request.Status.State]++
		if request.Status.State != api.StateActive {
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

	fmt.Printf("nodes      %d registered, %d leased\n", summary.Nodes.Registered, summary.Nodes.Leased)
	fmt.Printf("requests   %d  %s\n", total(summary.Requests), states(summary.Requests))
	fmt.Printf("paths      %d  %s\n", total(summary.Paths), states(summary.Paths))
	fmt.Printf("sessions   %d running\n", summary.Sessions)

	if len(summary.Nodes.NoOutputRoot) > 0 {
		fmt.Printf("\n%s: no output root, cannot be a destination\n",
			strings.Join(summary.Nodes.NoOutputRoot, ", "))
	}

	switch {
	case len(summary.Unhealthy) > 0:
		// fall through to the table below
	case total(summary.Requests) == 0:
		// "everything is active" is true of an empty fleet and useless. Say which of the two
		// nothing-is-wrong states this is.
		fmt.Println("\nno replication requests")
		return
	default:
		fmt.Println("\neverything is active")
		return
	}

	fmt.Println()
	out := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(out, "KIND\tNAME\tSTATE\tREASON")
	for _, item := range summary.Unhealthy {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", item.Kind, item.Name, item.State, item.Reason)
	}
	_ = out.Flush()
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
	order := []api.State{
		api.StateInvalid, api.StateFailed, api.StateDegraded,
		api.StateWaiting, api.StateEstablishing, api.StatePaused, api.StateActive,
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

func printRequestTable(requests []api.Request) {
	if len(requests) == 0 {
		fmt.Println("no replication requests")
		return
	}

	out := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(out, "NAME\tSTATE\tPATHS\tSOURCE\tDESTINATIONS\tLABELS")
	for _, request := range requests {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\n",
			request.Name,
			request.Status.State,
			pathSummary(request.Status),
			request.Source.Node+"/"+request.Source.Domain,
			destinations(request.Destinations),
			sortedLabels(request.Labels),
		)
	}
	_ = out.Flush()

	// The reason is what says which destination is wrong, so it must not be lost to a column
	// width. Printed under the table rather than in it.
	for _, request := range requests {
		if request.Status.Reason != "" && request.Status.State != api.StateActive {
			fmt.Printf("\n%s: %s\n", request.Name, request.Status.Reason)
		}
	}
}

// pathSummary is the "1 of 3 active" numerator and denominator (§9.1).
func pathSummary(status api.RequestStatus) string {
	total := len(status.Paths)
	if total == 0 {
		return "0"
	}
	if count := status.Counts[status.State]; count != total {
		return fmt.Sprintf("%d/%d", count, total)
	}
	return fmt.Sprintf("%d", total)
}

func destinations(list []api.Destination) string {
	names := make([]string, 0, len(list))
	for _, dst := range list {
		names = append(names, dst.Endpoint())
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
	default:
		return ""
	}
}
