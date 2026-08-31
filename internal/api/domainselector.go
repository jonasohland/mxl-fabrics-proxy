package api

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// DomainSelectorKind names one way of choosing a request's source domains.
type DomainSelectorKind string

const (
	// DomainSelectorKindName addresses one domain directly, by its fleet-wide identity.
	DomainSelectorKindName DomainSelectorKind = "name"

	// DomainSelectorKindLabels matches domains by the labels an operator attached to them.
	DomainSelectorKindLabels DomainSelectorKind = "labels"
)

// DomainSelector chooses which of a node's domains a request replicates from (§10.7).
//
// A domain's *name* is the domain-level UUID: it is embedded in path identity (§5.4), session
// identity and the `domain` metric label, so renaming one would tear down running media on a
// metadata edit. Keeping identity fixed makes relabelling free — and it turns naming into
// *selection*, which is the same move §9.1 already made one layer down for flows.
//
//	"domain": { "name": "media/cameras" }
//	"domain": { "labels": { "role": "cameras" } }
//
// # Exactly one kind
//
// A copy of [Selector]'s discipline rather than a fresh design, and every argument on that type
// transfers verbatim — including the sharpest one. This is the second deliberate exception to the
// package's "never fail on an unrecognised key" rule (see doc.go): an unknown kind is an error,
// because ignoring one silently *widens* the selection to whatever remained, and for a mechanism
// that moves uncompressed video between hosts that is the wrong direction to fail in.
//
// # The direct form is not a fallback
//
// `name` addresses **any** domain, including one another request replicates into, which is how
// `A→B→C` is written (§10.6). A manifest that names a domain is self-contained, where a label
// selector depends on a `kind: domain` document someone may not have applied — so the union stays
// two-kinded because the second kind is *selection*, not a second spelling of *naming*.
//
// *This supersedes a `{"path": …}` direct form*, which addressed only a discovered domain and left
// the second hop of a chain unspellable: with two identity grammars there was no way to write it.
// One grammar removes the problem rather than reconciling it.
type DomainSelector struct {
	// Name addresses one domain directly. A [*Domain] rather than a string, so the union's direct
	// form carries the same structured value a destination does and §10.6's "parsed at exactly one
	// boundary" rule is not quietly broken by the selector: the manifest parses `media/cameras`
	// into it, and nothing else does.
	Name *Domain `json:"name,omitempty"`

	// Labels matches every domain on the source node carrying all of these keys with exactly
	// these values (§10.7). Equality, ANDed, and never empty — see [DomainSelector.Validate].
	Labels map[string]string `json:"labels,omitempty"`
}

// SelectDomain is the direct form: address one domain by its identity.
//
// A constructor rather than a parser — it takes the structured value. `media/cameras` becomes one
// of these in the manifest and in the `label` verb, and nowhere else (§10.6).
func SelectDomain(domain Domain) DomainSelector { return DomainSelector{Name: &domain} }

// SelectLabels is the matching form.
func SelectLabels(labels map[string]string) DomainSelector { return DomainSelector{Labels: labels} }

// Kind returns the selector's kind, or "" if none or several are set.
func (s DomainSelector) Kind() DomainSelectorKind {
	switch {
	case s.Name != nil && s.Labels == nil:
		return DomainSelectorKindName
	case s.Labels != nil && s.Name == nil:
		return DomainSelectorKindLabels
	default:
		return ""
	}
}

// Validate enforces exactly-one-of, and that the chosen kind is usable.
//
// Called from UnmarshalJSON and MarshalJSON both, because a selector built in Go — by the CLI, by
// a test, by the Kubernetes adapter — bypasses the decoder entirely, and an invalid one must not
// reach the wire in either direction.
//
// **A label selector with no keys is refused here rather than only in the manifest** (§10.7). An
// empty map matches every domain on the node, which expands a request's source set across whatever
// that node happens to hold — and it is reachable by omission rather than by intent, since
// `domain: {}` and a `domain:` whose keys were all deleted are both easy to write. The manifest's
// scalar-versus-map rule disposes of the *syntax* question and does not refuse the value, so
// something must.
func (s DomainSelector) Validate() error {
	var set []DomainSelectorKind
	if s.Name != nil {
		set = append(set, DomainSelectorKindName)
	}
	if s.Labels != nil {
		set = append(set, DomainSelectorKindLabels)
	}

	switch len(set) {
	case 1:
	case 0:
		return fmt.Errorf("source.domain: exactly one of %s must be set", strings.Join(domainSelectorKindNames(), ", "))
	default:
		names := make([]string, 0, len(set))
		for _, k := range set {
			names = append(names, string(k))
		}
		return fmt.Errorf("source.domain: %s are both set, expected exactly one", strings.Join(names, " and "))
	}

	if s.Name != nil {
		if err := s.Name.Valid(); err != nil {
			return fmt.Errorf("source.domain.name %q: %w", s.Name, err)
		}
	}
	if s.Labels != nil && len(s.Labels) == 0 {
		return fmt.Errorf("source.domain.labels is empty, which would match every domain on the node")
	}
	return nil
}

// Matches reports whether a domain carrying these labels satisfies the selector: **equality, every
// key ANDed** (§10.7).
//
// There is no `in`, no `exists`, no negation, no wildcard and no value grammar of any kind — a
// value is a string compared whole, case-sensitively. That is the obvious v1 and it is chosen for
// the same reason §9.1 gives for the flow selector: the restriction is what keeps the extension
// additive. `in`, `notin` and `exists` arrive as a **third union kind** rather than by widening
// what a map value may say, because widening the value grammar cannot be taken back — a request
// whose value happened to look like an expression would change meaning under the upgrade,
// silently, and in the direction of matching *more*.
//
// It takes the label map rather than a domain record so that the caller decides what a missing
// record means. That is what keeps "no labels" from accidentally matching an empty selector the
// validator is supposed to have refused already.
func (s DomainSelector) Matches(labels map[string]string) bool {
	if len(s.Labels) == 0 {
		return false
	}
	for key, want := range s.Labels {
		if labels[key] != want {
			return false
		}
	}
	return true
}

// String renders the selector for a log line or an error message.
func (s DomainSelector) String() string {
	switch s.Kind() {
	case DomainSelectorKindName:
		return s.Name.String()
	case DomainSelectorKindLabels:
		keys := make([]string, 0, len(s.Labels))
		for key := range s.Labels {
			keys = append(keys, key+"="+s.Labels[key])
		}
		slices.Sort(keys)
		return "{" + strings.Join(keys, ",") + "}"
	default:
		return "<invalid>"
	}
}

func domainSelectorKindNames() []string {
	return []string{string(DomainSelectorKindName), string(DomainSelectorKindLabels)}
}

// UnmarshalJSON decodes a selector, rejecting both an unknown kind and more than one kind.
func (s *DomainSelector) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("source.domain: %w", err)
	}

	var unknown []string
	for key := range raw {
		if !slices.Contains(domainSelectorKindNames(), key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return fmt.Errorf("source.domain: unknown kind %s (known kinds: %s)",
			strings.Join(quoteAll(unknown), ", "), strings.Join(domainSelectorKindNames(), ", "))
	}

	type plain DomainSelector
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("source.domain: %w", err)
	}

	out := DomainSelector(decoded)
	if err := out.Validate(); err != nil {
		return err
	}

	*s = out
	return nil
}

// MarshalJSON refuses to emit a selector that could not be decoded back.
func (s DomainSelector) MarshalJSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	type plain DomainSelector
	return json.Marshal(plain(s))
}
