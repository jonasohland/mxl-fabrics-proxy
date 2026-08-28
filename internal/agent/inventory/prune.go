package inventory

import (
	"log/slog"
	"path/filepath"

	"github.com/jonasohland/mxl-utils/pkg/mxl"
)

// prune is the [mxl.DomainReceiver] the discoverer reports into when this node has output roots,
// and it is what makes a search path above a root safe (§10.6).
//
// **A root is written, not read.** Nothing found by scanning inside one is reported, in either
// direction, so a directory under a root has exactly one name and exactly one owner: the
// reconciler, through [Inventory.Materialise] and [Inventory.Release], which call the receivers
// directly and bypass this. Discovery can neither create nor withdraw an output domain.
//
// Both methods are filtered, and the removal half is the one that is easy to leave out. The
// discoverer only reports directories that currently contain a flow, so an unfiltered
// [Inventory.RemoveDomain] would withdraw a materialised domain the moment its last flow was
// released — forgetting a domain a live session still targets, and, since the add side is pruned,
// never bringing it back.
//
// The add half is what keeps a materialised domain correctly named. [Inventory.AddDomain] returns
// early for a path it already knows, so whoever arrives first names the directory permanently:
// without this, a `<root>/cam1` left holding a flow by a SIGKILLed worker would be discovered at
// startup, named by its path, and keep that name through the materialisation that follows — and
// the server, which matches a session's destination by name, would leave the path in ESTABLISHING
// with nothing anywhere explaining why.
//
// The cost is stated rather than hidden: a domain some *other* actor creates inside a root is
// invisible as a source. That is the right meaning for a root, and it is consistent with §10.6
// already conceding that this project cannot distinguish a directory it created from one that was
// already there.
//
// This is not a new idea in the stack — mxl-utils' own discoverer excludes its `static` paths from
// `added`/`removed` for the same reason, so that a configured mapping's identity comes from
// configuration rather than from a scan.
type prune struct {
	// roots are the cleaned root paths. Fixed at construction, like everything else the resolver
	// depends on, so this needs no lock.
	roots []string

	// keep is [Inventory.byPath]: the configured input mappings, which must be reported even when
	// they sit inside a root. A root is allowed to be an ancestor of an input mapping, so this is
	// not a corner case but the layout that permission exists for.
	keep map[string]string

	recv []mxl.DomainReceiver
	log  *slog.Logger
}

// receivers returns what the discoverer reports into: the inventory and the watcher directly, or
// both behind a [prune] when this node has output roots.
//
// Unwrapped when there are no roots, so a source-only node runs the same code path it always did
// and the filter cannot be a thing that has to be reasoned about where it does nothing.
func (i *Inventory) receivers(watcher *mxl.Watcher) []mxl.DomainReceiver {
	recv := []mxl.DomainReceiver{i, watcher}
	if len(i.roots) == 0 {
		return recv
	}

	roots := make([]string, 0, len(i.roots))
	for _, root := range i.roots {
		roots = append(roots, root.Path)
	}
	return []mxl.DomainReceiver{&prune{roots: roots, keep: i.byPath, recv: recv, log: i.log}}
}

// logExclusions names every root a search path covers, at startup.
//
// Without it the failure mode is an operator pointing a search path at their MXL area and asking
// why one domain never appears. The rule is deliberate (see [prune]) but it is not guessable from
// the outside, so it is said once where it is cheap to say.
func (i *Inventory) logExclusions() {
	for _, dir := range i.search {
		var hidden []string
		for _, root := range i.roots {
			// Only one nesting is reachable: validateRoots refuses equality and refuses a root
			// above a search path, so a root a search path relates to at all is beneath it.
			if within(dir, root.Path) {
				hidden = append(hidden, root.Name)
			}
		}
		if len(hidden) > 0 {
			i.log.Info("search path excludes output roots; a root is written, not read",
				"search_path", dir, "roots", hidden)
		}
	}
}

// AddDomain implements [mxl.DomainReceiver].
func (p *prune) AddDomain(path string) {
	if p.hidden(path) {
		p.log.Debug("ignoring domain discovered inside an output root", "path", path)
		return
	}
	for _, recv := range p.recv {
		recv.AddDomain(path)
	}
}

// RemoveDomain implements [mxl.DomainReceiver].
func (p *prune) RemoveDomain(path string) {
	if p.hidden(path) {
		return
	}
	for _, recv := range p.recv {
		recv.RemoveDomain(path)
	}
}

// hidden reports whether a discovered path falls inside an output root.
//
// At-or-under, not merely under: flows directly in a root would make the root itself a domain, and
// the root is the write area whether or not something put a flow at its top level.
func (p *prune) hidden(path string) bool {
	path = filepath.Clean(path)
	if _, mapped := p.keep[path]; mapped {
		return false
	}
	for _, root := range p.roots {
		if within(root, path) {
			return true
		}
	}
	return false
}
