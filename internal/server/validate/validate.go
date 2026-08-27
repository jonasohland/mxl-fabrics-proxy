// Package validate decides whether a replication request can ever work (§7.2).
//
// The whole point is the split between two kinds of "not running":
//
//   - **INVALID** — needs user action and never resolves by itself. A destination domain that
//     is not mapped, two nodes with no fabric in common, a pinned provider neither offers.
//     Rejecting these at request time is what stops the API from accumulating requests that sit
//     in WAITING looking like they might come good.
//   - **WAITING** — waiting for something that may plausibly appear: a flow that is not being
//     produced yet, an agent that is down. Not this package's business; those are decided per
//     path by the reconciler, which is the thing that can see inventory change.
//
// Everything here is a pure function of a fleet snapshot, so the same code answers "can I
// accept this POST" and "is this stored request still valid" — and the two can never drift into
// disagreeing about what INVALID means.
package validate

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/negotiate"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
)

// Result is one reason a request or path cannot work.
type Result struct {
	Code    api.ReasonCode
	Message string
}

func (r Result) String() string { return fmt.Sprintf("%s: %s", r.Code, r.Message) }

// Spec checks everything about a request that follows from node registrations alone: the
// endpoints, the destination domain, the requested scheduling priority, and whether any
// interface pair exists at all.
//
// It returns nil when the request is acceptable, and a [Result] naming precisely what an
// operator has to change when it is not. It never returns "not yet" — a request that is fine
// but not currently satisfiable is WAITING, and that is decided elsewhere.
//
// The negotiated result is returned too, because the caller needs it anyway and negotiating
// twice is how the answer at validation time and the answer at assignment time drift apart.
func Spec(spec api.RequestSpec, fleet *state.Fleet, cfg negotiate.Config) (negotiate.Result, *Result) {
	src, dst := spec.Source, spec.Destination

	// A session from a (node, domain) to itself would have the initiator and the target reading
	// and writing one ring buffer. Same node, *different* domain is legitimate and is exactly
	// what the legacy loopback configuration does.
	if src.Node == dst.Node && src.Domain == dst.Domain {
		return negotiate.Result{}, &Result{
			Code:    api.ReasonSameEndpoint,
			Message: fmt.Sprintf("source and destination are both %s/%s", src.Node, src.Domain),
		}
	}

	source, ok := fleet.Nodes[src.Node]
	if !ok {
		return negotiate.Result{}, &Result{
			Code:    api.ReasonNodeNotRegistered,
			Message: fmt.Sprintf("source node %q has never registered", src.Node),
		}
	}
	destination, ok := fleet.Nodes[dst.Node]
	if !ok {
		return negotiate.Result{}, &Result{
			Code:    api.ReasonNodeNotRegistered,
			Message: fmt.Sprintf("destination node %q has never registered", dst.Node),
		}
	}

	// The single most important invariant in the design (§13, invariant 6). A destination domain
	// must be a name the destination agent explicitly mapped: a raw path is never accepted from
	// the API, and a domain the agent merely *discovered* by a search path does not count.
	// Without this the API is a remote arbitrary-filesystem-write primitive on every node in the
	// fleet, and it has to hold whatever authentication is configured.
	mapping, ok := destination.Value.Domain(dst.Domain)
	switch {
	case !ok:
		return negotiate.Result{}, &Result{
			Code: api.ReasonDomainNotMapped,
			Message: fmt.Sprintf("node %q has no domain %q; it maps %s",
				dst.Node, dst.Domain, domainList(destination.Value.Domains)),
		}
	case !mapping.Configured:
		return negotiate.Result{}, &Result{
			Code: api.ReasonDomainNotMapped,
			Message: fmt.Sprintf("domain %q on node %q was discovered, not configured, and a discovered domain is never a replication destination",
				dst.Domain, dst.Node),
		}
	}

	// Requested but unavailable scheduling priority fails now rather than producing workers that
	// silently run at normal priority — which would look like a performance problem in the
	// media, not a configuration problem in the request.
	if spec.SchedPrio != nil {
		for _, node := range []state.Entry[state.NodeRecord]{source, destination} {
			if !node.Value.Capabilities.SchedPrio {
				return negotiate.Result{}, &Result{
					Code:    api.ReasonSchedPrioUnavailable,
					Message: fmt.Sprintf("node %q cannot apply sched_prio: it advertises neither CAP_SYS_NICE nor RLIMIT_RTPRIO", node.Value.Node),
				}
			}
		}
	}

	result, err := negotiate.Negotiate(
		source.Value.Capabilities.Fabrics,
		destination.Value.Capabilities.Fabrics,
		spec.Provider, cfg)
	if err != nil {
		var negErr *negotiate.Error
		if errors.As(err, &negErr) {
			return negotiate.Result{}, &Result{Code: negErr.Code, Message: negErr.Message}
		}
		return negotiate.Result{}, &Result{Code: api.ReasonNoSharedFabric, Message: err.Error()}
	}

	return result, nil
}

// PathRef is one expanded path, with the precedence that decides who loses a conflict.
type PathRef struct {
	ID   string
	Path state.PathIdentity

	// Since is the creation time of the earliest request that expanded onto this path. It is
	// the tie-breaker for a conflict, and the reason is operational rather than aesthetic: the
	// older path is the one that is probably already carrying media, so a newly created request
	// that collides with it must be the one that fails. Ties break on ID, so the answer is the
	// same on every replica.
	Since time.Time
}

// Conflicts finds the two ways a set of individually valid paths can be jointly wrong (§7.2).
//
// It returns a result per offending path ID; paths absent from the map are fine.
//
// **Flow conflict.** Two sources replicating into one (destination node, domain, flow ID) means
// two producers writing one ring buffer, which corrupts it. Nothing downstream detects this —
// both sessions look healthy and the media is garbage.
//
// **Loops.** A→B plus B→A for one flow ID feeds a flow back into itself. Chains are fine and
// useful (A→B→C), so this is genuinely cycle detection over the per-flow graph rather than a
// pairwise check: A→B→C→A is the same mistake spelled longer, and it must not be the case that
// adding a third hop hides it.
//
// Paths are considered in precedence order and a path is rejected only against those already
// accepted, so an existing path is never invalidated by a newly created one.
func Conflicts(paths []PathRef) map[string]Result {
	ordered := slices.Clone(paths)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].Since.Equal(ordered[j].Since) {
			return ordered[i].Since.Before(ordered[j].Since)
		}
		return ordered[i].ID < ordered[j].ID
	})

	out := map[string]Result{}

	// Which source is already writing each destination flow.
	holder := map[api.FlowAddress]api.FlowAddress{}

	// Per flow ID, the edges accepted so far, as (node/domain) → set of (node/domain).
	edges := map[string]map[string][]string{}

	for _, ref := range ordered {
		src, dst := ref.Path.Source, ref.Path.Destination
		destFlow := api.FlowAddress{Node: dst.Node, Domain: dst.Domain, Flow: src.Flow}

		if existing, taken := holder[destFlow]; taken {
			if existing != src {
				out[ref.ID] = Result{
					Code: api.ReasonFlowConflict,
					Message: fmt.Sprintf("%s/%s already receives flow %s from %s/%s; two producers into one flow corrupts the ring buffer",
						dst.Node, dst.Domain, src.Flow, existing.Node, existing.Domain),
				}
				continue
			}
			// Same source, same destination, same flow: this is the deduplicated path itself
			// appearing twice, which is not a conflict.
		}

		from, to := endpoint(src.Node, src.Domain), endpoint(dst.Node, dst.Domain)
		graph := edges[src.Flow]
		if graph == nil {
			graph = map[string][]string{}
			edges[src.Flow] = graph
		}
		if reaches(graph, to, from) {
			out[ref.ID] = Result{
				Code: api.ReasonLoop,
				Message: fmt.Sprintf("replicating flow %s from %s to %s closes a loop: %s already reaches %s",
					src.Flow, from, to, to, from),
			}
			continue
		}

		holder[destFlow] = src
		graph[from] = append(graph[from], to)
	}

	return out
}

func endpoint(node, domain string) string { return node + "/" + domain }

// reaches reports whether there is already a path from one endpoint to another in the accepted
// edges for a flow.
func reaches(graph map[string][]string, from, to string) bool {
	if from == to {
		return true
	}
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		for _, next := range graph[at] {
			if next == to {
				return true
			}
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

func domainList(domains []api.DomainMapping) string {
	if len(domains) == 0 {
		return "no domains at all"
	}
	names := make([]string, 0, len(domains))
	for _, d := range domains {
		name := fmt.Sprintf("%q", d.Name)
		if !d.Configured {
			name += " (discovered)"
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}
