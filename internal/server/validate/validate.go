// Package validate decides whether a replication request can ever work (§7.2).
//
// The whole point is the split between two kinds of "not running":
//
//   - **INVALID** — needs user action and never resolves by itself. A destination naming an
//     area the node does not advertise, or advertises without the `write` grant, two nodes with no
//     fabric in common, a pinned provider neither offers.
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

// Pairing checks everything about **one (source, destination) pairing of a request** that follows
// from node registrations alone: the endpoints, the destination domain, the requested scheduling
// priority, and whether any interface pair exists at all.
//
// It returns nil when that pairing is acceptable, and a [Result] naming precisely what an operator
// has to change when it is not. It never returns "not yet" — a pairing that is fine but not
// currently satisfiable is WAITING, and that is decided elsewhere.
//
// **Per pairing, not per request**, because a request fans in and out (§9.1) and its pairings fail
// independently: a node that dropped its writable area must not stop the request's other two
// destinations from establishing new sessions. The caller decides what a failure here means for the
// request as a whole — at POST it is a rejection, on a stored request it is one INVALID leg
// alongside working ones.
//
// *This used to be per destination*, and was per destination only because there was one source. It
// was always per pairing in substance: an interface config is negotiated for a session, and a
// session has two ends (§10.3).
//
// Indices rather than the values themselves, because several of these messages have to name **both
// ends** — "source and destination are both edge-01/fast/ingest" does not say which of nine sources
// it is — and the position in the request is the only handle an operator has on a source that
// carries no name of its own.
//
// The negotiated result is returned too, because the caller needs it anyway and negotiating
// twice is how the answer at validation time and the answer at assignment time drift apart.
func Pairing(spec api.RequestSpec, srcIndex, dstIndex int, fleet *state.Fleet, cfg negotiate.Config) (negotiate.Result, *Result) {
	src, dst := spec.Sources[srcIndex], spec.Destinations[dstIndex]

	// A session from a (node, domain) to itself would have the initiator and the target reading
	// and writing one ring buffer. Same node, *different* domain is legitimate and is exactly
	// what the loopback configuration does.
	//
	// **It applies to a named source only** (§7.2, §10.7). With a name, this is an operator having
	// written the same string twice — a typo, decidable from the request, and refused. A label
	// selector matching the destination's own domain is not a typo: it is the selector doing what
	// it was asked to, and refusing the request would put its author at the mercy of which domains
	// happen to carry a label. There the pairing is *elided* and the rest of the expansion stands,
	// which happens in the reconciler, where the expansion is.
	//
	// Refusing the whole request for one bad pairing is right *here* and nowhere else: with both
	// ends named it is a typo, and the author can see both halves of it in the file they just wrote.
	if src.Domain.Kind() == api.DomainSelectorKindName &&
		src.Node == dst.Node && src.Domain.Name.Equal(dst.Domain) {
		return negotiate.Result{}, &Result{
			Code: api.ReasonSameEndpoint,
			Message: fmt.Sprintf("sources[%d] and destinations[%d] are both %s/%s",
				srcIndex, dstIndex, src.Node, dst.DomainName()),
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

	// *There used to be a resolved output root returned alongside the verdict here*, because a
	// path identity carried it as a term of its own and a shadow path had to reproduce it exactly.
	// The area is inside the domain's name now (§10.6), so a destination's identity is complete
	// the moment the request is read and nothing has to be resolved to build one.
	if bad := resolveArea(dst, destination.Value); bad != nil {
		return negotiate.Result{}, bad
	}

	// *`sched_prio` used to be checked here.* It is a property of the **host**, not of a pairing, so
	// it belongs in [Request]: checked once over every node the request names, reported once naming
	// the node that lacks it. Checked per pairing it produced one failure per pairing, each naming
	// whichever end happened to be examined first — which makes a single missing capability look
	// like several problems and defeats the attribution rule §9.1 asks for.

	result, err := negotiate.Negotiate(
		source.Value.Capabilities.Fabrics,
		destination.Value.Capabilities.Fabrics,
		spec.ProviderFor(dst), cfg)
	if err != nil {
		var negErr *negotiate.Error
		if errors.As(err, &negErr) {
			return negotiate.Result{}, &Result{Code: negErr.Code, Message: negErr.Message}
		}
		return negotiate.Result{}, &Result{Code: api.ReasonNoSharedFabric, Message: err.Error()}
	}

	return result, nil
}

// Request checks the things that are wrong about a request **as a whole** rather than about one of
// its pairings, and that follow from the request body alone (§7.2, §9.1).
//
// Two rules today, and what they have in common is the reason they are not in [Pairing]: neither
// belongs to a pairing, and reporting either one per pairing would make one problem look like
// several and point at whichever end happened to be examined first (§9.1).
//
//   - `duplicate_source_flow` — two sources pinning the same flow UUID, with a destination in
//     common, are two initiators writing one destination ring buffer. [Conflicts]' flow_conflict
//     arriving from inside a single request, and decidable here because both sources pinned rather
//     than selected. *Not* the general case: a source that selects rather than pins can collide
//     with anything at any time, and that collision is [api.ReasonFlowConflict] on the path,
//     resolved by §7.5. This catches only the form an operator could have seen in the file they
//     wrote.
//   - `sched_prio_unavailable` — the capability is a property of the **host** (§10.2), so it is
//     checked over every node the request names and the rejection names the node, not the pairing.
//     Requested but unavailable priority fails now rather than producing workers that silently run
//     at normal priority, which would look like a performance problem in the media rather than a
//     configuration problem in the request.
//
// Both live beside [Pairing] rather than in [api.RequestSpec.Validate] for the reason the whole
// package exists: this is the code that answers both "can I accept this POST" and "is this stored
// request still valid", and a rule that only ran at POST would let a request written straight into
// the store reach an agent as an assignment.
func Request(spec api.RequestSpec, fleet *state.Fleet) *Result {
	// **A request with every destination parked is not validated against the fleet at all** (§7.2,
	// §9.1). It is asking for nothing, so there is nothing to refuse: both rules below describe a
	// pairing that would exist, and neither would. `duplicate_source_flow` in particular is only a
	// corruption because two sources *share a destination*, and a request with no enabled one shares
	// none.
	//
	// The cost is accepted and is worth knowing: a parked route can be broken without saying so, and
	// finds out when it is enabled. Structural validation is the exception and runs unconditionally
	// in [api.RequestSpec.Validate], so nothing unspellable can be parked in the store to fail later
	// on the click that turns it on. The alternative — reporting it anyway — gives a request two
	// states, INVALID and DISABLED, and one field to report them in.
	if !api.AnyEnabled(spec.Destinations) {
		return nil
	}

	if bad := duplicateSourceFlow(spec); bad != nil {
		return bad
	}
	return schedPrio(spec, fleet)
}

// schedPrio checks the requested priority against every node the request names.
//
// Unregistered nodes are skipped: [Pairing] reports those as `node_not_registered`, which is the
// more actionable message, and a node that does not exist has no capabilities to be missing.
func schedPrio(spec api.RequestSpec, fleet *state.Fleet) *Result {
	if spec.SchedPrio == nil {
		return nil
	}

	names := make([]string, 0, len(spec.Sources)+len(spec.Destinations))
	for _, src := range spec.Sources {
		names = append(names, src.Node)
	}
	for _, dst := range spec.Destinations {
		// A parked destination names a node this request is not currently asking anything of, so a
		// missing capability there is not a reason to invalidate the legs that are running (§9.1).
		if dst.Disabled {
			continue
		}
		names = append(names, dst.Node)
	}

	for _, name := range names {
		node, registered := fleet.Nodes[name]
		if !registered || node.Value.Capabilities.SchedPrio {
			continue
		}
		return &Result{
			Code:    api.ReasonSchedPrioUnavailable,
			Message: fmt.Sprintf("node %q cannot apply sched_prio: no CAP_SYS_NICE or RLIMIT_RTPRIO", name),
		}
	}
	return nil
}

func duplicateSourceFlow(spec api.RequestSpec) *Result {
	if len(spec.Sources) < 2 {
		return nil
	}

	// Which source first pinned each flow UUID. Only pinned selectors participate — a group hint or
	// `all` may or may not produce that flow, and refusing on a maybe is refusing a request that
	// probably works.
	holder := map[string]int{}
	for i, src := range spec.Sources {
		if src.Select.Kind() != api.SelectorKindFlow {
			continue
		}
		flow := src.Select.Flow
		first, taken := holder[flow]
		if !taken {
			holder[flow] = i
			continue
		}

		// "Share a destination" needs no test: a request is the full cross product of its two lists
		// (§9.1), so any two of its sources share *every* destination it names. The check is
		// therefore two sources, one pinned flow ID — and it would become an intersection the moment
		// a request could pair its ends selectively, which §10.8 explicitly does not do.
		//
		// Two entries that are the same source are caught by the dedup rule in
		// [api.RequestSpec.Validate] and never reach here through the API. Tolerated rather than
		// reported for a record written straight into the store: they expand to one path, which is
		// not the corruption this refuses.
		if spec.Sources[first].Describe() == src.Describe() {
			continue
		}
		return &Result{
			Code: api.ReasonDuplicateSourceFlow,
			Message: fmt.Sprintf("sources[%d] (%s) and sources[%d] (%s) both pin flow %s into the same destination",
				first, spec.Sources[first].Describe(), i, src.Describe(), flow),
		}
	}
	return nil
}

// resolveArea checks that a destination names an area the node advertises and grants writing on,
// and is the whole of the authority this API has over a node's filesystem (§10.6, §13,
// invariant 6).
//
// **A destination is always a name inside an area the operator granted `write` on.** A raw path is
// never accepted, a node advertising no writable area is not a destination at all, and the server
// never picks an area on the operator's behalf. Without this the API is a remote
// arbitrary-filesystem-write primitive on every node in the fleet, and it has to hold regardless
// of what authentication is configured.
//
// *There used to be a third branch here: a node advertising several roots and a request naming
// none, refused as `ambiguous_output_root`.* It is structurally unreachable now — a destination
// always names its area, because the area is the first segment of the domain's name (§7.2).
//
// The destination agent checks all of this again before it resolves a path. That duplication is
// deliberate and is the one place in the tree where it earns its keep: it costs a map lookup, and
// it is the difference between one buggy control plane and files written wherever an area reaches.
func resolveArea(dst api.Destination, node state.NodeRecord) *Result {
	// Structural, and normally caught at POST by [api.RequestSpec.Validate]. Reachable here for a
	// request written straight into the store, or stored before the rule existed — which must
	// fail legibly rather than reaching an agent as an assignment.
	if err := dst.Domain.Valid(); err != nil {
		return &Result{
			Code:    api.ReasonMalformedDomainName,
			Message: fmt.Sprintf("invalid destination domain %q: %s", dst.DomainName(), err),
		}
	}

	area := node.Capabilities.FindArea(dst.Domain.Area)
	if area == nil {
		return &Result{
			Code: api.ReasonUnknownArea,
			Message: fmt.Sprintf("node %q advertises no area %q, it has %s",
				dst.Node, dst.Domain.Area, areaList(node.Capabilities.Areas)),
		}
	}
	if !area.Write {
		return &Result{
			Code: api.ReasonAreaNotWritable,
			Message: fmt.Sprintf("node %q advertises area %q but does not grant writing on it",
				dst.Node, dst.Domain.Area),
		}
	}
	return nil
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

// Conflicts finds the three ways a set of individually valid paths can be jointly wrong (§7.2).
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
// **No materialised domain inside another.** `fast/studio-a` and `fast/studio-a/cam1` would make
// one domain directory a container for another (§10.6). It is checked here rather than per request
// because the two paths need not share a source, a destination flow or a request —
// `studio-a/cam1 → edge-01/fast/studio-a` and `studio-b/cam2 → edge-01/fast/studio-a/cam1`
// collide, and nothing either request can see says so.
//
// *A third conflict used to live here: one domain name materialised under two different output
// roots.* It is unconstructible now that the area is in the name (§7.2, §10.6).
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

	// The domains accepted so far, per node, for the nesting check.
	nestable := map[string][]api.Domain{}

	for _, ref := range ordered {
		src, dst := ref.Path.Source, ref.Path.Destination
		destFlow := api.FlowAddress{Node: dst.Node, Domain: dst.DomainName(), Flow: src.Flow}

		// **No materialised domain inside another.** `fast/studio-a` and `fast/studio-a/cam1`
		// would make one domain directory a container for another, which is a shape nothing else
		// in the design has: the outer one's flows and the inner one's would share a tree, and
		// removing either is a question with no answer (§10.6).
		//
		// An exact slice-prefix test in both directions, which is what the element form is worth
		// here — the string spelling of this question has to work around `studio-ab` looking like
		// a child of `studio-a`. Within one area only: two domains in different areas are two
		// directory trees and cannot nest, whatever their elements look like.
		//
		// *The root-collision check that used to sit beside this is gone*, along with the
		// shadow-path exemption it needed: a destination's identity is complete the moment the
		// request is read now, so there is no such thing as a path whose area is unresolved
		// (§10.6).
		if outer, nested := nestedDomain(dst.Domain, nestable[dst.Node]); nested {
			out[ref.ID] = Result{
				Code: api.ReasonDomainNameInUse,
				Message: fmt.Sprintf("domain %q on node %q nests with %q, which another path materialises",
					dst.DomainName(), dst.Node, outer),
			}
			continue
		}

		if existing, taken := holder[destFlow]; taken {
			if existing != src {
				// **Both sources, not the winner alone** (§7.5). Fan-in makes two paths of one
				// request colliding routine rather than merely reachable, and there the tie falls
				// all the way through to the path ID — deterministic, and *arbitrary* from the
				// operator's point of view, because nothing in the request says which of two sources
				// of one flow ID was meant. A message naming only the incumbent reads as an
				// explanation when it is really a coin toss, and the fix is to change one of the two
				// lines the operator can only find if both are named.
				out[ref.ID] = Result{
					Code: api.ReasonFlowConflict,
					Message: fmt.Sprintf("%s/%s receives flow %s from %s/%s, so it cannot also receive it from %s/%s",
						dst.Node, dst.DomainName(), src.Flow,
						existing.Node, existing.Domain, src.Node, src.Domain),
				}
				continue
			}
			// Same source, same destination, same flow: this is the deduplicated path itself
			// appearing twice, which is not a conflict.
		}

		from, to := endpoint(src.Node, src.Domain), endpoint(dst.Node, dst.DomainName())
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
		nestable[dst.Node] = append(nestable[dst.Node], dst.Domain)
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

// areaList renders a node's areas for an error message, marking the writable ones — because
// "this node has no area `fast`" and "it has one but you may not write into it" are different
// operator problems and the list is where the distinction is cheapest to show.
func areaList(areas []api.Area) string {
	if len(areas) == 0 {
		return "none"
	}
	names := make([]string, 0, len(areas))
	for _, area := range areas {
		grants := "read-only"
		if area.Write {
			grants = "writable"
		}
		names = append(names, fmt.Sprintf("%q (%s)", area.Name, grants))
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// nestedDomain reports whether a domain nests with one already accepted on that node, in either
// direction, and which one.
func nestedDomain(domain api.Domain, accepted []api.Domain) (api.Domain, bool) {
	for _, other := range accepted {
		if domain.NestedIn(other) || other.NestedIn(domain) {
			return other, true
		}
	}
	return api.Domain{}, false
}
