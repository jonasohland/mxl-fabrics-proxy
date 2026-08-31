package api

import (
	"errors"
	"fmt"
)

// PathPolicy says whether two requests in one namespace may hold one path (§9.3).
//
// The zero value is [PathsShared], which is the default and is deliberately the permissive one:
// refcounting is the base model and forbidding is the special case. Two requests expanding onto
// one path share one path, one session and one worker pair — nothing is doubled and nothing is
// corrupted, which is §9.1's refcounting working exactly as designed.
//
// What overlap costs is honesty in a matrix: two lit cells that are one stream, counts that do
// not sum, and a cell that goes dark on a click that stopped nothing. Those are real problems for
// a renderer and not problems for the fleet — which gives the governing line, and this is the one
// rule on the far side of it: **conflict rules that protect data integrity are mandatory;
// conflict rules that protect legibility belong to whoever is doing the reading.**
// [ReasonFlowConflict] is the mandatory kind and is never optional for anybody.
type PathPolicy string

const (
	// PathsShared lets two requests in this namespace hold one path. The default.
	PathsShared PathPolicy = "shared"

	// PathsExclusive refuses it: the loser reports INVALID with [ReasonNamespaceOverlap] naming
	// the incumbent, and the path — held by the winner — carries on.
	PathsExclusive PathPolicy = "exclusive"
)

// Exclusive reports whether this policy forbids two requests holding one path.
//
// Written as a method rather than an equality so that the zero value's meaning lives in exactly
// one place: an unset policy is [PathsShared], and every caller that compared against a constant
// would otherwise have to remember that.
func (p PathPolicy) Exclusive() bool { return p == PathsExclusive }

// Validate rejects a policy this build does not know.
//
// Unknown is refused rather than defaulted, which is the same choice [Selector] makes and for the
// same reason: silently reading an unrecognised value as `shared` widens what is permitted, and
// widening is the wrong direction to fail in for something that decides whether two requests may
// move media over one edge.
func (p PathPolicy) Validate() error {
	switch p {
	case "", PathsShared, PathsExclusive:
		return nil
	default:
		return fmt.Errorf("paths: unknown policy %q, expected %q or %q", p, PathsShared, PathsExclusive)
	}
}

// DefaultNamespace is where a request that named no namespace lives.
//
// It is a real namespace and not an exemption: it holds a record like any other, `paths` means
// the same thing in it, and it is where everything on a fleet driven from the CLI ends up. It
// stays [PathsShared], which is the catch-all's correct setting — hand-written manifests land
// here and none of them asked for a partition.
//
// It is the one namespace that cannot be deleted.
const DefaultNamespace = "default"

// Namespace is a partition of requests, and a first-class object in desired state (§9.3).
//
// **It partitions requests and nothing else** — not nodes, not domains, not destinations. Two
// namespaces landing one flow in one destination domain is fan-in, which §9.1 supports and §10.6
// refcounts so the domain is materialised once; forbidding it would make the fleet genuinely
// disjoint at the cost of the arrangement fan-in exists for.
//
// *This supersedes the namespace being the value of a reserved `namespace` label.* That spelling
// was justified by an existing CLI mechanism — `--prune -l namespace=nab` already meant "make this
// namespace equal this file" — rather than by the model, and it cost two things worth more than
// the mechanism saved. `namespace` is a legal user label, so a label an operator wrote for their
// own reasons silently became a partition key. And the manifest wanted a plain `namespace:` field
// while the wire wanted a label, which left the two spellings to be reconciled and a disagreement
// between them refused rather than resolved. A property removes both.
//
// Request-level defaults — a provider pin, an idle teardown, a bandwidth budget once §13's
// admission control exists — are all plausible later and all easier to add than to take back, so
// v1 carries none of them.
type Namespace struct {
	Name string `json:"name"`

	// Paths says whether two requests here may hold one path. Empty is [PathsShared].
	Paths PathPolicy `json:"paths,omitempty"`

	Description string `json:"description,omitempty"`
}

// Validate checks a namespace body's structure.
func (n Namespace) Validate() error {
	if err := ValidNamespace(n.Name); err != nil {
		return err
	}
	if len(n.Description) > maxDescriptionLength {
		return fmt.Errorf("description is longer than %d characters", maxDescriptionLength)
	}
	return n.Paths.Validate()
}

// SameAs reports whether two namespaces are the same intent, and is what makes re-applying an
// unchanged one write nothing.
//
// The same discipline the request POST follows, one object over: a namespace record's revision
// moving wakes every watcher in the fleet, and desired state is low-churn by assumption (§8.3).
func (n Namespace) SameAs(other Namespace) bool {
	return n.Name == other.Name &&
		n.Paths.normalised() == other.Paths.normalised() &&
		n.Description == other.Description
}

// normalised collapses the zero value onto its meaning, so that a record written as `""` and one
// written as `"shared"` compare equal rather than looking like a change on every apply.
func (p PathPolicy) normalised() PathPolicy {
	if p == "" {
		return PathsShared
	}
	return p
}

// Normalise returns the namespace as it is stored: an unset policy spelled out.
//
// Every stored namespace says what its policy is rather than some saying it and the rest implying
// it — the same move [DefaultNamespace] makes one level up. It has to run *before* the unchanged
// comparison, not after, or normalising a spec after deciding whether it differs would make every
// apply of a policy-less document look like a change and write on every pass.
func (n Namespace) Normalise() Namespace {
	n.Paths = n.Paths.normalised()
	return n
}

// maxDescriptionLength bounds the one free-text field on a namespace. Generous, because it is
// prose an operator writes for other operators; bounded, because it is durable state on a key
// every reconcile reads.
const maxDescriptionLength = 1024

// ValidNamespace checks a namespace name (§9.3).
//
// ASCII letters, digits, `-` and `_`, non-empty. Constrained where an ordinary label value is free
// text, because this one is a path segment in a URL, a store key and a `-n` argument on a command
// line — the same reasoning that validates a request name more strictly than the wire type does
// (§9.1).
//
// It lives here rather than in the server because the manifest checks it too, and a namespace a
// file accepts and a POST rejects is the split that makes `apply --dry-run` less useful than the
// apply.
func ValidNamespace(ns string) error {
	if ns == "" {
		return fmt.Errorf("namespace must not be empty: omit it for the %q namespace", DefaultNamespace)
	}
	if len(ns) > maxNameLength {
		return fmt.Errorf("namespace is longer than %d characters", maxNameLength)
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

// NamespaceList is GET /v1/namespaces.
type NamespaceList struct {
	Namespaces []NamespaceInfo `json:"namespaces"`
}

// NamespaceInfo is a namespace as the API returns it: the spec plus what the fleet makes of it.
type NamespaceInfo struct {
	Namespace

	// Requests is how many requests reference this namespace. It is what makes a refused DELETE
	// legible before the operator tries one, and it is the number that refusal quotes.
	Requests int `json:"requests"`
}
