package api

import (
	"bytes"
	"encoding/json"
	"errors"
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
type Source struct {
	Node   string `json:"node"`
	Domain string `json:"domain"`

	// Select is a selector, not a flow ID — see [Selector].
	Select Selector `json:"select"`
}

// Destination names where to replicate to.
//
// There is no selector here: a destination is a (node, domain) pair, and the flow keeps its
// ID across the replication — the same flow ID existing on both nodes is the point (§3).
//
// A request carries a *list* of these (§9.1). That asymmetry with [Source] is deliberate: the
// source side already has a selector and the destination side cannot have one, every path in a
// fan-out shares a source and therefore a fate, and grouping several sources into one
// destination is the arrangement that produces the two-producers-one-ring-buffer conflict §7.2
// exists to reject.
type Destination struct {
	Node string `json:"node"`

	// Domain is the output domain to replicate into, created inside [Destination.Root] if it does
	// not exist yet (§10.6).
	//
	// **A list of path elements, not a path.** `["studio-a","cam1"]` materialises
	// `<root>/studio-a/cam1`. Each element must satisfy [ValidDomainName], and the whole must
	// satisfy [ValidDomainElements].
	//
	// The element form is the invariant that stops this API being a remote
	// arbitrary-filesystem-write primitive on every node in the fleet (§7.2, §13), and it holds
	// regardless of what authentication is configured. Because no element can contain a separator
	// or be `..`, joining them onto a root produces exactly `root + "/" + DomainPath(elements)` —
	// an equality the agent checks on the whole path, with no prefix reasoning and no boundary
	// case for a separator to hide in. A raw path is never accepted, and there is nothing here for
	// one to be spelled as.
	//
	// A manifest writes it as `domain: studio-a/cam1` and the CLI splits it there. **Nothing else
	// in the system ever parses a domain string** — see [ValidDomainElements].
	//
	// The domain needs no prior existence and has no lifecycle of its own. It is materialised by
	// the first path that targets it and forgotten when the last one goes, on the refcount that
	// already governs paths — so there is no create API, no delete API, and no "delete while
	// referenced" conflict to resolve.
	Domain []string `json:"domain"`

	// Root names which of the destination node's advertised output roots the domain is created
	// under (§10.6). Optional when the node advertises exactly one, which is the common case.
	//
	// A node with more than one and a request naming none is INVALID, listing the candidates,
	// rather than being resolved by a guess. The cost is recorded rather than hidden: a request
	// that worked becomes ambiguous the day its destination node grows a second root. Taken
	// deliberately — the friendly case is overwhelmingly the common one, and the error carries
	// its own fix.
	Root string `json:"root,omitempty"`

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
}

// DomainName renders the domain as the single string everything downstream carries — the
// assignment, the path and session identity, the `domain` metric label (§10.6).
func (d Destination) DomainName() string { return DomainPath(d.Domain) }

// Endpoint is the (node, domain) pair this destination names, which is what makes two
// destinations the same destination. The root is not part of it: two entries naming one domain
// under two roots are one name over two directories, which is exactly what must be rejected.
func (d Destination) Endpoint() string { return d.Node + "/" + d.DomainName() }

// RequestSpec is durable user intent: "replicate what this selector matches, from here to
// there" (§3, §9.1).
//
// A request is never cancelled because its session is failing. Failure is made *observable*
// (§11) — a peer being unreachable is no reason to drop the intent, any more than it is a
// reason to restart and drop every other flow.
type RequestSpec struct {
	// Name is a client-supplied idempotency key. POSTing an existing name returns the existing
	// request rather than creating a second one.
	//
	// Required, which is a plan decision beyond §9.1's "add a name": it makes every create
	// idempotent rather than only the creates that remembered to opt in. The Kubernetes
	// adapter on the roadmap is a controller that re-reconciles on every resync, and without
	// create-or-get it either creates duplicate requests forever or maintains its own ID
	// mapping. Anything hand-rolling a POST has the same problem on retry.
	Name string `json:"name"`

	Source Source `json:"source"`

	// Destinations is where the source goes. One source, many destinations — see [Destination]
	// for why the list is on this side and not the other.
	//
	// At least one is required. A request with three destinations and a selector matching two
	// flows owns six paths, each with its own state, and the request's status aggregates over
	// them (§11).
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
	// One key is reserved and means something to the server: [LabelNamespace]. See
	// [RequestSpec.Namespace].
	Labels map[string]string `json:"labels,omitempty"`
}

const (
	// LabelNamespace is the label whose value partitions requests (§7b of the UI handoff).
	//
	// It is a label rather than a field of its own because `apply --prune` already takes a
	// label selector, so `--prune -l namespace=nab` already spells "make the fleet's nab
	// namespace equal this file" and needs nothing added to say it. A namespace is therefore a
	// prune scope, a manifest file and a matrix, all the same set.
	//
	// Deliberately not called a *group*: that word is taken by the NMOS group hint, which is
	// the vocabulary of selectors and is unrelated.
	LabelNamespace = "namespace"

	// DefaultNamespace is where a request with no [LabelNamespace] label lives.
	//
	// It is a real namespace and not an exemption — the rule that two requests in one namespace
	// may not hold one path applies to it exactly as to any other. On a fleet driven from the
	// CLI it is also where everything is, which is the reason it cannot be exempt: a partition
	// that most requests sit outside of buys nothing.
	DefaultNamespace = "default"
)

// Namespace is the partition this request belongs to.
//
// The server fills the label in on write (see [RequestSpec.EnsureNamespace]), so a stored request
// always carries it. This still defaults, for two readers that see specs the write path has not
// touched: a record written before the label existed, and a manifest on its way in.
func (s RequestSpec) Namespace() string {
	if ns := s.Labels[LabelNamespace]; ns != "" {
		return ns
	}
	return DefaultNamespace
}

// EnsureNamespace writes [DefaultNamespace] into the labels when no namespace is set.
//
// Every stored request says which namespace it is in, rather than some saying it and the rest
// implying it. The implied form is workable for a reader — [RequestSpec.Namespace] defaults — but
// it makes the label mean two different things depending on which request you are holding, and
// it makes `--prune -l namespace=default` silently miss exactly the requests that are in it.
//
// It has to run before the unchanged comparison (invariant 13), not after: normalising a spec
// after deciding whether it differs from the stored one would make every apply of a
// label-less manifest look like a change and write on every pass.
func (s *RequestSpec) EnsureNamespace() {
	if s.Labels[LabelNamespace] != "" {
		return
	}
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	s.Labels[LabelNamespace] = DefaultNamespace
}

// ValidNamespace checks a namespace name.
//
// It lives here rather than in the server because the manifest checks it too, and a namespace a
// file accepts and a POST rejects is the split that makes `apply --dry-run` less useful than the
// apply. Constrained where other label values are free text because it names a partition an
// operator selects, prunes and files manifests by, and ends up in `--prune -l namespace=…` on a
// command line.
func ValidNamespace(ns string) error {
	if ns == "" {
		return fmt.Errorf("namespace must not be empty: omit it for the %q namespace", DefaultNamespace)
	}
	for _, r := range ns {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return errors.New("namespace may contain only letters, digits, - and _")
		}
	}
	return nil
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

	if s.Source.Node == "" {
		return fmt.Errorf("source.node is required")
	}
	if s.Source.Domain == "" {
		return fmt.Errorf("source.domain is required")
	}
	if err := s.Source.Select.Validate(); err != nil {
		return fmt.Errorf("source.select: %w", err)
	}
	if len(s.Destinations) == 0 {
		return fmt.Errorf("at least one destination is required")
	}

	// Two entries naming one (node, domain) are the same path written twice. Deduplicating
	// silently would hide a copy-paste error in a manifest, and the two entries can disagree
	// about the root or the provider, at which point there is no answer to pick.
	seen := make(map[string]int, len(s.Destinations))
	for i, dst := range s.Destinations {
		where := fmt.Sprintf("destinations[%d]", i)

		if dst.Node == "" {
			return fmt.Errorf("%s.node is required", where)
		}
		if len(dst.Domain) == 0 {
			return fmt.Errorf("%s.domain is required", where)
		}
		// Structural, and checkable here because it needs no server state: a destination domain is
		// a directory this API is asking a node to create, so the name rule is part of the request
		// body rather than a property of the fleet (§10.6).
		//
		// The server checks it again during reconciliation and reports [ReasonMalformedDomainName]
		// there. Not redundant: validate runs over *stored* requests on every reconcile, so a
		// request written straight into the store, or stored before this rule existed, must still
		// be refused legibly rather than reaching an agent as an assignment.
		if err := ValidDomainElements(dst.Domain); err != nil {
			return fmt.Errorf("%s.domain %q: %w", where, dst.DomainName(), err)
		}
		if dst.Root != "" {
			if err := ValidDomainName(dst.Root); err != nil {
				return fmt.Errorf("%s.root %q: %w", where, dst.Root, err)
			}
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
	// ID is server-assigned and is what DELETE /v1/requests/{id} takes.
	ID string `json:"id"`

	RequestSpec

	CreatedAt time.Time     `json:"created_at"`
	Status    RequestStatus `json:"status"`
}

// RequestStatus aggregates over the request's paths (§9.1, §11).
//
// A request owns a *set* of paths, including in the pinned-flow case where the set has size
// one. That is why the API returns both the summary and the breakdown: "1 of 3 active" is the
// answer an operator needs from a group-hint request, and it has no meaning in a
// one-flow-per-request model.
type RequestStatus struct {
	// State is the aggregate. ACTIVE only when every path is.
	State State `json:"state"`

	// Reason and ReasonCode explain a non-ACTIVE aggregate. For an INVALID request they are
	// the user action needed (§7.2).
	Reason     string     `json:"reason,omitempty"`
	ReasonCode ReasonCode `json:"reason_code,omitempty"`

	// Counts is the per-state breakdown across Paths — the "1 of 3" numerator and denominator
	// without the client having to fold Paths itself.
	Counts map[State]int `json:"counts,omitempty"`

	// Paths is the expansion of the selector against the current inventory, recomputed on
	// every reconcile. A selector matching nothing is simply a request with zero paths, which
	// composes with WAITING at no extra cost (§9.1).
	Paths []PathStatus `json:"paths"`
}

// RequestList is GET /v1/requests.
type RequestList struct {
	Requests []Request `json:"requests"`
}
