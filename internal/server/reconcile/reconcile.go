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
//  1. Validate each request against registrations (§7.2). An INVALID request expands to
//     nothing.
//  2. Expand each valid request's selector against inventory into paths, deduplicating: N
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

	// Requests is each request's aggregate status, keyed by request ID.
	Requests map[string]api.RequestStatus

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
	requests []string

	// since is the creation time of the earliest request on this path, and decides who loses a
	// conflict: the older path is the one probably already carrying media.
	since time.Time

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
func (p *pathPlan) merge(record state.RequestRecord, defaultTeardown time.Duration) {
	p.requests = append(p.requests, record.ID)
	if p.since.IsZero() || record.CreatedAt.Before(p.since) {
		p.since = record.CreatedAt
	}

	spec := record.Spec

	if len(p.requests) == 1 {
		p.pin = slices.Clone(spec.Provider)
	} else if !spec.Provider.IsEmpty() {
		if p.pin.IsEmpty() {
			p.pin = slices.Clone(spec.Provider)
		} else {
			kept := make(api.ProviderPin, 0, len(p.pin))
			for _, provider := range p.pin {
				if spec.Provider.Allows(provider) {
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

// Compute derives everything from one fleet snapshot.
func Compute(fleet *state.Fleet, cfg Config) *Result {
	cfg.setDefaults()

	result := &Result{
		Revision:    fleet.Revision,
		Sessions:    map[string]state.SessionRecord{},
		Assignments: map[string]api.AssignmentSet{},
		Paths:       map[string]api.Path{},
		Requests:    map[string]api.RequestStatus{},
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
	requestPaths := map[string][]string{}
	invalidRequests := map[string]validate.Result{}
	negotiated := map[string]negotiate.Result{}

	// Valid requests first, so that a path a valid request wants is never turned into a shadow
	// path by an invalid one that happens to name the same edge.
	for _, id := range sortedKeys(fleet.Requests) {
		record := fleet.Requests[id].Value

		agreed, bad := validate.Spec(record.Spec, fleet, cfg.Negotiate)
		if bad != nil {
			invalidRequests[id] = *bad
			continue
		}
		negotiated[id] = agreed

		for _, address := range expand(fleet, record.Spec) {
			identity := state.PathIdentity{Source: address, Destination: record.Spec.Destination}
			pid := identity.ID()

			plan := plans[pid]
			if plan == nil {
				plan = &pathPlan{id: pid, identity: identity}
				plans[pid] = plan
			}
			plan.merge(record, cfg.IdleTeardown)
			requestPaths[id] = append(requestPaths[id], pid)
		}
	}

	// **An invalid request stops new sessions being created; it does not stop running ones.**
	//
	// The naive reading of INVALID is "this cannot work, remove it", and applied to a request
	// whose session is already carrying media that is a teardown triggered by a *registration*
	// changing — an attachment that disappeared while an agent re-probed, a domain mapping
	// edited on the destination node. A request is durable intent and the system never cancels
	// one on its behalf (§11); validity governs admission, and cancelling is the user's job.
	//
	// So an invalid request still expands, onto shadow paths that retain whatever session exists
	// and carry its assignments forward untouched.
	for _, id := range sortedKeys(invalidRequests) {
		record := fleet.Requests[id].Value
		verdict := invalidRequests[id]

		for _, address := range expand(fleet, record.Spec) {
			identity := state.PathIdentity{Source: address, Destination: record.Spec.Destination}
			pid := identity.ID()
			requestPaths[id] = append(requestPaths[id], pid)

			if _, wanted := plans[pid]; wanted {
				// Some other request wants this path and is valid; it decides.
				continue
			}
			plans[pid] = &pathPlan{id: pid, identity: identity, invalid: &verdict, requests: []string{id}}
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

	summarise(result, fleet, requestPaths, invalidRequests)
	return result
}

// negotiatedFor picks the interface configuration for a path.
//
// A path shared by several requests may have a narrower pin than any one of them (they
// intersect), so it is negotiated once more from the aggregate rather than inheriting whichever
// request happened to be validated first.
func negotiatedFor(plan *pathPlan, perRequest map[string]negotiate.Result, fleet *state.Fleet, cfg Config) (negotiate.Result, *validate.Result) {
	if plan.pinConflict {
		return negotiate.Result{}, &validate.Result{
			Code:    api.ReasonPinNotViable,
			Message: "the requests sharing this path pin providers with nothing in common; one session cannot satisfy both",
		}
	}

	if len(plan.requests) == 1 {
		if result, ok := perRequest[plan.requests[0]]; ok {
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

// expand turns a selector into source flow addresses (§9.1).
//
// A pinned flow ID always expands to exactly one address, whether or not the flow is currently
// observed: a request naming a specific flow deserves to be told that *that* flow is missing,
// rather than silently having no paths at all. A group hint expands to whatever matches, and
// matching nothing is simply a request with zero paths.
func expand(fleet *state.Fleet, spec api.RequestSpec) []api.FlowAddress {
	src := spec.Source
	switch spec.Source.Select.Kind() {
	case api.SelectorKindFlow:
		return []api.FlowAddress{{Node: src.Node, Domain: src.Domain, Flow: spec.Source.Select.Flow}}

	case api.SelectorKindGroupHint:
		var out []api.FlowAddress
		for _, flow := range fleet.Flows(src.Node, src.Domain) {
			if flow.GroupHint == nil || !spec.Source.Select.GroupHint.Matches(*flow.GroupHint) {
				continue
			}
			out = append(out, api.FlowAddress{Node: src.Node, Domain: src.Domain, Flow: flow.ID})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Flow < out[j].Flow })
		return out

	default:
		// Not reachable through the API — a selector with no kind cannot be decoded or encoded
		// (§9.1) — but a stored record is not a decode away from anything, so it is handled
		// rather than assumed.
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
func retain(fleet *state.Fleet, plans map[string]*pathPlan, requestPaths map[string][]string, sessionsByPath map[string][]state.SessionRecord, defaultTeardown time.Duration) {
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
		for _, rid := range sortedKeys(fleet.Requests) {
			spec := fleet.Requests[rid].Value.Spec
			if spec.Destination != identity.Destination ||
				spec.Source.Node != identity.Source.Node ||
				spec.Source.Domain != identity.Source.Domain {
				continue
			}
			if spec.Source.Select.Kind() == api.SelectorKindFlow && spec.Source.Select.Flow != identity.Source.Flow {
				continue
			}
			plan.merge(fleet.Requests[rid].Value, defaultTeardown)
			requestPaths[rid] = append(requestPaths[rid], pid)
		}
	}
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
	b.result.Paths[plan.id] = api.Path{
		PathStatus: status,
		Requests:   slices.Compact(slices.Sorted(slices.Values(plan.requests))),
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
			status.Reason = "source flow has not been produced for " + idle.Round(time.Second).String() + "; workers stopped"
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
	destination, observed := b.fleet.Flow(dst.Node, dst.Domain, record.Path.Source.Flow)
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
		return "waiting for the destination agent to start its target worker"
	case session.Target.State != api.WorkerReady:
		return "target worker is " + string(session.Target.State)
	case session.Initiator == nil:
		return "waiting for the source agent to start its initiator worker"
	default:
		return "initiator worker is " + string(session.Initiator.State)
	}
}

// assign emits the worker assignments for a session (§5.3 steps 2 and 5).
func (b *builder) assign(plan *pathPlan, record state.SessionRecord, flow api.FlowInventory) {
	src, dst := record.Path.Source, record.Path.Destination

	common := api.Assignment{
		SessionID:                   record.ID,
		Domain:                      dst.Domain,
		FlowID:                      src.Flow,
		Interface:                   record.Interface,
		Fabric:                      record.Fabric,
		NoNetworkLatencyMeasurement: b.cfg.NoNetworkLatencyMeasurement,
		SchedPrio:                   plan.schedPrio,
		IdleTimeout:                 api.Millis(b.cfg.IdleTimeout),
		ConnectTimeout:              api.Millis(b.cfg.ConnectTimeout),
		Labels:                      plan.labels,
	}

	target := common
	target.Role = api.RoleTarget
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

	initiator := common
	initiator.Role = api.RoleInitiator
	initiator.Domain = src.Domain
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
		return "neither agent is currently leased; nothing is withdrawn while the fleet cannot be observed"
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

func summarise(result *Result, fleet *state.Fleet, requestPaths map[string][]string, invalid map[string]validate.Result) {
	for id := range fleet.Requests {
		spec := fleet.Requests[id].Value.Spec

		pathIDs := slices.Compact(slices.Sorted(slices.Values(requestPaths[id])))

		if bad, ok := invalid[id]; ok {
			// The paths are reported too, because an invalid request may still have a session
			// running underneath it, and hiding that would make a request that is moving media
			// look like one that never started.
			statuses := make([]api.PathStatus, 0, len(pathIDs))
			for _, pid := range pathIDs {
				if path, ok := result.Paths[pid]; ok {
					statuses = append(statuses, path.PathStatus)
				}
			}
			result.Requests[id] = api.RequestStatus{
				State:      api.StateInvalid,
				Reason:     bad.Message,
				ReasonCode: bad.Code,
				Paths:      statuses,
			}
			continue
		}

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

		status := api.RequestStatus{Paths: statuses, Counts: counts}

		switch {
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
				status.Reason = "the selector matches no flow in " + spec.Source.Node + "/" + spec.Source.Domain
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
