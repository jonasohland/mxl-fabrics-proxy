package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

func newClient(t *testing.T, servers ...string) *Client {
	t.Helper()
	c, err := New(Options{Servers: servers, RequestTimeout: 2 * time.Second})
	require.NoError(t, err)
	return c
}

func TestNewRejectsUnusableURLs(t *testing.T) {
	_, err := New(Options{})
	assert.ErrorContains(t, err, "no server URL")

	_, err = New(Options{Servers: []string{"ctrl:2283"}})
	assert.ErrorContains(t, err, "http or https")

	_, err = New(Options{Servers: []string{"http://"}})
	assert.ErrorContains(t, err, "no host")

	// A trailing slash must not survive into every path, or every request is a double slash.
	c, err := New(Options{Servers: []string{"http://ctrl:2283/"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"http://ctrl:2283"}, c.Servers())
}

func TestRegisterRoundTrip(t *testing.T) {
	var got api.NodeRegistration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, api.PathRegister, r.URL.Path)
		assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		writeJSON(t, w, http.StatusOK, api.RegistrationResponse{
			Lease:             "7",
			TTL:               api.Millis(15 * time.Second),
			HeartbeatInterval: api.Millis(5 * time.Second),
		})
	}))
	defer srv.Close()

	c, err := New(Options{Servers: []string{srv.URL}, Token: "secret"})
	require.NoError(t, err)

	resp, err := c.Register(t.Context(), api.NodeRegistration{Node: "edge-01", Instance: "i-1"})
	require.NoError(t, err)
	assert.Equal(t, "7", resp.Lease)
	assert.Equal(t, 5*time.Second, resp.HeartbeatInterval.Duration())
	assert.Equal(t, "edge-01", got.Node)
}

// A node name is operator-assigned free-form text, validated for uniqueness and not for URL
// safety, so it has to survive the trip as the name it is.
func TestNodeNameIsEscapedInPaths(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL)
	require.NoError(t, c.ReportInventory(t.Context(), api.InventorySnapshot{Node: "rack/1"}))
	assert.Equal(t, "/agent/v1/rack%2F1/inventory", path)
}

func TestTypedErrorsAreClassified(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   api.Error
		check  func(error) bool
	}{
		{"not ready", http.StatusServiceUnavailable, api.Error{Code: api.CodeNotReady, Message: "settling"}, IsNotReady},
		{"reregister", http.StatusConflict, api.Error{Code: api.CodeReregister, Message: "no lease"}, IsReregister},
		{"claimed", http.StatusConflict, api.Error{Code: api.CodeNodeClaimed, Details: map[string]string{"holder": "i-2"}}, IsNodeClaimed},
		{"skew", http.StatusBadRequest, api.Error{Code: api.CodeVersionSkew}, IsVersionSkew},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, tc.status, tc.body)
			}))
			defer srv.Close()

			_, err := newClient(t, srv.URL).Heartbeat(t.Context(), api.Heartbeat{Node: "edge-01"})
			require.Error(t, err)
			assert.True(t, tc.check(err), "expected %s to be classified", tc.body.Code)
			assert.Equal(t, tc.body.Code, Code(err))
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusConflict, api.Error{Code: api.CodeNodeClaimed, Details: map[string]string{"holder": "i-2"}})
	}))
	defer srv.Close()

	_, err := newClient(t, srv.URL).Heartbeat(t.Context(), api.Heartbeat{Node: "edge-01"})
	assert.Equal(t, "i-2", Detail(err, "holder"))
}

// An intermediary that is not this project still has to arrive as a typed error with its status
// intact, rather than as a decode failure that hides which one it was.
func TestUntypedErrorBodiesStillClassify(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("<html><body>502 from the proxy</body></html>"))
	}))
	defer srv.Close()

	_, err := newClient(t, srv.URL).Assignments(t.Context(), "edge-01", 0, 0)
	require.Error(t, err)
	assert.True(t, IsNotReady(err), "a bare 503 must read as not-ready, not as an answer")

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.Status)
}

// The property everything in §4.2 rests on: a poll that fails yields no set at all, so no caller
// can reconcile against one it did not receive.
func TestAssignmentsNeverYieldsASetOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusServiceUnavailable, api.Error{
			Code:    api.CodeNotReady,
			Message: "the reconciler has not settled",
		})
	}))
	defer srv.Close()

	set, err := newClient(t, srv.URL).Assignments(t.Context(), "edge-01", 0, 0)
	assert.Nil(t, set, "not-ready must not be representable as an empty set")
	require.Error(t, err)
	assert.True(t, IsNotReady(err))
}

func TestAssignmentsSendsCursorAndWait(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		writeJSON(t, w, http.StatusOK, api.AssignmentSet{Node: "edge-01", Revision: 42, Assignments: []api.Assignment{}})
	}))
	defer srv.Close()

	set, err := newClient(t, srv.URL).Assignments(t.Context(), "edge-01", 41, 20*time.Second)
	require.NoError(t, err)
	assert.Equal(t, int64(42), set.Revision)
	assert.Empty(t, set.Assignments)
	assert.Contains(t, query, "rev=41")
	assert.Contains(t, query, "wait=20")

	// No cursor on the first poll: the server answers immediately rather than holding.
	_, err = newClient(t, srv.URL).Assignments(t.Context(), "edge-01", 0, 0)
	require.NoError(t, err)
	assert.Empty(t, query)
}

// Belt and braces against plan §4.5: the server should already refuse, and a client that
// accepted it anyway would walk backwards through two assignment versions and restart workers on
// every swing.
func TestAssignmentsRefusesARevisionThatWentBackwards(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, api.AssignmentSet{Node: "edge-01", Revision: 7})
	}))
	defer srv.Close()

	set, err := newClient(t, srv.URL).Assignments(t.Context(), "edge-01", 9, 0)
	assert.Nil(t, set)
	assert.ErrorContains(t, err, "behind the cursor")
}

func TestAssignmentsRefusesAnswersForAnotherNode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, api.AssignmentSet{Node: "edge-02", Revision: 1})
	}))
	defer srv.Close()

	_, err := newClient(t, srv.URL).Assignments(t.Context(), "edge-01", 0, 0)
	assert.ErrorContains(t, err, "edge-02")
}

// Transport failures move to the next replica; a typed answer does not, because every replica
// shares one store and would give the same one.
func TestFailoverOnTransportFailureOnly(t *testing.T) {
	var good atomic.Int64
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		good.Add(1)
		writeJSON(t, w, http.StatusOK, api.AssignmentSet{Node: "edge-01", Revision: 3})
	}))
	defer ok.Close()

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	c := newClient(t, deadURL, ok.URL)
	set, err := c.Assignments(t.Context(), "edge-01", 0, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(3), set.Revision)
	assert.Equal(t, int64(1), good.Load())

	// Sticky: the working replica is tried first from now on, which is what keeps a revision
	// cursor meaningful across consecutive polls (plan §4.5).
	assert.Equal(t, ok.URL, c.server(0))

	var refusals atomic.Int64
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refusals.Add(1)
		writeJSON(t, w, http.StatusServiceUnavailable, api.Error{Code: api.CodeNotReady})
	}))
	defer refusing.Close()

	c = newClient(t, refusing.URL, ok.URL)
	_, err = c.Assignments(t.Context(), "edge-01", 0, 0)
	assert.True(t, IsNotReady(err))
	assert.Equal(t, int64(1), refusals.Load(), "a typed answer must not be retried elsewhere")
}

// Every attempt must have a body to send, or a retry against the second replica posts nothing.
func TestBodyIsResentOnFailover(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	var got api.InventorySnapshot
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()

	err := newClient(t, deadURL, ok.URL).ReportInventory(t.Context(), api.InventorySnapshot{
		Node:    "edge-01",
		Domains: []api.DomainInventory{{Name: "cameras", Configured: true}},
	})
	require.NoError(t, err)
	assert.Equal(t, "edge-01", got.Node)
	require.Len(t, got.Domains, 1)
	assert.Equal(t, "cameras", got.Domains[0].Name)
}

// The long poll is held open on purpose, so the transport deadline has to be derived from the
// wait rather than from a fixed client timeout.
func TestLongPollOutlivesTheDefaultTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writeJSON(t, w, http.StatusOK, api.AssignmentSet{Node: "edge-01", Revision: 1})
	}))
	defer srv.Close()

	c, err := New(Options{Servers: []string{srv.URL}, RequestTimeout: 50 * time.Millisecond})
	require.NoError(t, err)

	set, err := c.Assignments(t.Context(), "edge-01", 0, time.Second)
	require.NoError(t, err)
	assert.Equal(t, int64(1), set.Revision)

	// An ordinary call still gets the configured bound.
	_, err = c.Heartbeat(t.Context(), api.Heartbeat{Node: "edge-01"})
	assert.Error(t, err)
}

func TestCancelledContextIsNotRetried(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := newClient(t, srv.URL, srv.URL).Assignments(ctx, "edge-01", 0, time.Minute)
	assert.Error(t, err)
	assert.Equal(t, int64(1), calls.Load(), "a cancelled caller is not a reason to ask somebody else")
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.NewEncoder(w).Encode(body))
}
