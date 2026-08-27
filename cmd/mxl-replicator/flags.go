package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// AuthFlags is the §13 v1 auth surface: a single optional shared bearer token.
//
// No-auth is a supported configuration for a trusted network and for development, so an
// empty token is not an error. The threat model that makes turning it on the right default
// outside a trusted network is recorded in §13: the agent API can claim to be a node and
// read other nodes' target_info (a set of RDMA rkeys), and the user API is a fleet-wide
// bandwidth exhaustion primitive.
type AuthFlags struct {
	AuthToken     string `help:"Shared bearer token. Prefer --auth-token-file: a token on the command line is visible in the process table." env:"MXL_REPLICATOR_AUTH_TOKEN"`
	AuthTokenFile string `help:"File to read the shared bearer token from." type:"existingfile"`
}

// Token resolves the configured token, reading the file if one was given. An empty return
// value with a nil error means authentication is disabled.
func (a AuthFlags) Token() (string, error) {
	if a.AuthToken != "" && a.AuthTokenFile != "" {
		return "", fmt.Errorf("--auth-token and --auth-token-file are mutually exclusive")
	}
	if a.AuthTokenFile == "" {
		return a.AuthToken, nil
	}

	raw, err := os.ReadFile(a.AuthTokenFile)
	if err != nil {
		return "", fmt.Errorf("read auth token file: %w", err)
	}

	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("auth token file %q is empty", a.AuthTokenFile)
	}

	return token, nil
}

// StoreFlags selects and configures the storage backend (§8).
//
// The interface is defined in etcd's terms — a revisioned KV store with CAS, prefix watch
// and TTL leases — and implemented over sqlite. etcd is the HA backend; sqlite is the
// single-node one, and with sqlite the single process is always the leader (§8.2).
type StoreFlags struct {
	Backend       string   `help:"Storage backend. sqlite is single-node; etcd is required for HA." enum:"sqlite,etcd" default:"sqlite"`
	SqlitePath    string   `help:"Path to the sqlite database file." default:"/var/lib/mxl-replicator/store.db" type:"path"`
	EtcdEndpoints []string `help:"etcd endpoints, e.g. http://etcd-0:2379."`
	EtcdPrefix    string   `help:"Key prefix for all etcd keys owned by this cluster." default:"/mxl-replicator"`
}

// Validate rejects combinations that cannot work, so the failure is a startup error rather
// than a confusing runtime one.
func (s StoreFlags) Validate() error {
	switch s.Backend {
	case "etcd":
		if len(s.EtcdEndpoints) == 0 {
			return fmt.Errorf("--store-backend=etcd requires --store-etcd-endpoints")
		}
	case "sqlite":
		if s.SqlitePath == "" {
			return fmt.Errorf("--store-backend=sqlite requires --store-sqlite-path")
		}
	}
	return nil
}

// TLSFlags configures optional TLS termination on the server itself. Terminating in an HTTP
// proxy in front of the server is equally supported (§13).
type TLSFlags struct {
	Cert string `help:"TLS certificate file. TLS is enabled when both --tls-cert and --tls-key are given." type:"existingfile"`
	Key  string `help:"TLS private key file." type:"existingfile"`
}

// Enabled reports whether TLS should be terminated by the server.
func (t TLSFlags) Enabled() bool { return t.Cert != "" && t.Key != "" }

// Validate rejects a half-configured pair.
func (t TLSFlags) Validate() error {
	if (t.Cert == "") != (t.Key == "") {
		return fmt.Errorf("--tls-cert and --tls-key must be given together")
	}
	return nil
}

// IdleFlags is the two-tier idle policy of §11.1.
//
// The two knobs are not redundant: they trade resume latency against resource cost.
//
//	source idle for      state    workers                      resume cost
//	seconds to minutes   PAUSED   running, waiting patiently   immediate
//	beyond the threshold PAUSED   none                         one re-establish, 1-2s
//
// Tearing down too eagerly means a source that stops and starts frequently loses its first
// grains to a re-establish every time; never tearing down means dormant flows hold ports,
// memory registrations and processes indefinitely.
type IdleFlags struct {
	IdleTimeout  time.Duration `help:"Worker no-grain timeout. 0 waits indefinitely, which is what makes PAUSED a real steady state instead of a ~13s restart loop. Requires a worker with the configurable timeout." default:"0"`
	IdleTeardown time.Duration `help:"Stop both workers of a session whose source has been idle for this long. 0 disables teardown. Default generously in minutes, not seconds." default:"5m"`
}
