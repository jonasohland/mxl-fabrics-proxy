// Package version reports the build identity of the mxl-replicator binary.
//
// Versions are reported to the server at agent registration (§10.2, §13.1): the server
// surfaces the fleet's version spread and refuses an agent newer than itself, because that
// is the one direction the "server is always upgraded first" promise does not cover.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is set at link time from `git describe --tags`. When it is empty the value is
// recovered from the module's build info, so a `go install` of this module still reports
// something meaningful.
var Version = ""

// Info is the build identity of this binary.
type Info struct {
	// Version is a semver-ish release identifier, e.g. "0.0.4-4-g02eef87".
	Version string `json:"version"`
	// Revision is the VCS commit, when the build recorded one.
	Revision string `json:"revision,omitempty"`
	// Modified reports whether the working tree was dirty at build time.
	Modified bool `json:"modified,omitempty"`
	// Go is the toolchain version the binary was built with.
	Go string `json:"go,omitempty"`
}

// Get returns the build identity, preferring the link-time Version and falling back to the
// embedded build info.
func Get() Info {
	info := Info{Version: Version}

	bi, ok := debug.ReadBuildInfo()
	if !ok {
		if info.Version == "" {
			info.Version = "unknown"
		}
		return info
	}

	info.Go = bi.GoVersion

	for _, setting := range bi.Settings {
		switch setting.Key {
		case "vcs.revision":
			info.Revision = setting.Value
		case "vcs.modified":
			info.Modified = setting.Value == "true"
		}
	}

	if info.Version == "" {
		// bi.Main.Version is "(devel)" for a plain `go build` and the module version for a
		// `go install module@version`. Either is more useful than an empty string.
		info.Version = bi.Main.Version
	}
	if info.Version == "" {
		info.Version = "unknown"
	}

	return info
}

// String renders the build identity as a single line, suitable for `--version`.
func String() string {
	info := Get()

	var b strings.Builder
	b.WriteString(info.Version)

	if info.Revision != "" {
		b.WriteString(" (")
		b.WriteString(info.Revision)
		if info.Modified {
			b.WriteString("-dirty")
		}
		b.WriteString(")")
	}
	if info.Go != "" {
		b.WriteString(" ")
		b.WriteString(info.Go)
	}

	return b.String()
}
