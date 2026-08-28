package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/jonasohland/mxl-replicator/internal/client"
)

// ClientFlags is what every verb that talks to the user API needs (§9.1).
//
// It reuses [AuthFlags] rather than growing a second, differently-spelled credential surface: an
// operator who has configured a token for the daemon should not have to learn a second flag name
// to use it from the command line.
type ClientFlags struct {
	Server []string `help:"URL of the mxl-replicator server. Repeatable for an HA deployment." default:"http://127.0.0.1:2283" env:"MXL_REPLICATOR_SERVER"`

	AuthFlags `embed:""`
}

func (c ClientFlags) client() (*client.Client, error) {
	token, err := c.Token()
	if err != nil {
		return nil, err
	}
	// Quiet by default: these are foreground commands whose output is the answer, and the
	// client's debug chatter about server failover would land in the middle of it.
	return client.New(client.Options{
		Servers: c.Server,
		Token:   token,
		Logger:  slog.New(slog.DiscardHandler),
	})
}

// OutputFlags selects how a read verb renders.
//
// `text` is for a human reading a terminal; `json` and `yaml` are for everything else. The API
// shapes are the source of truth in both machine formats — no reshaping, so a script written
// against `-o json` is written against the documented API.
type OutputFlags struct {
	Output string `short:"o" help:"Output format." enum:"text,json,yaml" default:"text"`
}

// warn writes a diagnostic to stderr, keeping stdout clean for the answer. The split matters
// here for the same reason it does in the worker: a caller parsing `-o json` must not have to
// filter warnings out of the document.
func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mxl-replicator: "+format+"\n", args...)
}
