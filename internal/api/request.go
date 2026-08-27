package api

import (
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
type Destination struct {
	Node string `json:"node"`

	// Domain must be a domain *name* the destination agent has explicitly mapped. A raw path
	// is never accepted (§7.2): that invariant is what stops this API from being a remote
	// arbitrary-filesystem-write primitive on every node in the fleet, and it holds regardless
	// of what authentication is configured (§13).
	Domain string `json:"domain"`
}

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

	Source      Source      `json:"source"`
	Destination Destination `json:"destination"`

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
	Labels map[string]string `json:"labels,omitempty"`
}

// maxNameLength bounds the idempotency key. 253 is the DNS-subdomain limit Kubernetes object
// names use, which is the shape the adapter will hand us.
const maxNameLength = 253

// Validate checks a request body's structure.
//
// Structure only: whether the nodes exist, whether the destination domain is mapped, whether a
// viable interface pair exists, whether this would form a loop — all of that is §7.2
// validation on the server, because it needs registrations and inventory that this type cannot
// see.
func (s RequestSpec) Validate() error {
	switch {
	case s.Name == "":
		return fmt.Errorf("name is required: it is the idempotency key")
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
	if s.Destination.Node == "" {
		return fmt.Errorf("destination.node is required")
	}
	if s.Destination.Domain == "" {
		return fmt.Errorf("destination.domain is required")
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
