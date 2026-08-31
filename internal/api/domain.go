package api

import (
	"fmt"
	"slices"
	"strings"
)

// MaxDomainNameLen bounds one **element** of a domain, and an area name with it.
// [MaxDomainPathLen] bounds the whole rendered domain, area segment included.
//
// Far below NAME_MAX, deliberately: an element is a directory name, but the domain it is part of
// is also a metric label, part of a path identity and a component of every error message about
// the session, and a 255-byte element is legible in none of those. Nothing in a real deployment
// is close to it.
const MaxDomainNameLen = 64

// ValidDomainName reports whether a name may be one element of a domain — a directory inside an
// operator-configured area (§10.6).
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
// Area names are checked with the same rule even though an area name is never joined into a path.
// Not load-bearing for traversal there — it is applied so that an area reads the same in a config
// file, a request, an error message and a metric label as the domains it holds, and because the
// area name is the *first segment of a domain's own name* and so has to be unambiguous about
// where it stops.
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
			return fmt.Errorf("contains byte %#x, not an ASCII letter, digit, '-', '_' or '.'", c)
		}
	}
	return nil
}

// DomainSeparator joins an area name to a domain's elements, and the elements to each other, when
// a domain has to be rendered as a single string — a metric label, a path identity, an error
// message (§10.6).
const DomainSeparator = "/"

// Limits on a domain as a whole. Per-element length is [MaxDomainNameLen].
//
// Both are legibility bounds rather than safety ones: safety comes from every element being a
// validated plain name, so nothing about the depth can escape the area. A domain nobody can read
// in a metric label or an error message is still a bad domain.
//
// **[MaxDomainPathLen] counts the area segment**, because it bounds the rendered name and the
// rendered name begins with the area. Said here rather than left to the call site, where the
// answer is invisible: measuring only the elements would let the cap loosen silently the day the
// area moved into the name.
const (
	MaxDomainElements = 8
	MaxDomainPathLen  = 255
)

// Domain is a domain's fleet-wide identity: the area it is in, and its path elements relative to
// that area (§10.6).
//
// **One kind of domain, whichever direction this project uses it in** — a place rather than a
// channel. *This supersedes input domains and output domains being separate concepts with separate
// identities.* A domain is a directory that holds flows; several processes routinely write
// different flows into one, and the single-writer constraint MXL actually enforces is per *flow*.
// Read as a multicast group rather than as a pipe, this project is one participant among a node's
// media functions rather than the proprietor of any directory.
//
// **Elements rather than a string**, because it is what makes the containment invariant
// *structural* rather than argued: joining validated elements onto an area's path yields exactly
// `area + "/" + join(elements)`, which the agent checks as an equality on the whole path. There is
// no prefix reasoning anywhere and no boundary case for a separator to hide in. It is also the
// other half of "parsed at exactly one boundary": the server, the assignment and the agent's
// resolver take structure and never text, so there is no second parser to disagree with the
// manifest's.
type Domain struct {
	// Area is the name of the area this domain lives in, as the node advertises it. It is the
	// first segment of the domain's rendered name and is never joined into a path by anything but
	// the agent, which looks it up among its own areas.
	Area string `json:"area"`

	// Elements are the path elements relative to the area. At least one: an area's own directory
	// is not a domain.
	Elements []string `json:"elements"`
}

// String renders the domain as the single string everything downstream carries: the assignment's
// domain, the path and session identity, the `domain` metric label, and every error message about
// the session.
//
// **Injective**, and that is the property the rest of the design leans on. Neither an area name
// nor an element can contain the separator — [ValidDomainName] refuses it — so the rendering and
// the (area, elements) pair are in bijection and nothing is lost by carrying the string
// downstream.
func (d Domain) String() string {
	if len(d.Elements) == 0 {
		return d.Area
	}
	return d.Area + DomainSeparator + strings.Join(d.Elements, DomainSeparator)
}

// IsZero reports whether this is the empty domain, which names nothing.
func (d Domain) IsZero() bool { return d.Area == "" && len(d.Elements) == 0 }

// Equal reports whether two domains are the same domain.
func (d Domain) Equal(other Domain) bool {
	return d.Area == other.Area && slices.Equal(d.Elements, other.Elements)
}

// Valid checks the whole domain: the area name, every element, the depth cap and the rendered
// length (§10.6).
func (d Domain) Valid() error {
	if d.Area == "" {
		return fmt.Errorf("names no area")
	}
	if err := ValidDomainName(d.Area); err != nil {
		return fmt.Errorf("area %q: %w", d.Area, err)
	}
	if err := ValidDomainElements(d.Elements); err != nil {
		return err
	}
	if rendered := d.String(); len(rendered) > MaxDomainPathLen {
		return fmt.Errorf("is %d bytes long, at most %d", len(rendered), MaxDomainPathLen)
	}
	return nil
}

// NestedIn reports whether this domain lies inside another — `fast/a` inside `fast/a/b`.
//
// **Within one area.** Two domains in different areas are two directory trees and cannot nest,
// whatever their elements look like.
//
// An exact slice-prefix test, which is the other thing the element form buys: the string spelling
// of this question has to work around `studio-ab` looking like a child of `studio-a`.
func (d Domain) NestedIn(outer Domain) bool {
	if d.Area != outer.Area || len(outer.Elements) >= len(d.Elements) {
		return false
	}
	return slices.Equal(d.Elements[:len(outer.Elements)], outer.Elements)
}

// ValidDomainElements checks a domain's elements: one or more, each a plain name (§10.6).
//
// Separate from [Domain.Valid] because the agent's resolver takes the elements alone, having
// already looked the area up among the ones it advertises.
func ValidDomainElements(elements []string) error {
	switch {
	case len(elements) == 0:
		return fmt.Errorf("empty")
	case len(elements) > MaxDomainElements:
		return fmt.Errorf("has %d elements, at most %d", len(elements), MaxDomainElements)
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
