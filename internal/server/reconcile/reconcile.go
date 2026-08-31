// Package reconcile turns desired and observed state into sessions and assignments (§7.3).
//
// [Compute] is a pure function of a fleet snapshot. That is the whole design: it is called by
// the leader's reconcile loop, which writes what it produced, and by the read-only handlers,
// which render it — so what an operator is shown and what the fleet is doing cannot disagree,
// and the answer to "would this request be valid" is computed by the same code that will decide
// it a second later.
//
// # What it does, in order
//
//  1. Validate each request's *destinations* against registrations (§7.2), one at a time. A
//     request fans out (§9.1) and its destinations fail independently: an unusable one makes the
//     request INVALID without stopping its siblings from establishing.
//  2. Expand each valid (request, destination) leg's selector against inventory into paths,
//     deduplicating: N requests naming one edge share one path, one session and one worker pair
//     (§9.1).
//  3. Reject the conflicts only visible across paths — two sources into one destination flow,
//     and loops (§7.2).
//  4. Admit or hold each path. A source that is not being produced starts no workers at all,
//     which is what keeps a dormant flow from costing anything (§11.1).
//  5. Derive the session, reusing the stored one so its negotiated interface config stays
//     pinned for the session's lifetime (§10.4).
//  6. Emit assignments — a target always, an initiator only once an epoch has been reported
//     (§5.3, invariant 3).
//
// # The rule that runs through all of it
//
// **An absence of observation is never evidence of absence.** A node whose agent is not holding
// a lease reports no inventory and no status, and every naive reading of that says "no flows,
// no sessions, tear it all down". An expired lease is not proof that a node's workers stopped
// (§4.2), so paths touching a node that is not live are *frozen*: their sessions are retained
// and their assignments carried forward verbatim, and nothing is withdrawn until there is a
// live agent to observe.
package reconcile

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/negotiate"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
	"github.com/jonasohland/mxl-replicator/internal/server/validate"
)

// Config is the reconciler's policy. Everything here is server configuration; nothing is
// derived from the fleet.
type Config struct {
	Negotiate negotiate.Config

	// IdleTimeout is the worker's no-grain timeout, written into every assignment. Zero means
	// wait indefinitely, which is the normal setting and what makes PAUSED a steady state rather
	// than a ~13 s restart cycle per idle flow (§11.1).
	IdleTimeout time.Duration

	// ConnectTimeout bounds an initiator's connect loop (WRS §3).
	ConnectTimeout time.Duration

	// IdleTeardown is the long-idle threshold: a session whose source has been idle this long
	// has both its workers stopped, leaving the path PAUSED with nothing running. Zero disables
	// teardown, keeping every established session hot indefinitely.
	//
	// It trades resume latency against resource cost, and the default belongs in minutes, not
	// seconds: tearing down eagerly costs a bursty source its first grains on every restart,
	// while never tearing down holds ports, memory registrations and processes for dormant flows
	// (§11.1).
	IdleTeardown time.Duration

	// NoNetworkLatencyMeasurement is written into **both** ends of every session. It must match
	// or the target reports garbage latency with no error at all, which is why it is a
	// server-level setting rather than per-side agent config (§5.5, invariant 8).
	NoNetworkLatencyMeasurement bool

	// DegradedRestarts and FailedRestarts classify a flapping session from restart count in the
	// agent's window — never from an exit code, which classifies nothing (§15.1, invariant 10).
	DegradedRestarts int
	FailedRestarts   int

	// Now is the clock. Injectable so idle teardown and session ages can be tested without
	// sleeping through them.
	Now func() time.Time

	// Idle reports how long a path's source has been idle, and is how the long-idle threshold is
	// measured at all: `producing` is a coarse hysteretic boolean with no timestamp attached
	// (§11.1), so the duration has to be kept by whoever is watching it.
	//
	// Nil means "never long enough", which is the correct reading for a caller that is only
	// rendering state: a follower replica must not conclude that a session the leader is holding
	// should have been torn down.
	Idle func(pathID string, producing bool) time.Duration
}

func (c *Config) setDefaults() {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Idle == nil {
		c.Idle = func(string, bool) time.Duration { return 0 }
	}
	if c.DegradedRestarts <= 0 {
		c.DegradedRestarts = DefaultDegradedRestarts
	}
	if c.FailedRestarts <= 0 {
		c.FailedRestarts = DefaultFailedRestarts
	}
}

// Restart thresholds. A worker that has cycled a few times in the agent's window is flapping; one
// that has cycled ten times is not going to fix itself, and both are surfaced rather than acted
// on — a request is durable intent and is never cancelled because its session is failing (§11).
const (
	DefaultDegradedRestarts = 3
	DefaultFailedRestarts   = 10
)

// Result is one reconcile pass: what should exist, and what to show for it.
type Result struct {
	// Revision is the fleet revision this was computed from.
	Revision int64

	// Sessions and Assignments are the desired contents of the derived key space. [Apply] makes
	// the store match them; a read-only caller ignores them.
	Sessions    map[string]state.SessionRecord
	Assignments map[string]api.AssignmentSet

	// Paths is every path the requests expand onto, keyed by path ID, with its status and the
	// requests that share it.
	Paths map[string]api.Path

	// Requests is each request's aggregate status, keyed by request ID (§9.3).
	Requests map[api.RequestID]api.RequestStatus

	// Structural is the subset of invalidity that refuses a `POST` (§7.2).
	//
	// **Validation is per path, not per request.** A request whose selector expands onto twenty
	// paths, one of which conflicts, is not refused: it reports nineteen paths and one invalid one
	// with its reason. What the `POST` refuses is what is structurally wrong — a destination naming
	// an area no node advertises, a spec that cannot expand at all — and that is exactly a
	// *destination-level* validation failure, which is decidable from the request plus node
	// registrations and says nothing about which flows happen to exist.
	//
	// The conflicts deliberately absent from this map are the ones that depend on the rest of the
	// fleet: [api.ReasonFlowConflict], [api.ReasonLoop] and [api.ReasonNamespaceOverlap]. A
	// selector's expansion is not something its author can enumerate before submitting it, so
	// refusing the whole request for one bad pairing would put the author at the mercy of fleet
	// state they did not write.
	Structural map[api.RequestID]validate.Result

	// Frozen is the set of session IDs whose assignments were carried forward untouched because
	// an endpoint's agent is not currently leased. Reported so the loop can log it: it is the
	// mechanism that keeps a control-plane blip from stopping media, and it should be visible
	// when it engages.
	Frozen []string
}

// SortedPaths returns the paths in a stable order, for rendering.
func (r *Result) SortedPaths() []api.Path {
	out := slices.Collect(maps.Values(r.Paths))
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// pathPlan is one deduplicated edge and everything the requests sharing it agreed on.
type pathPlan struct {
	id       string
	identity state.PathIdentity

	// requests are the IDs sharing this path — the refcount. The path is torn down when the
	// last of them goes away (§3).
	requests []api.RequestID

	// since is the creation time of the earliest request on this path, and decides who loses a
	// conflict: the older path is the one probably already carrying media.
	since time.Time

	// namespace is the partition this path's workers are labelled with (§12). A path shared
	// across namespaces takes the last merged one — arbitrary, but deterministic, which is what
	// matters: a value that differed between replicas would restart workers.
	namespace string

	// The settings the sharing requests agreed on. Where they disagree, the resolution is
	// always the conservative one — see [pathPlan.merge].
	labels     map[string]string
	schedPrio  *int
	pin        api.ProviderPin
	teardown   time.Duration
	noTeardown bool

	// pinConflict records that intersecting the sharing requests' pins left nothing. It cannot
	// be represented by an empty pin, which means "pinned nothing, negotiate freely" — and
	// silently negotiating freely is precisely the substitution §10.4 forbids, arrived at by
	// two requests that each asked for something specific.
	pinConflict bool

	// invalid marks a shadow path: one that only an INVALID request wants. It never gains a new
	// session, but it does retain and report whatever session is already running on it.
	invalid *validate.Result
}

// merge folds one more request into a shared path.
//
// Every conflict resolves toward *not breaking the other request*:
//
//   - Provider pins intersect. The session has to satisfy every request sharing it, so a path
//     shared by a verbs-pinned request and a tcp-pinned one is not viable — reported as such
//     rather than silently satisfying one of them (§10.4).
//   - The highest requested scheduling priority wins, since the workers are shared.
//   - Idle teardown takes the most conservative value: a request asking to keep a bursty feed
//     hot holds the workers up for everyone sharing them.
//   - Labels merge, later request IDs winning. Arbitrary, but deterministic, which is what
//     matters — an assignment that differed between replicas would restart workers.
//
// The pin is passed in rather than read from the record: it is the *destination's* effective pin
// (§10.4), which a request with a per-destination override spells differently for each of its
// legs.
func (p *pathPlan) merge(record state.RequestRecord, pin api.ProviderPin, defaultTeardown time.Duration) {
	p.requests = append(p.requests, record.ID)
	if p.since.IsZero() || record.CreatedAt.Before(p.since) {
		p.since = record.CreatedAt
	}

	spec := record.Spec

	if len(p.requests) == 1 {
		p.pin = slices.Clone(pin)
	} else if !pin.IsEmpty() {
		if p.pin.IsEmpty() {
			p.pin = slices.Clone(pin)
		} else {
			kept := make(api.ProviderPin, 0, len(p.pin))
			for _, provider := range p.pin {
				if pin.Allows(provider) {
					kept = append(kept, provider)
				}
			}
			if len(kept) == 0 {
				p.pinConflict = true
			}
			p.pin = kept
		}
	}

	if spec.SchedPrio != nil && (p.schedPrio == nil || *spec.SchedPrio > *p.schedPrio) {
		p.schedPrio = spec.SchedPrio
	}

	teardown := defaultTeardown
	if spec.IdleTeardown != nil {
		teardown = spec.IdleTeardown.Duration()
	}
	if teardown <= 0 {
		p.noTeardown = true
	} else if teardown > p.teardown {
		p.teardown = teardown
	}

	p.namespace = spec.NamespaceOrDefault()

	if len(spec.Labels) > 0 {
		if p.labels == nil {
			p.labels = map[string]string{}
		}
		maps.Copy(p.labels, spec.Labels)
	}
}

// tornDown reports whether a session on this path has been idle long enough to stop its workers.
func (p *pathPlan) tornDown(idle time.Duration) bool {
	if p.noTeardown || p.teardown <= 0 {
		return false
	}
	return idle >= p.teardown
}

// leg is one (request, destination) pair, with the root resolved and the provider pin already
// reduced to the one that applies to this destination.
//
// A request fans out to many destinations (§9.1) and they are validated, negotiated and expanded
// independently — so this, not the request, is the unit the reconciler works on.
type leg struct {
	request api.RequestID
	record  state.RequestRecord
	dst     api.Destination
	pin     api.ProviderPin
	agreed  negotiate.Result
	bad     *validate.Result
}

// identicalFailures reports whether every leg failed the same way, which means the cause is not
// destination-specific.
func identicalFailures(failures []legFailure) bool {
	for _, failure := range failures[1:] {
		if failure.Result != failures[0].Result {
			return false
		}
	}
	return true
}

// legFailure is one destination a request cannot use, kept per request so that the failure is
// reported even when the leg expands to no paths at all — a request whose source flow does not
// exist yet and whose destination names an area the node does not advertise is INVALID, not
// WAITING, and saying WAITING would let a POST through that §7.2 requires be rejected.
type legFailure struct {
	Destination api.Destination
	Result      validate.Result

	// Structural marks a failure that follows from the request and the node registrations alone,
	// which is the only kind a `POST` refuses (§7.2). Destination validation is structural;
	// losing a namespace overlap is not, because it depends on what *another* request expanded
	// onto and on inventory neither author controls. See [Result.Structural].
	Structural bool
}

// Compute derives everything from one fleet snapshot.
func Compute(fleet *state.Fleet, cfg Config) *Result {
	cfg.setDefaults()

	result := &Result{
		Revision:    fleet.Revision,
		Sessions:    map[string]state.SessionRecord{},
		Assignments: map[string]api.AssignmentSet{},
		Paths:       map[string]api.Path{},
		Requests:    map[api.RequestID]api.RequestStatus{},
		Structural:  map[api.RequestID]validate.Result{},
	}

	// Every registered node gets an assignment set, including an empty one. A node with nothing
	// to do must be told so positively rather than by the absence of a key — the two are
	// indistinguishable to a poll, and the whole not-ready discipline exists because that
	// difference is a fleet-wide outage (§4.2).
	for node := range fleet.Nodes {
		result.Assignments[node] = api.AssignmentSet{Node: node}
	}

	sessionsByPath := indexSessionsByPath(fleet)

	plans := map[string]*pathPlan{}
	requestPaths := map[api.RequestID][]string{}
	invalidLegs := map[api.RequestID][]legFailure{}

	// The expansion depends on the source and its two selectors only, so it is the same for every
	// leg of one request and is worth resolving once — a request with eight destinations would
	// otherwise walk the whole inventory eight times. It is also where the exclusion list comes
	// from, and that has to be per request rather than per leg.
	expansions := map[api.RequestID]expansion{}
	expansionOf := func(record state.RequestRecord) expansion {
		if cached, ok := expansions[record.ID]; ok {
			return cached
		}
		out := expand(fleet, record.Spec)
		expansions[record.ID] = out
		return out
	}

	// The negotiated interface config for a path, kept only for the single-request case — see
	// [negotiatedFor]. Keyed by path rather than by request because a request now contributes one
	// path per destination, and those destinations can negotiate differently.
	negotiated := map[string]negotiate.Result{}

	// A leg is one (request, destination) pair: the unit validation and expansion work on, since
	// a request fans out and its destinations succeed or fail independently (§9.1).
	var valid, invalid []leg
	for _, id := range fleet.SortedRequestIDs() {
		record := fleet.Requests[id].Value
		for _, dst := range record.Spec.Destinations {
			// *The resolved output root used to be written back onto the destination here*, so
			// that a shadow path carried the same identity a real one would. A destination's
			// identity is complete the moment the request is read now — the area is the first
			// segment of the domain's name (§10.6) — so there is nothing left to resolve into it.
			agreed, bad := validate.Destination(record.Spec, dst, fleet, cfg.Negotiate)

			this := leg{request: id, record: record, dst: dst, pin: record.Spec.ProviderFor(dst), agreed: agreed}
			if bad != nil {
				this.bad = bad
				invalid = append(invalid, this)
				invalidLegs[id] = append(invalidLegs[id], legFailure{Destination: dst, Result: *bad, Structural: true})
				continue
			}
			valid = append(valid, this)
		}
	}

	// Two requests in one namespace may not hold one path. Legs that lose that contest are
	// moved across to `invalid` before anything is planned, so the winner's path is built
	// exactly as if the loser had never named it.
	if overlaps := namespaceOverlaps(fleet, valid); len(overlaps) > 0 {
		kept := valid[:0]
		for i, leg := range valid {
			bad, lost := overlaps[i]
			if !lost {
				kept = append(kept, leg)
				continue
			}
			leg.bad = &bad
			invalid = append(invalid, leg)
			invalidLegs[leg.request] = append(invalidLegs[leg.request], legFailure{Destination: leg.dst, Result: bad})
		}
		valid = kept
	}

	// Valid legs first, so that a path a valid one wants is never turned into a shadow path by an
	// invalid leg that happens to name the same edge.
	for _, leg := range valid {
		for _, address := range expansionOf(leg.record).addresses {
			// **A self-pair a label selector produced is elided, not rejected** (§7.2, §10.7). A
			// named source resolving to the destination is a typo and is refused by
			// [validate.Destination]; a *matched* one is the selector doing what it was asked to,
			// and refusing the whole request would put its author at the mercy of which domains
			// happen to carry a label. The rest of the expansion stands.
			if address.Node == leg.dst.Node && address.Domain == leg.dst.DomainName() {
				continue
			}

			identity := state.PathIdentity{Source: address, Destination: leg.dst}
			pid := identity.ID()

			plan := plans[pid]
			if plan == nil {
				plan = &pathPlan{id: pid, identity: identity}
				plans[pid] = plan
			}
			plan.merge(leg.record, leg.pin, cfg.IdleTeardown)
			negotiated[pid] = leg.agreed
			requestPaths[leg.request] = append(requestPaths[leg.request], pid)
		}
	}

	// **An invalid leg stops new sessions being created; it does not stop running ones.**
	//
	// The naive reading of INVALID is "this cannot work, remove it", and applied to a leg whose
	// session is already carrying media that is a teardown triggered by a *registration* changing
	// — an attachment that disappeared while an agent re-probed, an area edited on the destination
	// node. A request is durable intent and the system never cancels one on its
	// behalf (§11); validity governs admission, and cancelling is the user's job.
	//
	// So an invalid leg still expands, onto shadow paths that retain whatever session exists and
	// carry its assignments forward untouched. Its *sibling* legs are unaffected: they were
	// planned above and establish normally, which is the point of validating per destination.
	for _, leg := range invalid {
		verdict := *leg.bad
		for _, address := range expansionOf(leg.record).addresses {
			if address.Node == leg.dst.Node && address.Domain == leg.dst.DomainName() {
				continue
			}

			identity := state.PathIdentity{Source: address, Destination: leg.dst}
			pid := identity.ID()
			requestPaths[leg.request] = append(requestPaths[leg.request], pid)

			if _, wanted := plans[pid]; wanted {
				// Some other leg wants this path and is valid; it decides.
				continue
			}
			plans[pid] = &pathPlan{id: pid, identity: identity, invalid: &verdict, requests: []api.RequestID{leg.request}}
		}
	}

	retain(fleet, plans, requestPaths, sessionsByPath, cfg.IdleTeardown)

	conflicts := validate.Conflicts(conflictRefs(plans))

	builder := &builder{fleet: fleet, cfg: cfg, result: result, sessionsByPath: sessionsByPath}
	for _, pid := range sortedKeys(plans) {
		plan := plans[pid]
		switch bad, ok := conflicts[pid]; {
		case ok:
			builder.invalidPath(plan, bad)
		case plan.invalid != nil:
			builder.invalidPath(plan, *plan.invalid)
		default:
			agreed, bad := negotiatedFor(plan, negotiated, fleet, cfg)
			builder.plan(plan, agreed, bad)
		}
	}

	for node, set := range result.Assignments {
		sorted := set
		sort.Slice(sorted.Assignments, func(i, j int) bool {
			if sorted.Assignments[i].SessionID != sorted.Assignments[j].SessionID {
				return sorted.Assignments[i].SessionID < sorted.Assignments[j].SessionID
			}
			return sorted.Assignments[i].Role < sorted.Assignments[j].Role
		})
		result.Assignments[node] = sorted
	}

	// Every request's expansion, whether or not it produced a leg: a request whose only
	// destination is invalid still has an exclusion list, and an operator looking at why it has no
	// paths needs it.
	for _, id := range fleet.SortedRequestIDs() {
		expansionOf(fleet.Requests[id].Value)
	}
	summarise(result, fleet, requestPaths, invalidLegs, expansions)
	return result
}

// negotiatedFor picks the interface configuration for a path.
//
// A path shared by several requests may have a narrower pin than any one of them (they
// intersect), so it is negotiated once more from the aggregate rather than inheriting whichever
// request happened to be validated first.
func negotiatedFor(plan *pathPlan, perPath map[string]negotiate.Result, fleet *state.Fleet, cfg Config) (negotiate.Result, *validate.Result) {
	if plan.pinConflict {
		return negotiate.Result{}, &validate.Result{
			Code:    api.ReasonPinNotViable,
			Message: "the requests sharing this path request incompatible providers",
		}
	}

	if len(plan.requests) == 1 {
		if result, ok := perPath[plan.id]; ok {
			return result, nil
		}
	}

	src, okSrc := fleet.Nodes[plan.identity.Source.Node]
	dst, okDst := fleet.Nodes[plan.identity.Destination.Node]
	if !okSrc || !okDst {
		return negotiate.Result{}, &validate.Result{
			Code:    api.ReasonNodeNotRegistered,
			Message: "a node on this path is no longer registered",
		}
	}

	result, err := negotiate.Negotiate(src.Value.Capabilities.Fabrics, dst.Value.Capabilities.Fabrics, plan.pin, cfg.Negotiate)
	if err != nil {
		var negErr *negotiate.Error
		if errors.As(err, &negErr) {
			return negotiate.Result{}, &validate.Result{Code: negErr.Code, Message: negErr.Message}
		}
		return negotiate.Result{}, &validate.Result{Code: api.ReasonNoSharedFabric, Message: err.Error()}
	}
	return result, nil
}

// expansion is what a request's source selectors resolve to: the flow addresses to replicate, and
// what was deliberately left out.
type expansion struct {
	addresses []api.FlowAddress

	// excluded is populated **here and nowhere downstream**, because by the time a PathStatus
	// exists the excluded flow is gone: the expander is the only place that ever holds both the
	// match and the reason it was dropped (§9.1).
	excluded []api.Exclusion
	dropped  int
}

func (e *expansion) exclude(node, domain, flow string, reason api.ExclusionReason) {
	if len(e.excluded) >= api.MaxExclusions {
		e.dropped++
		return
	}
	e.excluded = append(e.excluded, api.Exclusion{Node: node, Domain: domain, Flow: flow, Reason: reason})
}

// expand resolves a request's source — first to a set of domains, then to flow addresses within
// them (§9.1, §10.7).
//
// # The domain half
//
// A **named** domain is taken as written: it addresses any domain, including one another request
// replicates into, which is how `A→B→C` is written (§10.6). A **label selector** matches domains
// on the source node by equality, ANDed — and then three things happen that the named form does
// not do, each of which is load-bearing:
//
//  1. The match is intersected with the domains inventory actually reports for that node. A label
//     on an unobserved domain is inert and must expand to nothing rather than to a path that
//     cannot resolve (§10.7).
//  2. Every flow the source node's own target worker is writing is dropped. **That is the whole of
//     the self-amplification guard, and it is one line** — the point of having moved it to the
//     flow, where the directory-granular version was a pruning pass in the agent.
//  3. A pairing whose resolved `(node, domain)` equals a destination's is *elided* rather than
//     refusing the request (§7.2, §10.7): a selector matching the destination's own domain is not
//     a typo, it is the selector doing what it was asked to, and refusing would put its author at
//     the mercy of which domains happen to carry a label. That elision lives in the caller, which
//     is where the destination is known.
//
// Step 1 and step 2 look redundant and are not: step 1 is about a domain that is not *there*,
// step 2 about a flow that must not be *matched*. Collapsing them loses the pending-label case,
// which is the case §10.7's "before or after" is entirely about.
//
// # The flow half
//
// A pinned flow ID always expands to exactly one address per domain, whether or not the flow is
// currently observed: a request naming a specific flow deserves to be told that *that* flow is
// missing, rather than silently having no paths at all. A group hint expands to whatever matches,
// and matching nothing is simply a request with zero paths.
func expand(fleet *state.Fleet, spec api.RequestSpec) expansion {
	src := spec.Source
	var out expansion

	switch src.Domain.Kind() {
	case api.DomainSelectorKindName:
		// Taken as written, and **not** filtered by provenance: naming a domain explicitly reaches
		// everything, which is what keeps chaining possible (§10.7).
		out.addresses = flowsIn(fleet, src, src.Domain.Name.String(), false, &out)

	case api.DomainSelectorKindLabels:
		for _, domain := range matchingDomains(fleet, src) {
			out.addresses = append(out.addresses, flowsIn(fleet, src, domain, true, &out)...)
		}

	default:
		// Not reachable through the API — a selector with no kind cannot be decoded or encoded
		// (§10.7) — but a stored record is not a decode away from anything, so it is handled
		// rather than assumed.
		return expansion{}
	}

	sort.Slice(out.addresses, func(i, j int) bool {
		if out.addresses[i].Domain != out.addresses[j].Domain {
			return out.addresses[i].Domain < out.addresses[j].Domain
		}
		return out.addresses[i].Flow < out.addresses[j].Flow
	})
	return out
}

// matchingDomains is steps 1 and 2 of the domain half: the label match, intersected with what the
// node actually reports.
//
// Ordered, because everything downstream of a reconcile has to be deterministic across replicas.
func matchingDomains(fleet *state.Fleet, src api.Source) []string {
	entry, observing := fleet.Inventory[src.Node]
	if !observing {
		return nil
	}

	var out []string
	for _, observed := range entry.Value.Domains {
		name := observed.Domain.String()
		if src.Domain.Matches(fleet.LabelsFor(src.Node, name)) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// flowsIn resolves the flow selector within one domain.
//
// skipReplicated is step 2 of the domain half — the self-amplification guard — and it is set for a
// label match and clear for a named domain. Its omission is invisible in every test that does not
// involve a node being both a source and a destination (§17), which is why that is the test to
// write first rather than last.
func flowsIn(fleet *state.Fleet, src api.Source, domain string, skipReplicated bool, out *expansion) []api.FlowAddress {
	switch src.Select.Kind() {
	case api.SelectorKindFlow:
		if skipReplicated {
			// A pinned flow expands whether or not it is observed, so the guard can only apply to
			// one this node *is* reporting — which is exactly the case it exists for.
			if flow, observed := fleet.Flow(src.Node, domain, src.Select.Flow); observed && flow.Replicated {
				out.exclude(src.Node, domain, src.Select.Flow, api.ExclusionSelfOutput)
				return nil
			}
		}
		return []api.FlowAddress{{Node: src.Node, Domain: domain, Flow: src.Select.Flow}}

	case api.SelectorKindGroupHint:
		var addresses []api.FlowAddress
		for _, flow := range fleet.Flows(src.Node, domain) {
			if flow.GroupHint == nil || !src.Select.GroupHint.Matches(*flow.GroupHint) {
				continue
			}
			if skipReplicated && flow.Replicated {
				out.exclude(src.Node, domain, flow.ID, api.ExclusionSelfOutput)
				continue
			}
			addresses = append(addresses, api.FlowAddress{Node: src.Node, Domain: domain, Flow: flow.ID})
		}
		return addresses

	default:
		return nil
	}
}

// retain adds paths for stored sessions that nothing expanded onto, when an endpoint's agent is
// not currently leased.
//
// This is the invariant of §4.2 applied to selector expansion. A group-hint request whose source
// node is down expands to nothing, because inventory is leased state and went away with the
// agent — and "no flows matched" is indistinguishable from "I cannot see this node". Without
// this, a control-plane-visible agent restart would withdraw every assignment on that node's
// peers, which is media stopping because a lease expired.
func retain(fleet *state.Fleet, plans map[string]*pathPlan, requestPaths map[api.RequestID][]string, sessionsByPath map[string][]state.SessionRecord, defaultTeardown time.Duration) {
	for _, pid := range sortedKeys(sessionsByPath) {
		if _, planned := plans[pid]; planned {
			continue
		}
		sessions := sessionsByPath[pid]
		if len(sessions) == 0 {
			continue
		}
		identity := sessions[0].Path
		if fleet.Live(identity.Source.Node) && fleet.Live(identity.Destination.Node) {
			// Both agents are reporting and nothing wants this path: it really is gone.
			continue
		}

		plan := &pathPlan{id: pid, identity: identity}
		plans[pid] = plan

		// Attribute the retained path to the requests that plausibly own it, so it appears in
		// their status rather than as an orphan. A group hint cannot be re-evaluated without the
		// inventory that went away with the agent, so a matching source and destination is as
		// close as this can get — and it is only ever used for display.
		for _, rid := range fleet.SortedRequestIDs() {
			spec := fleet.Requests[rid].Value.Spec
			// A named source has to match; a label selector cannot be re-evaluated without the
			// inventory that went away with the agent, so it is taken as plausible. This is only
			// ever used for display (see the comment above).
			if spec.Source.Node != identity.Source.Node {
				continue
			}
			if name := spec.Source.Domain.Name; name != nil && name.String() != identity.Source.Domain {
				continue
			}
			if spec.Source.Select.Kind() == api.SelectorKindFlow && spec.Source.Select.Flow != identity.Source.Flow {
				continue
			}
			// Which of the request's destinations this retained path belongs to, if any. It
			// decides the pin, so it has to be the matching one rather than the request as a
			// whole — a fan-out request can pin verbs for one destination and tcp for another.
			dst, ok := matchingDestination(spec, identity.Destination)
			if !ok {
				continue
			}
			plan.merge(fleet.Requests[rid].Value, spec.ProviderFor(dst), defaultTeardown)
			requestPaths[rid] = append(requestPaths[rid], pid)
		}
	}
}

// matchingDestination finds which of a request's destinations a path identity belongs to.
//
// At most one can match: [api.RequestSpec.Validate] rejects two destinations naming one
// (node, domain), and the root only ever narrows the match further.
func matchingDestination(spec api.RequestSpec, path api.Destination) (api.Destination, bool) {
	for _, dst := range spec.Destinations {
		if sameDestination(dst, path) {
			return dst, true
		}
	}
	return api.Destination{}, false
}

// sameDestination compares a *request's* destination with a *path's*.
//
// *This used to have to allow for a request that spelled no output root matching a path carrying
// the resolved one.* A destination always names its area now (§10.6), so the two are the same
// value and the comparison is an equality.
func sameDestination(spec, path api.Destination) bool {
	return spec.Node == path.Node && spec.Domain.Equal(path.Domain)
}

// namespaceOverlaps finds legs that would put a second request from one namespace onto a path
// another request in that namespace already holds. It returns a verdict per index into `legs`;
// an index absent from the map is fine.
//
// **Opt-in, per namespace** (§9.3). A namespace whose `paths` policy is `shared` — the default,
// and the zero value — is skipped entirely: two of its requests holding one path share one path,
// one session and one worker pair, which is §9.1's refcounting working exactly as designed.
//
// **Why this is a rule at all, and why it is the one that is optional.** A path is refcounted and
// works perfectly well held by two requests — that is what makes fan-in expressible (§9.1,
// §10.6). What overlap breaks is the claim a *namespace* makes: that its requests are a
// partition, so a set of them can be drawn as a matrix where a cell means one edge and clearing
// it stops exactly the paths in it. Sharing inside a namespace makes two cells one edge,
// silently, and the interface has no honest way to draw it.
//
// That is legibility, not integrity, and it gives the governing line: conflict rules that protect
// data integrity are mandatory ([api.ReasonFlowConflict] — two initiators into one ring buffer,
// never optional for anybody); conflict rules that protect legibility belong to whoever is doing
// the reading. Hence a policy on the namespace and a default that does not surprise a client
// which never heard of the rule.
//
// **Precedence is by UpdatedAt, then request ID**, which is not the same choice
// [validate.Conflicts] makes and the difference is deliberate. There it is creation time,
// because the question is which path is probably already carrying media. Here the question is
// who moved: an overlap appears when somebody writes a request, and the writer is the one who
// should hear about it. Using creation time would let an old request be edited to swallow a
// newer one's path, refusing nothing at the POST that caused it and flipping an untouched
// request to INVALID instead. UpdatedAt puts the refusal in front of whoever typed.
//
// It also gives admission for free. A POST stamps UpdatedAt with now, so a new or edited request
// is always the most recent and always the one that loses — which is how `handleCreateRequest`
// comes to reject it with this reason before writing anything.
//
// The two cases it fires on are worth naming, because only one of them is decidable when the
// request is written. A flow carries at most one parsed group hint, so two different hints, two
// different types under one hint, and two different pinned flows can never collide however
// producers behave; the collisions are a hint against a narrower hint of the same name, the same
// flow pinned twice, and a pinned flow against a hint that matches it. That last one is the only
// one that can *arrive* later, when a producer republishes a flow under a new group — which is
// why this lives in the reconcile rather than only in validation.
func namespaceOverlaps(fleet *state.Fleet, legs []leg) map[int]validate.Result {
	type claim struct {
		request api.RequestID
		ns      string
	}

	// Nothing to police in a permissive namespace, and skipping it here rather than filtering the
	// result keeps the expansion work off the common path entirely.
	exclusive := map[string]bool{}
	interesting := false
	for _, leg := range legs {
		ns := leg.record.Spec.NamespaceOrDefault()
		if _, known := exclusive[ns]; known {
			continue
		}
		exclusive[ns] = fleet.Namespace(ns).Paths.Exclusive()
		interesting = interesting || exclusive[ns]
	}
	if !interesting {
		return nil
	}

	order := make([]int, len(legs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := legs[order[a]].record, legs[order[b]].record
		if !x.UpdatedAt.Equal(y.UpdatedAt) {
			return x.UpdatedAt.Before(y.UpdatedAt)
		}
		return x.ID.String() < y.ID.String()
	})

	// The expansion depends on the source and selector only, so it is the same for every leg of
	// one request and is worth resolving once — a request with eight destinations otherwise
	// walks the whole inventory eight times.
	expansions := map[api.RequestID][]api.FlowAddress{}
	expansionOf := func(record state.RequestRecord) []api.FlowAddress {
		if cached, ok := expansions[record.ID]; ok {
			return cached
		}
		addresses := expand(fleet, record.Spec).addresses
		expansions[record.ID] = addresses
		return addresses
	}

	held := map[string]claim{} // path id -> the request in that namespace already on it
	out := map[int]validate.Result{}

	for _, i := range order {
		leg := legs[i]
		ns := leg.record.Spec.NamespaceOrDefault()
		if !exclusive[ns] {
			continue
		}

		for _, address := range expansionOf(leg.record) {
			pid := state.PathIdentity{Source: address, Destination: leg.dst}.ID()

			// Keyed on both, so a path held in one namespace says nothing about another.
			key := pid + "\x00" + ns
			switch prior, taken := held[key]; {
			case !taken:
				held[key] = claim{request: leg.record.ID, ns: ns}
			case prior.request == leg.record.ID:
				// A request cannot collide with itself: two destinations naming one endpoint are
				// refused by Validate, and one selector yields each flow address once.
			default:
				out[i] = validate.Result{
					Code: api.ReasonNamespaceOverlap,
					Message: fmt.Sprintf(
						"request %q already replicates %s/%s %s to %s in namespace %q",
						prior.request.Name, address.Node, address.Domain, address.Flow,
						leg.dst.Endpoint(), ns),
				}
			}
			if _, lost := out[i]; lost {
				break
			}
		}
	}
	return out
}

func conflictRefs(plans map[string]*pathPlan) []validate.PathRef {
	refs := make([]validate.PathRef, 0, len(plans))
	for _, pid := range sortedKeys(plans) {
		plan := plans[pid]
		refs = append(refs, validate.PathRef{ID: plan.id, Path: plan.identity, Since: plan.since})
	}
	return refs
}

func indexSessionsByPath(fleet *state.Fleet) map[string][]state.SessionRecord {
	out := map[string][]state.SessionRecord{}
	for _, id := range sortedKeys(fleet.Sessions) {
		record := fleet.Sessions[id].Value
		pid := record.Path.ID()
		out[pid] = append(out[pid], record)
	}
	return out
}

// builder assembles one path's outcome into the result.
type builder struct {
	fleet          *state.Fleet
	cfg            Config
	result         *Result
	sessionsByPath map[string][]state.SessionRecord
}

func (b *builder) status(plan *pathPlan) api.PathStatus {
	return api.PathStatus{
		ID:          plan.id,
		Source:      plan.identity.Source,
		Destination: plan.identity.Destination,
	}
}

func (b *builder) emit(plan *pathPlan, status api.PathStatus, session *api.Session) {
	// Rendered `<namespace>/<name>`, which is the joinable spelling: a path's refcount list is
	// read against `GET /v1/requests`, and a bare name would be ambiguous across namespaces (§9.3).
	refs := make([]string, 0, len(plan.requests))
	for _, id := range plan.requests {
		refs = append(refs, id.String())
	}
	b.result.Paths[plan.id] = api.Path{
		PathStatus: status,
		Requests:   slices.Compact(slices.Sorted(slices.Values(refs))),
		Session:    session,
	}
}

// negotiationFailures are the reason codes that describe two nodes' current capabilities rather
// than a mistake in the request. They are the ones that can *become* true after a request was
// accepted, and the ones that can become false again on their own.
var negotiationFailures = []api.ReasonCode{
	api.ReasonNoSharedFabric,
	api.ReasonNoSharedProvider,
	api.ReasonNoSharedCapability,
	api.ReasonPinNotViable,
}

// invalidPath reports a path that cannot be established, and — crucially — does not tear down
// one that already is.
func (b *builder) invalidPath(plan *pathPlan, bad validate.Result) {
	status := b.status(plan)
	status.State = api.StateInvalid
	status.Reason = bad.Message
	status.ReasonCode = bad.Code

	sessions := b.sessionsByPath[plan.id]

	// A session that is already running and whose negotiation no longer succeeds is FAILED, not
	// INVALID, and the distinction is the one §11 draws: INVALID never resolves by itself and
	// needs user action, while a fabric that stopped being advertised can come back on its own —
	// an agent re-probing, a card reset, a driver reloaded. Saying INVALID would tell an
	// operator to go and edit a request that is perfectly correct (§10.4).
	if len(sessions) > 0 && slices.Contains(negotiationFailures, bad.Code) {
		status.State = api.StateFailed
		status.ReasonCode = api.ReasonFabricGone
	}

	var session *api.Session
	for _, record := range sessions {
		b.result.Sessions[record.ID] = record
		b.carryForward(record)
		if session == nil {
			status.SessionID = record.ID
			session = b.session(record)
		}
	}

	b.emit(plan, status, session)
}

// plan decides one path: whether a session should exist, what it is, and what the operator is
// told about it.
func (b *builder) plan(plan *pathPlan, negotiated negotiate.Result, bad *validate.Result) {
	if bad != nil {
		b.invalidPath(plan, *bad)
		return
	}

	identity := plan.identity
	src, dst := identity.Source, identity.Destination
	status := b.status(plan)

	// Frozen: an endpoint's agent is not leased, so there is no observation of it — and an
	// expired lease is not proof that its workers stopped (§4.2). Keep every session on this
	// path and carry its assignments forward exactly as they are.
	if !b.fleet.Live(src.Node) || !b.fleet.Live(dst.Node) {
		status.State = api.StateWaiting
		status.ReasonCode = api.ReasonAgentNotLeased
		status.Reason = notLeasedReason(b.fleet, src.Node, dst.Node)

		var session *api.Session
		for _, record := range b.sessionsByPath[plan.id] {
			b.result.Sessions[record.ID] = record
			b.result.Frozen = append(b.result.Frozen, record.ID)
			b.carryForward(record)
			if session == nil {
				status.SessionID = record.ID
				session = b.session(record)
			}
		}
		b.emit(plan, status, session)
		return
	}

	flow, observed := b.fleet.Flow(src.Node, src.Domain, src.Flow)
	if !observed {
		// Both agents are live and the source is not reporting this flow, so it genuinely is not
		// there. Any session for it goes, and with it the workers.
		status.State = api.StateWaiting
		status.ReasonCode = api.ReasonFlowNotFound
		status.Reason = "flow " + src.Flow + " is not present in " + src.Node + "/" + src.Domain
		b.emit(plan, status, nil)
		return
	}

	hash := state.FlowDefHash(flow.Definition)
	sessionID := state.SessionID(identity, hash)
	stored, exists := b.fleet.Sessions[sessionID]

	idle := b.cfg.Idle(plan.id, flow.Producing)
	if !flow.Producing && (!exists || plan.tornDown(idle)) {
		// Admission, and long-idle teardown, which are the same condition seen at two different
		// ages: the source is not sending. Starting workers for a dormant flow costs ports,
		// memory registrations and processes for nothing, and — because every target restart
		// changes the epoch — a full control-plane round trip every ~13 s, forever (§11.1).
		status.State = api.StatePaused
		status.ReasonCode = api.ReasonSourceIdle
		status.Reason = "source flow is not being produced"
		if exists && plan.tornDown(idle) {
			status.Reason = "workers stopped, the source flow has not been produced for " + idle.Round(time.Second).String()
		}
		b.emit(plan, status, nil)
		return
	}

	record := stored.Value
	if !exists {
		record = state.SessionRecord{
			ID:          sessionID,
			Path:        identity,
			FlowDefHash: hash,
			Fabric:      negotiated.Fabric,
			Interface:   negotiated.Interface,
			CreatedAt:   b.cfg.Now(),
		}
	}
	b.result.Sessions[record.ID] = record
	status.SessionID = record.ID

	// The negotiated provider is pinned for the session's lifetime. If the fabric it is using
	// is no longer advertised by both ends, the session fails with a reason — it does **not**
	// re-negotiate onto whatever is left, because a silent downgrade at 3am is the thing §10.4
	// exists to prevent, and a clearly failed path triggers the operator's failover procedure
	// while a struggling one does not.
	if reason := b.fabricGone(record); reason != "" {
		status.State = api.StateFailed
		status.ReasonCode = api.ReasonFabricGone
		status.Reason = reason
		// The workers keep running on the assignment they have: this says the fabric is no
		// longer *advertised*, which is not the same as the established connection being dead.
		b.assign(plan, record, flow)
		b.emit(plan, status, b.session(record))
		return
	}

	b.assign(plan, record, flow)

	session := b.session(record)
	sessionState, reason, code := b.derive(record, flow, session)
	status.State, status.Reason, status.ReasonCode = sessionState, reason, code
	b.emit(plan, status, session)
}

// fabricGone reports whether the session's pinned (provider, fabric) has stopped being
// advertised by either end.
func (b *builder) fabricGone(record state.SessionRecord) string {
	for _, node := range []string{record.Path.Source.Node, record.Path.Destination.Node} {
		entry, ok := b.fleet.Nodes[node]
		if !ok {
			return "node " + node + " is no longer registered"
		}
		if entry.Value.Capabilities.FindFabric(record.Interface.Provider, record.Fabric) == nil {
			return "node " + node + " no longer advertises " + string(record.Interface.Provider) + " on fabric " + record.Fabric
		}
	}
	return ""
}

// session renders what both agents last reported about a session.
func (b *builder) session(record state.SessionRecord) *api.Session {
	out := &api.Session{
		ID:        record.ID,
		Fabric:    record.Fabric,
		Interface: record.Interface,
	}

	if status, ok := b.fleet.SessionStatus(record.Path.Destination.Node, record.ID, api.RoleTarget); ok {
		out.Epoch = status.Epoch
		out.Target = endpointFrom(record.Path.Destination.Node, status)
	}
	if status, ok := b.fleet.SessionStatus(record.Path.Source.Node, record.ID, api.RoleInitiator); ok {
		out.Initiator = endpointFrom(record.Path.Source.Node, status)
	}
	return out
}

func endpointFrom(node string, status api.SessionStatus) *api.SessionEndpoint {
	return &api.SessionEndpoint{
		Node:       node,
		State:      status.State,
		Address:    status.Address,
		Service:    status.Service,
		Restarts:   status.Restarts,
		StartedAt:  status.StartedAt,
		Reason:     status.Reason,
		ReasonCode: status.ReasonCode,
	}
}

// derive is §11 and §4h: what state a session with both its assignments in place is actually in.
//
// The order of the tests is the interesting part. Flapping outranks moving media, because a
// session that is currently transferring but has restarted six times is a problem an operator
// has to see; and ACTIVE is decided from the **destination flow's** head index, never from
// worker accounting, because a worker can report healthy transfers while producing a flow
// nothing can read (§11).
func (b *builder) derive(record state.SessionRecord, sourceFlow api.FlowInventory, session *api.Session) (api.State, string, api.ReasonCode) {
	restarts := 0
	for _, endpoint := range []*api.SessionEndpoint{session.Target, session.Initiator} {
		if endpoint != nil && endpoint.Restarts > restarts {
			restarts = endpoint.Restarts
		}
	}

	switch {
	case restarts >= b.cfg.FailedRestarts:
		return api.StateFailed, restartReason(restarts), api.ReasonWorkerRestarts
	case restarts >= b.cfg.DegradedRestarts:
		return api.StateDegraded, restartReason(restarts), api.ReasonWorkerRestarts
	}

	ready := func(endpoint *api.SessionEndpoint) bool {
		return endpoint != nil && endpoint.State == api.WorkerReady
	}
	if !ready(session.Target) || !ready(session.Initiator) {
		return api.StateEstablishing, establishingReason(session), ""
	}

	dst := record.Path.Destination
	destination, observed := b.fleet.Flow(dst.Node, dst.DomainName(), record.Path.Source.Flow)
	if observed && destination.Producing {
		return api.StateActive, "", ""
	}

	// Established, no media. Which end is quiet is the whole value of PAUSED: it separates "the
	// plumbing is broken" from "the source is not producing", which look identical from a
	// no-media alarm and have completely different owners.
	if !sourceFlow.Producing {
		return api.StatePaused, "source flow is not being produced", api.ReasonSourceIdle
	}
	return api.StatePaused, "the source is producing but the destination flow is not advancing", ""
}

func restartReason(restarts int) string {
	return "worker has restarted " + strconv.Itoa(restarts) + " times in the classification window"
}

func establishingReason(session *api.Session) string {
	switch {
	case session.Target == nil:
		return "waiting for destination to start the target"
	case session.Target.State != api.WorkerReady:
		return "target worker is " + string(session.Target.State)
	case session.Initiator == nil:
		return "waiting for source to start the initiator"
	default:
		return "initiator worker is " + string(session.Initiator.State)
	}
}

// assign emits the worker assignments for a session (§5.3 steps 2 and 5).
func (b *builder) assign(plan *pathPlan, record state.SessionRecord, flow api.FlowInventory) {
	src, dst := record.Path.Source, record.Path.Destination

	common := api.Assignment{
		SessionID:                   record.ID,
		FlowID:                      src.Flow,
		Interface:                   record.Interface,
		Fabric:                      record.Fabric,
		NoNetworkLatencyMeasurement: b.cfg.NoNetworkLatencyMeasurement,
		SchedPrio:                   plan.schedPrio,
		IdleTimeout:                 api.Millis(b.cfg.IdleTimeout),
		ConnectTimeout:              api.Millis(b.cfg.ConnectTimeout),
		Namespace:                   plan.namespace,
		Labels:                      plan.labels,
	}

	target := common
	target.Role = api.RoleTarget
	// **One domain field, the same structured value for both roles** (§10.6). The destination's
	// is the request's own; the source's is looked up from the agent's report below, so that the
	// structure always comes from whoever computed the identity rather than from splitting a
	// rendered string.
	target.Domain = dst.Domain
	target.FlowDef = append(json.RawMessage(nil), flow.Definition...)
	b.append(dst.Node, target)

	// **Never assign an initiator before an epoch has been reported** (§5.3, invariant 3). The
	// ordering is mandatory, not an optimisation: openFlow fails outright if the flow does not
	// exist yet, and the connect loop waits a long time on an endpoint that is not there.
	//
	// Requiring the target to be *ready*, not merely to have reported an epoch once, is what
	// withdraws the initiator while a target is restarting: the blob it holds describes memory
	// registrations that died with the old process, so an initiator left running against it
	// moves no data and reports nothing wrong.
	status, ok := b.fleet.SessionStatus(dst.Node, record.ID, api.RoleTarget)
	if !ok || status.State != api.WorkerReady || status.Epoch == "" || status.TargetInfo == "" {
		return
	}

	// The source domain's structure comes from the source agent's own inventory report. It is
	// there by construction — the path exists because that node reported this flow in this domain
	// — and the fallback is a degenerate one-element domain rather than a parse, because parsing a
	// domain string anywhere outside the manifest is the thing §10.6 forbids.
	sourceDomain, ok := b.fleet.Domain(src.Node, src.Domain)
	if !ok {
		return
	}

	initiator := common
	initiator.Role = api.RoleInitiator
	initiator.Domain = sourceDomain
	initiator.Epoch = status.Epoch
	initiator.TargetInfo = status.TargetInfo
	initiator.Peer = &api.PeerEndpoint{Node: dst.Node, Address: status.Address, Service: status.Service}
	b.append(src.Node, initiator)
}

// carryForward copies a frozen session's existing assignments through untouched.
func (b *builder) carryForward(record state.SessionRecord) {
	for _, node := range []string{record.Path.Source.Node, record.Path.Destination.Node} {
		entry, ok := b.fleet.Assignments[node]
		if !ok {
			continue
		}
		for _, assignment := range entry.Value.Assignments {
			if assignment.SessionID == record.ID {
				b.append(node, assignment)
			}
		}
	}
}

func (b *builder) append(node string, assignment api.Assignment) {
	set, ok := b.result.Assignments[node]
	if !ok {
		// A node with assignments but no registration: it deregistered, or the store lost the
		// key. Emit the set anyway rather than dropping the assignment silently.
		set = api.AssignmentSet{Node: node}
	}
	set.Assignments = append(set.Assignments, assignment)
	b.result.Assignments[node] = set
}

func notLeasedReason(fleet *state.Fleet, src, dst string) string {
	switch {
	case !fleet.Live(src) && !fleet.Live(dst):
		return "neither agent is currently leased"
	case !fleet.Live(src):
		return "source agent " + src + " is not currently leased"
	default:
		return "destination agent " + dst + " is not currently leased"
	}
}

// aggregateOrder is the precedence a request's state is folded to: the first state present among
// its paths wins.
//
// It runs worst-first with one deliberate exception — ESTABLISHING outranks PAUSED and ACTIVE
// but not WAITING. A request with one path coming up and one running is "coming up"; a request
// with one path waiting on a missing flow is waiting, whatever the rest are doing. The counts
// carry the detail an operator actually reads ("1 of 3 active"), so this only has to be
// defensible, not clever.
var aggregateOrder = []api.State{
	api.StateInvalid,
	api.StateFailed,
	api.StateDegraded,
	api.StateWaiting,
	api.StateEstablishing,
	api.StatePaused,
	api.StateActive,
}

func summarise(result *Result, fleet *state.Fleet, requestPaths map[api.RequestID][]string, invalid map[api.RequestID][]legFailure, expansions map[api.RequestID]expansion) {
	for id := range fleet.Requests {
		spec := fleet.Requests[id].Value.Spec

		pathIDs := slices.Compact(slices.Sorted(slices.Values(requestPaths[id])))

		statuses := make([]api.PathStatus, 0, len(pathIDs))
		counts := map[api.State]int{}
		for _, pid := range pathIDs {
			path, ok := result.Paths[pid]
			if !ok {
				continue
			}
			statuses = append(statuses, path.PathStatus)
			counts[path.State]++
		}

		status := api.RequestStatus{
			Paths:           statuses,
			Counts:          counts,
			Excluded:        expansions[id].excluded,
			ExcludedDropped: expansions[id].dropped,
		}

		// What a POST refuses, and only that. A namespace overlap sits in `invalid[id]` beside the
		// structural failures and is deliberately absent here: it makes the request INVALID and
		// does not make it unacceptable, because it depends on another request's expansion (§7.2).
		for _, failure := range invalid[id] {
			if failure.Structural {
				result.Structural[id] = failure.Result
				break
			}
		}

		switch {
		case len(invalid[id]) > 0:
			// **A request with any unusable destination is INVALID, whatever its other
			// destinations are doing.** Its paths and counts are still reported, because the
			// sibling legs establish normally and may well be carrying media — hiding that would
			// make a request that is moving video look like one that never started.
			//
			// Reported from the leg rather than from the paths because a leg that expands to *no*
			// paths still has to say why: a request whose source flow does not exist yet and
			// whose destination advertises no output root is INVALID and must be refused at POST,
			// where reading it off the (empty) path set would call it WAITING and let it through.
			failure := invalid[id][0]
			status.State = api.StateInvalid
			status.ReasonCode = failure.Result.Code
			status.Reason = failure.Result.Message

			// **Only blame a destination when the destination is what failed.** Validation is per
			// leg, but several of the things it checks are about the *source* or the request —
			// an unregistered source node, a sched_prio the source cannot apply — and those fail
			// **every** leg identically. Naming one destination for those would point at the wrong
			// end, and "(and 2 more)" would suggest two further problems rather than the same one
			// counted three times.
			//
			// Both halves are load-bearing: one failing leg among three is destination-specific
			// even though its failures trivially "all match", so the count has to be checked too.
			requestWide := len(invalid[id]) == len(spec.Destinations) && identicalFailures(invalid[id])
			if len(spec.Destinations) > 1 && !requestWide {
				status.Reason = fmt.Sprintf("destination %s: %s", failure.Destination.Endpoint(), failure.Result.Message)
				if extra := len(invalid[id]) - 1; extra > 0 {
					status.Reason += fmt.Sprintf(" (and %d more destination(s))", extra)
				}
			}

		case len(statuses) == 0:
			// A selector that matched nothing is a request with zero paths (§9.1) — but if the
			// source agent is not reporting at all, that is not the same statement, and saying
			// "matched nothing" would be a claim this server is in no position to make.
			status.State = api.StateWaiting
			if !fleet.Live(spec.Source.Node) {
				status.ReasonCode = api.ReasonAgentNotLeased
				status.Reason = "source agent " + spec.Source.Node + " is not currently leased"
			} else {
				status.ReasonCode = api.ReasonFlowNotFound
				status.Reason = "the selector matches no flow in " + spec.Source.Node + "/" + spec.Source.Domain.String()
			}
		default:
			for _, candidate := range aggregateOrder {
				if counts[candidate] == 0 {
					continue
				}
				status.State = candidate
				for _, path := range statuses {
					if path.State == candidate {
						status.Reason, status.ReasonCode = path.Reason, path.ReasonCode
						break
					}
				}
				break
			}
		}

		result.Requests[id] = status
	}
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
