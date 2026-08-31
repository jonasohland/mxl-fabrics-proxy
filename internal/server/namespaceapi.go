package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
	"github.com/jonasohland/mxl-replicator/internal/store"
)

// Namespace CRUD (§9.3).
//
// A namespace is a first-class object in desired state: it holds a name, a `paths` policy and a
// description, and it is what makes `GET /v1/namespaces` a complete answer rather than a
// projection of the request set.
//
// Existence is deliberately asymmetric — **auto-create on first reference, explicit create
// allowed, never auto-delete**. The auto-create lives on the request write path
// ([Server.ensureNamespace]), not here, because an ordering dependency on this endpoint is
// exactly what the auto-create exists to remove: an adapter would otherwise need create authority
// on namespaces and a create-if-missing step in front of every request.

// handleCreateNamespace is POST /v1/namespaces: create or update, keyed on the name.
//
// Same shape as the request POST, one object over — an unchanged apply writes nothing and says so
// through [api.HeaderOutcome]. That matters more here than it looks: a namespace record's
// revision moving wakes every watcher in the fleet, and this key is read by every reconcile.
//
// An explicit document beats an auto-create because apply orders namespaces before requests
// (§9.1). A file declaring `paths: exclusive` alongside a request in that namespace lands the
// declaration first; a request that arrives on its own gets the defaults.
func (s *Server) handleCreateNamespace(w http.ResponseWriter, r *http.Request) {
	dryRun, err := boolParam(r, api.QueryDryRun)
	if err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}

	spec, ok := decodeBody[api.Namespace](w, r)
	if !ok {
		return
	}
	if err := spec.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}
	// Normalised before the comparison below, not after: an unset policy stored as `shared` and a
	// document that spells `shared` are the same intent, and deciding otherwise would write on
	// every apply.
	spec = spec.Normalise()

	ctx := r.Context()
	key := store.NamespaceKey(spec.Name)

	existing, err := state.Get[state.NamespaceRecord](ctx, s.store, key)
	if err != nil {
		storeError(w, s.logger, "read namespace", err)
		return
	}

	record := state.NamespaceRecord{
		Name:      spec.Name,
		Spec:      spec,
		CreatedAt: s.now(),
		UpdatedAt: s.now(),
	}
	if existing.Found {
		record.CreatedAt = existing.Value.CreatedAt
	}

	unchanged := existing.Found && existing.Value.Spec.SameAs(spec)

	outcome := api.OutcomeCreated
	switch {
	case unchanged:
		outcome = api.OutcomeUnchanged
	case existing.Found:
		outcome = api.OutcomeUpdated
	}

	if !dryRun && !unchanged {
		if _, _, err := state.PutJSON(ctx, s.store, key, record, existing.Prior(), state.WriteOptions{CAS: true}); err != nil {
			if errors.Is(err, store.ErrCompareFailed) {
				writeError(w, http.StatusConflict, api.CodeInvalidRequest, "the namespace was modified concurrently")
				return
			}
			storeError(w, s.logger, "write namespace", err)
			return
		}
		s.logger.Info("namespace applied", "namespace", spec.Name, "paths", spec.Paths, "updated", existing.Found)
	}

	code := http.StatusCreated
	if existing.Found {
		code = http.StatusOK
	}
	w.Header().Set(api.HeaderOutcome, outcome)
	writeJSON(w, code, s.namespaceInfo(r, spec))
}

// handleListNamespaces is GET /v1/namespaces.
//
// Every stored record, plus a row for any namespace a request references that has no record yet.
// The second half is a repair rather than a feature: the auto-create is a real write and this
// should never fire, but a request whose namespace write failed between the two (§9.3) is
// otherwise invisible here while being perfectly visible in `GET /v1/requests`.
func (s *Server) handleListNamespaces(w http.ResponseWriter, r *http.Request) {
	v, ok := s.loadView(w, r)
	if !ok {
		return
	}

	inUse := v.fleet.NamespacesInUse()

	seen := map[string]bool{}
	list := api.NamespaceList{Namespaces: []api.NamespaceInfo{}}
	for _, name := range sortedKeys(v.fleet.Namespaces) {
		seen[name] = true
		list.Namespaces = append(list.Namespaces, api.NamespaceInfo{
			Namespace: v.fleet.Namespace(name),
			Requests:  inUse[name],
		})
	}
	for _, name := range sortedKeys(inUse) {
		if seen[name] {
			continue
		}
		list.Namespaces = append(list.Namespaces, api.NamespaceInfo{
			Namespace: v.fleet.Namespace(name),
			Requests:  inUse[name],
		})
	}
	writeJSON(w, http.StatusOK, list)
}

// handleGetNamespace is GET /v1/namespaces/{ns}.
//
// It answers for [api.DefaultNamespace] whether or not a record exists. That namespace cannot be
// deleted and every request that named none is in it, so reporting it as missing would be a 404
// for something that demonstrably exists.
func (s *Server) handleGetNamespace(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("ns")

	v, ok := s.loadView(w, r)
	if !ok {
		return
	}

	_, stored := v.fleet.Namespaces[name]
	inUse := v.fleet.NamespacesInUse()
	if !stored && inUse[name] == 0 && name != api.DefaultNamespace {
		writeError(w, http.StatusNotFound, api.CodeNotFound, "no namespace "+name)
		return
	}

	writeJSON(w, http.StatusOK, api.NamespaceInfo{
		Namespace: v.fleet.Namespace(name),
		Requests:  inUse[name],
	})
}

// handleDeleteNamespace is DELETE /v1/namespaces/{ns}.
//
// **Refused while any request references it, with the count in the message.** The system never
// cancels intent on the user's behalf (§11), and a cascading delete here is a cascading teardown
// of live media. [api.DefaultNamespace] is not deletable at all: it is where every request that
// named no namespace lives, so removing it would make the catch-all a dangling reference.
func (s *Server) handleDeleteNamespace(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("ns")
	ctx := r.Context()

	if name == api.DefaultNamespace {
		writeError(w, http.StatusConflict, api.CodeInvalidRequest,
			fmt.Sprintf("the %q namespace cannot be deleted", api.DefaultNamespace))
		return
	}

	v, ok := s.loadView(w, r)
	if !ok {
		return
	}
	if count := v.fleet.NamespacesInUse()[name]; count > 0 {
		writeError(w, http.StatusConflict, api.CodeInvalidRequest,
			fmt.Sprintf("namespace %q still has %d request(s); cancel them first", name, count))
		return
	}

	existing, stored := v.fleet.Namespaces[name]
	if !stored {
		writeError(w, http.StatusNotFound, api.CodeNotFound, "no namespace "+name)
		return
	}

	if _, err := s.store.Delete(ctx, store.NamespaceKey(name), store.IfRevision(existing.Rev)); err != nil {
		storeError(w, s.logger, "delete namespace", err)
		return
	}

	s.logger.Info("namespace deleted", "namespace", name)
	w.WriteHeader(http.StatusNoContent)
}

// namespaceInfo renders a namespace with its refcount, re-reading it so the count is right even
// for a namespace this request just created.
func (s *Server) namespaceInfo(r *http.Request, spec api.Namespace) api.NamespaceInfo {
	info := api.NamespaceInfo{Namespace: spec}
	fleet, err := state.Load(r.Context(), s.store)
	if err == nil {
		info.Requests = fleet.NamespacesInUse()[spec.Name]
	}
	return info
}
