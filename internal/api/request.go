package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

// ProviderPin expresses what a request will accept as a transport (§10.4).
//
// Encoded as either a bare string or an array, so "pinned" and "prefer, but this is
// acceptable" are the same mechanism rather than two:
//
//	"provider": "verbs"           // verbs or the request fails
//	"provider": ["verbs", "tcp"]  // prefer verbs, tcp acceptable
//	                              // omitted: the server's configured order, default
//	                              // EFA > Verbs > TCP > SHM
//
// The list is ordered by preference. Whatever is pinned is honoured or the request fails — it
// is **never silently substituted**. Landing on tcp when verbs was asked for is a performance
// cliff, not a graceful degradation: a path that carried 1080p60 over verbs may simply not
// keep up, and the dropped grains look like a source problem rather than a routing decision
// made on the operator's behalf.
type ProviderPin []Provider

// IsEmpty reports whether the request pinned nothing, leaving the server free to negotiate in
// its configured preference order.
func (p ProviderPin) IsEmpty() bool { return len(p) == 0 }

// Allows reports whether provider is acceptable to this request. An empty pin allows
// everything.
func (p ProviderPin) Allows(provider Provider) bool {
	return p.IsEmpty() || slices.Contains(p, provider)
}

// Validate rejects unknown provider names, so a typo fails at request time rather than
// producing an INVALID request nobody can explain.
func (p ProviderPin) Validate() error {
	for _, provider := range p {
		if !KnownProvider(provider) {
			return fmt.Errorf("provider: unknown provider %q", provider)
		}
	}
	return nil
}

// UnmarshalJSON accepts a bare string, an array of strings, or null.
func (p *ProviderPin) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" {
		*p = nil
		return nil
	}

	if strings.HasPrefix(trimmed, "\"") {
		var single Provider
		if err := json.Unmarshal(data, &single); err != nil {
			return fmt.Errorf("provider: %w", err)
		}
		*p = ProviderPin{single}
		return nil
	}

	var list []Provider
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("provider: expected a string or an array of strings: %w", err)
	}
	*p = list
	return nil
}

// MarshalJSON emits the scalar form for a single provider, so a request round-trips to the
// bytes it was written with.
func (p ProviderPin) MarshalJSON() ([]byte, error) {
	switch len(p) {
	case 0:
		return []byte("null"), nil
	case 1:
		return json.Marshal(p[0])
	default:
		return json.Marshal([]Provider(p))
	}
}

// Source names where to replicate from (§9.1).
//
// A request carries a *list* of these, symmetrically with [Destination] — see
// [RequestSpec.Sources].
type Source struct {
	// Node is pinned, not selected. Keeping the expansion one node wide is what stops §10.8's
	// cross-product hazards arriving with domain selectors, and node labels do not exist.
	Node string `json:"node"`

	// Domain is a **selector**, not a name (§10.7): `{"name": "media/cameras"}` addresses one
	// domain directly, `{"labels": {…}}` matches by label.
	//
	// *This used to be a bare domain name.* A domain name is the domain-level UUID, and a UUID is
	// rarely what a user means — which is the same argument §9.1 makes one layer down for the flow
	// selector, arriving at the same answer: selection rather than a rename, because renaming a
	// domain would re-identify every path through it.
	Domain DomainSelector `json:"domain"`

	// Select is a selector, not a flow ID — see [Selector].
	Select Selector `json:"select"`
}

// Endpoint is the (node, domain) pair a **named** source resolves to, which is what makes a source
// and a destination the same place.
//
// Only meaningful when the domain selector is a name. A label selector has no endpoint until the
// expansion resolves it, which is why the pairing it produces is elided in the reconciler rather
// than refused here (§7.2, §10.7).
func (s Source) Endpoint() string {
	if s.Domain.Kind() != DomainSelectorKindName {
		return ""
	}
	return s.Node + DomainSeparator + s.Domain.Name.String()
}

// Describe renders a source as `<node>/<domain>` for a message or a status row. A label selector
// renders its labels, because "studio-a/{role=cameras}" is what the operator wrote and a resolved
// domain list would be a different question's answer.
func (s Source) Describe() string { return s.Node + DomainSeparator + s.Domain.String() }

// Destination names where to replicate to.
//
// There is no selector here: a destination is a (node, domain) pair, and the flow keeps its
// ID across the replication — the same flow ID existing on both nodes is the point (§3).
//
// A request carries a *list* of these, and a list of [Source] beside it: it fans in as well as out
// (§9.1).
//
// *There used to be an asymmetry here, argued for at length: the source side already had a selector
// and this side could not have one, every path in a fan-out shared a source and therefore a fate,
// and grouping several sources into one destination was said to be the arrangement that produces
// the two-producers-one-ring-buffer conflict §7.2 exists to reject.* Two of those did not survive.
// A source's node is still *pinned*, so a list of sources is a list of things the author typed
// rather than a second selector, and the cross-product hazard §10.8 warns about is not entered. And
// the fan-out grouping inverts: twelve sources into one domain is 12× ingress on the destination
// node, which is the binding direction for the ingest wall that motivates fan-in at all.
//
// The two that survived became requirements and are discharged rather than argued away: shared fate
// is genuinely gone, so the aggregate grew [StatePartial] and a per-source breakdown; and the
// corruption case is real, so its decidable form is refused at POST as
// [ReasonDuplicateSourceFlow] and its undecidable form stays [ReasonFlowConflict] on the path.
type Destination struct {
	Node string `json:"node"`

	// Domain is where to replicate into: an **area name and a list of path elements**, never a
	// path (§10.6). `{"area": "fast", "elements": ["studio-a","cam1"]}` materialises
	// `<fast>/studio-a/cam1` and renders as `fast/studio-a/cam1`.
	//
	// *This supersedes a separate `root:` field alongside a bare element list, which could be
	// omitted on a node advertising exactly one root.* The area is part of the domain's name now,
	// so omitting it would be omitting half the name — a small verbosity cost, paid to have one
	// identity grammar for every domain instead of two.
	//
	// The structured form is the invariant that stops this API being a remote
	// arbitrary-filesystem-write primitive on every node in the fleet (§7.2, §13), and it holds
	// regardless of what authentication is configured. Because no element can contain a separator
	// or be `..`, joining them onto the area's path produces exactly
	// `area.Path + "/" + join(elements)` — an equality the agent checks on the whole path, with no
	// prefix reasoning and no boundary case for a separator to hide in. A raw path is never
	// accepted, and there is nothing here for one to be spelled as.
	//
	// A manifest writes it as `domain: fast/studio-a/cam1` and the CLI splits it there. **Nothing
	// else in the system ever parses a domain string** (§10.6).
	//
	// The domain needs no prior existence and has no lifecycle of its own. It is materialised by
	// the first path that targets it and forgotten when the last one goes, on the refcount that
	// already governs paths — so there is no create API, no delete API, and no "delete while
	// referenced" conflict to resolve.
	Domain Domain `json:"domain"`

	// Provider overrides [RequestSpec.Provider] for this destination alone. Empty inherits it.
	//
	// This is the one per-destination override, and it exists because a provider is negotiated
	// per session and therefore per (source, destination) pair (§10.3): with destinations on
	// different nodes, one request-level pin can be right for one and *unsatisfiable* for
	// another. IdleTeardown and SchedPrio stay request-level — they degrade, or are rejected
	// with a reason naming the node, so splitting the request is the honest fix rather than an
	// override that hides a node's missing capability.
	//
	// It overrides rather than intersects. Intersecting would make "verbs here, tcp there" a
	// pin conflict instead of the perfectly ordinary request it is.
	Provider ProviderPin `json:"provider,omitempty"`

	// Disabled parks this leg: the entry stays in the spec and expands to nothing (§9.1).
	//
	// **This is the only place a request spells *off*.** Without it the desired set has no value
	// for "not running" — a route that is off is a route that does not exist — so taking a leg out
	// of service means deleting it and typing it back, and the spec somebody wrote and reviewed is
	// gone in between. A manifest has the same hole: a file edited down to park a route for a night
	// stops describing intent, and `--prune` cancels whatever the edited-down file no longer names.
	//
	// The effective spec is every source against every *enabled* destination. A disabled entry
	// produces no pairing, so no path, no session and no assignment — nothing below the request
	// changes, which is what makes this arithmetic on the cross product rather than a new noun.
	//
	// **It is here rather than on the request because dropping a destination is already the
	// operation that stops one leg**, clearing that column across every one of the request's
	// sources; this is that operation made non-destructive. The relationship only runs one way,
	// which settles it: a request whose every destination is disabled asks for nothing, so a
	// request-level flag is derivable from these and these are not derivable from it. That is also
	// why [RequestStatus.State] reports [StateDisabled] by folding rather than by reading a stored
	// flag — there is nothing here for a stored one to drift from.
	//
	// **Spelled `disabled`, never `enabled`.** A default-true boolean is a trap on a wire format
	// where `omitempty` drops the zero value: an inverted flag that goes missing stops every leg in
	// the fleet, where a missing one here keeps media running.
	//
	// The asymmetry with [RequestSpec.Sources], which has no such flag, is deliberate and is the
	// one [Destination.Provider] already has: what varies is whichever end the operator listed
	// several of, and disabling either end of a pairing kills it, so one flag on one end says
	// everything about one. A flag on a *(source, destination)* pairing is refused outright — it
	// would make a request an arbitrary bitmap over the grid rather than sources × destinations,
	// which the expansion cannot describe and a manifest cannot spell (§9.1).
	//
	// Disabling **stops media**: it is a cancellation of these legs with the text kept, so it
	// carries a cancellation's blast radius and wants the same `?dry_run=true` preview. It is not a
	// soft delete — the request still exists, still holds its name, and is still pruned by a file
	// that stops naming it.
	Disabled bool `json:"disabled,omitempty"`
}

// DomainName renders the destination domain as the single string everything downstream carries —
// the assignment, the path and session identity, the `domain` metric label (§10.6).
func (d Destination) DomainName() string { return d.Domain.String() }

// Endpoint is the (node, domain) pair this destination names, which is what makes two
// destinations the same destination.
//
// *The resolved output root used to be excluded from it deliberately*, because two entries naming
// one domain under two roots were one name over two directories. The area is inside the domain's
// name now, so `fast/ingest` and `bulk/ingest` are already two endpoints and there is nothing to
// exclude.
func (d Destination) Endpoint() string { return d.Node + DomainSeparator + d.DomainName() }

// AnyEnabled reports whether any of these destinations is not parked (§9.1).
//
// The question every consumer of [Destination.Disabled] actually asks, written once so that "this
// request is asking for nothing" and "this request reports [StateDisabled]" cannot come to disagree.
// An empty list is false, which is the honest answer and is unreachable through [RequestSpec.Validate]
// anyway.
func AnyEnabled(destinations []Destination) bool {
	for _, dst := range destinations {
		if !dst.Disabled {
			return true
		}
	}
	return false
}

// RequestID is a request's identity: its namespace and its name (§9.1, §9.3).
//
// **Names are scoped to the namespace, not fleet-wide.** A namespace that does not namespace
// names is half the concept, and the consumer this partition exists for is the one that proves
// it: a Kubernetes adapter naming requests after pods inherits Kubernetes' own namespacing, so two
// identically-named pods in two of its namespaces collide here unless the adapter prefixes — and
// prefixing is exactly what having a namespace should remove.
//
// The cost is that every request ID in a URL, a CLI argument or a UI key gains a second
// component. Nothing downstream is affected, since path identity does not include the request
// (§5.4).
type RequestID struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// String renders the pair as `<namespace>/<name>`, which is what a log line, a path's refcount
// list and a CLI argument carry.
//
// Injective, because neither half can contain a separator: [ValidNamespace] refuses one outright
// and the server's request-name rule does the same (§9.1).
func (id RequestID) String() string { return id.Namespace + "/" + id.Name }

// RequestSpec is durable user intent: "replicate what these selectors match, from these places to
// those places" (§3, §9.1).
//
// A request is never cancelled because its session is failing. Failure is made *observable*
// (§11) — a peer being unreachable is no reason to drop the intent, any more than it is a
// reason to restart and drop every other flow.
type RequestSpec struct {
	// Namespace is the partition this request belongs to, and half of its ID (§9.3).
	//
	// **A real property, not a label.** Empty means [DefaultNamespace] to a reader that has a
	// spec the write path has not touched — a manifest on its way in, a record written before
	// this field existed — and the server writes it out on every stored request, so the value is
	// never merely implied.
	//
	// It is not a label because a namespace decides which requests may not overlap and what
	// `--prune` catches, and burying that in a free-text map means an operator's own label
	// silently becomes a partition key.
	Namespace string `json:"namespace,omitempty"`

	// Name is a client-supplied idempotency key, unique **within its namespace**. POSTing an
	// existing name returns the existing request rather than creating a second one.
	//
	// Required, which is a plan decision beyond §9.1's "add a name": it makes every create
	// idempotent rather than only the creates that remembered to opt in. The Kubernetes
	// adapter on the roadmap is a controller that re-reconciles on every resync, and without
	// create-or-get it either creates duplicate requests forever or maintains its own ID
	// mapping. Anything hand-rolling a POST has the same problem on retry.
	Name string `json:"name"`

	// Sources is where to replicate from. **Always a list, with no singular `source:` beside it**
	// (§9.1).
	//
	// The scalar-or-list spelling [ProviderPin] uses was the alternative and is refused. It earns
	// its keep there because the scalar is the *rare* case and the list is the interesting one; here
	// it is the other way round, so nearly every request would be written in the singular and the
	// list would be the shape nobody has seen. Two spellings of the common case is two shapes to
	// read, two to test and two for a UI to render, bought to save four characters.
	//
	// The cost is that this invalidates every stored request and every manifest written against the
	// previous field name. That is a real migration and it rides the major version (§16).
	//
	// At least one is required. A request with two sources and three destinations owns six
	// *pairings*, and each pairing's selector expands to its own set of paths — so the path count is
	// the sum over pairings, not the product of two path counts.
	Sources []Source `json:"sources"`

	// Destinations is where the sources go. At least one is required.
	//
	// A request with three destinations and a selector matching two flows owns six paths, each with
	// its own state, and the request's status aggregates over them (§11).
	Destinations []Destination `json:"destinations"`

	Provider ProviderPin `json:"provider,omitempty"`

	// IdleTeardown overrides the agent's global long-idle threshold for this request's
	// sessions (§11.1). Nil takes the global default; zero disables teardown, keeping the
	// workers hot indefinitely.
	//
	// It exists because "this feed is bursty, keep it hot" is a real operational requirement:
	// tearing down too eagerly costs a source that stops and starts frequently its first
	// grains to a re-establish every time, and never tearing down holds ports, memory
	// registrations and processes for dormant flows.
	IdleTeardown *Milliseconds `json:"idle_teardown_ms,omitempty"`

	// SchedPrio requests SCHED_FIFO for this request's workers. Rejected at request time if a
	// participating node lacks the capability (§7.2), rather than silently running at normal
	// priority.
	SchedPrio *int `json:"sched_prio,omitempty"`

	// Labels ride along into worker metrics as user labels, as the legacy proxy's
	// subscription labels did (§12).
	//
	// `namespace` is an ordinary user label again, now that [RequestSpec.Namespace] is a real
	// property (§9.3). It stays reserved as a *metric* label, because the session's namespace is
	// emitted as a dimension of its own (§12) — so a user label of that name is dropped rather
	// than mangled, exactly as any other colliding one is.
	Labels map[string]string `json:"labels,omitempty"`
}

// RequestID is this request's identity: its namespace and its name (§9.1).
//
// An empty namespace reads as [DefaultNamespace], for a spec the write path has not touched yet.
//
// Spelled out rather than called `ID`, because [Request] embeds this type and carries an `ID`
// field of its own — the rendered string — and a method that a field shadows is a method nothing
// can call.
func (s RequestSpec) RequestID() RequestID {
	return RequestID{Namespace: s.NamespaceOrDefault(), Name: s.Name}
}

// NamespaceOrDefault is the namespace this request is in, defaulting an unset one.
func (s RequestSpec) NamespaceOrDefault() string {
	if s.Namespace == "" {
		return DefaultNamespace
	}
	return s.Namespace
}

// SameAs reports whether two specs are the same intent.
//
// This is the definition of "unchanged" for an apply, and it lives here so that the server's
// decision not to write (invariant 13) and the client's report of `unchanged` cannot disagree —
// an apply that says "unchanged" while the server wrote, or the reverse, is worse than either
// answer on its own.
//
// Compared as encoded JSON rather than with reflect.DeepEqual, because that is the form both
// sides round-trip through and it collapses the distinctions the wire does not carry: a nil slice
// and an empty one, a nil map and an empty one. A false negative is not a correctness bug, but it
// is store churn, which is the whole thing being avoided.
func (s RequestSpec) SameAs(other RequestSpec) bool {
	left, err := json.Marshal(s)
	if err != nil {
		return false
	}
	right, err := json.Marshal(other)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

// ProviderFor returns the pin that applies to one of this request's destinations: the
// destination's own override when it has one, the request-level pin otherwise (§10.4).
func (s RequestSpec) ProviderFor(dst Destination) ProviderPin {
	if !dst.Provider.IsEmpty() {
		return dst.Provider
	}
	return s.Provider
}

// maxNameLength bounds the idempotency key. 253 is the DNS-subdomain limit Kubernetes object
// names use, which is the shape the adapter will hand us.
const maxNameLength = 253

// Validate checks a request body's structure.
//
// Structure only: whether the nodes exist, whether the destination node advertises the output
// root, whether a viable interface pair exists, whether this would form a loop — all of that is
// §7.2 validation on the server, because it needs registrations and inventory that this type
// cannot see.
//
// The destination domain *name* is the one thing on that side checked here, because it is the
// only one that needs nothing but the request body itself (§10.6).
func (s RequestSpec) Validate() error {
	switch {
	case s.Name == "":
		return fmt.Errorf("name is required")
	case len(s.Name) > maxNameLength:
		return fmt.Errorf("name is longer than %d characters", maxNameLength)
	}
	for _, r := range s.Name {
		if unicode.IsControl(r) {
			return fmt.Errorf("name must not contain control characters")
		}
	}

	// Empty is legal and means [DefaultNamespace]; anything spelled out is held to the grammar,
	// because it becomes a URL segment and a store key (§9.3).
	if s.Namespace != "" {
		if err := ValidNamespace(s.Namespace); err != nil {
			return err
		}
	}

	if len(s.Sources) == 0 {
		return fmt.Errorf("at least one source is required")
	}

	// Two *identical* sources are one source written twice: they expand to the same addresses, and
	// every path they produce is the same path. Same argument as the destination rule below —
	// deduplicating silently would hide a copy-paste error in a manifest.
	//
	// The whole source is the key, not its endpoint, because two sources sharing a `(node, domain)`
	// with different selectors is an ordinary thing to write: `{group_hint: cam1}` and
	// `{group_hint: cam2}` out of one domain is a request with two legs, not a duplicate.
	//
	// Overlap short of equality is deliberately not chased. Two group hints or two label selectors
	// can match the same flow without being equal, which is not decidable from the request body and
	// is not an error anyway — the expansion collapses them by path identity, which is where overlap
	// has always been resolved (§9.1).
	sourceSeen := make(map[string]int, len(s.Sources))
	for i, src := range s.Sources {
		where := fmt.Sprintf("sources[%d]", i)

		if src.Node == "" {
			return fmt.Errorf("%s.node is required", where)
		}
		if err := src.Domain.Validate(); err != nil {
			return fmt.Errorf("%s.domain: %w", where, err)
		}
		if err := src.Select.Validate(); err != nil {
			return fmt.Errorf("%s.select: %w", where, err)
		}

		key, err := json.Marshal(src)
		if err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if first, dup := sourceSeen[string(key)]; dup {
			return fmt.Errorf("%s and sources[%d] are the same source, %s", where, first, src.Describe())
		}
		sourceSeen[string(key)] = i
	}

	// **Entries, not enabled entries** (§9.1). A request whose only destination is parked is legal
	// and is the whole point of [Destination.Disabled]: requiring an enabled one would forbid
	// precisely the state the flag exists to represent, and would make un-parking the last leg the
	// only way to keep a route on file. What such a request reports is [StateDisabled], decided by
	// the reconciler, not a validation error decided here.
	if len(s.Destinations) == 0 {
		return fmt.Errorf("at least one destination is required")
	}

	// Two entries naming one (node, domain) are the same path written twice. Deduplicating
	// silently would hide a copy-paste error in a manifest, and the two entries can disagree
	// about the root or the provider, at which point there is no answer to pick.
	//
	// **A parked entry is not exempt**, and the temptation to exempt it is worth naming: parking a
	// `tcp` variant of a destination beside the live `verbs` one reads like a useful thing to keep
	// on file, and it has no answer for which pin applies the moment both are enabled — which is
	// the same reason the rule exists at all.
	seen := make(map[string]int, len(s.Destinations))
	for i, dst := range s.Destinations {
		where := fmt.Sprintf("destinations[%d]", i)

		if dst.Node == "" {
			return fmt.Errorf("%s.node is required", where)
		}
		// Structural, and checkable here because it needs no server state: a destination domain is
		// a directory this API is asking a node to create, so the name rule is part of the request
		// body rather than a property of the fleet (§10.6).
		//
		// The server checks it again during reconciliation and reports [ReasonMalformedDomainName]
		// there. Not redundant: validate runs over *stored* requests on every reconcile, so a
		// request written straight into the store, or stored before this rule existed, must still
		// be refused legibly rather than reaching an agent as an assignment.
		if err := dst.Domain.Valid(); err != nil {
			return fmt.Errorf("%s.domain %q: %w", where, dst.DomainName(), err)
		}
		if err := dst.Provider.Validate(); err != nil {
			return fmt.Errorf("%s.%w", where, err)
		}

		if first, dup := seen[dst.Endpoint()]; dup {
			return fmt.Errorf("%s and destinations[%d] both name %s", where, first, dst.Endpoint())
		}
		seen[dst.Endpoint()] = i
	}

	if err := s.Provider.Validate(); err != nil {
		return err
	}
	if s.IdleTeardown != nil && *s.IdleTeardown < 0 {
		return fmt.Errorf("idle_teardown_ms must not be negative")
	}
	return nil
}

// Request is a stored request as the API returns it.
type Request struct {
	// ID is the rendered `<namespace>/<name>` pair (§9.1). Not server-assigned: both halves are
	// already in the embedded spec, and this is the joinable spelling — it is what a path's
	// refcount list carries and what a log line prints.
	ID string `json:"id"`

	RequestSpec

	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at,omitzero"`
	Status    RequestStatus `json:"status"`
}

// RequestStatus aggregates over the request's paths (§9.1, §11).
//
// A request owns a *set* of paths, including in the pinned-flow case where the set has size
// one. That is why the API returns the summary, the per-path breakdown and — since both ends are
// lists — a per-source breakdown beside it. "1 of 3 active" is the answer an operator needs from a
// group-hint request and has no meaning in a one-flow-per-request model; "studio-c is dark, the
// other two studios are fine" is the answer they need from a fan-in and has no meaning in a
// one-source model.
type RequestStatus struct {
	// State is the aggregate. ACTIVE only when every path is, and [StatePartial] when they
	// disagree with at least one of them ACTIVE.
	State State `json:"state"`

	// Reason and ReasonCode explain a non-ACTIVE aggregate. For an INVALID request they are
	// the user action needed (§7.2).
	Reason     string     `json:"reason,omitempty"`
	ReasonCode ReasonCode `json:"reason_code,omitempty"`

	// Counts is the per-state breakdown across Paths — the "1 of 3" numerator and denominator
	// without the client having to fold Paths itself.
	Counts map[State]int `json:"counts,omitempty"`

	// Sources is the same fold done per source, in the order the request lists them (§9.1, §11).
	//
	// It is what makes a failure attributable to the right *end*. A request with twelve sources and
	// one destination folds to one line, and that line cannot say which camera is dark — but the
	// per-source row can, and the node an operator has to go and look at is the source's, not the
	// destination's.
	//
	// Always present and always the full list, including sources contributing no paths at all: a
	// source that expanded to nothing is exactly the one an operator is looking for, and omitting it
	// would hide it behind eleven that worked.
	Sources []SourceStatus `json:"sources,omitempty"`

	// Paths is the expansion of the selectors against the current inventory, recomputed on
	// every reconcile. A selector matching nothing is simply a request with zero paths, which
	// composes with WAITING at no extra cost (§9.1).
	Paths []PathStatus `json:"paths"`

	// Excluded is what the expansion left out, **and it is not decoration** (§9.1).
	//
	// A path that does not exist has no status to carry a reason, so a flow a selector skipped is
	// invisible in a paths-only rendering — and §10.7's self-output rule skips flows deliberately,
	// on a node that is also a replication destination, which is precisely where an operator's
	// broad selector will meet it. Under the superseded directory-granular rule the whole domain
	// was absent, which was at least legible as a category; per-flow provenance is finer and
	// therefore *less* obvious, and this is where that cost is paid back.
	//
	// **"Did not match the labels" is not a reason and is never listed**: that set is unbounded
	// and is the ordinary case.
	//
	// Not a []PathStatus, and it must not be modelled as one — an excluded flow is precisely a
	// flow that produced no path.
	Excluded []Exclusion `json:"excluded,omitempty"`

	// ExcludedDropped is how many entries the cap discarded. A silent cap here reads as "nothing
	// else was excluded", which is the one thing this list must not say when it is untrue (§9.1).
	ExcludedDropped int `json:"excluded_dropped,omitempty"`
}

// SourceStatus is one source of a request, folded over the paths that source produced (§9.1, §11).
//
// The same fold as the request's own, which is deliberate — an operator reading a source row is
// asking the same question one level down, and two different folds would answer it two different
// ways. [StatePartial] therefore appears here too: a source whose flows disagree is partial in
// exactly the sense the request is.
type SourceStatus struct {
	// Source is the entry as the request wrote it, not as it resolved. A label selector renders its
	// labels — the operator is looking for the line they typed, and a resolved domain list is a
	// different question's answer.
	Source Source `json:"source"`

	State      State      `json:"state"`
	Reason     string     `json:"reason,omitempty"`
	ReasonCode ReasonCode `json:"reason_code,omitempty"`

	// Counts is this source's per-state breakdown, and Paths the IDs it expanded onto.
	//
	// IDs rather than a second copy of [PathStatus]: a request with twelve sources into three
	// destinations would otherwise carry every path's full status twice, and the paths are already
	// in [RequestStatus.Paths] to be joined against.
	Counts map[State]int `json:"counts,omitempty"`
	Paths  []string      `json:"paths,omitempty"`
}

// MaxExclusions bounds [RequestStatus.Excluded].
//
// It needs a bound — a broad selector against a busy destination node can exclude a lot — and what
// the number *is* is arbitrary, so it is picked once here rather than at the call site. Sized so
// that a realistic node's worth of replicated flows fits: past this an operator is reading a
// summary, not a list, and [RequestStatus.ExcludedDropped] is what tells them so.
const MaxExclusions = 32

// Exclusion is one flow a request's expansion deliberately left out (§9.1, §10.7).
type Exclusion struct {
	Node   string `json:"node"`
	Domain string `json:"domain"`
	Flow   string `json:"flow"`

	Reason ExclusionReason `json:"reason"`
}

// ExclusionReason says why a matched flow produced no path.
type ExclusionReason string

const (
	// ExclusionSelfOutput is a flow this node's own target worker is writing (§10.6, §10.7).
	//
	// The one reason today. A label selector never matches this project's own output, because a
	// network of receivers that forward what they receive is exactly where loops come from — and
	// the topology such a thing settles into is decided by §7.5's precedence rather than by
	// anything an operator wrote. Naming a domain directly still reaches everything: explicit
	// chaining is intent, matched chaining is emergence.
	ExclusionSelfOutput ExclusionReason = "self_output"
)

// RequestList is GET /v1/requests.
type RequestList struct {
	Requests []Request `json:"requests"`
}
