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
//  1. Validate each request's *(source, destination) pairings* against registrations (§7.2), one at
//     a time. A request fans in and out (§9.1) and its pairings fail independently: an unusable one
//     makes the request INVALID without stopping its siblings from establishing.
//  2. Expand each valid leg's source selectors against inventory into paths, deduplicating: N
//     requests naming one edge share one path, one session and one worker pair (§9.1).
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
	"strings"
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

// leg is one **(request, source, destination) pairing**, with the provider pin already reduced to
// the one that applies to this destination.
//
// A request fans in and out (§9.1) and its pairings are validated, negotiated and expanded
// independently — so this, not the request, is the unit the reconciler works on. A request with two
// sources and three destinations has six legs.
//
// *This used to be a (request, destination) pair*, and it was that only because there was one
// source. Validation and negotiation were always per pairing in substance: an interface config is
// negotiated for a session, and a session has two ends (§10.3).
type leg struct {
	request api.RequestID
	record  state.RequestRecord

	// srcIndex and dstIndex are positions in the spec's two lists. Carried alongside the values
	// because a source has no name of its own, so its position is the only handle a message or a
	// status row has on it (§9.1).
	srcIndex int
	dstIndex int

	src    api.Source
	dst    api.Destination
	pin    api.ProviderPin
	agreed negotiate.Result
	bad    *validate.Result
}

// identicalFailures reports whether every leg failed the same way, which means the cause is neither
// source- nor destination-specific.
func identicalFailures(failures []legFailure) bool {
	for _, failure := range failures[1:] {
		if failure.Result != failures[0].Result {
			return false
		}
	}
	return true
}

// legFailure is one pairing a request cannot use, kept per request so that the failure is
// reported even when the leg expands to no paths at all — a request whose source flow does not
// exist yet and whose destination names an area the node does not advertise is INVALID, not
// WAITING, and saying WAITING would let a POST through that §7.2 requires be rejected.
type legFailure struct {
	SourceIndex int
	Source      api.Source
	Destination api.Destination
	Result      validate.Result

	// Structural marks a failure that follows from the request and the node registrations alone,
	// which is the only kind a `POST` refuses (§7.2). Pairing validation is structural;
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

	// A failure of the request as a whole rather than of one of its pairings ([validate.Request]).
	// Kept apart from `invalidLegs` because it belongs to no leg and must not be attributed to one:
	// naming a destination for `duplicate_source_flow` would send an operator to a node that is
	// working fine, when the thing to change is two lines of the request (§9.1).
	requestInvalid := map[api.RequestID]validate.Result{}

	// Where each leg's paths went, so the status can be folded per source as well as over the
	// request (§9.1, §11). Only the source index is keyed on — a source's row aggregates over every
	// destination it was paired with, which is the grouping that answers "which camera is dark".
	sourcePaths := map[api.RequestID]map[int][]string{}
	addSourcePath := func(id api.RequestID, srcIndex int, pid string) {
		byIndex := sourcePaths[id]
		if byIndex == nil {
			byIndex = map[int][]string{}
			sourcePaths[id] = byIndex
		}
		byIndex[srcIndex] = append(byIndex[srcIndex], pid)
	}

	// The expansion depends on **one source** and its two selectors only, so it is the same for
	// every destination that source is paired with and is worth resolving once — a request with
	// eight destinations would otherwise walk the whole inventory eight times per source.
	//
	// Keyed by (request, source index) rather than by request, which is the whole of the fan-in
	// change here: the sources of one request expand independently and there is no longer one answer
	// to cache for the request as a whole.
	//
	// The exclusion list is still per *request*, because it is what the request's status reports and
	// an operator reading it wants one list, not one per source. It is accumulated across the
	// sources into `excluded` below.
	type expansionKey struct {
		request api.RequestID
		source  int
	}
	expansions := map[expansionKey]expansion{}
	expansionOf := func(record state.RequestRecord, srcIndex int) expansion {
		key := expansionKey{request: record.ID, source: srcIndex}
		if cached, ok := expansions[key]; ok {
			return cached
		}
		out := expand(fleet, record.Spec.Sources[srcIndex])
		expansions[key] = out
		return out
	}

	// The negotiated interface config for a path, kept only for the single-request case — see
	// [negotiatedFor]. Keyed by path rather than by request because a request contributes one path
	// per pairing, and those pairings can negotiate differently.
	negotiated := map[string]negotiate.Result{}

	// A leg is one (request, source, destination) pairing: the unit validation and expansion work
	// on, since a request fans in and out and its pairings succeed or fail independently (§9.1).
	var valid, invalid []leg
	for _, id := range fleet.SortedRequestIDs() {
		record := fleet.Requests[id].Value

		// A request-wide failure is not attributable to any pairing, so it is recorded once and the
		// legs are still built: `duplicate_source_flow` refuses the POST, and on a stored request it
		// must not tear down sessions that are running (§7.2, and the same argument the invalid-leg
		// loop below makes at greater length).
		if bad := validate.Request(record.Spec, fleet); bad != nil {
			result.Structural[id] = *bad
			requestInvalid[id] = *bad
		}

		for srcIndex, src := range record.Spec.Sources {
			for dstIndex, dst := range record.Spec.Destinations {
				// **A parked destination is not a pairing** (§7.2, §9.1). It produces no leg, so it
				// is validated against nothing, expands to nothing and carries no session — which is
				// the whole of the behaviour, and it is deliberately here rather than deeper: past
				// this point a leg is a thing the fleet is being asked for, and a parked one is not
				// being asked for at all. Its codes are reported when it is enabled, and
				// `?dry_run=true` is how an operator finds out before flipping it.
				if dst.Disabled {
					continue
				}
				// *The resolved output root used to be written back onto the destination here*, so
				// that a shadow path carried the same identity a real one would. A destination's
				// identity is complete the moment the request is read now — the area is the first
				// segment of the domain's name (§10.6) — so there is nothing left to resolve into it.
				agreed, bad := validate.Pairing(record.Spec, srcIndex, dstIndex, fleet, cfg.Negotiate)

				this := leg{
					request:  id,
					record:   record,
					srcIndex: srcIndex,
					dstIndex: dstIndex,
					src:      src,
					dst:      dst,
					pin:      record.Spec.ProviderFor(dst),
					agreed:   agreed,
				}
				if bad != nil {
					this.bad = bad
					invalid = append(invalid, this)
					invalidLegs[id] = append(invalidLegs[id], legFailure{
						SourceIndex: srcIndex, Source: src, Destination: dst, Result: *bad, Structural: true,
					})
					continue
				}
				valid = append(valid, this)
			}
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
			invalidLegs[leg.request] = append(invalidLegs[leg.request], legFailure{
				SourceIndex: leg.srcIndex, Source: leg.src, Destination: leg.dst, Result: bad,
			})
		}
		valid = kept
	}

	// Valid legs first, so that a path a valid one wants is never turned into a shadow path by an
	// invalid leg that happens to name the same edge.
	for _, leg := range valid {
		for _, address := range expansionOf(leg.record, leg.srcIndex).addresses {
			// **A self-pair a label selector produced is elided, not rejected** (§7.2, §10.7). A
			// named source resolving to the destination is a typo and is refused by
			// [validate.Pairing]; a *matched* one is the selector doing what it was asked to,
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
			addSourcePath(leg.request, leg.srcIndex, pid)
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
	// planned above and establish normally, which is the point of validating per pairing.
	for _, leg := range invalid {
		verdict := *leg.bad
		for _, address := range expansionOf(leg.record, leg.srcIndex).addresses {
			if address.Node == leg.dst.Node && address.Domain == leg.dst.DomainName() {
				continue
			}

			identity := state.PathIdentity{Source: address, Destination: leg.dst}
			pid := identity.ID()
			requestPaths[leg.request] = append(requestPaths[leg.request], pid)
			addSourcePath(leg.request, leg.srcIndex, pid)

			if _, wanted := plans[pid]; wanted {
				// Some other leg wants this path and is valid; it decides.
				continue
			}
			plans[pid] = &pathPlan{id: pid, identity: identity, invalid: &verdict, requests: []api.RequestID{leg.request}}
		}
	}

	retain(fleet, plans, requestPaths, addSourcePath, sessionsByPath, cfg.IdleTeardown)

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

	// Every source's expansion, whether or not it produced a leg: a request whose only
	// destination is invalid still has an exclusion list, and an operator looking at why it has no
	// paths needs it.
	//
	// The request's list is the concatenation over its sources, in source order, and the cap is
	// applied to the joined list rather than per source — `excluded_dropped` counts what an operator
	// is not being shown, and a per-source cap would let twelve sources show 12×32 entries while
	// each claimed to have dropped nothing (§9.1).
	excluded := map[api.RequestID]expansion{}
	for _, id := range fleet.SortedRequestIDs() {
		record := fleet.Requests[id].Value
		var joined expansion
		for srcIndex := range record.Spec.Sources {
			joined.absorb(expansionOf(record, srcIndex))
		}
		excluded[id] = joined
	}

	summarise(result, fleet, requestPaths, sourcePaths, invalidLegs, requestInvalid, excluded)
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

// expansion is what **one source's** selectors resolve to: the flow addresses to replicate, and
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

// absorb folds one source's expansion into a request-wide one, for the status the request reports
// (§9.1).
//
// The cap is re-applied as entries arrive rather than being a per-source limit, so a request with
// twelve sources shows [api.MaxExclusions] entries in total and counts the rest — a per-source cap
// would show 12× that while every source claimed to have dropped nothing.
//
// Addresses are deliberately *not* folded: nothing needs a request's addresses in one list, and
// producing one would lose which source each came from, which is what the per-source status and the
// "name the right end" rule both rest on (§11).
func (e *expansion) absorb(other expansion) {
	for _, entry := range other.excluded {
		e.exclude(entry.Node, entry.Domain, entry.Flow, entry.Reason)
	}
	e.dropped += other.dropped
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
// missing, rather than silently having no paths at all. A group hint and `all` expand to whatever
// matches, and matching nothing is simply a request with zero paths.
//
// **One source, not the request.** A request's sources expand independently (§9.1), and folding
// them here would lose which source produced which address — which is exactly what the per-source
// status breakdown and the "name the right end" rule for failures both need (§11).
func expand(fleet *state.Fleet, src api.Source) expansion {
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

	case api.SelectorKindGroupHint, api.SelectorKindAll:
		// `all` is a group hint that matches everything, and it is written as one branch rather than
		// two because the two differ in a predicate and nothing else — the self-output guard, the
		// "matched nothing is zero paths" behaviour and the ordering are all the same, and splitting
		// them is how the guard gets added to one and not the other (§10.7).
		matches := func(flow api.FlowInventory) bool {
			if src.Select.Kind() == api.SelectorKindAll {
				return true
			}
			return flow.GroupHint != nil && src.Select.GroupHint.Matches(*flow.GroupHint)
		}

		var addresses []api.FlowAddress
		for _, flow := range fleet.Flows(src.Node, domain) {
			if !matches(flow) {
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
func retain(
	fleet *state.Fleet,
	plans map[string]*pathPlan,
	requestPaths map[api.RequestID][]string,
	attribute func(id api.RequestID, srcIndex int, pid string),
	sessionsByPath map[string][]state.SessionRecord,
	defaultTeardown time.Duration,
) {
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

			srcIndex, ok := matchingSource(spec, identity.Source)
			if !ok {
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
			attribute(rid, srcIndex, pid)
		}
	}
}

// matchingSource finds which of a request's sources a path identity plausibly came from, and is
// **display-only** — see [retain], its one caller.
//
// "Plausibly", because this runs precisely when the inventory that would answer it properly has
// gone away with a lease. A named source has to match exactly; a label selector cannot be
// re-evaluated at all, so it is taken as plausible rather than dropped, which is the direction that
// keeps a retained path visible in the status of the request that is probably holding it.
//
// The **first** match wins where the first destination match is unique, and that asymmetry is
// deliberate rather than an oversight: [api.RequestSpec.Validate] rejects two identical sources, but
// two *different* ones can both plausibly match the same address once a label selector is involved,
// and there is nothing here that could choose between them. Picking the first keeps the answer
// deterministic, and it is only deciding which row of a status display a frozen path appears in.
func matchingSource(spec api.RequestSpec, path api.FlowAddress) (int, bool) {
	for i, src := range spec.Sources {
		if src.Node != path.Node {
			continue
		}
		if name := src.Domain.Name; name != nil && name.String() != path.Domain {
			continue
		}
		if src.Select.Kind() == api.SelectorKindFlow && src.Select.Flow != path.Flow {
			continue
		}
		return i, true
	}
	return 0, false
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

	// The expansion depends on one source and its selectors only, so it is the same for every
	// destination that source is paired with and is worth resolving once — a request with eight
	// destinations otherwise walks the whole inventory eight times per source.
	type expansionKey struct {
		request api.RequestID
		source  int
	}
	expansions := map[expansionKey][]api.FlowAddress{}
	expansionOf := func(record state.RequestRecord, srcIndex int) []api.FlowAddress {
		key := expansionKey{request: record.ID, source: srcIndex}
		if cached, ok := expansions[key]; ok {
			return cached
		}
		addresses := expand(fleet, record.Spec.Sources[srcIndex]).addresses
		expansions[key] = addresses
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

		for _, address := range expansionOf(leg.record, leg.srcIndex) {
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
		PathID:                      plan.id,
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
//
// **The two ends may be one node**, and the loop below reads every assignment that node holds for
// the session rather than the one belonging to this end — so visiting it twice copies both roles
// twice. Each reconcile pass reads back what the last one wrote, so that is not an off-by-one but
// a doubling: 2^n after n passes, and a same-node session left running for a couple of minutes
// grows the assignment document into the hundreds of megabytes and takes the server out with it.
//
// Deduplicating here rather than in [builder.append] on purpose: append is also the path a fresh
// plan takes, where a node legitimately receives two assignments for one session — the target and
// the initiator of a loopback — and a dedup there could not tell the two cases apart.
func (b *builder) carryForward(record state.SessionRecord) {
	nodes := []string{record.Path.Source.Node}
	if record.Path.Destination.Node != record.Path.Source.Node {
		nodes = append(nodes, record.Path.Destination.Node)
	}

	for _, node := range nodes {
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
//
// [api.StatePartial] is deliberately absent: it is not a state a path can be in, so it cannot be
// found among them. It is applied on top of this fold, in [fold], where the whole set is visible.
var aggregateOrder = []api.State{
	api.StateInvalid,
	api.StateFailed,
	api.StateDegraded,
	api.StateWaiting,
	api.StateEstablishing,
	api.StatePaused,
	api.StateActive,
}

// verdict is what one set of paths and leg failures folds to: a state and the reason for it.
type verdict struct {
	state  api.State
	reason string
	code   api.ReasonCode
}

func summarise(
	result *Result,
	fleet *state.Fleet,
	requestPaths map[api.RequestID][]string,
	sourcePaths map[api.RequestID]map[int][]string,
	invalid map[api.RequestID][]legFailure,
	requestInvalid map[api.RequestID]validate.Result,
	expansions map[api.RequestID]expansion,
) {
	for id := range fleet.Requests {
		spec := fleet.Requests[id].Value.Spec

		statuses, counts := collectPaths(result, requestPaths[id])

		status := api.RequestStatus{
			Paths:           statuses,
			Counts:          counts,
			Excluded:        expansions[id].excluded,
			ExcludedDropped: expansions[id].dropped,
		}

		// What a POST refuses, and only that. A namespace overlap sits in `invalid[id]` beside the
		// structural failures and is deliberately absent here: it makes the request INVALID and
		// does not make it unacceptable, because it depends on another request's expansion (§7.2).
		// A request-wide failure is already in `result.Structural`, written where it was found.
		for _, failure := range invalid[id] {
			if failure.Structural {
				result.Structural[id] = failure.Result
				break
			}
		}

		// A failure of the request as a whole outranks its legs' — it is about the request body, so
		// no leg's message would name the thing to change (§7.2).
		blocking := invalid[id]
		var whole *validate.Result
		if bad, ok := requestInvalid[id]; ok {
			whole = &bad
		}

		decided := fold(spec.Sources, spec.Destinations, statuses, counts, blocking, whole, fleet)
		status.State, status.Reason, status.ReasonCode = decided.state, decided.reason, decided.code

		// The per-source breakdown, in the order the request lists its sources and including the
		// ones that produced nothing — a source that expanded to no paths is exactly the one an
		// operator is looking for (§9.1, §11).
		//
		// Folded by the same rules as the request, one source wide. A request-wide failure is
		// deliberately not pushed down onto the rows: `duplicate_source_flow` implicates two sources
		// and blaming either alone would be a claim the request body does not support.
		for i, src := range spec.Sources {
			rowPaths := slices.Compact(slices.Sorted(slices.Values(sourcePaths[id][i])))
			rowStatuses, rowCounts := collectPaths(result, rowPaths)

			var rowFailures []legFailure
			for _, failure := range invalid[id] {
				if failure.SourceIndex == i {
					rowFailures = append(rowFailures, failure)
				}
			}

			row := fold([]api.Source{src}, spec.Destinations, rowStatuses, rowCounts, rowFailures, nil, fleet)
			status.Sources = append(status.Sources, api.SourceStatus{
				Source:     src,
				State:      row.state,
				Reason:     row.reason,
				ReasonCode: row.code,
				Counts:     rowCounts,
				Paths:      pathIDsOf(rowStatuses),
			})
		}

		result.Requests[id] = status
	}
}

// fold reduces a set of paths and the failures of the legs that produced them to one state (§11).
//
// Used for a request and for each of its sources, which is the point: an operator reading a source
// row is asking the same question one level down, and two folds would answer it two different ways.
func fold(
	sources []api.Source,
	destinations []api.Destination,
	statuses []api.PathStatus,
	counts map[api.State]int,
	failures []legFailure,
	whole *validate.Result,
	fleet *state.Fleet,
) verdict {
	var worst verdict

	switch {
	case !api.AnyEnabled(destinations):
		// **Everything is parked, so this is asking for nothing** (§9.1, §11). First in the switch
		// rather than last: with no enabled destination there are no legs to have failed and no
		// paths to fold, so every branch below would reach the empty-set case and call it WAITING —
		// which claims a flow is missing and that this resolves by itself, and both are false.
		//
		// It applies to a source row as well as to the request, because a row of a fully parked
		// request is dark for this reason and not for one of its own.
		worst = verdict{
			state:  api.StateDisabled,
			reason: disabledReason(destinations),
			code:   api.ReasonAllDestinationsDisabled,
		}

	case whole != nil:
		worst = verdict{state: api.StateInvalid, reason: whole.Message, code: whole.Code}

	case len(failures) > 0:
		// **Any unusable pairing makes this INVALID, whatever the others are doing.** Its paths and
		// counts are still reported, because the sibling legs establish normally and may well be
		// carrying media — hiding that would make a request that is moving video look like one that
		// never started.
		//
		// Reported from the leg rather than from the paths because a leg that expands to *no* paths
		// still has to say why: a request whose source flow does not exist yet and whose destination
		// advertises no output root is INVALID and must be refused at POST, where reading it off the
		// (empty) path set would call it WAITING and let it through.
		worst = verdict{
			state:  api.StateInvalid,
			reason: describeFailures(failures, len(sources), len(destinations)),
			code:   failures[0].Result.Code,
		}

	case len(statuses) == 0:
		// A selector that matched nothing is zero paths (§9.1) — but if a source's agent is not
		// reporting at all, that is not the same statement, and "matched nothing" would be a claim
		// this server is in no position to make.
		worst = noPaths(sources, fleet)

	default:
		for _, candidate := range aggregateOrder {
			if counts[candidate] == 0 {
				continue
			}
			worst = verdict{state: candidate}
			for _, path := range statuses {
				if path.State == candidate {
					worst.reason, worst.code = path.Reason, path.ReasonCode
					break
				}
			}
			break
		}
	}

	// **PARTIAL: some of what was asked for is working and some is not** (§11). It outranks
	// everything `worst` can hold, INVALID included, because §7.2 already settled that one bad path
	// among twenty does not condemn the other nineteen — and the top line is exactly where promoting
	// it would undo that.
	//
	// The condition is *disagreement with something working*, not merely "not all active": a set
	// where nothing is ACTIVE folds worst-first as before, because PARTIAL claims something is
	// working and must not be said when nothing is.
	active := counts[api.StateActive]
	if active > 0 && (active < len(statuses) || len(failures) > 0 || whole != nil) {
		return verdict{
			state:  api.StatePartial,
			reason: fmt.Sprintf("%d of %d paths active; %s: %s", active, len(statuses), worst.state, worst.reason),
			code:   worst.code,
		}
	}
	return worst
}

// disabledReason says which legs are parked, which is the one thing an operator reading DISABLED
// still has to be told (§9.1).
//
// The state already says the request is off; what it does not say is *what would come back*, and for
// anything past one destination that is the question — a request parked in June is read in September
// by somebody deciding whether to switch it on. Named rather than counted for that reason, and capped
// so a twelve-destination request does not put a paragraph in a status column.
func disabledReason(destinations []api.Destination) string {
	const limit = 3

	names := make([]string, 0, len(destinations))
	for _, dst := range destinations {
		names = append(names, dst.Endpoint())
	}

	switch {
	case len(names) == 1:
		return "disabled: " + names[0]
	case len(names) > limit:
		return fmt.Sprintf("disabled: %s and %d more",
			strings.Join(names[:limit], ", "), len(names)-limit)
	default:
		return "disabled: " + strings.Join(names, ", ")
	}
}

// noPaths explains an empty path set, which is a legitimate steady state and not a failure.
func noPaths(sources []api.Source, fleet *state.Fleet) verdict {
	var dark []api.Source
	for _, src := range sources {
		if !fleet.Live(src.Node) {
			dark = append(dark, src)
		}
	}

	if len(dark) > 0 {
		reason := "source agent " + dark[0].Node + " is not currently leased"
		if extra := len(dark) - 1; extra > 0 {
			reason += fmt.Sprintf(" (and %d more source(s))", extra)
		}
		return verdict{state: api.StateWaiting, reason: reason, code: api.ReasonAgentNotLeased}
	}

	reason := "the selector matches no flow in " + sources[0].Describe()
	if len(sources) > 1 {
		reason = fmt.Sprintf("the selectors match no flow in any of the %d sources", len(sources))
	}
	return verdict{state: api.StateWaiting, reason: reason, code: api.ReasonFlowNotFound}
}

// describeFailures renders the reason a set of leg failures produces, attributing it to the **end**
// it belongs to (§9.1, §11).
//
// Three-way now that both ends are lists, and each way is a different node for an operator to go
// and look at:
//
//   - Every pairing failed identically — the cause is the request, not either end. An unregistered
//     node named by nothing else, a `sched_prio` no participant can apply. Named plainly, with no
//     prefix and no "(and 2 more)", which would suggest further problems rather than the same one
//     counted six times.
//   - One source failed against every destination — the cause is that source. This is the case
//     fan-in adds, and getting it wrong is the expensive kind of wrong: naming a destination for a
//     dark camera sends an operator to the receiving node, where everything is fine.
//   - One destination failed against every source — the cause is that destination. The case that
//     existed before, unchanged.
//
// Anything else is genuinely per pairing and names both ends of the first, since there is no
// smaller true statement to make.
func describeFailures(failures []legFailure, sources, destinations int) string {
	if len(failures) == 0 {
		return ""
	}
	first := failures[0]

	if len(failures) == sources*destinations && identicalFailures(failures) {
		return first.Result.Message
	}

	failingSources := map[int]bool{}
	failingDestinations := map[string]bool{}
	for _, failure := range failures {
		failingSources[failure.SourceIndex] = true
		failingDestinations[failure.Destination.Endpoint()] = true
	}

	var prefix string
	switch {
	case sources > 1 && len(failingSources) == 1 && len(failures) == destinations:
		prefix = "source " + first.Source.Describe()
	case destinations > 1 && len(failingDestinations) == 1 && len(failures) == sources:
		prefix = "destination " + first.Destination.Endpoint()
	case sources > 1 && destinations > 1:
		prefix = first.Source.Describe() + " → " + first.Destination.Endpoint()
	case sources > 1:
		prefix = "source " + first.Source.Describe()
	case destinations > 1:
		prefix = "destination " + first.Destination.Endpoint()
	}

	reason := first.Result.Message
	if prefix != "" {
		reason = prefix + ": " + reason
	}
	if extra := len(failures) - 1; extra > 0 {
		reason += fmt.Sprintf(" (and %d more)", extra)
	}
	return reason
}

// collectPaths resolves path IDs to their statuses, deduplicated and in a stable order, and counts
// them by state.
func collectPaths(result *Result, ids []string) ([]api.PathStatus, map[api.State]int) {
	unique := slices.Compact(slices.Sorted(slices.Values(ids)))

	statuses := make([]api.PathStatus, 0, len(unique))
	counts := map[api.State]int{}
	for _, pid := range unique {
		path, ok := result.Paths[pid]
		if !ok {
			continue
		}
		statuses = append(statuses, path.PathStatus)
		counts[path.State]++
	}
	return statuses, counts
}

func pathIDsOf(statuses []api.PathStatus) []string {
	out := make([]string, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, status.ID)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
