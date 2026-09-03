package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// A stand-in for what vite writes: an index that names one content-hashed bundle. The hash in the
// filename is the whole of why `assets/` is cacheable and nothing else here is.
func builtApp() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                {Data: []byte(`<!doctype html><div id="app"></div>`)},
		"assets/index-CYU33cWT.js":  {Data: []byte(`export const app = 1`)},
		"assets/index-CYU33cWT.css": {Data: []byte(`:root{}`)},
		"favicon.svg":               {Data: []byte(`<svg/>`)},
	}
}

func withUI(c *Config) { c.UI = builtApp() }

// get fetches without the harness's JSON assumptions and without a token, which is the browser's
// position: the assets are outside authenticate, so nothing here should ever need one.
func (h *harness) get(t *testing.T, path string) response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.http.URL+path, nil)
	require.NoError(t, err)

	resp, err := h.http.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return response{status: resp.StatusCode, body: body, header: resp.Header}
}

// The router uses history mode, so every one of its routes is a URL the browser asks *this server*
// for on a reload or a pasted link. Each of them is the index; the app decides from there.
func TestTheUIIsServedWithAnIndexFallback(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withUI)

	for _, path := range []string{
		"/",
		"/nodes",
		"/ns/nab/grid",
		"/nodes/edge-01/domains/media/cameras", // several segments deep, and no such file
		"/favicon.svg",                         // a real file that is not under assets/
	} {
		resp := h.get(t, path)
		require.Equal(t, http.StatusOK, resp.status, "path %s", path)
	}

	assert.Contains(t, string(h.get(t, "/ns/nab/grid").body), `id="app"`)

	// A miss under assets/ is a stale bundle reference, and answering it with HTML turns a plain
	// 404 into a MIME-type error in the console that says nothing about what is wrong.
	assert.Equal(t, http.StatusNotFound, h.get(t, "/assets/index-00000000.js").status)

	// The mount is bare `/` rather than `GET /` — the mux refuses that beside the method-less API
	// prefixes — so the method check the pattern would have made is in the handler.
	post := h.do(http.MethodPost, "/", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, post.status)
	assert.Equal(t, "GET, HEAD", post.header.Get("Allow"))
}

// The index names the hashed bundles, so a cached index pins a browser to the previous
// deployment's JavaScript — while a hashed bundle cannot go stale at all, because a changed file
// is a changed URL. This is the only response this server marks cacheable.
func TestOnlyHashedAssetsAreCacheable(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withUI)

	bundle := h.get(t, "/assets/index-CYU33cWT.js")
	require.Equal(t, http.StatusOK, bundle.status)
	assert.Equal(t, "public, max-age=31536000, immutable", bundle.header.Get("Cache-Control"))
	assert.Contains(t, bundle.header.Get("Content-Type"), "javascript")

	for _, path := range []string{"/", "/nodes", "/favicon.svg"} {
		assert.Equal(t, "no-store", h.get(t, path).header.Get("Cache-Control"), "path %s", path)
	}
}

// The app is mounted last and matched last, so no route it might ever use can shadow an API one.
// The token is the other half of the same point: it guards the *agent* API too, so it cannot be
// what a page has to present to load (`ui.md` §6, §13).
func TestTheUIShadowsNoAPIRouteAndCarriesNoToken(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withUI, func(c *Config) { c.Token = "s3cret" })

	// Served to a browser holding nothing.
	assert.Equal(t, http.StatusOK, h.get(t, "/").status)
	assert.Equal(t, http.StatusOK, h.get(t, "/assets/index-CYU33cWT.js").status)

	// While the APIs underneath are unchanged: still guarded, still JSON, never the index.
	for _, path := range []string{api.PathRequests, api.AssignmentsPath("edge-01")} {
		resp := h.get(t, path)
		require.Equal(t, http.StatusUnauthorized, resp.status, "path %s", path)
		assert.Equal(t, api.CodeUnauthorized, resp.apiError(t).Code)
	}

	health := h.get(t, "/healthz")
	require.Equal(t, http.StatusOK, health.status)
	assert.Contains(t, health.header.Get("Content-Type"), "application/json")

	metrics := h.get(t, "/metrics")
	require.Equal(t, http.StatusOK, metrics.status)
	assert.True(t, strings.HasPrefix(string(metrics.body), "#"), "body: %s", metrics.body)

	// An unknown path *under* an API prefix belongs to that API, not to the app.
	assert.NotContains(t, string(h.get(t, "/v1/nonsense").body), `id="app"`)
}

// The default: a server behind a proxy that fronts the assets itself must not also answer for
// them, or the deployment has two copies of the app and one of them is whichever binary is oldest.
func TestWithoutTheUIRootIsNotServed(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	assert.Equal(t, http.StatusNotFound, h.get(t, "/").status)
	assert.Equal(t, http.StatusNotFound, h.get(t, "/nodes").status)
}
