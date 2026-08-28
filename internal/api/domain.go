package api

import (
	"fmt"
	"slices"
	"strings"
)

// MaxDomainNameLen bounds one **element** of an output domain, and an output root name with it.
// [MaxDomainPathLen] bounds the whole rendered domain.
//
// Far below NAME_MAX, deliberately: an element is a directory name, but the domain it is part of
// is also a metric label, part of a path identity and a component of every error message about
// the session, and a 255-byte element is legible in none of those. Nothing in a real deployment
// is close to it.
const MaxDomainNameLen = 64

// ValidDomainName reports whether a name may be an output domain — a directory replication
// creates inside an operator-configured root (§10.6).
//
// This is the character-set half of the invariant that stops this API being a remote
// arbitrary-filesystem-write primitive on every node in the fleet (§7.2, §13), and it is the
// primary half: a name that passes has no separator and is not `..`, so joining it onto a root
// yields a direct child by construction. The agent applies a containment check on top, which is
// the backstop if this rule is ever loosened rather than a second opinion on it.
//
// It lives here, in the shared wire package, for the same reason [SHMFabric] does: the server
// rejects a request naming something else, the agent independently refuses to resolve it, and
// the duplication is only worth its keep if the two cannot disagree about what the rule is. A
// second copy is how a deliberate duplicate quietly becomes a divergence.
//
// Root names are checked with the same rule even though a root name is never joined into a path.
// Not load-bearing for traversal there — it is applied so that a root reads the same in a config
// file, a request, an error message and a metric label as the domains it holds.
func ValidDomainName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("empty")
	case len(name) > MaxDomainNameLen:
		return fmt.Errorf("longer than %d bytes", MaxDomainNameLen)
	case name == "." || name == "..":
		return fmt.Errorf("names an existing directory rather than a new one")
	}

	// A leading dot is a hidden directory: invisible to an operator listing the root, and skipped
	// by mxl-utils' discovery, so a materialised domain named that way could never be observed
	// and its path could never reach ACTIVE (§10.6). A leading dash is a name that reads as a
	// flag in every tool it will ever be handed to.
	if name[0] == '.' || name[0] == '-' {
		return fmt.Errorf("must not begin with %q", name[:1])
	}

	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.'
		if !ok {
			// Byte-wise, so a multi-byte character is reported as the byte that failed rather than
			// decoded — the names this refuses are frequently unicode lookalikes for names it
			// accepts, and rendering one back to an operator as though it were ASCII is how the
			// error message becomes part of the confusion.
			return fmt.Errorf("contains byte %#x; a domain name is ASCII letters, digits, '-', '_' and '.'", c)
		}
	}
	return nil
}

// DomainSeparator joins the elements of an output domain when one has to be rendered as a single
// string — a metric label, a path identity, an error message (§10.6).
const DomainSeparator = "/"

// Limits on an output domain as a whole. Per-element length is [MaxDomainNameLen].
//
// Both are legibility bounds rather than safety ones: safety comes from every element being a
// validated plain name, so nothing about the depth can escape the root. A domain nobody can read
// in a metric label or an error message is still a bad domain.
const (
	MaxDomainElements = 8
	MaxDomainPathLen  = 255
)

// DomainPath renders an output domain's elements as the single string everything downstream of the
// request carries: the assignment's name, the path and session identity, the `domain` metric
// label, and every error message about the session.
//
// Injective, and that is the property the rest of the design leans on. No element can contain the
// separator — [ValidDomainName] refuses it — so the rendering and the element list are in
// bijection and nothing is lost by carrying the string downstream.
func DomainPath(elements []string) string { return strings.Join(elements, DomainSeparator) }

// ValidDomainElements checks an output domain: one or more elements, each a plain name (§10.6).
//
// **The elements are the model and the wire form; a `a/b` string exists only in a manifest file,
// and only the CLI ever parses one.** That is what keeps the containment invariant structural
// rather than argued: joining validated elements onto a root yields exactly
// `root + "/" + DomainPath(elements)`, which the agent checks as an equality on the whole path.
// There is no prefix reasoning anywhere, and no boundary case for a separator to hide in.
func ValidDomainElements(elements []string) error {
	switch {
	case len(elements) == 0:
		return fmt.Errorf("empty")
	case len(elements) > MaxDomainElements:
		return fmt.Errorf("has %d elements; at most %d", len(elements), MaxDomainElements)
	}
	if rendered := DomainPath(elements); len(rendered) > MaxDomainPathLen {
		return fmt.Errorf("is %d bytes long; at most %d", len(rendered), MaxDomainPathLen)
	}

	for i, element := range elements {
		if err := ValidDomainName(element); err != nil {
			if len(elements) == 1 {
				return err
			}
			return fmt.Errorf("element %d (%q): %w", i+1, element, err)
		}
	}
	return nil
}

// NestedIn reports whether one output domain lies inside another — `["a"]` and `["a","b"]`.
//
// An exact slice-prefix test, which is the other thing the element form buys: the string spelling
// of this question has to work around `studio-ab` looking like a child of `studio-a`.
func NestedIn(inner, outer []string) bool {
	if len(outer) >= len(inner) {
		return false
	}
	return slices.Equal(inner[:len(outer)], outer)
}
