// Package manifest reads the declarative file `mxl-replicator apply` takes (§9.1, plan M8b).
//
// A manifest is multi-document YAML — one replication request per document, `---` separated —
// that decodes to a set of [api.RequestSpec]. It is the interface an operator actually uses; the
// HTTP API underneath is the contract.
//
// # Why this needs no machinery
//
// `POST /v1/requests` is create-or-update keyed on the client-supplied name, and that name *is*
// the request's ID (§9.1). So a file naming a set of requests is already an apply: each document
// is one POST, and `DELETE /v1/requests/{name}` removes what a document named. The idempotency
// key that §9.1 asked for on the Kubernetes adapter's behalf turns out to be the whole mechanism.
//
// # Why it is not a Kubernetes manifest
//
// No `apiVersion`, no `kind`. The shape is close enough that the roadmapped adapter is a
// mechanical conversion, but this project is not Kubernetes and pretending otherwise costs every
// operator two lines of ceremony per document to express nothing. If a second object type ever
// appears, an optional `kind:` defaulting to the request is additive and costs nothing then.
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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// Document is one request as a manifest spells it.
//
// It is a separate type from [api.RequestSpec] rather than YAML tags on it, for two reasons. The
// wire type keeps its selector as a tagged union (§9.1) while the file flattens it onto the
// source, which is friendlier to write and no less strict — the union rule becomes validation
// rather than syntax. And the file is a user interface with its own compatibility story, which
// should be free to diverge from the wire contract rather than pinning it.
type Document struct {
	// Name is the request's identity: its ID, its idempotency key, and what `delete` takes.
	Name string `yaml:"name"`

	Source       SourceDoc        `yaml:"source"`
	Destinations []DestinationDoc `yaml:"destinations"`

	// Provider is the default pin for every destination (§10.4). A bare string pins; a list
	// expresses "prefer this, that is acceptable"; omitted, the server negotiates in its
	// configured order. Never silently substituted.
	Provider ProviderDoc `yaml:"provider"`

	IdleTeardownMS *int64            `yaml:"idle_teardown_ms"`
	SchedPrio      *int              `yaml:"sched_prio"`
	Labels         map[string]string `yaml:"labels"`

	// where this document came from — a file path and, past the first, its index. Carried on the
	// document so that an error raised *after* parsing can still say which one. Unexported, so
	// the strict decoder neither sees it nor can be fed it.
	where string
}

// SourceDoc is where to replicate from. Exactly one of Flow and GroupHint must be set.
//
// The selector is flattened onto the source here where the wire type nests it under `select`.
// §9.1's tagged-union discipline is about the wire type and survives intact: this decodes into
// [api.Selector] and the exactly-one rule is enforced below.
type SourceDoc struct {
	Node   string `yaml:"node"`
	Domain string `yaml:"domain"`

	// Flow pins one flow ID.
	Flow string `yaml:"flow"`

	// GroupHint selects every flow carrying a matching NMOS group hint. Omitting `type` selects
	// every flow sharing the name, which is how a camera's video and audio are replicated
	// together.
	GroupHint *GroupHintDoc `yaml:"group_hint"`
}

// GroupHintDoc is the `urn:x-nmos:tag:grouphint/v1.0` selector.
type GroupHintDoc struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
}

// DestinationDoc is one place the source goes.
type DestinationDoc struct {
	Node string `yaml:"node"`

	// Domain is the output domain, written as a path: `ingest`, or `studio-a/cam1` to nest it
	// under the root. **This is the only place in the system where a domain is a string.**
	//
	// [ParseDomain] splits it here, at the file boundary, and everything downstream — the wire
	// type, the server, the assignment, the agent's resolver — carries the elements. That is what
	// keeps the containment invariant structural: the resolver takes a validated list and can
	// check its work as an equality on the whole path, with no separator semantics to get wrong
	// and no second parser to disagree with this one (§10.6).
	Domain string `yaml:"domain"`

	// Root names which of the node's output roots the domain is created under. Optional when the
	// node advertises exactly one, which is the common case (§10.6).
	Root string `yaml:"root"`

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

// Spec converts a document to the wire type and validates it.
//
// Validation is [api.RequestSpec.Validate] plus the one rule that only exists in the file: the
// selector is flattened here, so "exactly one kind" has to be checked rather than being
// guaranteed by the shape.
func (d Document) Spec() (api.RequestSpec, error) {
	var spec api.RequestSpec

	selector, err := d.Source.selector()
	if err != nil {
		return spec, err
	}

	spec = api.RequestSpec{
		Name:      d.Name,
		Source:    api.Source{Node: d.Source.Node, Domain: d.Source.Domain, Select: selector},
		Provider:  d.Provider.pin(),
		SchedPrio: d.SchedPrio,
		Labels:    d.Labels,
	}
	for i, dst := range d.Destinations {
		elements, err := ParseDomain(dst.Domain)
		if err != nil {
			return api.RequestSpec{}, fmt.Errorf("destinations[%d].domain: %w", i, err)
		}
		spec.Destinations = append(spec.Destinations, api.Destination{
			Node:     dst.Node,
			Domain:   elements,
			Root:     dst.Root,
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
		return api.Selector{}, errors.New("source names both flow and group_hint; a selector is exactly one kind")
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

// Parse decodes a multi-document manifest into its documents, **without** converting them to
// request specs.
//
// The split is what lets one file serve two verbs with different needs. `apply` needs every
// document to be a complete, valid request; `delete` needs only the names, because it removes what
// the file *named* and has no use for where any of it was going. A manifest that has drifted from
// what is deployed — or that was only ever written to record the names — must still be able to
// delete, or "delete what this file created" stops working exactly when it is most wanted.
//
// What Parse does check on every document, whichever verb asked for it:
//
//   - **unknown keys**, because a typo is a typo in a file you are deleting from too, and it may
//     mean the name you meant is somewhere this did not look;
//   - **that there is a name**, because a document without one identifies nothing.
//
// origin names the source for error messages — a file path, or "-" for stdin. Errors carry it
// along with the document's index, because a file with eight documents in it needs to say which
// one is wrong.
func Parse(r io.Reader, origin string) ([]Document, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	var docs []Document
	for index := 0; ; index++ {
		var doc Document
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where(origin, index), err)
		}

		// An empty document — a stray `---`, or a file ending in one — is skipped rather than
		// being reported as a request with no name. Writing a trailing separator is a habit, not
		// a mistake.
		if isEmpty(doc) {
			continue
		}
		if doc.Name == "" {
			return nil, fmt.Errorf("%s: name is required: it is the request's identity", where(origin, index))
		}

		doc.where = where(origin, index)
		docs = append(docs, doc)
	}
	return docs, nil
}

// Specs converts documents to request specs, validating each in full. This is what `apply` needs
// and `delete` does not.
func Specs(docs []Document) ([]api.RequestSpec, error) {
	specs := make([]api.RequestSpec, 0, len(docs))
	for _, doc := range docs {
		spec, err := doc.Spec()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", doc.where, err)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// Names returns what the documents identify, which is all `delete` needs.
func Names(docs []Document) []string {
	names := make([]string, 0, len(docs))
	for _, doc := range docs {
		names = append(names, doc.Name)
	}
	return names
}

func isEmpty(doc Document) bool {
	return doc.Name == "" && doc.Source == (SourceDoc{}) && len(doc.Destinations) == 0 &&
		len(doc.Provider) == 0 && doc.IdleTeardownMS == nil && doc.SchedPrio == nil && len(doc.Labels) == 0
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
// Duplicate names across the whole set are an error. Two documents naming one request means the
// second silently overwrites the first, which is never what was meant — and for `delete` it means
// the file disagrees with itself about what there is to remove.
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
		if seen[doc.Name] {
			return nil, fmt.Errorf("request %q is named by more than one document; the second would silently replace the first", doc.Name)
		}
		seen[doc.Name] = true
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

// ParseDomain splits an output domain written as a path into its elements (§10.6).
//
// **The only parser of a domain string in the tree.** The wire type, the server, the assignment
// and the agent's resolver all carry `[]string`; a `studio-a/cam1` spelling exists in a manifest
// because it is what an operator wants to type, and it stops existing here.
//
// Every rejection below is a shape that would otherwise have to be handled — or silently absorbed
// — by whatever split the string later. Refusing them at the one boundary is why nothing
// downstream needs a rule about separators at all.
func ParseDomain(domain string) ([]string, error) {
	switch {
	case domain == "":
		return nil, errors.New("is required")
	case strings.HasPrefix(domain, api.DomainSeparator):
		// An absolute path is what a raw filesystem path looks like, and accepting one is the
		// thing this whole design exists to prevent (§7.2, §13). It is also what a *discovered*
		// domain is named by, so refusing it here is what keeps a requested domain and a
		// discovered one from ever colliding.
		return nil, errors.New("is absolute; an output domain is named relative to its root, never as a path")
	case strings.HasSuffix(domain, api.DomainSeparator):
		return nil, errors.New("ends with a separator")
	case strings.Contains(domain, api.DomainSeparator+api.DomainSeparator):
		return nil, errors.New("contains an empty element")
	}

	elements := strings.Split(domain, api.DomainSeparator)
	if err := api.ValidDomainElements(elements); err != nil {
		return nil, err
	}
	return elements, nil
}
