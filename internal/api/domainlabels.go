package api

import (
	"fmt"
	"maps"
	"slices"
)

// DomainLabels is the operator's labels on one `(node, domain)` (§10.7).
//
// **Desired state written by a *user* about a node**, which is why it does not live under the
// node's registration: that key is written by the agent (§4). It is the naming half of the old
// `-m` flag, moved — what the flag did well was give a domain a short fleet-wide name; what it did
// badly was make that name a startup argument, so naming a domain cost an agent restart and an
// agent restart re-establishes every flow on the node (§6.1, §16).
//
// # Annotation, never identity
//
// A domain's identity is `<area>/<elements>`, permanently (§10.6). Labels are additional key/value
// pairs attached to `(node, domain)` and nothing else. The tempting alternative — a label supplies
// the domain's *name* — does not survive contact: the domain name is embedded in path identity
// (§5.4), session identity and the `domain` metric label, so renaming a domain would re-identify
// every path through it, tearing down running media on a metadata edit.
//
// # The agent never sees one
//
// Label records are joined against inventory **server-side**, so nothing new reaches the agent, no
// new state is held there, and §4.2's fail-static surface is unchanged. That separation is
// load-bearing rather than tidy: if labels flowed *down* — to feed the discoverer's static list, so
// a labelled-but-empty domain stayed visible — the API could point an agent at a path the host
// never granted. Read-only, but still an exfiltration primitive bounded only by "must look like an
// MXL flow" (§10.7).
type DomainLabels struct {
	Node   string `json:"node"`
	Domain Domain `json:"domain"`

	Labels map[string]string `json:"labels,omitempty"`

	// Declared is the key set the last **apply** declared, sorted.
	//
	// It is `kubectl.kubernetes.io/last-applied-configuration` doing the one job it exists for,
	// reduced to what a flat map needs: not the previous document, just its key set. Without a
	// memory of what the file declared *before*, "remove what this apply no longer declares"
	// cannot be distinguished from "remove what this apply never mentioned" — and those are the
	// two behaviours the whole ownership decision is between (§9.1).
	//
	// Three properties it has to have, and each is a way to get it wrong:
	//
	//   - **Sorted and canonical**, or the no-write-if-unchanged check compares two spellings of
	//     one set and writes on every apply.
	//   - **Written only by an apply.** A patch — the `label` verb — leaves it exactly as it found
	//     it. That is the entire mechanism by which an imperative edit survives a later apply, and
	//     it is one `if`.
	//   - **Not a timestamp and not an owner.** It is a key set, and nothing server-side may
	//     rewrite this record as a side effect: a label record's revision moving wakes every
	//     watcher and moves every request's expansion.
	Declared []string `json:"declared,omitempty"`
}

// LabelName is the one conventional key: what an operator calls this domain (§10.7).
//
// Conventional rather than special. It is rendered as an additional `domain_name` metric label
// beside the identity-valued `domain` one (§12), and its *value* is held to the element grammar
// (§10.6) — the rule outlived the flag it was written for, because a string that ends up in a
// metric label wants the same constraints whether it is identity or decoration.
//
// Deliberately **not** enforced unique per node: identity is the domain name, so two domains
// sharing a `name` label is cosmetic rather than ambiguous, and enforcing it would make a label
// write a cross-record check for no invariant.
const LabelName = "name"

// IsEmpty reports whether this record holds nothing at all.
//
// **An empty result deletes the record** rather than storing one with no labels. The two are
// indistinguishable to every reader — [DomainSelector.Matches] is refused against an empty
// selector, and a domain with no labels matches nothing else — so storing the empty one is a key
// that accumulates and never gets collected (§9.1).
//
// The condition is empty `Labels` *and* empty `Declared`: an apply that declares nothing while an
// imperative key remains must keep the record and its key.
func (d DomainLabels) IsEmpty() bool { return len(d.Labels) == 0 && len(d.Declared) == 0 }

// SameAs reports whether two records are the same state, which is what makes a re-applied
// unchanged label set write nothing.
//
// **`Declared` is part of "unchanged"**, or an apply that changes only which keys it owns silently
// does nothing — and what it owns is what a *later* apply will remove (§9.1).
func (d DomainLabels) SameAs(other DomainLabels) bool {
	return d.Node == other.Node &&
		d.Domain.Equal(other.Domain) &&
		maps.Equal(d.Labels, other.Labels) &&
		slices.Equal(d.Declared, other.Declared)
}

// DomainLabelWrite is the body of POST /v1/nodes/{node}/domains: **two shapes on one endpoint**
// (§9.1).
//
// The two gestures send different bodies, and that is what carries the ownership rule. An *apply*
// sends the full map it declares; an *edit* sends a patch — keys to set, keys to remove, which is
// what `role=cameras role-` already is on the command line. The server merges an apply against the
// keys the last apply declared and merges a patch against nothing.
//
// Same tagged-union discipline as everything else on this wire: exactly one shape set, unknown
// shape refused. It puts two write semantics behind one handler, which is worth saying at the top
// of it — but a second path segment would put two spellings of one object on the wire, which §19
// already argued down for the CLI.
type DomainLabelWrite struct {
	// Domain is which of the node's domains this write is about. Structured, like everywhere else:
	// the manifest and the `label` verb parse `media/cameras`, and nothing else does (§10.6).
	Domain Domain `json:"domain"`

	// Apply is the full map this writer declares. It **owns the keys it declares**: it sets them,
	// removes the ones it declared last time and no longer does, and leaves every other key alone
	// (§9.1).
	//
	// No `omitempty`: an apply that declares *nothing* is a real and meaningful write — it removes
	// whatever it declared last time — and omitempty would drop the empty map on the way out,
	// turning "declare nothing" into "this is not an apply at all". Distinguishing an empty map
	// from an absent one is the whole of the tagged union here.
	Apply map[string]string `json:"apply"`

	// Patch is an imperative edit, merged against nothing.
	Patch *DomainLabelPatch `json:"patch,omitempty"`
}

// DomainLabelPatch is the imperative half: keys to set and keys to remove.
//
// The verb sends a patch rather than a read-modify-write, and the reason is worth recording: RMW
// on a shared record has a lost-update window — two operators labelling one domain between the
// same read and write lose one edit, silently — which is the failure mode this record's whole
// ownership story exists to avoid. §9.1 records that this is the one record several writers are
// structurally expected to touch.
type DomainLabelPatch struct {
	Set    map[string]string `json:"set,omitempty"`
	Remove []string          `json:"remove,omitempty"`
}

// Kind returns "apply" or "patch", or "" if neither or both are set.
func (w DomainLabelWrite) Kind() string {
	switch {
	case w.Apply != nil && w.Patch == nil:
		return "apply"
	case w.Patch != nil && w.Apply == nil:
		return "patch"
	default:
		return ""
	}
}

// Validate enforces exactly-one-shape and the domain's own grammar.
//
// The *labels* are not validated here: that rule belongs to the server, which is the party that
// decides what it is willing to accept as a metric label (§12), exactly as it does for a request's
// user labels.
func (w DomainLabelWrite) Validate() error {
	if err := w.Domain.Valid(); err != nil {
		return fmt.Errorf("domain %q: %w", w.Domain, err)
	}
	switch w.Kind() {
	case "apply", "patch":
	case "":
		if w.Apply == nil && w.Patch == nil {
			return fmt.Errorf("exactly one of apply, patch must be set")
		}
		return fmt.Errorf("apply and patch are both set, expected exactly one")
	}
	if w.Patch != nil && len(w.Patch.Set) == 0 && len(w.Patch.Remove) == 0 {
		return fmt.Errorf("patch sets and removes nothing")
	}
	return nil
}

// Merge applies this write to a stored record and returns the result (§9.1).
//
// **An apply is a three-way merge against `Declared`.** Given a stored map `L`, a stored declared
// set `D`, and an applied map `A`:
//
//	L' = (L − (D − keys(A))) ∪ A        D' = keys(A)
//
// Eleven characters of set arithmetic and it is the entire `kubectl apply` semantic for a flat
// map. Worth reading exactly that way, because every bug in this area is a plausible-looking
// variant of it: `L − D ∪ A` drops imperative keys, `L ∪ A` never removes anything.
//
// **A patch merges against nothing and does not touch `Declared`.** `key=value` sets, `key-`
// removes, and neither changes what a future apply believes it owns. That is what makes `label`
// durable: an operator may name a domain interactively and keep that name, in a fleet whose
// requests are applied from git by somebody else.
func (w DomainLabelWrite) Merge(stored DomainLabels) DomainLabels {
	out := DomainLabels{
		Node:     stored.Node,
		Domain:   stored.Domain,
		Labels:   maps.Clone(stored.Labels),
		Declared: slices.Clone(stored.Declared),
	}
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}

	switch w.Kind() {
	case "apply":
		// L − (D − keys(A)): remove what the last apply declared and this one does not. Keys this
		// writer never declared — an imperative `label` edit — are left exactly where they are.
		for _, key := range out.Declared {
			if _, still := w.Apply[key]; !still {
				delete(out.Labels, key)
			}
		}
		maps.Copy(out.Labels, w.Apply)
		out.Declared = slices.Sorted(maps.Keys(w.Apply))

	case "patch":
		maps.Copy(out.Labels, w.Patch.Set)
		for _, key := range w.Patch.Remove {
			delete(out.Labels, key)
		}
		// Declared is untouched, deliberately. See the type comment.
	}

	if len(out.Labels) == 0 {
		out.Labels = nil
	}
	if len(out.Declared) == 0 {
		out.Declared = nil
	}
	return out
}

// DomainLabelResult is what a label write returns: the record, and what it moved (§9.1).
//
// **The blast radius is printed on the real write, not only on a dry run**, and it is a blast
// radius rather than a confirmation prompt: the CLI is scripted by the same operators who use it
// interactively, and a verb that blocks on a tty is a verb that hangs in a pipeline.
//
// A label joins or removes a domain from a request's expansion, so it starts and stops media
// exactly as a request does — one level of indirection away, which makes it *easier* to do by
// accident rather than harder. Removing `role=cameras` from a domain five requests select can tear
// down running sessions, and a verb whose whole purpose is a quick interactive edit is the worst
// place for that to be unpreviewed.
type DomainLabelResult struct {
	DomainLabels

	// Stopped are the paths this write removes. Each carries the requests that were feeding it,
	// which is what `path.requests[]` already answers — so this is a renderer rather than a
	// computation (§9.1).
	//
	// Note these are torn down immediately: a label removing a path is not a node going away, so
	// nothing is frozen (§4.2).
	Stopped []Path `json:"stopped,omitempty"`

	// Started are the paths this write creates.
	Started []Path `json:"started,omitempty"`
}

// DomainInfo is one domain as GET /v1/nodes/{node}/domains reports it (§9.1).
//
// The join has to render four things that disagree with each other, so the response type is built
// deliberately rather than grown: an observed domain with labels, an observed domain without, a
// **labelled domain nobody observes** — that is how an operator sees a label applied before the
// producer came up — and the settling flag that says which of those the answer can be trusted for.
type DomainInfo struct {
	Domain Domain `json:"domain"`

	// Observed reports that the node's agent currently reports this domain. A label on a domain
	// the node does not report is accepted and inert — a pending record, not an error (§10.7).
	Observed bool `json:"observed"`

	Labels map[string]string `json:"labels,omitempty"`

	// Flows are the flows in it, empty for a domain that is labelled but not observed.
	Flows []FlowInventory `json:"flows,omitempty"`
}

// Name is the value of the conventional `name` label, or empty (§10.7).
func (d DomainInfo) Name() string { return d.Labels[LabelName] }
