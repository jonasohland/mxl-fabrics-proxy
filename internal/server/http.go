package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/metrics"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
	"github.com/jonasohland/mxl-replicator/internal/store"
)

// routes builds the mux.
//
// The two APIs live under distinct prefixes because they have different auth, different clients,
// different rate profiles and different compatibility guarantees — and because an operator must
// be able to expose one at an ingress without exposing the other. Of the two the **agent** API
// is the privileged one: anything that can call it can claim to be a node, inject fabricated
// inventory, and read other nodes' target_info, which is a set of RDMA rkeys (§9, §13).
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated, and deliberately so: these are for a load balancer, a kubelet and a
	// Prometheus, none of which is going to carry a bearer token.
	//
	// /metrics discloses counts and states, node names and build versions — no domain paths, no
	// flow definitions and no target_info, which is the set of RDMA rkeys that makes the agent API
	// the privileged one (§13). An operator who wants it closed anyway puts it behind the same
	// ingress rule as everything else; the token is not the mechanism for that.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.Handle("GET /metrics", metrics.Handler(s.registry, s.logger.With("module", "metrics")))

	agent := http.NewServeMux()
	agent.HandleFunc("POST "+api.PathRegister, s.handleRegister)
	agent.HandleFunc("POST "+api.AgentPrefix+"/{node}/heartbeat", s.handleHeartbeat)
	agent.HandleFunc("POST "+api.AgentPrefix+"/{node}/inventory", s.handleInventory)
	agent.HandleFunc("POST "+api.AgentPrefix+"/{node}/status", s.handleStatus)
	agent.HandleFunc("GET "+api.AgentPrefix+"/{node}/assignments", s.handleAssignments)

	// Requests live under their namespace, because `(namespace, name)` is a request's ID (§9.3).
	// `GET /v1/requests` stays as the fleet-wide list, narrowable with `?namespace=`: a list has
	// to be readable without already knowing which partitions exist.
	user := http.NewServeMux()
	user.HandleFunc("POST "+api.PathNamespaces, s.handleCreateNamespace)
	user.HandleFunc("GET "+api.PathNamespaces, s.handleListNamespaces)
	user.HandleFunc("GET "+api.PathNamespaces+"/{ns}", s.handleGetNamespace)
	user.HandleFunc("DELETE "+api.PathNamespaces+"/{ns}", s.handleDeleteNamespace)
	user.HandleFunc("POST "+api.PathNamespaces+"/{ns}/requests", s.handleCreateRequest)
	user.HandleFunc("GET "+api.PathNamespaces+"/{ns}/requests", s.handleListNamespaceRequests)
	user.HandleFunc("GET "+api.PathNamespaces+"/{ns}/requests/{name}", s.handleGetRequest)
	user.HandleFunc("DELETE "+api.PathNamespaces+"/{ns}/requests/{name}", s.handleDeleteRequest)
	user.HandleFunc("GET "+api.PathRequests, s.handleListRequests)
	user.HandleFunc("GET "+api.PathNodes, s.handleListNodes)
	user.HandleFunc("GET "+api.PathNodes+"/{node}/domains", s.handleNodeDomains)
	user.HandleFunc("POST "+api.PathNodes+"/{node}/domains", s.handleLabelDomain)
	user.HandleFunc("GET "+api.PathFlows, s.handleFlows)
	user.HandleFunc("GET "+api.PathPaths, s.handlePaths)

	// One middleware per surface, so that adding per-node credentials or mTLS to the agent API
	// later does not touch the user API — or any handler (§13).
	mux.Handle(api.AgentPrefix+"/", s.authenticate(surfaceAgent, agent))
	mux.Handle(api.UserPrefix+"/", s.authenticate(surfaceUser, user))

	return s.recoverPanics(noStore(mux))
}

// noStore marks every response this server produces as uncacheable.
//
// **Nothing this API returns is ever cacheable, and there are no exceptions to police.** Every
// response is either live fleet state or the result of an operation, and the health endpoints are
// the liveness of *this process* — a cached `/readyz` is a load balancer routing to a replica that
// said it was not settled (§7.3). Stating it once for the whole mux is therefore both correct and
// the cheapest thing to be sure of.
//
// It exists for one response in particular. `GET /agent/v1/{node}/assignments` is a GET with a
// cursor in its query string, and an intermediary that caches it — CloudFront's default cache
// policy will, since nothing here previously said otherwise — hands an agent a stale assignment
// set. That is the one failure fail-static cannot defend against: §4.2 protects an agent from *no
// answer*, not from a successfully-retrieved wrong one, so the set is acted on. If it carries a
// stale epoch, the result is §5.2's worst case — an initiator running against rkeys that no longer
// exist, moving no data, with nothing anywhere reporting an error.
//
// `no-store` rather than `no-cache`: the latter permits storing the response and revalidating,
// which is a correct-but-useless distinction here and one more thing for an intermediary to get
// subtly wrong. It is set before the handler runs so that it covers error responses and panics
// too, and a handler that wanted to override it still can.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// recoverPanics keeps one bad request from taking the process down.
//
// net/http already recovers panics in handlers; this exists to log them with the server's own
// logger and to return a JSON error body, so a client sees the same error shape it sees for
// everything else rather than a bare connection reset.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil && recovered != http.ErrAbortHandler {
				s.logger.Error("panic in handler", "method", r.Method, "path", r.URL.Path, "panic", recovered)
				writeError(w, http.StatusInternalServerError, api.CodeInternal, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Liveness is deliberately not a fleet health check. The legacy proxy had the right instinct
	// here — its health endpoint stays green when a transfer fails, because a peer being
	// unreachable is no reason to restart and drop every other flow (§11).
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	record, err := state.Get[state.ReconcilerRecord](r.Context(), s.store, store.KeyReconciler)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, api.CodeInternal, "store unreachable: "+err.Error())
		return
	}
	if !record.Found || !record.Value.Settled {
		// Readiness is a property of the *fleet's* control plane, not of this process: a
		// follower serves assignments too, and it must not report ready while the answer it
		// would give is "I don't know yet" (§7.3, plan §4.2).
		writeError(w, http.StatusServiceUnavailable, api.CodeNotReady, "the reconciler has not settled")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "leader": record.Value.Leader})
}

// --- helpers -----------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func writeError(w http.ResponseWriter, status int, code api.ErrorCode, message string, details ...string) {
	body := api.Error{Code: code, Message: message}
	if len(details) >= 2 {
		body.Details = map[string]string{}
		for i := 0; i+1 < len(details); i += 2 {
			body.Details[details[i]] = details[i+1]
		}
	}
	writeJSON(w, status, body)
}

// maxBodyBytes bounds a request body. Inventory from a node with many flows is the largest thing
// on either API and is nowhere near this; the limit exists so an unauthenticated peer cannot
// make the server allocate without bound.
const maxBodyBytes = 8 << 20

func decodeBody[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var body T

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, "cannot decode request body: "+err.Error())
		return body, false
	}
	return body, true
}

// storeError maps a store failure onto a response.
//
// Everything unexpected is a 503 rather than a 500: the server itself is fine, its store is not,
// and the distinction is what tells an operator which thing to look at. It is also what an agent
// should retry against.
func storeError(w http.ResponseWriter, logger *slog.Logger, what string, err error) {
	if errors.Is(err, store.ErrCompareFailed) {
		writeError(w, http.StatusConflict, api.CodeInternal, what+": concurrent update, retry")
		return
	}
	logger.Error("store operation failed", "operation", what, "error", err)
	writeError(w, http.StatusServiceUnavailable, api.CodeInternal, what+": "+err.Error())
}

// queryDuration reads a bounded, non-negative number of seconds from the query string.
func queryDuration(r *http.Request, name string, def, maximum time.Duration) time.Duration {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds < 0 {
		return def
	}
	return min(time.Duration(seconds*float64(time.Second)), maximum)
}

func queryInt64(r *http.Request, name string) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", name, raw)
	}
	return value, nil
}
