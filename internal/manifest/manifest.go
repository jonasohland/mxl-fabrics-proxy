// Package manifest reads the declarative file `mxl-replicator apply` takes (§9.1, plan M8b).
//
// A manifest is multi-document YAML — one object per document, `---` separated — that decodes to
// a set of [api.RequestSpec] and [api.Namespace]. It is the interface an operator actually uses;
// the HTTP API underneath is the contract.
//
// # Why this needs no machinery
//
// `POST /v1/namespaces/{ns}/requests` is create-or-update keyed on the client-supplied name, and
// `(namespace, name)` *is* the request's ID (§9.1, §9.3). So a file naming a set of requests is
// already an apply: each document is one POST, and `DELETE` removes what a document named. The
// idempotency key that §9.1 asked for on the Kubernetes adapter's behalf turns out to be the
// whole mechanism.
//
// # `kind:` names the object, and defaults to `request`
//
// *This supersedes "the file is deliberately not a Kubernetes manifest — no `apiVersion`, no
// `kind`", which allowed for exactly this: an optional `kind:` defaulting to the request is
// additive and costs nothing when a second object type appears.* Two appeared at once —
// `namespace` (§9.3) and `domain` (§10.7) — which is the reason to add it deliberately and once
// rather than twice in a row. An unrecognised `kind` is an error, per the strict-decoding rule
// below.
//
// There is still no `apiVersion`: that would be ceremony expressing nothing, and the shape stays
// close enough that the roadmapped Kubernetes adapter (§19) is a mechanical conversion.
//
// # Apply orders documents by kind
//
// Namespaces, then requests, regardless of file order (see [SortForApply]). The end state does not
// depend on it, since `Compute` is recomputed and namespaces auto-create on reference (§9.3), but
// the *intermediate* state does: a request applied before the namespace document that makes its
// namespace exclusive would be admitted and then invalidated, which reads as the apply having
// broken something.
//
// # Strict decoding
//
// An unrecognised key is an **error**. This is deliberately the opposite of the rule for
// `TargetInfo` (§5.2), where an unknown field arrives from an independently-versioned upstream
// and failing closed would take out replication on an unrelated upgrade. Here the file is written
// by a human against this binary, and a typo'd key that silently does nothing is precisely the
// failure a declarative format exists to prevent.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// Kind names what a document is.
type Kind string

const (
	// KindRequest is the default, and what a document with no `kind:` is.
	KindRequest Kind = "request"

	// KindNamespace is a request partition and its rules (§9.3).
	KindNamespace Kind = "namespace"

	// KindDomain is the operator's labels on one node's domain (§10.7).
	KindDomain Kind = "domain"
)

// applyOrder is the order kinds are applied in, lowest first. See the package doc.
var applyOrder = []Kind{KindNamespace, KindDomain, KindRequest}

// Document is one object in a manifest: a kind, and exactly one of the typed bodies.
//
// A tagged union rather than a struct with every kind's fields on it, so that a `paths:` key on a
// request document is the error it ought to be rather than a field that is quietly ignored.
type Document struct {
	Kind Kind

	Request   *RequestDoc
	Namespace *NamespaceDoc
	Domain    *DomainDoc

	// where this document came from — a file path and, past the first, its index. Carried on the
	// document so that an error raised *after* parsing can still say which one.
	where string
}

// Where is the document's origin, for an error message.
func (d Document) Where() string { return d.where }

// Name is what the document identifies, whatever its kind. It is what `delete -f` reads.
func (d Document) Name() string {
	switch {
	case d.Request != nil:
		return d.Request.Name
	case d.Namespace != nil:
		return d.Namespace.Name
	case d.Domain != nil:
		// A domain document is identified by the pair, because a domain name means nothing without
		// the node it is on (§10.7).
		return d.Domain.Node + ":" + d.Domain.Domain
	default:
		return ""
	}
}

// ID is the identity of a request document. Only meaningful for [KindRequest].
func (d Document) ID() api.RequestID {
	if d.Request == nil {
		return api.RequestID{}
	}
	return api.RequestID{Namespace: d.Request.namespace(), Name: d.Request.Name}
}

// key is what makes two documents "the same object" for the duplicate check. The kind is in it
// because a namespace and a request may legitimately share a name.
func (d Document) key() string { return string(d.Kind) + "/" + d.qualified() }

func (d Document) qualified() string {
	if d.Request != nil {
		return d.ID().String()
	}
	return d.Name()
}

// NamespaceDoc is `kind: namespace` (§9.3).
type NamespaceDoc struct {
	Kind Kind   `yaml:"kind"`
	Name string `yaml:"name"`

	// Paths is `shared` (the default) or `exclusive`. Declaring it here is what makes it beat the
	// defaults an auto-create would have written, because apply orders namespaces first.
	Paths string `yaml:"paths"`

	Description string `yaml:"description"`
}

// Spec converts the document to the wire type and validates it.
func (d NamespaceDoc) Spec() (api.Namespace, error) {
	spec := api.Namespace{
		Name:        d.Name,
		Paths:       api.PathPolicy(d.Paths),
		Description: d.Description,
	}
	if err := spec.Validate(); err != nil {
		return api.Namespace{}, err
	}
	return spec.Normalise(), nil
}

// DomainDoc is `kind: domain`: the operator's labels on one `(node, domain)` (§10.7).
//
// **An apply owns the keys it declares.** It sets them, removes the ones it declared last time and
// no longer does, and leaves every other key alone — so an imperative `label` edit survives a
// later apply that does not mention it (§9.1). *This supersedes "a `kind: domain` document
// replaces the whole label set", which was reached by consistency with the request POST and is
// wrong for a reason that is easy to miss: a request's spec has one writer by construction, and a
// domain's label map has no such owner.*
//
// Scoping is a different rule that reads similarly, and is worth restating: a file carrying three
// of these touches three records and leaves every other domain in the fleet alone. `--prune` does
// not extend to labels and there is no other mechanism that would.
type DomainDoc struct {
	Kind Kind `yaml:"kind"`

	// Node is the node the domain is on. Not validated against the fleet: a label on a domain
	// nobody reports is an accepted, inert, pending record (§10.7).
	Node string `yaml:"node"`

	// Domain is `<area>/<elements>`, split by [ParseDomain] — the one parser of a domain string in
	// the tree (§10.6).
	Domain string `yaml:"domain"`

	Labels map[string]string `yaml:"labels"`
}

// Write converts the document to the wire type: an **apply**, carrying the full map it declares.
func (d DomainDoc) Write() (string, api.DomainLabelWrite, error) {
	if d.Node == "" {
		return "", api.DomainLabelWrite{}, errors.New("node is required")
	}

	domain, err := ParseDomain(d.Domain)
	if err != nil {
		return "", api.DomainLabelWrite{}, fmt.Errorf("domain: %w", err)
	}

	// An explicit empty map, not nil: `labels: {}` and an omitted `labels:` both mean "this
	// document declares no keys", which removes whatever it declared last time. Nil would be the
	// *patch* shape, and this is never that.
	labels := d.Labels
	if labels == nil {
		labels = map[string]string{}
	}

	write := api.DomainLabelWrite{Domain: domain, Apply: labels}
	if err := write.Validate(); err != nil {
		return "", api.DomainLabelWrite{}, err
	}
	return d.Node, write, nil
}

// RequestDoc is one replication request as a manifest spells it.
//
// It is a separate type from [api.RequestSpec] rather than YAML tags on it, for two reasons. The
// wire type keeps its selector as a tagged union (§9.1) while the file flattens it onto the
// source, which is friendlier to write and no less strict — the union rule becomes validation
// rather than syntax. And the file is a user interface with its own compatibility story, which
// should be free to diverge from the wire contract rather than pinning it.
type RequestDoc struct {
	Kind Kind `yaml:"kind"`

	// Name is the request's identity within its namespace: half of its ID, its idempotency key,
	// and what `delete` takes.
	Name string `yaml:"name"`

	// Namespace is the partition this request belongs to (§9.3). Omitted, it is
	// [api.DefaultNamespace].
	//
	// **A real property on the wire too**, which it did not used to be: it was a reserved label,
	// and the file's plain `namespace:` field had to be reconciled against `labels.namespace` with
	// a disagreement between them refused rather than resolved. Both spellings are gone in favour
	// of one, and `namespace` under `labels:` is now an ordinary user label meaning nothing to the
	// server.
	Namespace string `yaml:"namespace"`

	Source       SourceDoc        `yaml:"source"`
	Destinations []DestinationDoc `yaml:"destinations"`

	// Provider is the default pin for every destination (§10.4). A bare string pins; a list
	// expresses "prefer this, that is acceptable"; omitted, the server negotiates in its
	// configured order. Never silently substituted.
	Provider ProviderDoc `yaml:"provider"`

	IdleTeardownMS *int64 `yaml:"idle_teardown_ms"`
	SchedPrio      *int   `yaml:"sched_prio"`

	Labels map[string]string `yaml:"labels"`
}

func (d RequestDoc) namespace() string {
	if d.Namespace == "" {
		return api.DefaultNamespace
	}
	return d.Namespace
}

// SourceDoc is where to replicate from. Exactly one of Flow and GroupHint must be set.
//
// The selector is flattened onto the source here where the wire type nests it under `select`.
// §9.1's tagged-union discipline is about the wire type and survives intact: this decodes into
// [api.Selector] and the exactly-one rule is enforced below.
type SourceDoc struct {
	Node string `yaml:"node"`

	// Domain is **a scalar for a name and a map for a label set**, which is the whole of the
	// disambiguation and needs no marker key: a name is `media/cameras`, a selector is
	// `{role: cameras}`, and YAML already tells the two apart (§9.1).
	//
	// It also disposes of `domain: {}` — an empty map matching every domain on the node — which
	// becomes a label selector with no keys and is refused as one by
	// [api.DomainSelector.Validate], rather than needing a rule of its own.
	Domain DomainSelectorDoc `yaml:"domain"`

	// Flow pins one flow ID.
	Flow string `yaml:"flow"`

	// GroupHint selects every flow carrying a matching NMOS group hint. Omitting `type` selects
	// every flow sharing the name, which is how a camera's video and audio are replicated
	// together.
	GroupHint *GroupHintDoc `yaml:"group_hint"`
}

// DomainSelectorDoc is `source.domain`: a scalar name or a label map (§9.1, §10.7).
//
// Flattened in the file, structured on the wire — the same discipline the flow selector and the
// destination domain follow. The tagged-union rule survives intact: the exactly-one check becomes
// validation rather than syntax.
type DomainSelectorDoc struct {
	Name   string
	Labels map[string]string
}

// UnmarshalYAML accepts `domain: media/cameras` and `domain: {role: cameras}`.
func (d *DomainSelectorDoc) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		return node.Decode(&d.Name)
	case yaml.MappingNode:
		// Decoded into a non-nil map even when empty, so that `domain: {}` reaches the validator as
		// a label selector with no keys — which is what refuses it — rather than as an absent one.
		d.Labels = map[string]string{}
		return node.Decode(&d.Labels)
	default:
		return fmt.Errorf("source.domain: expected a name or a label map, got %s", kindName(node.Kind))
	}
}

// selector converts the flattened form to the wire type and validates it.
func (d DomainSelectorDoc) selector() (api.DomainSelector, error) {
	var out api.DomainSelector
	switch {
	case d.Name != "" && d.Labels != nil:
		return out, errors.New("source.domain is both a name and a label set")
	case d.Name != "":
		// **The one parser of a domain string in the tree** (§10.6). A `name` selector carries the
		// same structured value a destination does, so the "parsed at exactly one boundary" rule is
		// not quietly broken by the selector.
		domain, err := ParseDomain(d.Name)
		if err != nil {
			return out, fmt.Errorf("source.domain %w", err)
		}
		out.Name = &domain
	case d.Labels != nil:
		out.Labels = d.Labels
	default:
		return out, errors.New("source.domain is required: a name like media/cameras, or a label set")
	}

	if err := out.Validate(); err != nil {
		return out, err
	}
	return out, nil
}

// GroupHintDoc is the `urn:x-nmos:tag:grouphint/v1.0` selector.
type GroupHintDoc struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
}

// DestinationDoc is one place the source goes.
type DestinationDoc struct {
	Node string `yaml:"node"`

	// Domain is where to replicate into, written as `<area>/<elements>`: `fast/ingest`, or
	// `fast/studio-a/cam1` to nest it. **This is the only place in the system where a domain is a
	// string.**
	//
	// [ParseDomain] splits it here, at the file boundary, and everything downstream — the wire
	// type, the server, the assignment, the agent's resolver — carries the structure. That is what
	// keeps the containment invariant structural: the resolver takes a validated area name and a
	// validated element list, and checks its work as an equality on the whole path, with no
	// separator semantics to get wrong and no second parser to disagree with this one (§10.6).
	//
	// *There used to be a separate `root:` key beside this, omittable on a node advertising
	// exactly one root.* The area is the first segment of the domain's name now, so omitting it
	// would be omitting half the name.
	Domain string `yaml:"domain"`

	// Provider overrides the document-level pin for this destination alone, because a provider is
	// negotiated per (source, destination) pair (§10.3).
	Provider ProviderDoc `yaml:"provider"`
}

// ProviderDoc accepts a bare string or a list, matching the wire type's two spellings.
type ProviderDoc []string

// UnmarshalYAML accepts `provider: verbs` and `provider: [verbs, tcp]`.
func (p *ProviderDoc) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var single string
		if err := node.Decode(&single); err != nil {
			return err
		}
		*p = ProviderDoc{single}
		return nil
	}
	var list []string
	if err := node.Decode(&list); err != nil {
		return fmt.Errorf("provider: expected a string or a list of strings: %w", err)
	}
	*p = list
	return nil
}

func (p ProviderDoc) pin() api.ProviderPin {
	if len(p) == 0 {
		return nil
	}
	pin := make(api.ProviderPin, 0, len(p))
	for _, name := range p {
		pin = append(pin, api.Provider(name))
	}
	return pin
}

// Spec converts a request document to the wire type and validates it.
//
// Validation is [api.RequestSpec.Validate] plus the one rule that only exists in the file: the
// selector is flattened here, so "exactly one kind" has to be checked rather than being
// guaranteed by the shape.
func (d RequestDoc) Spec() (api.RequestSpec, error) {
	var spec api.RequestSpec

	selector, err := d.Source.selector()
	if err != nil {
		return spec, err
	}

	domain, err := d.Source.Domain.selector()
	if err != nil {
		return spec, err
	}

	spec = api.RequestSpec{
		Namespace: d.namespace(),
		Name:      d.Name,
		Source:    api.Source{Node: d.Source.Node, Domain: domain, Select: selector},
		Provider:  d.Provider.pin(),
		SchedPrio: d.SchedPrio,
		Labels:    d.Labels,
	}
	for i, dst := range d.Destinations {
		domain, err := ParseDomain(dst.Domain)
		if err != nil {
			return api.RequestSpec{}, fmt.Errorf("destinations[%d].domain: %w", i, err)
		}
		spec.Destinations = append(spec.Destinations, api.Destination{
			Node:     dst.Node,
			Domain:   domain,
			Provider: dst.Provider.pin(),
		})
	}
	if d.IdleTeardownMS != nil {
		teardown := api.Milliseconds(*d.IdleTeardownMS)
		spec.IdleTeardown = &teardown
	}

	if err := spec.Validate(); err != nil {
		return api.RequestSpec{}, err
	}
	return spec, nil
}

// selector enforces the tagged union the flattened spelling cannot enforce structurally.
func (s SourceDoc) selector() (api.Selector, error) {
	switch {
	case s.Flow != "" && s.GroupHint != nil:
		return api.Selector{}, errors.New("source names both flow and group_hint")
	case s.Flow != "":
		return api.Selector{Flow: s.Flow}, nil
	case s.GroupHint != nil:
		if s.GroupHint.Name == "" {
			return api.Selector{}, errors.New("source.group_hint.name is required")
		}
		return api.Selector{GroupHint: &api.GroupHintSelector{
			Name: s.GroupHint.Name,
			Type: s.GroupHint.Type,
		}}, nil
	default:
		return api.Selector{}, errors.New("source names no selector: give it a flow or a group_hint")
	}
}

// Parse decodes a multi-document manifest into its documents, **without** converting them to wire
// specs.
//
// The split is what lets one file serve two verbs with different needs. `apply` needs every
// document to be a complete, valid object; `delete` needs only the kinds and the names, because it
// removes what the file *named* and has no use for where any of it was going. A manifest that has
// drifted from what is deployed — or that was only ever written to record the names — must still
// be able to delete, or "delete what this file created" stops working exactly when it is most
// wanted.
//
// What Parse does check on every document, whichever verb asked for it:
//
//   - **the kind**, because an unrecognised one means this file was written against a different
//     binary and nothing here can be trusted to mean what it says;
//   - **unknown keys**, because a typo is a typo in a file you are deleting from too, and it may
//     mean the name you meant is somewhere this did not look;
//   - **that there is a name**, because a document without one identifies nothing.
//
// origin names the source for error messages — a file path, or "-" for stdin. Errors carry it
// along with the document's index, because a file with eight documents in it needs to say which
// one is wrong.
func Parse(r io.Reader, origin string) ([]Document, error) {
	decoder := yaml.NewDecoder(r)

	var docs []Document
	for index := 0; ; index++ {
		// Two passes over one document: the first reads the tree so the kind can be found, the
		// second decodes it strictly into the type that kind names. Strictness is a property of a
		// *decoder*, not of a node, so re-encoding the tree is what buys unknown-key errors on a
		// type the first pass could not have known to use.
		var node yaml.Node
		err := decoder.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where(origin, index), err)
		}

		// An empty document — a stray `---`, or a file ending in one — is skipped rather than
		// being reported as an object with no name. Writing a trailing separator is a habit, not
		// a mistake.
		if isEmptyNode(&node) {
			continue
		}

		doc, err := decodeDocument(&node)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where(origin, index), err)
		}
		if doc.Name() == "" {
			return nil, fmt.Errorf("%s: name is required", where(origin, index))
		}

		doc.where = where(origin, index)
		docs = append(docs, doc)
	}
	return docs, nil
}

// decodeDocument reads the kind, then decodes the body strictly as that kind.
func decodeDocument(node *yaml.Node) (Document, error) {
	var probe struct {
		Kind Kind `yaml:"kind"`
	}
	if err := node.Decode(&probe); err != nil {
		return Document{}, err
	}
	if probe.Kind == "" {
		probe.Kind = KindRequest
	}

	raw, err := yaml.Marshal(node)
	if err != nil {
		return Document{}, err
	}
	strict := func(into any) error {
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if err := decoder.Decode(into); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}

	switch probe.Kind {
	case KindRequest:
		var body RequestDoc
		if err := strict(&body); err != nil {
			return Document{}, err
		}
		return Document{Kind: KindRequest, Request: &body}, nil

	case KindNamespace:
		var body NamespaceDoc
		if err := strict(&body); err != nil {
			return Document{}, err
		}
		return Document{Kind: KindNamespace, Namespace: &body}, nil

	case KindDomain:
		var body DomainDoc
		if err := strict(&body); err != nil {
			return Document{}, err
		}
		return Document{Kind: KindDomain, Domain: &body}, nil

	default:
		return Document{}, fmt.Errorf("unknown kind %q, expected one of %s", probe.Kind, kindList())
	}
}

func kindList() string {
	names := make([]string, 0, len(applyOrder))
	for _, kind := range applyOrder {
		names = append(names, string(kind))
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// SortForApply orders documents by kind — namespaces, then requests — keeping file order within a
// kind. See the package doc for why the *intermediate* state depends on it even though the end
// state does not.
//
// A copy, because a caller may want to report the file in the order it was written.
func SortForApply(docs []Document) []Document {
	out := slices.Clone(docs)
	sort.SliceStable(out, func(i, j int) bool {
		return slices.Index(applyOrder, out[i].Kind) < slices.Index(applyOrder, out[j].Kind)
	})
	return out
}

// SortForDelete is [SortForApply] reversed: requests before the namespaces they live in, because
// a namespace delete is refused while any request references it (§9.3).
func SortForDelete(docs []Document) []Document {
	out := SortForApply(docs)
	slices.Reverse(out)
	return out
}

// Requests returns the request documents alone, in the order given.
func Requests(docs []Document) []Document {
	var out []Document
	for _, doc := range docs {
		if doc.Kind == KindRequest {
			out = append(out, doc)
		}
	}
	return out
}

// Specs converts request documents to wire specs, validating each in full. This is what `apply`
// needs and `delete` does not. Documents of other kinds are skipped.
func Specs(docs []Document) ([]api.RequestSpec, error) {
	var specs []api.RequestSpec
	for _, doc := range docs {
		if doc.Kind != KindRequest {
			continue
		}
		spec, err := doc.Request.Spec()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", doc.where, err)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// IDs returns the identities of the request documents, which is what `delete` needs.
func IDs(docs []Document) []api.RequestID {
	ids := make([]api.RequestID, 0, len(docs))
	for _, doc := range docs {
		if doc.Kind == KindRequest {
			ids = append(ids, doc.ID())
		}
	}
	return ids
}

// isEmptyNode reports whether a decoded document carried nothing. A null node is what a stray
// `---` produces; an empty mapping is what a document of only comments produces.
//
// Decoding into a [yaml.Node] yields the *document* node rather than its content, so the wrapper
// is unwrapped first — without that, every empty document looks like a mapping with one child and
// a trailing `---` becomes an object with no name.
func isEmptyNode(node *yaml.Node) bool {
	for node != nil && node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return true
		}
		node = node.Content[0]
	}
	switch {
	case node == nil, node.Kind == 0, node.Tag == "!!null":
		return true
	default:
		return node.Kind == yaml.MappingNode && len(node.Content) == 0
	}
}

// kindName renders a YAML node kind for an error message.
func kindName(kind yaml.Kind) string {
	switch kind {
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.ScalarNode:
		return "a scalar"
	default:
		return "something else"
	}
}

func where(origin string, index int) string {
	if index == 0 {
		return origin
	}
	return fmt.Sprintf("%s document %d", origin, index+1)
}

// ParseFile reads one manifest file, or stdin when path is "-".
func ParseFile(path string) ([]Document, error) {
	if path == "-" {
		return Parse(os.Stdin, "-")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return Parse(file, path)
}

// Load reads every path given to -f and returns the documents they hold, in file order.
//
// A path may be a file, a directory, or "-" for stdin. A directory contributes its `*.yaml` and
// `*.yml` entries in sorted order and is **not** searched recursively: a manifest directory is a
// flat set of files an operator maintains, and recursing would sweep up whatever else lives
// under it.
//
// Duplicate objects across the whole set are an error. Two documents naming one request means the
// second silently overwrites the first, which is never what was meant — and for `delete` it means
// the file disagrees with itself about what there is to remove. Duplication is judged on the
// **kind and the qualified name**, so a request called `cam1` in two namespaces is two objects and
// a namespace called `cam1` beside a request called `cam1` is two objects.
func Load(paths []string) ([]Document, error) {
	if len(paths) == 0 {
		return nil, errors.New("no manifest given: pass -f <file>, -f <directory> or -f -")
	}

	var docs []Document
	for _, path := range paths {
		files, err := expand(path)
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			parsed, err := ParseFile(file)
			if err != nil {
				return nil, err
			}
			docs = append(docs, parsed...)
		}
	}

	seen := make(map[string]bool, len(docs))
	for _, doc := range docs {
		if seen[doc.key()] {
			return nil, fmt.Errorf("%s %q is named by more than one document", doc.Kind, doc.qualified())
		}
		seen[doc.key()] = true
	}
	return docs, nil
}

// expand turns one -f argument into the files it names.
func expand(path string) ([]string, error) {
	if path == "-" {
		return []string{"-"}, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".yaml", ".yml":
			files = append(files, filepath.Join(path, entry.Name()))
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s: no .yaml or .yml files", path)
	}
	slices.Sort(files)
	return files, nil
}

// ParseDomain splits a domain written as `<area>/<elements>` into its structured form (§10.6).
//
// **The only parser of a domain string in the tree.** The wire type, the server, the assignment
// and the agent's resolver all carry [api.Domain]; a `fast/studio-a/cam1` spelling exists in a
// manifest because it is what an operator wants to type, and it stops existing here.
//
// The first segment is the area and the rest are the elements. At least two segments, therefore:
// an area's own directory is not a domain (§10.6).
//
// Every rejection below is a shape that would otherwise have to be handled — or silently absorbed
// — by whatever split the string later. Refusing them at the one boundary is why nothing
// downstream needs a rule about separators at all.
func ParseDomain(domain string) (api.Domain, error) {
	switch {
	case domain == "":
		return api.Domain{}, errors.New("is required")
	case strings.HasPrefix(domain, api.DomainSeparator):
		// An absolute path is what a raw filesystem path looks like, and accepting one is the
		// thing this whole design exists to prevent (§7.2, §13).
		return api.Domain{}, errors.New("is absolute, expected <area>/<elements>")
	case strings.HasSuffix(domain, api.DomainSeparator):
		return api.Domain{}, errors.New("ends with a separator")
	case strings.Contains(domain, api.DomainSeparator+api.DomainSeparator):
		return api.Domain{}, errors.New("contains an empty element")
	}

	segments := strings.Split(domain, api.DomainSeparator)
	if len(segments) < 2 {
		return api.Domain{}, fmt.Errorf("names only the area %q; a domain is <area>/<elements>", domain)
	}

	parsed := api.Domain{Area: segments[0], Elements: segments[1:]}
	if err := parsed.Valid(); err != nil {
		return api.Domain{}, err
	}
	return parsed, nil
}
