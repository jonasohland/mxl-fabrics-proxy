package api

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// SelectorKind names one way of choosing source flows.
type SelectorKind string

const (
	SelectorKindFlow      SelectorKind = "flow"
	SelectorKindGroupHint SelectorKind = "group_hint"
)

// Selector chooses which source flows a request replicates (§9.1).
//
// A UUID is rarely what a user actually means. An operator means "whatever camera 1 is
// publishing"; the Kubernetes adapter means "everything this pod exposes". Pinning UUIDs
// forces both of them to run their own discovery loop and rewrite requests whenever a producer
// republishes a flow under a new ID — which is exactly the work the server already does. So a
// pinned flow ID is *one kind* of selector rather than the only shape the API can express.
//
// # Exactly one kind
//
// A Selector is a tagged union with exactly one kind set, not a bag of optional fields that
// are implicitly ANDed. That is what makes adding a `label`, `format` or `regexp` kind later
// purely additive: a new kind cannot change the meaning of an existing request, because an
// existing request can never have had two kinds set.
//
// The rejection is the invariant, so this type is the one deliberate exception to the
// package's "never fail on an unrecognised key" rule (see doc.go): an unknown kind is an
// error, not something to ignore. Ignoring it would silently *widen* the selection to whatever
// remained — for a mechanism that moves uncompressed video between hosts, that is the wrong
// direction to fail in.
//
// The cost is recorded rather than hidden: an older client cannot decode a request that uses a
// selector kind added after it was built. It fails loudly instead of displaying something
// false, and it is the same trade §9.1 makes when it calls the union "the design rule that
// keeps this extensible".
//
// # A request owns a set of paths
//
// Whatever the kind, a selector expands to N paths, and N changes as flows appear and
// disappear. Even a pinned flow ID is modelled as a set of size one (§9.1) — see
// [RequestStatus].
type Selector struct {
	// Flow pins a single flow by UUID. Spelled as a bare string on the wire:
	//
	//	"select": {"flow": "5592a23b-0974-45bb-9388-89ea81c42537"}
	Flow string `json:"flow,omitempty"`

	// GroupHint selects every flow carrying a matching NMOS group hint:
	//
	//	"select": {"group_hint": {"name": "Studio A:Camera 1", "type": "video"}}
	GroupHint *GroupHintSelector `json:"group_hint,omitempty"`
}

// GroupHintSelector matches on the NMOS group hint tag (§9.1).
//
// This selector kind comes essentially free: the hint is a tag in flow_def.json,
// mxl-utils parses urn:x-nmos:tag:grouphint/v1.0 into Name and Type, the agent reports the
// parsed value as part of inventory, and the server matches on it.
type GroupHintSelector struct {
	// Name is the group name — everything before the final colon of the tag value, which is
	// how mxl-utils splits it, so "Studio A:Camera 1:video" has the name "Studio A:Camera 1".
	// Matched by exact, case-sensitive string equality.
	Name string `json:"name"`

	// Type is optional. Omitted, it selects every flow sharing the name — which is how you
	// replicate a camera's video and audio together with one request.
	Type string `json:"type,omitempty"`
}

// GroupHint is the parsed hint as observed on a flow (§6).
type GroupHint struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Matches reports whether an observed hint satisfies the selector.
func (s GroupHintSelector) Matches(hint GroupHint) bool {
	if s.Name != hint.Name {
		return false
	}
	return s.Type == "" || s.Type == hint.Type
}

// Kind returns the selector's kind, or "" if none or several are set.
func (s Selector) Kind() SelectorKind {
	switch {
	case s.Flow != "" && s.GroupHint == nil:
		return SelectorKindFlow
	case s.GroupHint != nil && s.Flow == "":
		return SelectorKindGroupHint
	default:
		return ""
	}
}

// Validate enforces exactly-one-of, and that the chosen kind is usable.
//
// Called from UnmarshalJSON and MarshalJSON both, because a Selector built in Go — by the
// importer, by a test, by the Kubernetes adapter — bypasses the decoder entirely, and an
// invalid one must not reach the wire in either direction.
func (s Selector) Validate() error {
	var set []SelectorKind
	if s.Flow != "" {
		set = append(set, SelectorKindFlow)
	}
	if s.GroupHint != nil {
		set = append(set, SelectorKindGroupHint)
	}

	switch len(set) {
	case 1:
	case 0:
		return fmt.Errorf("selector: exactly one of %s must be set", strings.Join(selectorKindNames(), ", "))
	default:
		names := make([]string, 0, len(set))
		for _, k := range set {
			names = append(names, string(k))
		}
		return fmt.Errorf("selector: %s are both set; a selector has exactly one kind", strings.Join(names, " and "))
	}

	if s.GroupHint != nil && s.GroupHint.Name == "" {
		return fmt.Errorf("selector: group_hint.name must not be empty")
	}
	return nil
}

func selectorKindNames() []string {
	return []string{string(SelectorKindFlow), string(SelectorKindGroupHint)}
}

// UnmarshalJSON decodes a selector, rejecting both an unknown kind and more than one kind.
func (s *Selector) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("selector: %w", err)
	}

	var unknown []string
	for key := range raw {
		if !slices.Contains(selectorKindNames(), key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return fmt.Errorf("selector: unknown kind %s (known kinds: %s); a newer server may support it",
			strings.Join(quoteAll(unknown), ", "), strings.Join(selectorKindNames(), ", "))
	}

	type plain Selector
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("selector: %w", err)
	}

	out := Selector(decoded)
	if err := out.Validate(); err != nil {
		return err
	}

	*s = out
	return nil
}

// MarshalJSON refuses to emit a selector that could not be decoded back.
func (s Selector) MarshalJSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	type plain Selector
	return json.Marshal(plain(s))
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}
