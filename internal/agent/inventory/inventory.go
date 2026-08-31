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
// Every flow in every observed domain is watched, not only flows with sessions: admission (§11.1)
// needs the liveness of flows nothing is replicating yet, and the destination flow's liveness is
// what ACTIVE is derived from.
//
// # Domains are discovered, never configured, and named by their area
//
// *This package used to take a list of operator-configured name→path mappings, hold them as the
// discoverer's `static` list, and resolve an assignment's domain name through them.* All of that
// is gone (§6, §16): a domain is found under a readable **area**, or materialised by the
// reconciler, and either way its fleet-wide name is `<area>/<elements>` — assigned by the
// innermost containing area (§10.6). What it is *called* beyond that identity is decided through
// the API, as labels (§10.7).
//
// # Discovery is not pruned, and membership is a union
//
// *This package used to hide everything inside an output root from discovery*, so that a
// directory under one had exactly one name and one owner. The naming rule removes the need: both
// namers produce the same string, so there is nothing to arbitrate. What survives is the
// withdrawal half, and it is done by **union** rather than by hiding — a domain is in inventory if
// discovery reports it *or* the reconciler materialised it and has not released it, and it leaves
// only when both say no. Without that, an unpruned withdrawal would forget a materialised domain
// the instant its last flow was released, while a session still targeted it (§10.6).
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
	"strings"
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

// Options configures an [Inventory].
type Options struct {
	// Areas are the directories this node has designated as somewhere MXL domains live, each
	// granting reading, writing or both (§10.6).
	//
	// `read` is where domains are discovered from; `write` is where replication may create them.
	// Neither implies the other and both default false, so a node with no readable area offers no
	// sources and one with no writable area accepts no destinations — one default, applied per
	// direction: access to a node's filesystem is opt-in.
	//
	// **Areas may nest**, and the innermost containing one names a directory. Equal paths are
	// refused; nothing else is, because nothing else is ambiguous (§10.6).
	Areas []api.Area

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

	// areas and byArea are fixed at construction, and are what every name on this node is derived
	// from: [Inventory.nameFor] for a directory's identity and [Inventory.Resolve] for a
	// destination's path. A resolver that could be widened at runtime would not be a pure function
	// of configuration (§10.6).
	areas  []api.Area
	byArea map[string]api.Area

	mu      sync.Mutex
	domains map[string]*domainState // keyed by path

	// discovered and materialised are the two halves of the union (§10.6). A domain is in
	// [Inventory.domains] if either holds it, and leaves only when both have let go.
	//
	// They are kept apart rather than refcounted because they are withdrawn by different parties
	// on different schedules: discovery drops a directory the moment its last flow goes, while the
	// reconciler holds one for exactly as long as a session targets it.
	discovered   map[string]struct{}
	materialised map[string]struct{}

	// replicated is the set of flows this node's own target workers are writing, pushed in by the
	// agent's reconcile (§6, §10.6). It is what [api.FlowInventory.Replicated] reports.
	//
	// **Pushed rather than pulled**, and derived from *running workers* rather than from the
	// assignment set: §10.6's safety argument is that provenance and production go absent
	// together, and that only holds if the flag tracks the worker. A provider func would avoid a
	// write path into this state from the reconcile goroutine; a push avoids this package holding
	// a reference back into the agent, which is the worse of the two couplings.
	replicated map[FlowRef]struct{}

	// watcher is set once [Run] starts and read by [Inventory.Materialise] from the reconcile
	// goroutine, so it is guarded like everything else here.
	watcher *mxl.Watcher
}

type domainState struct {
	name  api.Domain
	flows map[string]*flowState
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
		domains:      map[string]*domainState{},
		discovered:   map[string]struct{}{},
		materialised: map[string]struct{}{},
		replicated:   map[FlowRef]struct{}{},
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

	areas, byArea, err := validateAreas(opts.Areas)
	if err != nil {
		return nil, err
	}
	inv.areas, inv.byArea = areas, byArea

	return inv, nil
}

// readable returns the paths discovery scans: every area granting `read` (§10.6).
func (i *Inventory) readable() []string {
	var out []string
	for _, area := range i.areas {
		if area.Read {
			out = append(out, area.Path)
		}
	}
	slices.Sort(out)
	return out
}

// Lookup resolves an observed domain to its local path: somewhere this node reads from, and an
// initiator's source (§10.6).
//
// It is a strict map lookup over the domains this agent is currently observing, keyed on the
// canonical `(area, elements)` identity, and it cannot be handed a path-shaped string at all —
// which is what stops an assignment from naming an arbitrary directory on this host (§7.2, §13).
//
// The destination side is [Inventory.Resolve], and the asymmetry is the point: a source is by
// definition something this agent *observes*, so it is resolved through what has been seen; a
// destination is something a request *asks for*, so it is resolved from configuration alone.
func (i *Inventory) Lookup(domain api.Domain) (string, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for path, observed := range i.domains {
		if observed.name.Equal(domain) {
			return path, true
		}
	}
	return "", false
}

// Run observes until ctx ends, then closes every mapping.
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

	// Receiver order matters: this inventory learns of a domain before the watcher starts
	// reporting the flows already in it, so a flow never arrives for a domain that is not there
	// yet. On the way out the order reverses in effect — the domain is forgotten first and the
	// watcher's removals land on nothing, which [Inventory.RemoveFlow] tolerates.
	//
	// **Nothing is filtered.** *There used to be a `prune` receiver in front of these, hiding
	// every output root from discovery.* Discovery now reports every domain in every readable
	// area, including one this project materialised and one it is currently writing into (§10.6);
	// the guard against replication feeding itself lives on the flow instead (§10.7).
	//
	// No static list: every domain this node has is one the discoverer finds under a readable
	// area, or one the reconciler materialised and drives in by hand (§6, §10.6).
	mxl.NewDiscoverer(ctx, &wg, []mxl.DomainReceiver{i, watcher}, i.readable(), nil)

	i.logAreas()

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

// logAreas says what this node's areas are, with their grants and their nesting, once at startup.
//
// Not decoration: the naming rule is longest-prefix over areas that may nest (§10.6), so an
// operator who cannot see the table cannot predict what a domain will be called. §10.6 asks for
// this line explicitly, in place of the exclusion list pruning used to print.
func (i *Inventory) logAreas() {
	for _, area := range i.areas {
		grants := make([]string, 0, 2)
		if area.Read {
			grants = append(grants, "read")
		}
		if area.Write {
			grants = append(grants, "write")
		}

		var inside string
		for _, other := range i.areas {
			// Only a proper ancestor matters, and only the innermost one: equal paths are refused
			// at startup, so whichever area contains this one most tightly is the one whose names
			// this area takes over.
			if other.Name == area.Name || !within(other.Path, area.Path) || other.Path == area.Path {
				continue
			}
			if inside == "" || len(i.byArea[inside].Path) < len(other.Path) {
				inside = other.Name
			}
		}

		i.log.Info("area",
			"name", area.Name, "path", area.Path,
			"grants", strings.Join(grants, "+"), "inside", inside)
	}

	i.log.Info("observing domains",
		"areas", len(i.areas), "readable", len(i.readable()),
		"interval", i.interval, "idle_after", i.idleAfter)
}

// AddDomain implements [mxl.DomainReceiver]: the **discovery** half of the union (§10.6).
func (i *Inventory) AddDomain(path string) {
	path = filepath.Clean(path)

	i.mu.Lock()
	i.discovered[path] = struct{}{}
	i.mu.Unlock()

	i.add(path)
}

// RemoveDomain implements [mxl.DomainReceiver]: the discovery half again.
//
// **It must not evict a materialised domain**, and that is the one correctness-critical line in
// the un-pruning (§10.6). The discoverer only reports directories that currently contain a flow,
// so an unconditional removal would forget a domain the instant its last flow was released — while
// a live session still targeted it, and with nothing to bring it back until a producer put a flow
// there. `materialised` is no longer "how a domain gets its short name"; it is purely membership
// this agent holds independently of scanning.
func (i *Inventory) RemoveDomain(path string) {
	path = filepath.Clean(path)

	i.mu.Lock()
	delete(i.discovered, path)
	i.mu.Unlock()

	i.remove(path)
}

// add brings a directory into inventory under the name [Inventory.nameFor] gives it. Idempotent.
func (i *Inventory) add(path string) {
	name, named := i.nameFor(path)
	if !named {
		// Outside every area, or an area's own directory rather than a domain inside one. Reported
		// at debug because the discoverer never produces one — it scans the areas themselves — so
		// this is only reachable through a caller that resolved a path some other way.
		i.log.Debug("ignoring a directory that is in no area", "path", path)
		return
	}

	i.mu.Lock()
	if _, exists := i.domains[path]; exists {
		i.mu.Unlock()
		return
	}
	i.domains[path] = &domainState{name: name, flows: map[string]*flowState{}}
	i.mu.Unlock()

	i.log.Info("domain appeared", "domain", name.String(), "path", path)
	i.onChange()
}

// remove drops a directory from inventory, unless the other half of the union still holds it. It
// reports whether it actually removed anything.
func (i *Inventory) remove(path string) bool {
	i.mu.Lock()
	if _, held := i.discovered[path]; held {
		i.mu.Unlock()
		return false
	}
	if _, held := i.materialised[path]; held {
		i.mu.Unlock()
		return false
	}
	domain, ok := i.domains[path]
	if ok {
		for _, flow := range domain.flows {
			flow.close()
		}
		delete(i.domains, path)
	}
	i.mu.Unlock()

	if !ok {
		return false
	}
	i.log.Info("domain went away", "domain", domain.name.String(), "path", path)
	i.onChange()
	return true
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

	i.log.Debug("flow appeared", "domain", name.String(), "flow", id, "readable", readable)
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
	i.log.Debug("flow went away", "domain", domain.name.String(), "flow", id)
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
			_, replicated := i.replicated[FlowRef{DomainPath: flow.path, FlowID: flow.id}]
			flows = append(flows, api.FlowInventory{
				ID:         flow.id,
				Definition: flow.def,
				GroupHint:  flow.hint,
				Producing:  flow.written.active,
				Replicated: replicated,
			})
		}
		slices.SortFunc(flows, func(a, b api.FlowInventory) int { return cmpString(a.ID, b.ID) })

		out = append(out, api.DomainInventory{Domain: domain.name, Flows: flows})
	}
	slices.SortFunc(out, func(a, b api.DomainInventory) int { return cmpString(a.Domain.String(), b.Domain.String()) })
	return out
}

// FlowRef addresses one flow by this agent's own coordinates: the resolved domain directory and
// the flow id. It is what [Inventory.SetReplicated] is keyed on, because that is the pair a
// [worker.Spec] carries.
type FlowRef struct {
	DomainPath string
	FlowID     string
}

// SetReplicated records which flows this node's own target workers are writing (§6, §10.6).
//
// Called from the agent's reconcile, which is the only thing that starts and stops workers, so the
// set is exactly the running ones. Replacing it wholesale rather than adding and removing is the
// same level-triggered discipline as everything else here: there is no delta to get out of step.
func (i *Inventory) SetReplicated(set map[FlowRef]struct{}) {
	i.mu.Lock()
	changed := len(set) != len(i.replicated)
	if !changed {
		for ref := range set {
			if _, held := i.replicated[ref]; !held {
				changed = true
				break
			}
		}
	}
	i.replicated = set
	i.mu.Unlock()

	// Only on a genuine transition. Inventory is compared before it is sent (§6), so a spurious
	// wake costs a comparison rather than a store write — but it also wakes the report loop, and
	// this is called on every reconcile pass.
	if changed {
		i.onChange()
	}
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
				"domain", domain.name.String(), "flow", flow.id)
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
