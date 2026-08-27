package server

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// surface names which of the two APIs a request arrived on.
//
// Both are guarded by the same shared token in v1, but they are separate concepts and the
// identity a request establishes carries which one it came in on — so per-node credentials or
// mTLS on the agent API can be added later without touching a handler (§13).
type surface string

const (
	surfaceAgent surface = "agent"
	surfaceUser  surface = "user"
)

// identity is what authentication established about a request.
type identity struct {
	// Surface is which API this arrived on.
	Surface surface

	// Authenticated is false when no token is configured. No-auth is a supported configuration
	// for a trusted network and for development, and the field records that the request was
	// *not* proven rather than pretending it was (§13).
	Authenticated bool
}

type identityKey struct{}

// identityFrom returns what authentication established, if the request came through the
// middleware.
func identityFrom(ctx context.Context) (identity, bool) {
	value, ok := ctx.Value(identityKey{}).(identity)
	return value, ok
}

// authenticate checks the shared bearer token (§13).
//
// v1 is one optional shared token, configured on the server and on every agent, with TLS
// terminated either here or by a proxy in front. No mTLS: certificate distribution and rotation
// across a DaemonSet is a larger operational commitment than this project should take on before
// it has users asking for it.
//
// The threat model, so the deferral is a decision rather than an oversight: a shared token means
// any holder can impersonate *any* node, so per-node credentials are the first upgrade if that
// matters; and the user API is a fleet-wide bandwidth-exhaustion primitive, since a replication
// request moves uncompressed video between hosts. Note that the invariant that actually protects
// the filesystem — a destination domain must be an explicitly mapped name — holds regardless of
// what is configured here (§7.2, invariant 6).
func (s *Server) authenticate(on surface, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := identity{Surface: on}

		if s.token != "" {
			presented, ok := bearerToken(r)
			// Constant time, so the comparison does not leak the token a byte at a time to
			// anything that can measure the response.
			if !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, http.StatusUnauthorized, api.CodeUnauthorized, "a valid bearer token is required")
				return
			}
			who.Authenticated = true
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, who)))
	})
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	return strings.TrimSpace(value), true
}
