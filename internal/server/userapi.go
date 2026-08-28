package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/metrics"
	"github.com/jonasohland/mxl-replicator/internal/server/reconcile"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
	"github.com/jonasohland/mxl-replicator/internal/store"
)

// view is one read of the fleet with everything derived from it.
//
// The read handlers and the reconciler run the *same* function over the same snapshot (§7.3), so
// what an operator is shown and what the fleet is being told to do cannot drift — and the answer
// to "would this request be valid" is computed by the code that will decide it a moment later,
// not by a second implementation of the same rules.
type view struct {
	fleet  *state.Fleet
	result *reconcile.Result
}

func (s *Server) view(ctx context.Context) (*view, error) {
	fleet, err := state.Load(ctx, s.store)
	if err != nil {
		return nil, err
	}
	return &view{fleet: fleet, result: reconcile.Compute(fleet, s.readCfg)}, nil
}

// loadView reads the fleet or writes the error response.
func (s *Server) loadView(w http.ResponseWriter, r *http.Request) (*view, bool) {
	v, err := s.view(r.Context())
	if err != nil {
		storeError(w, s.logger, "read fleet", err)
		return nil, false
	}
	return v, true
}

// handleCreateRequest is POST /v1/requests (§9.1).
//
// Create **or update**, keyed on the client-supplied name. The name is required precisely so
// that every create is idempotent rather than only the ones that remembered to opt in: the
// Kubernetes adapter on the roadmap re-reconciles on every resync, and anything hand-rolling a
// POST has the same problem on retry. It is also what makes `mxl-replicator apply` an apply
// rather than a bespoke protocol: a file naming a set of requests is already one.
//
// Validation happens here rather than by leaving something stuck in WAITING (§7.2). The split
// matters: a request that is *valid but not yet satisfiable* — the flow is not being produced,
// the agent is down — is accepted and reports WAITING, while one that can never work is refused
// now, with a reason naming what to change.
//
// # ?dry_run=true
//
// Everything above happens and nothing is written. This is nearly free because the accept path
// already builds a candidate fleet and reconciles it — that is how it rejects INVALID — so the
// only difference is skipping the store write. It is what lets `apply --dry-run` report the
// outcome including cross-request conflicts, rather than diffing specs and guessing.
//
// Its one limitation, which the CLI has to state rather than hide: the candidate fleet contains
// this request plus *stored* state, so two new requests in one file that conflict with **each
// other** both pass a dry run and the second fails on the real apply. A batch endpoint would fix
// it and is not worth the API surface.
func (s *Server) handleCreateRequest(w http.ResponseWriter, r *http.Request) {
	dryRun, err := boolParam(r, api.QueryDryRun)
	if err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}

	spec, ok := decodeBody[api.RequestSpec](w, r)
	if !ok {
		return
	}
	if err := spec.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}
	if err := validateRequestName(spec.Name); err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}
	if err := validateLabels(spec.Labels); err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}

	ctx := r.Context()
	v, ok := s.loadView(w, r)
	if !ok {
		return
	}

	existing := v.fleet.Requests[spec.Name]
	record := state.RequestRecord{
		ID:        spec.Name,
		Spec:      spec,
		CreatedAt: s.now(),
		UpdatedAt: s.now(),
	}
	if existing.Found {
		record.CreatedAt = existing.Value.CreatedAt
	}

	// Decide against the fleet *as it would be* with this request in it, using the reconciler
	// itself. That is what makes the rejection here and the classification a second later the
	// same rule — including the conflicts that are only visible across requests, like two
	// sources replicating into one destination flow, or a loop.
	candidate := fleetWithRequest(v.fleet, record)
	status := reconcile.Compute(candidate, s.readCfg).Requests[record.ID]
	if status.State == api.StateInvalid {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, status.Reason,
			"reason_code", string(status.ReasonCode))
		return
	}

	// **Re-applying an unchanged request writes nothing** (invariant 13). §8.3's write-volume
	// sizing holds only because desired state is low-churn, and a controller re-reconciling on
	// every resync — which is exactly what the idempotency key exists to support — would bump the
	// revision and trigger a reconcile on every pass without this.
	//
	// The comparison is over the *spec* alone. Including UpdatedAt, which is stamped fresh above,
	// would mean it could never succeed.
	unchanged := existing.Found && existing.Value.Spec.SameAs(spec)

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
	default:
		if _, _, err := state.PutJSON(ctx, s.store, store.RequestKey(record.ID), record, existing.Prior(), state.WriteOptions{CAS: true}); err != nil {
			if errors.Is(err, store.ErrCompareFailed) {
				writeError(w, http.StatusConflict, api.CodeInvalidRequest, "the request was modified concurrently; retry")
				return
			}
			storeError(w, s.logger, "write request", err)
			return
		}

		s.logger.Info("replication request accepted",
			"request", record.ID,
			"source", record.Spec.Source.Node+"/"+record.Spec.Source.Domain,
			"destinations", destinationList(record.Spec.Destinations),
			"updated", existing.Found)
	}

	// 201 for "created", 200 for "already there" — held to for a dry run too, so a client learns
	// which it would have been without a second round trip. What the status code *cannot* say is
	// whether anything was written, since an unchanged apply is also a 200; that is what
	// [api.HeaderOutcome] is for.
	code := http.StatusCreated
	if existing.Found {
		code = http.StatusOK
	}
	w.Header().Set(api.HeaderOutcome, outcome)
	writeJSON(w, code, api.Request{
		ID:          record.ID,
		RequestSpec: record.Spec,
		CreatedAt:   record.CreatedAt,
		Status:      status,
	})
}

// boolParam reads an optional boolean query parameter. An unparseable value is an error rather
// than a silent false: ?dry_run=yes must not quietly write to the store.
func boolParam(r *http.Request, name string) (bool, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s: %q is not a boolean", name, raw)
	}
	return value, nil
}

func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	v, ok := s.loadView(w, r)
	if !ok {
		return
	}

	list := api.RequestList{Requests: []api.Request{}}
	for _, id := range sortedKeys(v.fleet.Requests) {
		list.Requests = append(list.Requests, v.request(id))
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	v, ok := s.loadView(w, r)
	if !ok {
		return
	}
	if _, found := v.fleet.Requests[id]; !found {
		writeError(w, http.StatusNotFound, api.CodeNotFound, "no request "+id)
		return
	}
	writeJSON(w, http.StatusOK, v.request(id))
}

// handleDeleteRequest is DELETE /v1/requests/{id}: cancel the intent.
//
// Cancelling a request is the only thing that removes one — the system never cancels one on the
// user's behalf because a session is failing (§11). The path underneath survives until the last
// request referencing it is gone, which is what refcounting is for, and the reconciler does that
// on its next pass rather than here.
func (s *Server) handleDeleteRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	existing, err := state.Get[state.RequestRecord](ctx, s.store, store.RequestKey(id))
	if err != nil {
		storeError(w, s.logger, "read request", err)
		return
	}
	if !existing.Found {
		writeError(w, http.StatusNotFound, api.CodeNotFound, "no request "+id)
		return
	}

	if _, err := s.store.Delete(ctx, store.RequestKey(id), store.IfRevision(existing.Rev)); err != nil {
		storeError(w, s.logger, "delete request", err)
		return
	}

	s.logger.Info("replication request cancelled", "request", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	v, ok := s.loadView(w, r)
	if !ok {
		return
	}

	list := api.NodeList{Nodes: []api.Node{}}
	for _, name := range sortedKeys(v.fleet.Nodes) {
		list.Nodes = append(list.Nodes, v.node(name))
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleNodeDomains(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("node")

	v, ok := s.loadView(w, r)
	if !ok {
		return
	}
	entry, found := v.fleet.Nodes[name]
	if !found {
		writeError(w, http.StatusNotFound, api.CodeNotFound, "no node "+name)
		return
	}

	domains := entry.Value.Domains
	if domains == nil {
		domains = []api.DomainMapping{}
	}
	writeJSON(w, http.StatusOK, api.DomainList{Node: name, Domains: domains})
}

// handleFlows is GET /v1/flows: the fleet-wide inventory, filterable.
//
// This is what the legacy proxy fetched peer-to-peer, one flow at a time, from whichever proxy
// happened to own it. Aggregating it here is a genuine simplification of the old design rather
// than a new feature: it falls straight out of agents reporting what they observe (§6).
func (s *Server) handleFlows(w http.ResponseWriter, r *http.Request) {
	v, ok := s.loadView(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	wantNode := query.Get("node")
	wantDomain := query.Get("domain")
	wantFlow := query.Get("flow")
	wantHint := query.Get("group_hint")
	wantType := query.Get("type")

	list := api.FlowList{Flows: []api.FlowEntry{}}
	for _, node := range sortedKeys(v.fleet.Inventory) {
		if wantNode != "" && node != wantNode {
			continue
		}
		snapshot := v.fleet.Inventory[node].Value
		for _, domain := range snapshot.Domains {
			if wantDomain != "" && domain.Name != wantDomain {
				continue
			}
			for _, flow := range domain.Flows {
				switch {
				case wantFlow != "" && flow.ID != wantFlow:
					continue
				case wantHint != "" && (flow.GroupHint == nil || flow.GroupHint.Name != wantHint):
					continue
				case wantType != "" && (flow.GroupHint == nil || flow.GroupHint.Type != wantType):
					continue
				}
				list.Flows = append(list.Flows, api.FlowEntry{Node: node, Domain: domain.Name, FlowInventory: flow})
			}
		}
	}

	sort.Slice(list.Flows, func(i, j int) bool {
		if list.Flows[i].Node != list.Flows[j].Node {
			return list.Flows[i].Node < list.Flows[j].Node
		}
		if list.Flows[i].Domain != list.Flows[j].Domain {
			return list.Flows[i].Domain < list.Flows[j].Domain
		}
		return list.Flows[i].ID < list.Flows[j].ID
	})

	writeJSON(w, http.StatusOK, list)
}

// handlePaths is GET /v1/paths: derived state, which is what an operator actually looks at.
//
// It reports `settling` explicitly rather than showing everything as WAITING while the server
// has not yet acted. That distinction is the difference between "this server has just started"
// and "the entire fleet has stopped", and anything scraping this would otherwise read the second
// (§7.3).
func (s *Server) handlePaths(w http.ResponseWriter, r *http.Request) {
	v, ok := s.loadView(w, r)
	if !ok {
		return
	}

	paths := v.result.SortedPaths()
	if paths == nil {
		paths = []api.Path{}
	}

	writeJSON(w, http.StatusOK, api.PathsResponse{
		Settling: !v.fleet.Reconciler.Found || !v.fleet.Reconciler.Value.Settled,
		Paths:    paths,
	})
}

// --- rendering ---------------------------------------------------------------------------

func (v *view) request(id string) api.Request {
	record := v.fleet.Requests[id].Value

	status, ok := v.result.Requests[id]
	if !ok {
		status = api.RequestStatus{State: api.StateWaiting, Paths: []api.PathStatus{}}
	}
	if status.Paths == nil {
		status.Paths = []api.PathStatus{}
	}

	return api.Request{
		ID:          record.ID,
		RequestSpec: record.Spec,
		CreatedAt:   record.CreatedAt,
		Status:      status,
	}
}

func (v *view) node(name string) api.Node {
	record := v.fleet.Nodes[name].Value

	node := api.Node{
		Name:         name,
		RegisteredAt: record.RegisteredAt,
		Capabilities: record.Capabilities,
		Domains:      record.Domains,
	}
	if node.Domains == nil {
		node.Domains = []api.DomainMapping{}
	}

	// Live is the lease and nothing else. There is deliberately no "last seen": a heartbeat does
	// not write, so the only honest timestamp available is when the lease was taken, and the
	// lease's existence already bounds liveness to within its TTL (§8.3).
	if lease, held := v.fleet.Leases[name]; held {
		node.Live = true
		node.Instance = lease.Value.Instance
		node.LastSeen = lease.Value.AcquiredAt
	}
	return node
}

// withRequest returns a shallow copy of the fleet with one request added or replaced, for
// deciding a POST against the state that would result from it.
func fleetWithRequest(fleet *state.Fleet, record state.RequestRecord) *state.Fleet {
	copied := *fleet
	copied.Requests = make(map[string]state.Entry[state.RequestRecord], len(fleet.Requests)+1)
	for id, entry := range fleet.Requests {
		copied.Requests[id] = entry
	}
	copied.Requests[record.ID] = state.Entry[state.RequestRecord]{Found: true, Value: record}
	return &copied
}

// validateRequestName bounds the idempotency key, which is also the request's ID and therefore
// appears in a URL and in a store key.
//
// Stricter than [api.RequestSpec.Validate], deliberately: that type is the wire contract and
// stays permissive, while this is the server deciding what it is willing to name things. The
// character set is what a Kubernetes object name and a DNS label allow, plus a colon, which
// operators reach for when they name things after a source and a destination.
func validateRequestName(name string) error {
	if err := validateName("name", name); err != nil {
		return err
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return errors.New("name may contain only letters, digits and the characters - _ . : — it is used as the request's ID, in URLs and in storage keys")
		}
	}
	if strings.HasPrefix(name, ".") {
		return errors.New("name must not begin with a dot")
	}
	return nil
}

// maxLabelKeyLength and maxLabelValueLength bound a label. Prometheus imposes no limit, so these
// are this project's: a label rides into every one of a session's metric series, and an unbounded
// one is an unbounded cardinality problem written into durable state.
const (
	maxLabelKeyLength   = 63
	maxLabelValueLength = 253
)

// validateLabels refuses a label the system cannot honour, rather than accepting it and dropping
// it later at scrape time (M6b, M8e).
//
// Labels do two jobs, and the second one is why this stopped being cosmetic:
//
//   - They ride into worker metrics as user labels (§12). An invalid name does not degrade a
//     metric — it invalidates it at collection time and takes its whole family down — so the
//     scrape path defends itself by dropping it. That defence is right, but a label silently
//     discarded is a label the operator believes is there.
//   - They scope `apply --prune` (M8e). A dropped label therefore silently changes what can
//     cancel the request, which is a much worse failure than a missing series.
//
// Stricter than [api.RequestSpec.Validate], on the same reasoning as [validateRequestName]: the
// wire type is the contract and stays permissive, while this is the server deciding what it is
// willing to accept.
func validateLabels(labels map[string]string) error {
	reserved := append(metrics.WorkerLabelNames(), metrics.LabelQuantile)

	for _, key := range sortedKeys(labels) {
		switch {
		case !metrics.ValidLabelName(key):
			return fmt.Errorf("label %q is not a usable Prometheus label name: letters, digits and underscore, not starting with a digit or with __", key)
		case slices.Contains(reserved, key):
			return fmt.Errorf("label %q is reserved: this project sets it on worker metrics itself (%s)",
				key, strings.Join(reserved, ", "))
		case len(key) > maxLabelKeyLength:
			return fmt.Errorf("label %q is longer than %d characters", key, maxLabelKeyLength)
		case len(labels[key]) > maxLabelValueLength:
			return fmt.Errorf("label %q has a value longer than %d characters", key, maxLabelValueLength)
		}
	}
	return nil
}

// destinationList renders a request's fan-out for a log line.
func destinationList(destinations []api.Destination) string {
	names := make([]string, 0, len(destinations))
	for _, dst := range destinations {
		names = append(names, dst.Endpoint())
	}
	return strings.Join(names, ",")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
