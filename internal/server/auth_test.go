package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// §13 asks for the identity authentication establishes to be carried on the request context, so
// that per-node credentials or mTLS can be added later without touching a handler. This is that
// seam, and the test is what keeps it wired while nothing downstream reads it yet.
func TestAuthenticateCarriesIdentity(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		token string
		want  bool
	}{
		{"with a token configured", "s3cret", true},
		// No-auth is a supported configuration, and the identity records that the request was
		// *not* proven rather than pretending it was.
		{"with authentication disabled", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{token: tc.token}

			var seen identity
			var found bool
			handler := s.authenticate(surfaceAgent, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen, found = identityFrom(r.Context())
			}))

			req := httptest.NewRequest(http.MethodGet, "/agent/v1/x/assignments", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			require.Equal(t, http.StatusOK, recorder.Code)
			require.True(t, found, "the middleware must put an identity on the context")
			assert.Equal(t, surfaceAgent, seen.Surface)
			assert.Equal(t, tc.want, seen.Authenticated)
		})
	}

	// A malformed header is not a token: "Bearer" with nothing after it, and a scheme this
	// server does not speak, both fail rather than falling through to an empty comparison.
	s := &Server{token: "s3cret"}
	handler := s.authenticate(surfaceUser, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("the handler must not run")
	}))
	for _, header := range []string{"", "Bearer", "Bearer ", "Basic s3cret", "s3cret"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/requests", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code, "header %q", header)
		assert.Equal(t, "Bearer", recorder.Header().Get("WWW-Authenticate"))
	}
}
