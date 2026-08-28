// Package inventory observes the flows in this node's MXL domains (§6, §11.1).
//
// It is the piece that replaces the legacy proxy's peer-to-peer flow fetch: rather than the
// receiving proxy asking the sending proxy for a flow definition over HTTP, every agent reports
// what it sees and the server aggregates a fleet-wide inventory. There is no agent-to-agent
// control-plane traffic left at all.
//
// # What it reports, and the three ways to get it wrong
//
// Per flow: the ID, the **verbatim** bytes of flow_def.json, the parsed NMOS group hint, and a
// coarse `producing` boolean. Each of the last three has a specific failure the plan calls out:
//
//   - `producing` comes from **head-index deltas across samples**, never from LastWriteTime.
//     The timestamp looks more convenient — one sample, no state — but it is TAI nanoseconds and
//     means nothing unless the host's TAI offset is configured. A delta needs no clock at all, so
//     take the version that cannot be wrong. LastWriteTime stays useful for diagnostics.
//   - [mxl.Flow.IsValid] is checked on **every** sample. A flow deleted and recreated under the
//     same ID is a different data file; the old mapping keeps working and keeps returning stale
//     values forever, so without the check a republished flow reports producing=false permanently
//     and is never replicated again.
//   - `producing` is **hysteretic and coarse**, and a raw head index never appears in a snapshot.
//     Inventory is a full snapshot written to the store, so a field that changed every frame
//     would make every snapshot differ and turn inventory into a per-heartbeat write stream —
//     trading the churn §11.1 exists to eliminate for a slower version of the same thing. Rate
//     and head index belong in metrics (§12).
//
// Every flow in every mapped domain is observed, not only flows with sessions: admission (§11.1)
// needs the liveness of flows nothing is replicating yet, and the destination flow's liveness is
// what ACTIVE is derived from.
package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/jonasohland/mxl-utils/pkg/mxl"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

const (
	// DefaultInterval is how often every flow is sampled. Fine enough that a producing flow is
	// noticed within a frame or two of starting, coarse enough that the cost is nothing:
	// GetInfo decodes from a live mapping and is documented as cheap enough to call on every
	// scrape (§11.1).
	DefaultInterval = 500 * time.Millisecond

	// DefaultIdleAfter is how long a head index must stand still before a flow is reported as no
	// longer producing.
	//
	// This is the hysteresis, and it is agent-local — a different knob from the server's
	// two-tier idle policy (§11.1), which decides what to *do* about an idle source. Generous
	// against a slow frame rate and against a sample landing either side of one: a 25 fps flow
	// moves every 40 ms, so several seconds of stillness is a producer that stopped rather than
	// scheduling noise.
	DefaultIdleAfter = 3 * time.Second
)

// Domain is one configured name→path mapping (§6.2).
type Domain struct {
	// Name is how the domain is addressed fleet-wide. Paths are agent-local and are never
	// accepted from the API (§7.2, §13).
	Name string

	// Path is the local filesystem path.
	Path string
}

// Options configures an [Inventory].
type Options struct {
	// Domains are the operator's explicit mappings. They go in as the discoverer's *static*
	// domains, which is exactly what that parameter is for: reported once at construction and
	// never retracted by scanning, so a configured domain stays visible while it is temporarily
	// empty.
	Domains []Domain

	// SearchPaths are recursively scanned for unconfigured domains. A discovered domain can be a
	// replication *source* but never a destination — that is the invariant that stops the API
	// being a remote arbitrary-filesystem-write primitive (§7.2, §13), and it is enforced by
	// [api.DomainInventory.Configured] travelling with every domain.
	//
	// A search path may sit above an output root; what it finds inside one is pruned. See [prune]
	// for why, and for the one thing that costs.
	SearchPaths []string

	// OutputRoots are the directories replication may create domains under (§10.6). They are the
	// *write* side, resolved by [Inventory.Output] from an assignment's root and domain names
	// alone, and **a root is written, not read**: [prune] hides every root from discovery, so a
	// search path may sit above one and nothing inside it is ever reported by a scan.
	//
	// No default. A node with none configured accepts no replication destinations, which is the
	// right posture for an opt-in that grants filesystem write authority.
	OutputRoots []Root

	// Interval is the sampling period. Defaults to [DefaultInterval].
	Interval time.Duration

	// IdleAfter is the hysteresis threshold. Defaults to [DefaultIdleAfter].
	IdleAfter time.Duration

	// OnChange is called, without holding any lock, whenever something happened that may have
	// changed the snapshot. It must not block; the caller coalesces.
	OnChange func()

	Logger *slog.Logger

	// Now is the clock, injectable for tests. Only ever used for head-index staleness, never for
	// anything that leaves this process.
	Now func() time.Time
}

// Inventory watches this node's domains and the flows in them.
//
// Safe for concurrent use: mxl-utils' discoverer and watcher call in from their own goroutines
// while the sample loop runs and the agent reads snapshots.
type Inventory struct {
	interval  time.Duration
	idleAfter time.Duration
	onChange  func()
	log       *slog.Logger
	now       func() time.Time

	// static and byPath are fixed at construction: the configured mappings, and the reverse
	// lookup a discovered domain never enters. byName is what resolves an assignment's domain
	// *name* to a path, and it is a strict map lookup by design — an agent that fell back to
	// treating an unmapped name as a path would hand the API the filesystem.
	static []Domain
	byName map[string]string
	byPath map[string]string
	search []string

	// roots and rootPaths are the output side, and are fixed at construction for the same reason
	// the input maps are: [Inventory.Output] is a pure function of configuration, and a resolver
	// that could be widened at runtime would not be one (§10.6).
	roots     []Root
	rootPaths map[string]string

	mu      sync.Mutex
	domains map[string]*domainState // keyed by path

	// materialised is the output domains this node is currently observing because a session
	// targets them, path → name. Unlike the maps above it changes at runtime, since an output
	// domain lives exactly as long as a path targets it (§10.6). It is what gives a materialised
	// domain its short name in [Inventory.AddDomain] — without it a domain this project created
	// would be reported under its path, like a discovered one.
	materialised map[string]string

	// watcher is set once [Run] starts and read by [Inventory.Materialise] from the reconcile
	// goroutine, so it is guarded like everything else here.
	watcher *mxl.Watcher
}

type domainState struct {
	name       string
	configured bool
	flows      map[string]*flowState
}

// flowState is one observed flow and everything needed to keep observing it.
type flowState struct {
	id   string
	dir  string
	path string // domain path

	// def is the verbatim flow_def.json. Verbatim is load-bearing: the destination worker
	// reproduces the source definition exactly, including NMOS fields nothing in this tree
	// models, and the session identity hashes these bytes (§5.4, §2a). Decoding and re-encoding
	// would drop the first and could move the second.
	def  json.RawMessage
	hint *api.GroupHint

	flow *mxl.Flow

	// written tracks head-index movement — the producer is producing, which is what the server
	// admits on (§11.1). read tracks last-read-time movement, which is agent-local and feeds
	// `mxl_reader_active` only (§12).
	//
	// Both are *deltas* of a counter, never a comparison against the wall clock, and that is what
	// keeps the TAI epoch out of this: `LastReadTime` is read as a number that changes when a
	// reader reads, not as a timestamp that means anything on its own.
	written liveness
	read    liveness

	// complained suppresses a per-sample log line for a flow that is present but unreadable —
	// a producer part-way through creating it, most often.
	complained bool
}

// New validates the configuration and builds an inventory. It observes nothing until [Run].
func New(opts Options) (*Inventory, error) {
	inv := &Inventory{
		interval:     opts.Interval,
		idleAfter:    opts.IdleAfter,
		onChange:     opts.OnChange,
		log:          opts.Logger,
		now:          opts.Now,
		byName:       map[string]string{},
		byPath:       map[string]string{},
		domains:      map[string]*domainState{},
		materialised: map[string]string{},
	}
	if inv.interval <= 0 {
		inv.interval = DefaultInterval
	}
	if inv.idleAfter <= 0 {
		inv.idleAfter = DefaultIdleAfter
	}
	if inv.log == nil {
		inv.log = slog.Default()
	}
	if inv.now == nil {
		inv.now = time.Now
	}
	if inv.onChange == nil {
		inv.onChange = func() {}
	}

	for _, domain := range opts.Domains {
		// **The same name rule as an output domain's elements**, and it has to be: names are flat
		// per node, so an input mapping and a rendered output domain live in one namespace
		// (§10.6). An input name containing a separator could equal a hierarchical output
		// domain's rendered form — `-m a/b=...` against `domain: a/b` — and the server's
		// collision check compares those two as strings.
		//
		// A tightening on §16's promise that `-m name=/path` carries over byte-compatible: the
		// *syntax* does, and a name legacy would have accepted but this refuses is now a startup
		// error rather than a name that works until the day something collides with it.
		if err := api.ValidDomainName(domain.Name); err != nil {
			return nil, fmt.Errorf("inventory: domain name %q (path %q): %w", domain.Name, domain.Path, err)
		}
		if !filepath.IsAbs(domain.Path) {
			return nil, fmt.Errorf("inventory: domain %q: path %q is not absolute", domain.Name, domain.Path)
		}

		path := filepath.Clean(domain.Path)
		if existing, ok := inv.byName[domain.Name]; ok {
			return nil, fmt.Errorf("inventory: domain %q is mapped twice, to %q and %q", domain.Name, existing, path)
		}
		if existing, ok := inv.byPath[path]; ok {
			// Two names for one directory would make the reverse lookup ambiguous, and would
			// let one flow appear twice in the fleet-wide inventory under two addresses.
			return nil, fmt.Errorf("inventory: path %q is mapped twice, as %q and %q", path, existing, domain.Name)
		}

		inv.byName[domain.Name] = path
		inv.byPath[path] = domain.Name
		inv.static = append(inv.static, Domain{Name: domain.Name, Path: path})
	}

	for _, path := range opts.SearchPaths {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("inventory: search path %q is not absolute", path)
		}
		inv.search = append(inv.search, filepath.Clean(path))
	}

	// Last, because the overlap rule is checked against the input mappings and search paths above
	// and needs them cleaned first.
	roots, rootPaths, err := validateRoots(opts.OutputRoots, inv.static, inv.search)
	if err != nil {
		return nil, err
	}
	inv.roots, inv.rootPaths = roots, rootPaths

	return inv, nil
}

// Mappings returns the configured domains as they are advertised at registration (§10.2).
//
// **Configured mappings only.** Discovered domains are deliberately absent: registration is
// durable desired state and changes only when the operator changes the node's configuration,
// whereas discovery comes and goes with whatever a producer happens to have created. A
// discovered domain reaches the server through the *inventory* snapshot instead, with
// Configured false — which is where high-churn observations belong (§4), and which is all the
// server needs, since a discovered domain is only ever a source.
func (i *Inventory) Mappings() []api.DomainMapping {
	out := make([]api.DomainMapping, 0, len(i.static))
	for _, domain := range i.static {
		out = append(out, api.DomainMapping{Name: domain.Name, Path: domain.Path, Configured: true})
	}
	slices.SortFunc(out, func(a, b api.DomainMapping) int { return cmpString(a.Name, b.Name) })
	return out
}

// Input resolves an *input* domain name to a local path: somewhere this node reads from, and an
// initiator's source (§10.6).
//
// It is a strict map lookup over the domains this agent knows — configured mappings, plus domains
// found under a search path — and it never interprets its argument as a path, which is what stops
// an assignment from naming an arbitrary directory on this host (§7.2, §13).
//
// The destination side is [Inventory.Output], and the asymmetry is the point: a source is by
// definition something this agent *observes*, so it is resolved through what has been seen; a
// destination is something a request *asks for*, so it is resolved from configuration alone.
func (i *Inventory) Input(name string) (string, bool) {
	if path, ok := i.byName[name]; ok {
		return path, true
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	for path, domain := range i.domains {
		if domain.name == name {
			return path, true
		}
	}
	return "", false
}

// Configured reports whether a domain name is an explicitly mapped one, and therefore usable as
// a replication destination (§7.2).
//
// The server validates this too, and is the authority. Checking it again here is deliberate
// duplication: it is the single most important invariant in the design, it costs one map lookup,
// and an agent that trusted the server on it would be one compromised or buggy control plane
// away from writing into any directory a search path can reach.
//
// **Superseded by [Inventory.Output], and removed once assignments carry a root** (§10.6, M10).
// It is still the destination check the agent applies today, because an assignment has no root to
// resolve against yet; when it has, a destination stops being an input mapping at all and this
// goes away rather than being kept alongside.
func (i *Inventory) Configured(name string) bool {
	_, ok := i.byName[name]
	return ok
}

// CreateDomains pre-creates the configured domain directories (§6.1).
//
// At startup rather than at assignment time: the worker does not create its domain directory
// (WRS §5.1), and doing it here keeps it off the establishment path that §6.1 wants inside
// 1–2 s. A directory that cannot be created is reported rather than left to fail later as a
// target worker dying before its metrics socket exists.
func (i *Inventory) CreateDomains() error {
	var errs []error
	for _, domain := range i.static {
		if err := os.MkdirAll(domain.Path, 0o755); err != nil {
			errs = append(errs, fmt.Errorf("create domain %q at %s: %w", domain.Name, domain.Path, err))
		}
	}
	return errors.Join(errs...)
}

// Run observes until ctx ends, then releases every mapping.
func (i *Inventory) Run(ctx context.Context) error {
	var wg sync.WaitGroup

	watcher, err := mxl.NewWatcher(ctx, &wg, []mxl.FlowReceiver{i})
	if err != nil {
		return fmt.Errorf("inventory: watch flows: %w", err)
	}
	i.mu.Lock()
	i.watcher = watcher
	// A reconcile can materialise a domain before this point, since the agent starts its poll loop
	// and this loop concurrently. [Inventory.Materialise] would have found a nil watcher, added the
	// domain here and nowhere else, and — being idempotent on the name it already recorded — would
	// never have got round to the watcher on a later pass. The domain would then report no flows
	// for as long as it existed, and its path could never leave ESTABLISHING (§11).
	pending := slices.Collect(maps.Keys(i.materialised))
	i.mu.Unlock()

	// Safe in the discoverer's receiver order: the inventory already knows these, which is exactly
	// what makes it safe for the watcher to start reporting flows in them.
	for _, path := range pending {
		watcher.AddDomain(path)
	}

	static := make([]string, 0, len(i.static))
	for _, domain := range i.static {
		static = append(static, domain.Path)
	}

	// Receiver order matters: this inventory learns of a domain before the watcher starts
	// reporting the flows already in it, so a flow never arrives for a domain that is not there
	// yet. On the way out the order reverses in effect — the domain is forgotten first and the
	// watcher's removals land on nothing, which [Inventory.RemoveFlow] tolerates.
	//
	// Both go behind [prune] when this node has output roots, so that the order is preserved for
	// the domains that are reported and neither receiver sees the ones that are not (§10.6).
	mxl.NewDiscoverer(ctx, &wg, i.receivers(watcher), i.search, static)

	i.log.Info("observing domains",
		"configured", len(i.static), "search_paths", len(i.search),
		"interval", i.interval, "idle_after", i.idleAfter)
	i.logExclusions()

	ticker := time.NewTicker(i.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			i.closeAll()
			return nil
		case <-ticker.C:
			if i.sample() {
				i.onChange()
			}
		}
	}
}

// AddDomain implements [mxl.DomainReceiver].
func (i *Inventory) AddDomain(path string) {
	path = filepath.Clean(path)

	i.mu.Lock()
	name, configured := i.byPath[path]
	if !configured {
		// An output domain this agent materialised: not a search-path find, and named by the
		// request that asked for it rather than by its path (§10.6).
		name, configured = i.materialised[path]
	}
	if !configured {
		// A discovered domain is named by its path. That is not a path the API can ever use —
		// resolution is a map lookup (see [Inventory.Input]) — it is simply the one string that is
		// certainly unique on this node and stable across restarts, and it is what an operator
		// looking at `GET /v1/flows` needs to see in order to find the thing.
		name = path
	}
	if _, exists := i.domains[path]; exists {
		i.mu.Unlock()
		return
	}
	i.domains[path] = &domainState{name: name, configured: configured, flows: map[string]*flowState{}}
	i.mu.Unlock()

	i.log.Info("domain appeared", "domain", name, "path", path, "configured", configured)
	i.onChange()
}

// RemoveDomain implements [mxl.DomainReceiver].
func (i *Inventory) RemoveDomain(path string) {
	path = filepath.Clean(path)

	i.mu.Lock()
	domain, ok := i.domains[path]
	if ok {
		for _, flow := range domain.flows {
			flow.close()
		}
		delete(i.domains, path)
	}
	i.mu.Unlock()

	if !ok {
		return
	}
	i.log.Info("domain went away", "domain", domain.name, "path", path)
	i.onChange()
}

// AddFlow implements [mxl.FlowReceiver]. The domain is a path, as mxl-utils reports it.
func (i *Inventory) AddFlow(domainPath, id string) {
	domainPath = filepath.Clean(domainPath)

	i.mu.Lock()
	domain, ok := i.domains[domainPath]
	if !ok {
		// The watcher saw a flow in a domain this inventory does not know about. Possible only in
		// the window around a domain being removed, and dropping it is right: the next scan
		// re-adds the domain and every flow in it.
		i.mu.Unlock()
		return
	}
	if _, exists := domain.flows[id]; exists {
		i.mu.Unlock()
		return
	}

	flow := &flowState{
		id:   id,
		path: domainPath,
		dir:  filepath.Join(domainPath, id+".mxl-flow"),
	}
	domain.flows[id] = flow

	// Open now rather than waiting for the first sample: a flow that appears and is immediately
	// replicated should not spend a sample interval invisible on the establishment path.
	i.refresh(flow)
	name, readable := domain.name, flow.def != nil
	i.mu.Unlock()

	i.log.Debug("flow appeared", "domain", name, "flow", id, "readable", readable)
	i.onChange()
}

// RemoveFlow implements [mxl.FlowReceiver].
func (i *Inventory) RemoveFlow(domainPath, id string) {
	domainPath = filepath.Clean(domainPath)

	i.mu.Lock()
	domain, ok := i.domains[domainPath]
	if !ok {
		i.mu.Unlock()
		return
	}
	flow, ok := domain.flows[id]
	if ok {
		flow.close()
		delete(domain.flows, id)
	}
	i.mu.Unlock()

	if !ok {
		return
	}
	i.log.Debug("flow went away", "domain", domain.name, "flow", id)
	i.onChange()
}

// Snapshot renders what is currently observed, as the agent reports it (§9.2).
//
// Deterministically ordered — domains by name, flows by ID — because the agent compares
// consecutive snapshots and only reports one that changed. Without a stable order every report
// would differ, and inventory would advance the store revision on every heartbeat forever,
// waking every watcher in the fleet (§8.3).
func (i *Inventory) Snapshot() []api.DomainInventory {
	i.mu.Lock()
	defer i.mu.Unlock()

	out := make([]api.DomainInventory, 0, len(i.domains))
	for _, domain := range i.domains {
		flows := make([]api.FlowInventory, 0, len(domain.flows))
		for _, flow := range domain.flows {
			if flow.def == nil {
				// The definition is not optional: the destination worker cannot create its local
				// flow without it (§5.3 step 2), so a flow that is not yet readable is not yet a
				// flow anyone can replicate. It appears once it is.
				continue
			}
			flows = append(flows, api.FlowInventory{
				ID:         flow.id,
				Definition: flow.def,
				GroupHint:  flow.hint,
				Producing:  flow.written.active,
			})
		}
		slices.SortFunc(flows, func(a, b api.FlowInventory) int { return cmpString(a.ID, b.ID) })

		out = append(out, api.DomainInventory{
			Name:       domain.name,
			Configured: domain.configured,
			Flows:      flows,
		})
	}
	slices.SortFunc(out, func(a, b api.DomainInventory) int { return cmpString(a.Name, b.Name) })
	return out
}

// Liveness is what one flow looks like to the agent's metrics (§12).
type Liveness struct {
	// Writing reports that the flow's head index has advanced recently — for a source flow its
	// producer is producing, for a destination flow the target worker is delivering. Identical to
	// the `producing` an inventory snapshot carries.
	Writing bool

	// Reading reports that something has consumed from the flow recently. Agent-local: no
	// admission or reconcile decision depends on it, so it is not in a snapshot and never reaches
	// the server.
	Reading bool

	// Definition is flow_def.json verbatim, or nil if the flow is not readable yet.
	Definition json.RawMessage
}

// Look returns the liveness of one flow, addressed the way a worker spec addresses it: by domain
// *path* and flow id.
//
// By path rather than by name because this is called with a [worker.Spec], where the path is the
// resolved thing and the name is only a label (§12). Reports false if this agent is not observing
// the flow — a destination flow in the moment before its target creates it, most often — and the
// caller emits nothing rather than a zero, since "not observed" and "idle" are different claims.
func (i *Inventory) Look(domainPath, flowID string) (Liveness, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()

	domain, ok := i.domains[domainPath]
	if !ok {
		return Liveness{}, false
	}
	flow, ok := domain.flows[flowID]
	if !ok {
		return Liveness{}, false
	}
	return Liveness{
		Writing:    flow.written.active,
		Reading:    flow.read.active,
		Definition: flow.def,
	}, true
}

// sample walks every flow once. It reports whether anything a snapshot would show changed.
func (i *Inventory) sample() bool {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.now()
	changed := false

	for _, domain := range i.domains {
		for _, flow := range domain.flows {
			if i.sampleFlow(domain, flow, now) {
				changed = true
			}
		}
	}
	return changed
}

func (i *Inventory) sampleFlow(domain *domainState, flow *flowState, now time.Time) bool {
	wasProducing, hadDef := flow.written.active, flow.def != nil

	// **Every sample.** A flow deleted and recreated under the same ID is a different data file;
	// the old mapping keeps working and keeps returning the values it had when the file went
	// away, so a republished flow would report producing=false forever and never be replicated
	// again. This is precisely what IsValid exists for (§11.1).
	if flow.flow == nil || !flow.flow.IsValid() {
		if flow.flow != nil {
			i.log.Info("flow was replaced under the same id; reopening",
				"domain", domain.name, "flow", flow.id)
		}
		flow.close()
		i.refresh(flow)
	}

	if flow.flow != nil {
		if _, runtime, err := flow.flow.GetInfo(); err != nil {
			// A mapping that stops decoding is a flow being torn down underneath us. Drop it and
			// let the next sample reopen; the watcher removes it outright if it is really gone.
			flow.close()
		} else {
			flow.written.observe(runtime.HeadIndex, now, i.idleAfter)
			flow.read.observe(runtime.LastReadTime, now, i.idleAfter)
		}
	}

	// Read activity is deliberately absent from this answer. It is not in the snapshot, and a
	// change signal that included it would report to the server every time a downstream consumer
	// started or stopped — a store write per flow per consumer, waking every watcher in the fleet
	// for something no reconcile depends on (§8.3).
	return flow.written.active != wasProducing || (flow.def != nil) != hadDef
}

// liveness turns a monotonically advancing counter into a coarse, hysteretic "something is
// touching this" boolean (§11.1).
//
// Coarse is the point. A raw counter changes on every sample, and anything derived from one
// without hysteresis flaps — which for the head index would mean an inventory snapshot that
// differs every time and a store write per heartbeat, fleet-wide (§6, §8.3).
type liveness struct {
	value    uint64
	seen     bool
	lastMove time.Time
	active   bool
}

// observe applies the hysteresis: idle → active on the first movement, active → idle only after
// the threshold.
func (l *liveness) observe(value uint64, now time.Time, idleAfter time.Duration) {
	switch {
	case !l.seen:
		// The first sample establishes a baseline and claims nothing. Activity is only ever
		// declared by having been *seen* to advance, never by the counter having a value at all —
		// a dormant flow has one of those too.
		l.value, l.seen, l.lastMove = value, true, now
	case value != l.value:
		l.value, l.lastMove, l.active = value, now, true
	case now.Sub(l.lastMove) >= idleAfter:
		l.active = false
	}
}

func (l *liveness) reset() { *l = liveness{} }

// refresh reads the flow definition and reopens the mapping. Called with the lock held.
func (i *Inventory) refresh(flow *flowState) {
	def, hint, err := readDefinition(flow.dir, i.log)
	if err != nil {
		if !flow.complained {
			// Debug, not warn: the overwhelmingly common cause is a producer part-way through
			// creating the flow, and the watcher fires on the directory appearing.
			i.log.Debug("flow definition is not readable yet", "flow", flow.id, "error", err)
			flow.complained = true
		}
		return
	}

	// A reopened flow may carry a *different* definition — that is what makes it a different
	// flow to the session-identity hash (§5.4), and reporting the old bytes would hide a
	// republish from the server that has to rebuild the session.
	flow.def, flow.hint, flow.complained = def, hint, false

	opened, err := mxl.Open(flow.path, flow.id)
	if err != nil {
		i.log.Debug("cannot map flow", "flow", flow.id, "error", err)
		return
	}
	flow.flow = opened
	flow.written.reset()
	flow.read.reset()
}

func (f *flowState) close() {
	if f.flow != nil {
		_ = f.flow.Close()
		f.flow = nil
	}
	f.written.reset()
	f.read.reset()
}

func (i *Inventory) closeAll() {
	i.mu.Lock()
	defer i.mu.Unlock()

	for _, domain := range i.domains {
		for _, flow := range domain.flows {
			flow.close()
		}
	}
}

// readDefinition returns flow_def.json verbatim, plus the parsed group hint.
//
// The bytes and the parse come from one read on purpose. mxl-utils' GetDefinition reopens the
// file and hands back a decoded struct, which is the right shape for its callers and the wrong
// one here twice over: the destination worker needs the original bytes including fields no
// struct models, and reading the file twice invites the two answers to disagree about a flow
// that was republished in between.
func readDefinition(dir string, log *slog.Logger) (json.RawMessage, *api.GroupHint, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "flow_def.json"))
	if err != nil {
		return nil, nil, err
	}
	if !json.Valid(raw) {
		// A partially written file. The producer creates the directory before the definition is
		// complete, so this is a race rather than a corruption, and the next sample re-reads it.
		return nil, nil, fmt.Errorf("flow_def.json is not valid json (%d bytes)", len(raw))
	}

	var decoded mxl.FlowDefinition
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, nil, fmt.Errorf("decode flow_def.json: %w", err)
	}

	var hint *api.GroupHint
	parsed, err := decoded.GetGroupHint()
	switch {
	case err == nil:
		hint = &api.GroupHint{Name: parsed.Name, Type: parsed.Type}
	case errors.Is(err, mxl.ErrMissingGroupHint):
		// Ordinary: not every flow carries one, and a group-hint selector simply does not match
		// it (§9.1).
	default:
		// Malformed rather than missing, which is worth saying out loud — an operator who wrote
		// a group hint expects it to select something.
		log.Warn("flow has a malformed group hint", "flow", decoded.ID, "error", err)
	}

	return json.RawMessage(raw), hint, nil
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
