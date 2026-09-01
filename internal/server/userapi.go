package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

// handleCreateRequest is POST /v1/namespaces/{ns}/requests (§9.1).
//
// Create **or update**, keyed on `(namespace, name)` — the request's ID and its idempotency key
// (§9.3). The name is required precisely so that every create is idempotent rather than only the
// ones that remembered to opt in: the Kubernetes adapter on the roadmap re-reconciles on every
// resync, and anything hand-rolling a POST has the same problem on retry. It is also what makes
// `mxl-replicator apply` an apply rather than a bespoke protocol: a file naming a set of requests
// is already one.
//
// # What it refuses
//
// **Only what is structurally invalid** (§7.2) — a malformed selector, an area no destination
// node advertises, a spec that cannot expand at all. Validation is otherwise *per path*: a
// request whose selector expands onto twenty paths, one of which conflicts, is accepted and
// reports nineteen paths and one invalid one. That is what makes selectors usable, since an
// expansion is not something its author can enumerate before submitting it — refusing the whole
// request for one bad pairing puts the author at the mercy of fleet state they did not write.
//
// *This supersedes refusing the POST whenever `Compute` returned INVALID for any reason at all,
// which is the position §7.2 argued down.* The distinction is carried by
// [reconcile.Result.Structural] rather than re-derived here, so request-time rejection and
// steady-state classification still run one `Compute` and cannot disagree.
//
// # ?dry_run=true
//
// Everything above happens and nothing is written — including the namespace auto-create below.
// This is nearly free because the accept path already builds a candidate fleet and reconciles it,
// so the only difference is skipping the store writes. It is what lets `apply --dry-run` report
// the outcome including cross-request conflicts, rather than diffing specs and guessing.
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

	ns := r.PathValue("ns")
	if err := api.ValidNamespace(ns); err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}

	spec, ok := decodeBody[api.RequestSpec](w, r)
	if !ok {
		return
	}

	// The URL is authoritative and the body may agree with it or say nothing. Disagreement is
	// refused rather than resolved: there is no defensible winner, and silently preferring either
	// would put the request in a namespace the caller appears to contradict.
	if spec.Namespace != "" && spec.Namespace != ns {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest,
			fmt.Sprintf("body names namespace %q but the URL names %q", spec.Namespace, ns))
		return
	}
	// Before the unchanged comparison below, deliberately: normalising after deciding whether the
	// spec differs would make every apply of a namespace-less body look like a change and write on
	// every pass (invariant 13).
	spec.Namespace = ns

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

	id := spec.RequestID()
	existing := v.fleet.Requests[id]
	record := state.RequestRecord{
		ID:        id,
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
	// sources replicating into one destination flow, or a loop, which are reported rather than
	// refused.
	candidate := fleetWithRequest(v.fleet, record)
	computed := reconcile.Compute(candidate, s.readCfg)
	if bad, structural := computed.Structural[id]; structural {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, bad.Message,
			"reason_code", string(bad.Code))
		return
	}
	status := computed.Requests[id]

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
		// same thing, which is also the truth. An unchanged request also skips the namespace
		// create, which is correct: a namespace referenced by a request that already exists
		// already got one when that request was written.
	default:
		// **The namespace first, then the request** (§9.3). Reversed, a failure in between leaves
		// a request referencing a namespace with no record — the exact state that makes
		// `GET /v1/namespaces` non-authoritative. This order leaves an inert empty namespace
		// instead, which is indistinguishable from a deliberately empty one and costs nothing. No
		// transaction is needed, because the two failure modes are not equally bad.
		if err := s.ensureNamespace(ctx, v.fleet, ns); err != nil {
			storeError(w, s.logger, "create namespace", err)
			return
		}

		if _, _, err := state.PutJSON(ctx, s.store, store.RequestKey(id.Namespace, id.Name), record, existing.Prior(), state.WriteOptions{CAS: true}); err != nil {
			if errors.Is(err, store.ErrCompareFailed) {
				writeError(w, http.StatusConflict, api.CodeInvalidRequest, "the request was modified concurrently")
				return
			}
			storeError(w, s.logger, "write request", err)
			return
		}

		s.logger.Info("replication request accepted",
			"request", id.String(),
			"sources", sourceList(record.Spec.Sources),
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
		ID:          id.String(),
		RequestSpec: record.Spec,
		CreatedAt:   record.CreatedAt,
		UpdatedAt:   record.UpdatedAt,
		Status:      status,
	})
}

// ensureNamespace creates a namespace record with defaults if there is none (§9.3).
//
// **Create-if-absent, never write-if-present.** An unconditional write would bump the namespace
// key's revision on every request write and wake every watcher in the fleet, which is the churn
// §8.3 is sized against — the same no-write-if-unchanged discipline the request itself follows,
// applied one key over.
//
// A concurrent creator losing the CAS is not an error: the record it lost to is the one this would
// have written, defaults and all.
func (s *Server) ensureNamespace(ctx context.Context, fleet *state.Fleet, ns string) error {
	if _, exists := fleet.Namespaces[ns]; exists {
		return nil
	}

	record := state.NamespaceRecord{
		Name:      ns,
		Spec:      api.Namespace{Name: ns}.Normalise(),
		CreatedAt: s.now(),
		UpdatedAt: s.now(),
	}
	_, _, err := state.PutJSON(ctx, s.store, store.NamespaceKey(ns), record, state.Prior{}, state.WriteOptions{CAS: true})
	if errors.Is(err, store.ErrCompareFailed) {
		return nil
	}
	return err
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

// handleListRequests is GET /v1/requests: the fleet-wide list across every partition (§9.1).
//
// `?namespace=` narrows it, which is the same set GET /v1/namespaces/{ns}/requests returns. Two
// spellings, deliberately: the namespaced collection is where a request is *created* and
// addressed, and a list has to be readable without knowing which partitions exist.
func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	s.listRequests(w, r, r.URL.Query().Get(api.QueryNamespace))
}

// handleListNamespaceRequests is GET /v1/namespaces/{ns}/requests.
func (s *Server) handleListNamespaceRequests(w http.ResponseWriter, r *http.Request) {
	s.listRequests(w, r, r.PathValue("ns"))
}

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request, ns string) {
	v, ok := s.loadView(w, r)
	if !ok {
		return
	}

	list := api.RequestList{Requests: []api.Request{}}
	for _, id := range v.fleet.SortedRequestIDs() {
		if ns != "" && id.Namespace != ns {
			continue
		}
		list.Requests = append(list.Requests, v.request(id))
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	id := api.RequestID{Namespace: r.PathValue("ns"), Name: r.PathValue("name")}

	v, ok := s.loadView(w, r)
	if !ok {
		return
	}
	if _, found := v.fleet.Requests[id]; !found {
		writeError(w, http.StatusNotFound, api.CodeNotFound, "no request "+id.String())
		return
	}
	writeJSON(w, http.StatusOK, v.request(id))
}

// handleDeleteRequest is DELETE /v1/namespaces/{ns}/requests/{name}: cancel the intent.
//
// Cancelling a request is the only thing that removes one — the system never cancels one on the
// user's behalf because a session is failing (§11). The path underneath survives until the last
// request referencing it is gone, which is what refcounting is for, and the reconciler does that
// on its next pass rather than here.
//
// The namespace record is left behind. Never auto-delete (§9.3): an empty namespace is inert, and
// removing one on the way past would make deleting the last request in a partition silently
// discard the operator's `paths: exclusive` declaration.
func (s *Server) handleDeleteRequest(w http.ResponseWriter, r *http.Request) {
	id := api.RequestID{Namespace: r.PathValue("ns"), Name: r.PathValue("name")}
	ctx := r.Context()
	key := store.RequestKey(id.Namespace, id.Name)

	existing, err := state.Get[state.RequestRecord](ctx, s.store, key)
	if err != nil {
		storeError(w, s.logger, "read request", err)
		return
	}
	if !existing.Found {
		writeError(w, http.StatusNotFound, api.CodeNotFound, "no request "+id.String())
		return
	}

	if _, err := s.store.Delete(ctx, key, store.IfRevision(existing.Rev)); err != nil {
		storeError(w, s.logger, "delete request", err)
		return
	}

	s.logger.Info("replication request cancelled", "request", id.String())
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
			if wantDomain != "" && domain.Domain.String() != wantDomain {
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
				list.Flows = append(list.Flows, api.FlowEntry{Node: node, Domain: domain.Domain.String(), FlowInventory: flow})
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

func (v *view) request(id api.RequestID) api.Request {
	record := v.fleet.Requests[id].Value

	status, ok := v.result.Requests[id]
	if !ok {
		status = api.RequestStatus{State: api.StateWaiting, Paths: []api.PathStatus{}}
	}
	if status.Paths == nil {
		status.Paths = []api.PathStatus{}
	}

	return api.Request{
		ID:          record.ID.String(),
		RequestSpec: record.Spec,
		CreatedAt:   record.CreatedAt,
		UpdatedAt:   record.UpdatedAt,
		Status:      status,
	}
}

func (v *view) node(name string) api.Node {
	record := v.fleet.Nodes[name].Value

	node := api.Node{
		Name:         name,
		RegisteredAt: record.RegisteredAt,
		Capabilities: record.Capabilities,
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
	copied.Requests = make(map[api.RequestID]state.Entry[state.RequestRecord], len(fleet.Requests)+1)
	maps.Copy(copied.Requests, fleet.Requests)
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
			return errors.New("name may contain only letters, digits and the characters - _ . :")
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
//   - Together with the namespace they scope `apply --prune` (M8e). A dropped label therefore
//     silently changes what can cancel the request, which is a much worse failure than a missing
//     series.
//
// `namespace` is in the reserved set for the second reason rather than the first: it is a real
// property now (§9.3) and an ordinary user label as far as *this* map is concerned, but it is
// emitted as a metric dimension of its own (§12), so a user label of that name would collide with
// one this project sets itself.
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
			return fmt.Errorf("label %q is reserved for worker metrics", key)
		case len(key) > maxLabelKeyLength:
			return fmt.Errorf("label %q is longer than %d characters", key, maxLabelKeyLength)
		case len(labels[key]) > maxLabelValueLength:
			return fmt.Errorf("label %q has a value longer than %d characters", key, maxLabelValueLength)
		}
	}
	return nil
}

// destinationList renders a request's fan-out for a log line.
func sourceList(sources []api.Source) string {
	names := make([]string, 0, len(sources))
	for _, src := range sources {
		names = append(names, src.Describe())
	}
	return strings.Join(names, ",")
}

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
