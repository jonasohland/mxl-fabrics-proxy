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

// Areas returns this node's areas, ordered by name, as registration advertises them (§10.2).
func (i *Inventory) Areas() []api.Area {
	out := slices.Clone(i.areas)
	slices.SortFunc(out, func(a, b api.Area) int { return cmpString(a.Name, b.Name) })
	return out
}

// nameFor is the naming rule, and everything about a domain's identity routes through it: a
// directory's fleet-wide name is its **innermost containing area's** name followed by its path
// elements relative to that area (§10.6).
//
// Longest prefix wins, which is the ordinary rule in the vocabulary this borrowed from. `media`
// being an ancestor of `fast` therefore produces nothing to disambiguate:
// `/dev/shm/mxl/replicated/ingest` is `fast/ingest` and never `media/replicated/ingest`, because
// `fast` contains it more tightly. Equal paths are refused at startup (see [validateAreas]), since
// that is the one arrangement this rule cannot decide.
//
// **This is what makes one identity grammar possible, and everything else depends on it.** A
// directory has exactly one name whether discovery found it or the reconciler created it, so the
// two namers cannot disagree — which is precisely the disagreement pruning used to exist to
// prevent (§10.6).
//
// An area's own directory is not a domain — a domain has at least one element — so
// `/dev/shm/mxl/replicated` is the area `fast` and not the domain `media/replicated`.
//
// Grants are deliberately not consulted. Naming is not authorisation: a domain in a write-only
// area still has a name, which is what lets a materialised one be reported at all (§10.7).
func (i *Inventory) nameFor(path string) (api.Domain, bool) {
	path = filepath.Clean(path)

	var best api.Area
	var rel string
	for _, area := range i.areas {
		if !within(area.Path, path) || path == area.Path {
			continue
		}
		if best.Name != "" && len(area.Path) <= len(best.Path) {
			continue
		}
		best, rel = area, strings.TrimPrefix(path, area.Path+string(filepath.Separator))
	}
	if best.Name == "" || rel == "" {
		return api.Domain{}, false
	}
	return api.Domain{Area: best.Name, Elements: strings.Split(rel, string(filepath.Separator))}, true
}

// Resolve turns a domain into the local directory a target worker writes into, from the area name
// and elements an assignment carried (§10.6).
//
// This function is the whole of the authority the API has over this node's filesystem, and it is
// deliberately a **pure function of this agent's own configuration and the domain it is given**.
// It consults no observed state: not the inventory, not discovery, not what happens to be on disk.
// The destination path is therefore reproducible from one config file and one assignment.
//
// **The `write` grant is checked here**, and it is the one line the unification adds: under the
// old split, "this is an output root" carried the grant by construction, and now that a name
// resolves against one table the grant is a field on the entry and has to be read (§10.6).
//
// The server validates all of this first and is the authority. Checking it again here is the one
// place in the tree where duplication earns its keep (§13): it costs a map lookup and a string
// walk, and it is the difference between one buggy or compromised control plane and files written
// wherever an area can reach.
//
// Out of scope, and named so it is a decision rather than an oversight: a pre-existing symlink at
// `<area>/<elements>` planted by something else on the host redirects the domain, and nothing here
// can see that. Whoever can plant it can already write into the area, which is where the flow was
// going anyway.
func (i *Inventory) Resolve(domain api.Domain) (string, error) {
	area, ok := i.byArea[domain.Area]
	if !ok {
		if domain.Area == "" {
			return "", fmt.Errorf("no area named, this node advertises %s", areaList(i.areas))
		}
		return "", fmt.Errorf("no area %q on this node, it advertises %s", domain.Area, areaList(i.areas))
	}
	if !area.Write {
		return "", fmt.Errorf("area %q on this node does not grant writing", domain.Area)
	}

	if err := api.ValidDomainElements(domain.Elements); err != nil {
		return "", fmt.Errorf("domain %q: %w", domain, err)
	}

	resolved, err := contain(area.Path, domain.Elements)
	if err != nil {
		return "", fmt.Errorf("domain %q: %w", domain, err)
	}
	return resolved, nil
}

// contain holds the containment half: the joined path must be exactly the area's path followed by
// the elements, in order.
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
// character-set check bypassed entirely, nothing an element can spell reaches outside the area,
// because anything that would have escaped changes the joined path and fails the equality. It
// promises containment and nothing else: an absolute-looking `/etc` as a sole element joins to
// `<area>/etc` and passes here, which is inside the area but is not the domain the request named.
// Refusing *that* is [api.ValidDomainElements]'s job, and it is why the two checks are both here
// rather than one of them.
func contain(area string, elements []string) (string, error) {
	joined := filepath.Join(append([]string{area}, elements...)...)

	want := area + string(filepath.Separator) + strings.Join(elements, string(filepath.Separator))
	if joined != want {
		return "", fmt.Errorf("resolves to %q, which is not %q inside %q", joined, strings.Join(elements, api.DomainSeparator), area)
	}
	return joined, nil
}

// CreateAreas pre-creates the writable areas at startup (§6.1, §10.6).
//
// Only the leaf MkdirAll for a domain is then ever on the establishment path, which is where the
// 1–2 s target for re-establishing a session is won or lost. An area that cannot be created is
// reported here, at startup, rather than as a target worker dying at assignment time — where it
// would look like a fabric problem rather than a directory that does not exist and cannot be made.
//
// **Writable areas only.** A read-only area is somewhere this project looks, not somewhere it may
// create anything, and creating one would be doing on startup exactly what the missing grant
// refuses at assignment time.
func (i *Inventory) CreateAreas() error {
	var errs []error
	for _, area := range i.areas {
		if !area.Write {
			continue
		}
		if err := os.MkdirAll(area.Path, 0o755); err != nil {
			errs = append(errs, fmt.Errorf("create area %q at %s: %w", area.Name, area.Path, err))
		}
	}
	return errors.Join(errs...)
}

// Materialise creates a domain's directory and starts observing it (§10.6).
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
// *Driving them by hand used to be the whole of it*, because pruning meant nothing a scan saw
// could name a materialised domain and nothing a scan stopped seeing could withdraw one. It is now
// the half of it that covers the **empty** case: a scan and the reconciler both report the same
// domain under the same name — [nameFor] guarantees it — and inventory holds the union of the two
// (see [Inventory.RemoveDomain]).
//
// **It takes a path and no name**, which is the shape the naming rule makes possible: a directory
// has exactly one name and [Inventory.nameFor] computes it. `materialised` stops being "how a
// domain gets its short name" and becomes purely membership this agent holds independently of
// scanning (§10.6).
//
// Idempotent, because the reconcile that calls it runs on every poll.
func (i *Inventory) Materialise(path string) error {
	path = filepath.Clean(path)

	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create domain at %s: %w", path, err)
	}

	i.mu.Lock()
	_, held := i.materialised[path]
	i.materialised[path] = struct{}{}
	watcher := i.watcher
	i.mu.Unlock()

	if held {
		return nil
	}

	i.add(path)
	if watcher != nil {
		watcher.AddDomain(path)
	}
	return nil
}

// Release stops holding a materialised domain, when the last session targeting it has gone
// (§10.6).
//
// It does **not** remove the directory, and it does not necessarily stop observing one either: the
// domain leaves inventory only when discovery has also let go of it, which is the union
// [Inventory.RemoveDomain] describes. A directory the MXL SDK has emptied is invisible to
// discovery, so in the ordinary case it does leave; one still holding a flow some other actor
// wrote stays, reported like any other domain (§10.6's leaked-directory case).
func (i *Inventory) Release(path string) {
	path = filepath.Clean(path)

	i.mu.Lock()
	if _, held := i.materialised[path]; !held {
		i.mu.Unlock()
		return
	}
	delete(i.materialised, path)
	watcher := i.watcher
	i.mu.Unlock()

	// remove is a no-op while discovery still holds it: a directory under a readable area that
	// still contains a flow is an ordinary domain this project happens to have created, and it
	// stays visible like any other (§10.6).
	if i.remove(path) && watcher != nil {
		watcher.RemoveDomain(path)
	}
}

// ValidateAreas checks a set of areas without building an [Inventory].
//
// It exists so the same rule can run at flag-parse time (§6.2): an operator who declared two areas
// on one directory should be told when the process starts, not when the first replication request
// is assigned. [New] applies it again — it has to, since it is what builds the lookup — and the
// two cannot disagree because there is only one of them.
func ValidateAreas(areas []api.Area) error {
	_, _, err := validateAreas(areas)
	return err
}

// validateAreas checks the operator's areas and builds the lookup [Inventory.Resolve] uses.
//
// **The one merged rule left is that no two areas share a path.** *This supersedes a table of
// overlap rules — a search path inside a root, a search path equal to a root, a root that is an
// ancestor of an input mapping.* All of it was arithmetic on a distinction that no longer exists:
// there is one kind of area now, **nesting is legal**, and the innermost containing one names a
// directory (§10.6). Equal paths are the one arrangement that rule cannot decide, so they are
// refused, naming both areas.
func validateAreas(areas []api.Area) ([]api.Area, map[string]api.Area, error) {
	out := make([]api.Area, 0, len(areas))
	byName := make(map[string]api.Area, len(areas))
	byPath := map[string]string{}

	for _, area := range areas {
		if err := api.ValidDomainName(area.Name); err != nil {
			// An area name is never joined into a path, so this is not load-bearing for traversal.
			// It is applied because the name *is* the first segment of every domain in it (§10.6),
			// so it has to read the same in a config file, a request, an error message and a metric
			// label as the elements beside it.
			return nil, nil, fmt.Errorf("inventory: area name %q: %w", area.Name, err)
		}
		if !area.Read && !area.Write {
			// A line that does nothing. Refused rather than ignored, because an operator who wrote
			// it believes the node has an area there.
			return nil, nil, fmt.Errorf("inventory: area %q grants neither read nor write", area.Name)
		}
		if !filepath.IsAbs(area.Path) {
			return nil, nil, fmt.Errorf("inventory: area %q: path %q is not absolute", area.Name, area.Path)
		}

		path := filepath.Clean(area.Path)
		if path == string(filepath.Separator) {
			// Every top-level directory on the host becomes a legal replication destination. Only
			// ever a mistake, and an expensive one to discover by having it work.
			return nil, nil, fmt.Errorf("inventory: area %q: the filesystem root is not an area", area.Name)
		}
		if existing, taken := byName[area.Name]; taken {
			return nil, nil, fmt.Errorf("inventory: area %q is declared twice, at %q and %q", area.Name, existing.Path, path)
		}
		if existing, taken := byPath[path]; taken {
			return nil, nil, fmt.Errorf("inventory: path %q is an area twice, as %q and %q", path, existing, area.Name)
		}

		area.Path = path
		byName[area.Name] = area
		byPath[path] = area.Name
		out = append(out, area)
	}

	return out, byName, nil
}

// within reports whether child is parent or sits beneath it. Boundary-aware, for the reason given
// on [contain]: `/dev/shm/mxl-evil` is not inside `/dev/shm/mxl`.
func within(parent, child string) bool {
	parent, child = filepath.Clean(parent), filepath.Clean(child)
	return parent == child || strings.HasPrefix(child, parent+string(filepath.Separator))
}

func areaList(areas []api.Area) string {
	if len(areas) == 0 {
		return "none"
	}
	names := make([]string, 0, len(areas))
	for _, area := range areas {
		names = append(names, fmt.Sprintf("%q", area.Name))
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}
