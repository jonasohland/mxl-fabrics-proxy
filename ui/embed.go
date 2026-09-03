// Package ui carries the built single-page app into the server binary (`ui.md` §6).
//
// The UI is **always same-origin with the API** — either served by this binary or served beside
// it behind a proxy that fronts both — so there is no API base URL to configure, every call the
// app makes is relative, and the server sends no `Access-Control-*` headers at all. This package
// is the first of those two shapes.
//
// It lives here, next to the app, because a `go:embed` pattern cannot reach outside its own
// package directory: `internal/server` cannot name `../../ui/app/dist`. What that package gets is
// an [fs.FS] and no knowledge of where the bytes came from.
package ui

import (
	"embed"
	"io/fs"
)

// The built app, as vite writes it (`ui/app/vite.config.ts` pins `outDir`).
//
// `all:` rather than a plain pattern so the directory matches even when the only thing in it is
// the tracked `.gitkeep`. That case is the whole reason this compiles at all: `ui/app/dist` is a
// build artefact and is not committed, and an embed of a directory that does not exist is a
// *compile* error — it would mean nobody could `go build ./...` without a working npm first. The
// placeholder keeps the Go build independent of the node one, and [Assets] reports which of the
// two a given binary ended up with.
//
//go:embed all:app/dist
var embedded embed.FS

// Assets returns the built app, rooted where `index.html` is. The second result is false in a
// binary built without running the UI build, which is the ordinary state of a `go build` and the
// reason `--server-ui` is a flag that can fail rather than a mode that is silently empty.
func Assets() (fs.FS, bool) {
	assets, err := fs.Sub(embedded, "app/dist")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		return nil, false
	}
	return assets, true
}
