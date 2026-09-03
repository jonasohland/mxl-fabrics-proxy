package server

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// The prefix vite writes its content-hashed bundles under. Everything below it is immutable by
// construction — the hash is in the filename — and everything outside it is either the index or
// something a hand-written link named.
const hashedAssetPrefix = "assets/"

// staticUI serves the embedded single-page app (`ui.md` §6).
//
// Three things about where this sits, all of them deliberate:
//
// **Outside [Server.authenticate].** The assets are not what the bearer token protects — the same
// credential opens the *agent* API, which is the privileged one (§13) — and a page that collects a
// credential cannot be behind the credential it collects. Where a token is configured, the
// recommended answer is still the proxy in front (or this server) injecting `Authorization` on the
// way through, so the browser never holds a fleet-wide credential at all — and the app never asks
// for one, because it asks only in response to a 401 it never sees in that deployment. Where nothing
// injects the header the app prompts and keeps the token in `localStorage`, which is what makes this
// route's placement load-bearing rather than merely tidy: that prompt is served from here.
//
// **Last in specificity.** It is registered as bare `/`, which `net/http`'s mux resolves against
// every other pattern before falling back here — so `/v1`, `/agent/v1`, `/healthz`, `/readyz` and
// `/metrics` are unreachable from this handler by construction, rather than by a prefix check that
// has to be kept in step with [Server.routes]. It cannot be `GET /`, which the mux rejects as
// conflicting with the method-less API prefixes (a more general path with fewer methods is neither
// more nor less specific), so the method check is here instead: everything but GET and HEAD is a
// 405, exactly as it would have been.
//
// **Index fallback, but not for assets.** The router uses history mode, so `/nodes/n1` is a URL
// the browser asks this server for and the answer is the index. That fallback stops at
// `assets/`: a miss there is a stale bundle reference, and answering it with HTML turns a plain
// 404 into a MIME-type error in the console that says nothing about what is wrong.
func staticUI(assets fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, api.CodeInvalidRequest, "method not allowed")
			return
		}

		name := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))

		// `..` cannot escape an fs.FS — Open rejects it — but it can name a path that does not
		// exist, and the answer to that is the same as any other miss.
		if name == "." || name == "/" || !fs.ValidPath(name) {
			serveIndex(w, r, assets)
			return
		}

		info, err := fs.Stat(assets, name)
		if err != nil || info.IsDir() {
			if strings.HasPrefix(name, hashedAssetPrefix) {
				http.NotFound(w, r)
				return
			}
			serveIndex(w, r, assets)
			return
		}

		// The one place in this server where a response is cacheable, and it is safe for the
		// reason the rest of the API is not: the content hash is in the filename, so a changed
		// file is a changed URL and there is nothing for a cached copy to go stale against. It
		// overrides the blanket `no-store` [noStore] set on the way in — see that function for why
		// the blanket rule exists.
		if strings.HasPrefix(name, hashedAssetPrefix) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}

		http.ServeFileFS(w, r, assets, name)
	})
}

// serveIndex answers with the app's entry point.
//
// It keeps the mux-wide `no-store`: the index names the hashed bundles, so a cached one is a
// browser pinned to the previous deployment's JavaScript for as long as its copy lives. Status is
// 200 rather than 404 even where the client-side router has no such route — the router owns that
// decision and renders its own answer.
func serveIndex(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	http.ServeFileFS(w, r, assets, "index.html")
}
