package server

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/metrics"
	"github.com/jonasohland/mxl-replicator/internal/server/reconcile"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
	"github.com/jonasohland/mxl-replicator/internal/store"
)

// Domain labels (§9.1, §10.7).
//
// An operator attaches key/value pairs to `(node, domain)` through the API, before or after the
// domain is discovered. **Labels annotate; they never rename** — a domain's identity is
// `<area>/<elements>`, permanently — and what they are for is *selection*: a request's source
// either names a domain directly or matches domains by label.
//
// The agent never sees one. Label records are joined against inventory here, so nothing new
// reaches it, no new state is held there, and §4.2's fail-static surface is unchanged (§10.7).

// handleLabelDomain is POST /v1/nodes/{node}/domains: label one `(node, domain)`.
//
// # Two body shapes
//
// An **apply** carries the full map it declares and owns those keys: it sets them, removes the
// ones it declared last time and no longer does, and leaves every other key alone. A **patch**
// carries keys to set and keys to remove, merges against nothing, and does not change what a
// future apply believes it owns (§9.1). The merge itself is [api.DomainLabelWrite.Merge]; what
// lives here is the write discipline around it.
//
// # The node is not validated against the fleet
//
// §10.7 makes a label on an unobserved domain an accepted, inert, pending record, and there is no
// coherent place to draw the line at the *node* level that does not also refuse the legitimate
// case — labelling a domain on a node that is being built. The mitigation is the read side:
// `GET /v1/nodes/{node}/domains` answers for an unregistered node, so a typo is visible rather
// than merely inert.
func (s *Server) handleLabelDomain(w http.ResponseWriter, r *http.Request) {
	dryRun, err := boolParam(r, api.QueryDryRun)
	if err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}

	node := r.PathValue("node")
	if err := validateName("node", node); err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}

	body, ok := decodeBody[api.DomainLabelWrite](w, r)
	if !ok {
		return
	}
	if err := body.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}
	if err := validateDomainLabels(body); err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}

	ctx := r.Context()
	key := store.DomainLabelsKey(node, body.Domain.String())

	v, ok := s.loadView(w, r)
	if !ok {
		return
	}

	existing, err := state.Get[api.DomainLabels](ctx, s.store, key)
	if err != nil {
		storeError(w, s.logger, "read domain labels", err)
		return
	}

	stored := existing.Value
	stored.Node, stored.Domain = node, body.Domain
	merged := body.Merge(stored)

	// **The blast radius**, computed the same way `?dry_run=true` on a request is: the real
	// `Compute` against a candidate fleet, rather than a second implementation of the rules
	// (§9.1). Nearly free, since the machinery already exists and is already generalised over a
	// candidate fleet.
	//
	// It is computed for the real write too, not only for a dry run: a label write starts and
	// stops media and nothing about the verb's ergonomics says so.
	stopped, started := labelImpact(v, s.readCfg, merged)

	// **No write if unchanged**, and `Declared` is part of "unchanged" (§9.1). A relabel is a
	// desired-state write that wakes every watcher and moves every request's expansion, so a
	// controller re-applying an identical label set must not cost a fleet-wide reconcile.
	unchanged := existing.Found && existing.Value.SameAs(merged)

	outcome := api.OutcomeCreated
	switch {
	case unchanged:
		outcome = api.OutcomeUnchanged
	case existing.Found:
		outcome = api.OutcomeUpdated
	}

	switch {
	case dryRun, unchanged:
		// Nothing to write. A dry run is told what would happen; an unchanged apply is told the
		// same thing, which is also the truth.
	case merged.IsEmpty():
		// **An empty result deletes the record** rather than storing one with no labels (§9.1).
		// The two are indistinguishable to every reader, so storing the empty one is a key that
		// accumulates and never gets collected.
		if !existing.Found {
			break
		}
		if _, err := s.store.Delete(ctx, key, store.IfRevision(existing.Rev)); err != nil {
			storeError(w, s.logger, "delete domain labels", err)
			return
		}
		outcome = api.OutcomeUpdated
		s.logger.Info("domain labels cleared", "node", node, "domain", body.Domain.String())
	default:
		if _, _, err := state.PutJSON(ctx, s.store, key, merged, existing.Prior(), state.WriteOptions{CAS: true}); err != nil {
			if errors.Is(err, store.ErrCompareFailed) {
				writeError(w, http.StatusConflict, api.CodeInvalidRequest, "the domain was labelled concurrently")
				return
			}
			storeError(w, s.logger, "write domain labels", err)
			return
		}
		s.logger.Info("domain labelled",
			"node", node, "domain", body.Domain.String(),
			"shape", body.Kind(), "labels", len(merged.Labels))
	}

	code := http.StatusCreated
	if existing.Found {
		code = http.StatusOK
	}
	w.Header().Set(api.HeaderOutcome, outcome)
	writeJSON(w, code, api.DomainLabelResult{DomainLabels: merged, Stopped: stopped, Started: started})
}

// labelImpact is the paths this write would remove and create (§9.1).
//
// A diff of two `Compute` passes over the same snapshot — one as things are, one as they would be
// — which is what keeps the answer and the reconcile that follows it from being two
// implementations of one rule.
func labelImpact(v *view, cfg reconcile.Config, record api.DomainLabels) (stopped, started []api.Path) {
	after := reconcile.Compute(fleetWithLabels(v.fleet, record), cfg)

	for _, path := range v.result.SortedPaths() {
		if _, still := after.Paths[path.ID]; !still {
			stopped = append(stopped, path)
		}
	}
	for _, path := range after.SortedPaths() {
		if _, already := v.result.Paths[path.ID]; !already {
			started = append(started, path)
		}
	}
	return stopped, started
}

// validateDomainLabels holds a label to what this server is willing to accept.
//
// The same rule request labels take — a usable Prometheus label name, not one this project sets
// itself, bounded key and value — because a domain label rides into metrics for the same reason
// (§12). *This is what `validateLabels` became after the namespace half of it was deleted; the
// reusable part is shared rather than copied, so the two cannot drift.*
//
// One addition: **the `name` key's *value* is held to the element grammar** (§10.6), because it is
// rendered as the `domain_name` metric label. It is the only key whose value is constrained,
// exactly as `namespace` was the only request label whose value was, and for the identical reason.
func validateDomainLabels(body api.DomainLabelWrite) error {
	set := body.Apply
	var remove []string
	if body.Patch != nil {
		set, remove = body.Patch.Set, body.Patch.Remove
	}

	if err := validateLabels(set); err != nil {
		return err
	}
	for _, key := range remove {
		if !metrics.ValidLabelName(key) {
			return fmt.Errorf("label %q is not a usable Prometheus label name, so it cannot be set and need not be removed", key)
		}
	}

	if value, ok := set[api.LabelName]; ok {
		if err := api.ValidDomainName(value); err != nil {
			return fmt.Errorf("label %q: %q: %w", api.LabelName, value, err)
		}
	}
	return nil
}

// handleNodeDomains is GET /v1/nodes/{node}/domains: the join (§9.1).
//
// It is the one endpoint that has to render four things that disagree with each other: an observed
// domain with labels, an observed domain without, a **labelled domain nobody observes** — that is
// how an operator sees a label applied before the producer came up — and the settling flag that
// says which of those the answer can be trusted for.
//
// *This used to report registration data — the node's configured mappings.* There is no configured
// mapping to report any more (§6), so it reports what the agent sees. A domain this project
// replicates *into* is listed like any other, since a domain is a place rather than a direction
// (§10.6); each flow carries whether this node is the one writing it.
//
// **It answers for a node with no registration at all**, listing the label records alone. A label
// write does not validate its node against the fleet (§10.7), so refusing the read would make a
// typo'd node name in a manifest a write that can never be read back — the one shape of mistake
// where an inert record has nowhere to be noticed.
func (s *Server) handleNodeDomains(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("node")

	v, ok := s.loadView(w, r)
	if !ok {
		return
	}

	_, registered := v.fleet.Nodes[node]
	inventory, observing := v.fleet.Inventory[node]

	labelled := map[string]api.DomainLabels{}
	for _, key := range v.fleet.SortedDomainKeys() {
		if key.Node == node {
			labelled[key.Domain] = v.fleet.DomainLabels[key].Value
		}
	}

	if !registered && !observing && len(labelled) == 0 {
		writeError(w, http.StatusNotFound, api.CodeNotFound, "no node "+node)
		return
	}

	domains := []api.DomainInfo{}
	seen := map[string]bool{}
	for _, observed := range inventory.Value.Domains {
		name := observed.Domain.String()
		seen[name] = true
		domains = append(domains, api.DomainInfo{
			Domain:   observed.Domain,
			Observed: true,
			Labels:   labelled[name].Labels,
			Flows:    observed.Flows,
		})
	}
	// The pending half: a label on a domain the node does not report is accepted and inert
	// (§10.7), and listing it is how the intent stays visible rather than lost.
	for _, name := range sortedKeys(labelled) {
		if seen[name] {
			continue
		}
		domains = append(domains, api.DomainInfo{Domain: labelled[name].Domain, Labels: labelled[name].Labels})
	}
	sort.Slice(domains, func(i, j int) bool {
		return domains[i].Domain.String() < domains[j].Domain.String()
	})

	writeJSON(w, http.StatusOK, api.DomainList{
		Node:     node,
		Settling: !v.fleet.Reconciler.Found || !v.fleet.Reconciler.Value.Settled,
		Domains:  domains,
	})
}

// fleetWithLabels returns a shallow copy of the fleet with one label record replaced or removed,
// for deciding a label write against the state that would result from it (§9.1).
//
// The same shape [fleetWithRequest] takes, and for the same reason: `?dry_run=true` runs the real
// `Compute` against a candidate fleet rather than a second implementation of the rules.
func fleetWithLabels(fleet *state.Fleet, record api.DomainLabels) *state.Fleet {
	copied := *fleet
	copied.DomainLabels = make(map[state.DomainKey]state.Entry[api.DomainLabels], len(fleet.DomainLabels)+1)
	for key, entry := range fleet.DomainLabels {
		copied.DomainLabels[key] = entry
	}

	key := state.DomainKey{Node: record.Node, Domain: record.Domain.String()}
	if record.IsEmpty() {
		delete(copied.DomainLabels, key)
	} else {
		copied.DomainLabels[key] = state.Entry[api.DomainLabels]{Found: true, Value: record}
	}
	return &copied
}

// sortedLabelKeys is [slices.Sorted] over a label map, for deterministic rendering.
func sortedLabelKeys(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
