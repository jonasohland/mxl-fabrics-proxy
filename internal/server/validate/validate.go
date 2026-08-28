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
	"path"
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

// Destination checks everything about **one destination of a request** that follows from node
// registrations alone: the endpoints, the destination domain, the requested scheduling priority,
// and whether any interface pair exists at all.
//
// It returns nil when that destination is acceptable, and a [Result] naming precisely what an
// operator has to change when it is not. It never returns "not yet" — a destination that is fine
// but not currently satisfiable is WAITING, and that is decided elsewhere.
//
// **Per destination, not per request**, because a request fans out (§9.1) and its destinations
// fail independently: a node that dropped its output root must not stop the request's other two
// destinations from establishing new sessions. The caller decides what a failure here means for
// the request as a whole — at POST it is a rejection, on a stored request it is one INVALID leg
// alongside working ones.
//
// The negotiated result is returned too, because the caller needs it anyway and negotiating
// twice is how the answer at validation time and the answer at assignment time drift apart.
func Destination(spec api.RequestSpec, dst api.Destination, fleet *state.Fleet, cfg negotiate.Config) (negotiate.Result, string, *Result) {
	src := spec.Source

	// A session from a (node, domain) to itself would have the initiator and the target reading
	// and writing one ring buffer. Same node, *different* domain is legitimate and is exactly
	// what the legacy loopback configuration does.
	if src.Node == dst.Node && src.Domain == dst.DomainName() {
		return negotiate.Result{}, "", &Result{
			Code:    api.ReasonSameEndpoint,
			Message: fmt.Sprintf("source and destination are both %s/%s", src.Node, src.Domain),
		}
	}

	source, ok := fleet.Nodes[src.Node]
	if !ok {
		return negotiate.Result{}, "", &Result{
			Code:    api.ReasonNodeNotRegistered,
			Message: fmt.Sprintf("source node %q has never registered", src.Node),
		}
	}
	destination, ok := fleet.Nodes[dst.Node]
	if !ok {
		return negotiate.Result{}, "", &Result{
			Code:    api.ReasonNodeNotRegistered,
			Message: fmt.Sprintf("destination node %q has never registered", dst.Node),
		}
	}

	// Resolved before anything else that can fail, and returned even when a *later* check
	// rejects the request. A path identity carries the resolved root (§10.6), and a shadow path
	// — the one an INVALID request still owns so that a session already running on it keeps
	// being reported — has to be the same identity as the real one or the running session is
	// invisible. Only a root that could not be resolved at all leaves this empty.
	root, bad := resolveRoot(dst, destination.Value)
	if bad != nil {
		return negotiate.Result{}, "", bad
	}
	// After the root is resolved, because the check needs the root's directory — and returning the
	// root regardless, so a shadow path keeps the identity a real path would have had.
	if bad := pathInUse(dst, root, destination.Value); bad != nil {
		return negotiate.Result{}, root, bad
	}

	// Requested but unavailable scheduling priority fails now rather than producing workers that
	// silently run at normal priority — which would look like a performance problem in the
	// media, not a configuration problem in the request.
	if spec.SchedPrio != nil {
		for _, node := range []state.Entry[state.NodeRecord]{source, destination} {
			if !node.Value.Capabilities.SchedPrio {
				return negotiate.Result{}, root, &Result{
					Code:    api.ReasonSchedPrioUnavailable,
					Message: fmt.Sprintf("node %q cannot apply sched_prio: it advertises neither CAP_SYS_NICE nor RLIMIT_RTPRIO", node.Value.Node),
				}
			}
		}
	}

	result, err := negotiate.Negotiate(
		source.Value.Capabilities.Fabrics,
		destination.Value.Capabilities.Fabrics,
		spec.ProviderFor(dst), cfg)
	if err != nil {
		var negErr *negotiate.Error
		if errors.As(err, &negErr) {
			return negotiate.Result{}, root, &Result{Code: negErr.Code, Message: negErr.Message}
		}
		return negotiate.Result{}, root, &Result{Code: api.ReasonNoSharedFabric, Message: err.Error()}
	}

	return result, root, nil
}

// resolveRoot decides which output root a destination is materialised under, and is the whole of
// the authority this API has over a node's filesystem (§10.6, §13, invariant 6).
//
// A destination is always a *name* inside an operator-configured root. A raw path is never
// accepted, a node that advertises no root is not a destination at all, and the server never
// picks between roots on the operator's behalf. Without this the API is a remote
// arbitrary-filesystem-write primitive on every node in the fleet, and it has to hold regardless
// of what authentication is configured.
//
// The destination agent checks all of this again before it resolves a path. That duplication is
// deliberate and is the one place in the tree where it earns its keep: it costs a map lookup, and
// it is the difference between one buggy control plane and files written wherever a root reaches.
func resolveRoot(dst api.Destination, node state.NodeRecord) (string, *Result) {
	// Structural, and normally caught at POST by [api.RequestSpec.Validate]. Reachable here for a
	// request written straight into the store, or stored before the rule existed — which must
	// fail legibly rather than reaching an agent as an assignment.
	if err := api.ValidDomainElements(dst.Domain); err != nil {
		return "", &Result{
			Code:    api.ReasonMalformedDomainName,
			Message: fmt.Sprintf("destination domain %q is not a usable domain name: %s", dst.DomainName(), err),
		}
	}

	// Names are flat per node (§10.6). A destination colliding with an input mapping would put
	// one name over two directories, and every place that carries a single domain string — the
	// assignment, the path identity, the session identity, the `domain` metric label — would then
	// be ambiguous. A *discovered* domain cannot collide by construction: it is named by its
	// path, and a path is not a name this accepts.
	if _, taken := node.Domain(dst.DomainName()); taken {
		return "", &Result{
			Code: api.ReasonDomainNameInUse,
			Message: fmt.Sprintf("node %q already maps %q as an input domain; one name cannot mean two directories on one node",
				dst.Node, dst.DomainName()),
		}
	}

	roots := node.Capabilities.OutputRoots
	switch {
	case len(roots) == 0:
		return "", &Result{
			Code: api.ReasonNoOutputRoot,
			Message: fmt.Sprintf("node %q advertises no output root, so it cannot be a replication destination; it maps %s as inputs",
				dst.Node, domainList(node.Domains)),
		}

	case dst.Root == "":
		if len(roots) > 1 {
			// Never a guess. The cost is recorded rather than hidden: a request that worked
			// becomes ambiguous the day its destination node grows a second root. Taken
			// deliberately — the single-root case is overwhelmingly the common one, and this
			// error carries its own fix.
			return "", &Result{
				Code: api.ReasonAmbiguousOutputRoot,
				Message: fmt.Sprintf("node %q advertises %d output roots (%s) and this request names none; name one",
					dst.Node, len(roots), rootList(roots)),
			}
		}
		return roots[0].Name, nil

	default:
		if node.Capabilities.FindRoot(dst.Root) == nil {
			return "", &Result{
				Code: api.ReasonUnknownOutputRoot,
				Message: fmt.Sprintf("node %q advertises no output root %q; it has %s",
					dst.Node, dst.Root, rootList(roots)),
			}
		}
		return dst.Root, nil
	}
}

// pathInUse refuses a destination whose resolved directory is one the node already maps as an
// input domain (§10.6).
//
// This is the case a root being allowed to sit *above* an input mapping leaves open, and the name
// check above does not cover it: `-m cams=/dev/shm/mxl/cameras` under root `fast=/dev/shm/mxl`
// collides on the path while the names differ. Left unchecked it would be one directory with two
// names — an input domain nothing writes to, and an output domain replication writes into.
//
// The agent refuses it independently and is the authority, since it holds the ground truth about
// its own filesystem. This runs so that a request is rejected at POST with a reason naming what to
// change, rather than being accepted and then failing on the node.
//
// Paths are joined with `path`, not `path/filepath`: these describe a *remote* node's filesystem,
// and the separator that matters is the destination's, not this server's.
func pathInUse(dst api.Destination, root string, node state.NodeRecord) *Result {
	advertised := node.Capabilities.FindRoot(root)
	// Empty is not a failure: the path is advertised for diagnostics only and an agent is entitled
	// to withhold it (§10.2). The agent's own check still holds.
	if advertised == nil || advertised.Path == "" {
		return nil
	}

	resolved := path.Join(append([]string{advertised.Path}, dst.Domain...)...)
	for _, mapping := range node.Domains {
		if mapping.Path == "" || path.Clean(mapping.Path) != resolved {
			continue
		}
		return &Result{
			Code: api.ReasonDomainPathInUse,
			Message: fmt.Sprintf("domain %q under output root %q on node %q resolves to %s, which that node maps as input domain %q; an input domain is never written to",
				dst.DomainName(), root, dst.Node, resolved, mapping.Name),
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
// **Domain names are flat per node.** Two paths materialising one domain name under two
// different output roots is one name over two directories (§10.6). It is checked here rather
// than per request because the two paths need not share a source, a destination flow or a
// request — `studio-a/cam1 → edge-01/ingest@fast` and `studio-b/cam2 → edge-01/ingest@bulk`
// collide, and nothing either request can see says so.
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

	// Which output root each accepted (node, domain) is materialised under.
	rootFor := map[string]string{}

	// The output domains accepted so far, per node, for the nesting check.
	nestable := map[string][][]string{}

	for _, ref := range ordered {
		src, dst := ref.Path.Source, ref.Path.Destination
		destFlow := api.FlowAddress{Node: dst.Node, Domain: dst.DomainName(), Flow: src.Flow}

		// A path with no resolved root materialises nothing: it is a shadow path, kept only so
		// that a session already running on it stays reported, and it is already INVALID for
		// whatever left its root unresolved (§10.6). It therefore neither claims a domain name
		// nor collides with one — a shadow path that claimed a name would evict the valid path
		// that is actually going to create the directory. It is still checked for the other two
		// conflicts below, because a *running* session does hold a destination flow.
		if dst.Root != "" {
			if root, taken := rootFor[endpoint(dst.Node, dst.DomainName())]; taken && root != dst.Root {
				out[ref.ID] = Result{
					Code: api.ReasonDomainNameInUse,
					Message: fmt.Sprintf("domain %q on node %q is already being materialised under output root %q; one name cannot mean two directories on one node",
						dst.DomainName(), dst.Node, root),
				}
				continue
			}

			// **No output domain inside another output domain.** `studio-a` and `studio-a/cam1`
			// would make one domain directory a container for another, which is a shape nothing
			// else in the design has: the outer one's flows and the inner one's would share a
			// tree, and removing either is a question with no answer.
			//
			// An exact slice-prefix test in both directions, which is what the element form is
			// worth here — the string spelling of this question has to work around `studio-ab`
			// looking like a child of `studio-a`.
			if outer, nested := nestedDomain(dst, nestable[dst.Node]); nested {
				out[ref.ID] = Result{
					Code: api.ReasonDomainNameInUse,
					Message: fmt.Sprintf("domain %q on node %q nests with %q, which another path already materialises; an output domain cannot contain another",
						dst.DomainName(), dst.Node, api.DomainPath(outer)),
				}
				continue
			}
		}

		if existing, taken := holder[destFlow]; taken {
			if existing != src {
				out[ref.ID] = Result{
					Code: api.ReasonFlowConflict,
					Message: fmt.Sprintf("%s/%s already receives flow %s from %s/%s; two producers into one flow corrupts the ring buffer",
						dst.Node, dst.DomainName(), src.Flow, existing.Node, existing.Domain),
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
		if dst.Root != "" {
			rootFor[to] = dst.Root
			nestable[dst.Node] = append(nestable[dst.Node], dst.Domain)
		}
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

func rootList(roots []api.OutputRoot) string {
	if len(roots) == 0 {
		return "none"
	}
	names := make([]string, 0, len(roots))
	for _, root := range roots {
		names = append(names, fmt.Sprintf("%q", root.Name))
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
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

// nestedDomain reports whether a destination's domain nests with one already accepted on that
// node, in either direction, and which one.
func nestedDomain(dst api.Destination, accepted [][]string) ([]string, bool) {
	for _, other := range accepted {
		if api.NestedIn(dst.Domain, other) || api.NestedIn(other, dst.Domain) {
			return other, true
		}
	}
	return nil, false
}
