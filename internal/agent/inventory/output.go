package inventory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// Root is one operator-configured output root: a directory replication is permitted to create
// domains under (§10.6).
//
// A node with no roots configured is not a replication destination at all, which is the correct
// posture for the one piece of configuration that grants the control plane write authority over
// this host's filesystem. Roots are static: they are advertised at registration alongside fabric
// attachments, and for the same reason — they change when the host is built, not when a flow is
// routed (§10.2).
type Root struct {
	// Name is how a request names the root. It never becomes part of a path; it is what
	// [Inventory.Output] looks up, and what an operator reads in `GET /v1/nodes`.
	Name string

	// Path is the local directory. Advertised for diagnostics only — the server never sends a
	// path to an agent (§10.2).
	Path string
}

// Roots returns the configured output roots, ordered by name, as registration advertises them
// (§10.2).
func (i *Inventory) Roots() []Root {
	out := slices.Clone(i.roots)
	slices.SortFunc(out, func(a, b Root) int { return cmpString(a.Name, b.Name) })
	return out
}

// Output resolves an output domain — the directory a target worker creates its local flow in —
// from the root name and domain name an assignment carried (§10.6).
//
// This function is the whole of the authority the API has over this node's filesystem, and it is
// deliberately a **pure function of this agent's own configuration and the two names it is
// given**. It consults no observed state: not the inventory, not discovery, not what happens to
// be on disk. The destination path is therefore reproducible from one config file and one
// assignment, which is both easier to reason about and easier to test than resolving a
// destination by walking the observed-domain map — which is what this replaces.
//
// The server validates all of this first and is the authority. Checking it again here is the one
// place in the tree where duplication earns its keep (§13): it costs a map lookup and a string
// walk, and it is the difference between one buggy or compromised control plane and files written
// wherever a root can reach.
//
// Out of scope, and named so it is a decision rather than an oversight: a pre-existing symlink at
// `<root>/<name>` planted by something else on the host redirects the domain, and nothing here
// can see that. Whoever can plant it can already write into the root, which is where the flow was
// going anyway.
func (i *Inventory) Output(root string, elements []string) (string, error) {
	name := api.DomainPath(elements)

	path, ok := i.rootPaths[root]
	if !ok {
		if root == "" {
			return "", fmt.Errorf("no output root named, this node advertises %s", rootList(i.roots))
		}
		return "", fmt.Errorf("no output root %q on this node, it advertises %s", root, rootList(i.roots))
	}

	if err := api.ValidDomainElements(elements); err != nil {
		return "", fmt.Errorf("output domain %q: %w", name, err)
	}

	resolved, err := contain(path, elements)
	if err != nil {
		return "", fmt.Errorf("output domain %q under root %q: %w", name, root, err)
	}

	// **An output domain may not land on a mapped input domain's directory.** A root is allowed to
	// be an ancestor of an input mapping (see [validateRoots]), so this is the exact case that
	// permission leaves open, and it is refused here where the resolved path is known.
	//
	// It is not caught by the server's name check alone: `-m cams=/dev/shm/mxl/cameras` under root
	// `fast=/dev/shm/mxl` collides on the *path* while the names `cams` and `cameras` differ.
	// Without this, one directory would be an input domain under one name and an output domain
	// under another, and this project would be writing into a domain it advertises as read-only.
	//
	// Checked against the **configured** mappings only, which is what keeps this a pure function
	// of configuration (§10.6). Discovered domains are not consulted and cannot be missed:
	// [prune] hides every root from discovery, so nothing discovered is ever inside one. A search
	// path *may* now sit above a root, and that premise survives the change — it is held by the
	// pruning rather than by the overlap rule that used to forbid the layout.
	if mapped, taken := i.byPath[resolved]; taken {
		return "", fmt.Errorf("output domain %q under root %q resolves to %s, which this node maps as input domain %q",
			name, root, resolved, mapped)
	}
	return resolved, nil
}

// contain holds the containment half: the joined path must be exactly the root followed by the
// elements, in order.
//
// The character-set half is [api.ValidDomainElements], which lives in the wire package because the
// server rejects the same domains and the two must not be able to disagree about the rule.
//
// **Stated as an equality on the whole path**, which is what the element form makes possible and
// is stronger than either spelling a single string admits. A prefix test is where this check is
// usually got wrong — `strings.HasPrefix("/dev/shm/mxl-evil", "/dev/shm/mxl")` is true — and the
// direct-child test this replaced could not express a hierarchy at all. Comparing against the
// path the elements *should* have produced needs no boundary reasoning: if [filepath.Join] cleaned
// anything away, or an element smuggled in a separator or a `..`, the two strings differ.
//
// Deliberately separate from [api.ValidDomainElements], and it holds on its own: with the
// character-set check bypassed entirely, nothing an element can spell reaches outside the root,
// because anything that would have escaped changes the joined path and fails the equality. It
// promises containment and nothing else: an absolute-looking `/etc` as a sole element joins to
// `<root>/etc` and passes here, which is inside the root but is not the domain the request named.
// Refusing *that* is [api.ValidDomainElements]'s job, and it is why the two checks are both here
// rather than one of them.
func contain(root string, elements []string) (string, error) {
	joined := filepath.Join(append([]string{root}, elements...)...)

	want := root + string(filepath.Separator) + strings.Join(elements, string(filepath.Separator))
	if joined != want {
		return "", fmt.Errorf("resolves to %q, which is not %q inside %q", joined, api.DomainPath(elements), root)
	}
	return joined, nil
}

// CreateRoots pre-creates the configured output roots, alongside [Inventory.CreateDomains] and
// for the same reason (§6.1, §10.6).
//
// Only the leaf MkdirAll is then ever on the establishment path, which is where the 1–2 s target
// for re-establishing a session is won or lost. A root that cannot be created is reported here,
// at startup, rather than as a target worker dying at assignment time — where it would look like
// a fabric problem rather than a directory that does not exist and cannot be made.
func (i *Inventory) CreateRoots() error {
	var errs []error
	for _, root := range i.roots {
		if err := os.MkdirAll(root.Path, 0o755); err != nil {
			errs = append(errs, fmt.Errorf("create output root %q at %s: %w", root.Name, root.Path, err))
		}
	}
	return errors.Join(errs...)
}

// Materialise creates an output domain and starts observing it (§10.6).
//
// Three steps, of which the middle one is the one that gets left out: `MkdirAll`, add it to the
// **flow watch set**, and then the caller starts the worker. §11 derives ACTIVE from the
// destination flow's head index *as reported by this agent's own inventory* — there is
// deliberately no separate "the destination is receiving" signal — so a domain this project
// writes into but does not observe can never leave ESTABLISHING, and it fails in exactly the
// confusing way of a session that looks healthy at both ends and reports no progress.
//
// Neither of mxl-utils' mechanisms can deliver a freshly-materialised domain, which is why this
// is driven by hand: the discoverer fixes its `static` list at construction, and it only ever
// reports directories that already contain a flow. So the two receivers are called here, **in
// the same order the discoverer would have called them** — this inventory first, the watcher
// second — so that a flow never arrives for a domain that is not there yet.
//
// Driven by hand is also the *whole* of it: this and [Inventory.Release] bypass [prune], which
// hides every root from discovery, so a materialised domain's lifecycle is owned entirely by the
// reconciler. Nothing a scan sees can name one, and nothing a scan stops seeing can withdraw one.
//
// Idempotent, because the reconcile that calls it runs on every poll.
func (i *Inventory) Materialise(name, path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create output domain %q at %s: %w", name, path, err)
	}

	i.mu.Lock()
	if i.materialised[path] == name {
		i.mu.Unlock()
		return nil
	}
	i.materialised[path] = name
	watcher := i.watcher
	i.mu.Unlock()

	i.AddDomain(path)
	if watcher != nil {
		watcher.AddDomain(path)
	}
	return nil
}

// Release stops observing a materialised output domain, when the last session targeting it has
// gone (§10.6).
//
// It does **not** remove the directory. The MXL SDK removes a flow directory when its writer is
// released, so what is left behind is empty — and an empty directory is invisible to discovery,
// so it is not reported as a domain, cannot be selected as a source, and appears in no inventory.
// Re-materialising is an idempotent MkdirAll over it. That now holds even under a search path
// covering the root, and for a stronger reason: [prune] hides the directory whether it is empty
// or not, so a leaked directory left holding stale content is not discovered either.
//
// Removal order mirrors the add: this inventory forgets the domain first and the watcher's
// removals then land on nothing, which [Inventory.RemoveFlow] tolerates.
func (i *Inventory) Release(name, path string) {
	i.mu.Lock()
	if i.materialised[path] != name {
		i.mu.Unlock()
		return
	}
	delete(i.materialised, path)
	watcher := i.watcher
	i.mu.Unlock()

	i.RemoveDomain(path)
	if watcher != nil {
		watcher.RemoveDomain(path)
	}
}

// ValidateRoots checks a set of output roots against the input mappings and search paths they
// will live alongside, without building an [Inventory].
//
// It exists so the same rule can run at flag-parse time (§6.2): an operator who has overlapped a
// root with a search path should be told when the process starts, not when the first replication
// request is assigned. [New] applies it again — it has to, since it is what builds the lookup —
// and the two cannot disagree because there is only one of them.
func ValidateRoots(roots []Root, mappings []Domain, search []string) error {
	_, _, err := validateRoots(roots, mappings, search)
	return err
}

// validateRoots checks the operator's roots and builds the lookup [Inventory.Output] uses.
//
// The overlap rule is the configuration half of "names are flat per node" (§10.6): one directory
// must have exactly one name on this node. A root nested in another root, or in an input mapping,
// would give a materialised output domain a second identity, and the fleet-wide inventory would
// carry one flow at two addresses.
//
// Two containments are permitted, each with the collision it could produce refused somewhere more
// precise: a root above an input mapping, refused per request by [Inventory.Output], and a search
// path above a root, refused structurally by [prune]. Both are the same layout — one directory
// tree holding domains this node reads and domains it writes — and refusing either would refuse a
// legal arrangement to prevent a case that is already covered.
func validateRoots(roots []Root, mappings []Domain, search []string) ([]Root, map[string]string, error) {
	out := make([]Root, 0, len(roots))
	paths := map[string]string{}
	byPath := map[string]string{}

	for _, root := range roots {
		if err := api.ValidDomainName(root.Name); err != nil {
			// A root name is never joined into a path, so this is not load-bearing for traversal.
			// It is applied anyway so that a root reads the same in a config file, a request, an
			// error message and a metric label as the domains it holds.
			return nil, nil, fmt.Errorf("inventory: output root name %q: %w", root.Name, err)
		}
		if !filepath.IsAbs(root.Path) {
			return nil, nil, fmt.Errorf("inventory: output root %q: path %q is not absolute", root.Name, root.Path)
		}

		path := filepath.Clean(root.Path)
		if path == string(filepath.Separator) {
			// Every top-level directory on the host becomes a legal replication destination. Only
			// ever a mistake, and an expensive one to discover by having it work.
			return nil, nil, fmt.Errorf("inventory: output root %q: the filesystem root is not an output root", root.Name)
		}
		if existing, ok := paths[root.Name]; ok {
			return nil, nil, fmt.Errorf("inventory: output root %q is declared twice, at %q and %q", root.Name, existing, path)
		}
		if existing, ok := byPath[path]; ok {
			return nil, nil, fmt.Errorf("inventory: path %q is an output root twice, as %q and %q", path, existing, root.Name)
		}
		for other := range byPath {
			if overlaps(path, other) {
				return nil, nil, fmt.Errorf("inventory: output roots %q and %q overlap: %q and %q", root.Name, byPath[other], path, other)
			}
		}
		for _, mapping := range mappings {
			// **A root may be an ancestor of an input domain.** `-m cameras=/dev/shm/mxl/cameras`
			// alongside `--output-root fast=/dev/shm/mxl` is a reasonable thing to want: one
			// directory holding this node's domains, some read, some written.
			//
			// It is safe because the collision it could produce is a collision on one *exact*
			// path, and that is caught where it is precise — [Inventory.Output] refuses to
			// resolve an output domain onto a mapped input domain's directory. Refusing the whole
			// containment here would be refusing a legal layout to prevent one nameable case.
			//
			// Equality and the other direction stay refused: a root that *is* a domain directory,
			// or that sits inside one, would put output domains inside a domain, and no
			// per-request check can undo that.
			if strictlyContains(path, mapping.Path) {
				continue
			}
			if overlaps(path, mapping.Path) {
				return nil, nil, fmt.Errorf("inventory: output root %q overlaps domain %q: %q and %q", root.Name, mapping.Name, path, mapping.Path)
			}
		}
		for _, dir := range search {
			// **A search path may be an ancestor of a root**, which is the same layout the mapping
			// loop above permits, seen from the other side: `--search-path /dev/shm/mxl` alongside
			// `--output-root fast=/dev/shm/mxl/replicated` is one MXL area per host, part of it
			// discovered and part of it written.
			//
			// Note the nesting is the *opposite* way round from the mapping case — there the root is
			// the ancestor, here the search path is — so the two `strictlyContains` calls are not a
			// copy-paste of each other. What makes both safe is the same thing: the collision they
			// could produce is refused where it is precise. For a mapping that is
			// [Inventory.Output]; here it is [prune], which hides every root from discovery, so a
			// directory inside a root has exactly one name and one owner.
			if strictlyContains(dir, path) {
				continue
			}
			if filepath.Clean(dir) == path {
				// Provably a no-op rather than merely redundant: [prune] hides the whole root, so
				// this search path could never report anything. Refused because it reads as
				// meaningful configuration.
				return nil, nil, fmt.Errorf("inventory: output root %q is also search path %q", root.Name, dir)
			}
			if overlaps(path, dir) {
				// The other direction stays refused, for the reason equality is: a search path
				// inside a root asks discovery to read the directory replication writes into, which
				// is the contradiction pruning exists to resolve.
				return nil, nil, fmt.Errorf("inventory: output root %q contains search path %q: %q and %q", root.Name, dir, path, dir)
			}
		}

		paths[root.Name] = path
		byPath[path] = root.Name
		out = append(out, Root{Name: root.Name, Path: path})
	}

	return out, paths, nil
}

// overlaps reports whether two absolute paths are the same directory or one contains the other.
// Boundary-aware, for the reason given on [contain].
//
// It cleans both sides rather than requiring cleaned input: [New] has already done so, but
// [ValidateRoots] is called from flag parsing with whatever the operator typed, and a trailing
// slash is not a reason to miss an overlap.
func overlaps(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+string(filepath.Separator)) ||
		strings.HasPrefix(b, a+string(filepath.Separator))
}

// within reports whether child is parent or sits beneath it. Boundary-aware, for the reason given
// on [contain].
func within(parent, child string) bool {
	parent, child = filepath.Clean(parent), filepath.Clean(child)
	return parent == child || strings.HasPrefix(child, parent+string(filepath.Separator))
}

// strictlyContains reports whether parent is a proper ancestor of child. Equality is *not*
// containment here: the cases this permits are a root above a domain and a search path above a
// root, and in both of those the equal case is a different thing entirely.
func strictlyContains(parent, child string) bool {
	return filepath.Clean(parent) != filepath.Clean(child) && within(parent, child)
}

func rootList(roots []Root) string {
	if len(roots) == 0 {
		return "none"
	}
	names := make([]string, 0, len(roots))
	for _, root := range roots {
		names = append(names, fmt.Sprintf("%q", root.Name))
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}
