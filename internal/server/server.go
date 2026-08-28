// Package server is the central connection management server (§7).
//
// Agents register with it, report the flows they observe and the workers they are running, and
// poll it for assignments. There is no agent-to-agent traffic on the control plane at all — the
// peer-to-peer flow fetch and keepalive of the legacy proxy are both gone, replaced by the
// server aggregating a fleet-wide inventory (§6).
//
// # Shape
//
// Every replica serves both HTTP surfaces; exactly one runs the reconciler, chosen by the
// [leader.Elector]. The API is stateless per request — it reads the store, decides, writes, and
// keeps nothing between calls — which is what lets several replicas sit behind a plain
// third-party HTTP proxy with no sticky sessions (§8.2).
//
// # The one rule to keep in mind while reading this package
//
// **"No assignments" and "I cannot answer" must never be spelled the same way.** An agent that
// receives an empty assignment set correctly tears down every worker it is running, so an empty
// set is only ever served once the reconciler has settled and its answer is a fact. Everything
// else — a fresh server, a leader change, a store restored from an empty backup, a replica whose
// view is behind the agent's cursor — is [api.CodeNotReady], which the agent treats exactly like
// a failed poll (§4.2, plan §4.2).
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/leader"
	"github.com/jonasohland/mxl-replicator/internal/server/reconcile"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
	"github.com/jonasohland/mxl-replicator/internal/store"
	"github.com/jonasohland/mxl-replicator/internal/version"
)

// Config configures a [Server].
type Config struct {
	// Store is the backing store. Required.
	Store store.Store

	// Elector decides which replica reconciles. Required; use [leader.Always] for a
	// single-process deployment.
	Elector leader.Elector

	Logger *slog.Logger

	// Listen is the address for both HTTP surfaces. TLSCert and TLSKey enable TLS here rather
	// than in a proxy in front; both or neither (§13).
	Listen  string
	TLSCert string
	TLSKey  string

	// Token is the shared bearer token. Empty disables authentication, which is a supported
	// configuration for a trusted network and for development (§13).
	Token string

	// HeartbeatInterval is how often agents are expected to renew their lease, and LeaseTTL how
	// long one survives without that. The agent takes its cadence from the registration
	// response, not from its own flags, so the settling window — expressed as a multiple of the
	// heartbeat — means the same thing on both sides (§7.3).
	HeartbeatInterval time.Duration
	LeaseTTL          time.Duration

	// SettlingHeartbeats sizes the settling window as a multiple of the heartbeat interval, so
	// it scales with the configured cadence rather than being an unrelated magic number (§7.3).
	SettlingHeartbeats int

	// MaxLongPollWait caps how long an assignment poll is held. It must stay below any
	// intermediate proxy's idle timeout: degrading to plain polling is acceptable, hanging is
	// not (§9.2).
	MaxLongPollWait time.Duration

	// Reconcile is the reconciler's policy — provider preference, idle timeouts, the matched
	// settings written into both ends of every session.
	Reconcile reconcile.Config

	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// Server serves both APIs and, while it holds leadership, runs the reconciler.
type Server struct {
	// store is not owned: the caller opened it and the caller closes it. A server that closed
	// the store it was handed would take the co-located agent's view down with it on a
	// combined-role node.
	store  store.Store
	logger *slog.Logger

	token       string
	listen      string
	tlsCert     string
	tlsKey      string
	heartbeat   time.Duration
	leaseTTL    time.Duration
	maxLongPoll time.Duration

	elector leader.Elector
	loop    *reconcile.Loop

	// readCfg is the reconciler policy used by the read-only handlers. It deliberately has no
	// idle tracker: a handler must render what the fleet is doing, not decide that a session the
	// leader is holding should have been torn down.
	readCfg reconcile.Config

	// registry is this server role's own, never the process default: a combined instance runs an
	// agent on a second listener with a second registry, and the two expose different things
	// (§4.7).
	registry *prometheus.Registry
	metrics  *controlMetrics

	now     func() time.Time
	handler http.Handler
}

// New builds a server. It does not listen or touch the store until [Server.Run].
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("server: no store")
	case cfg.Elector == nil:
		return nil, errors.New("server: no elector")
	case cfg.Listen == "":
		return nil, errors.New("server: no listen address")
	case (cfg.TLSCert == "") != (cfg.TLSKey == ""):
		return nil, errors.New("server: TLS needs both a certificate and a key")
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 5 * time.Second
	}
	if cfg.LeaseTTL <= cfg.HeartbeatInterval {
		return nil, fmt.Errorf("server: lease TTL (%s) must exceed the heartbeat interval (%s)", cfg.LeaseTTL, cfg.HeartbeatInterval)
	}
	if cfg.MaxLongPollWait <= 0 {
		cfg.MaxLongPollWait = 30 * time.Second
	}

	readCfg := cfg.Reconcile
	readCfg.Now = cfg.Now
	readCfg.Idle = nil

	// Timed at the seam rather than inside the backends, so sqlite and etcd are measured the same
	// way (§8.1). Both the handlers and the reconcile loop go through the wrapper.
	control := newControlMetrics()
	observed := observe(cfg.Store, control)

	s := &Server{
		store:       observed,
		logger:      cfg.Logger,
		token:       cfg.Token,
		listen:      cfg.Listen,
		tlsCert:     cfg.TLSCert,
		tlsKey:      cfg.TLSKey,
		heartbeat:   cfg.HeartbeatInterval,
		leaseTTL:    cfg.LeaseTTL,
		maxLongPoll: cfg.MaxLongPollWait,
		elector:     cfg.Elector,
		readCfg:     readCfg,
		metrics:     control,
		now:         cfg.Now,
	}

	s.loop = reconcile.NewLoop(reconcile.LoopOptions{
		Store:              observed,
		Config:             cfg.Reconcile,
		Logger:             cfg.Logger.With("module", "reconcile"),
		Leader:             cfg.Elector.Name(),
		Heartbeat:          cfg.HeartbeatInterval,
		SettlingHeartbeats: cfg.SettlingHeartbeats,
		Now:                cfg.Now,
		Hooks:              control.reconcileHooks(),
	})

	s.registerMetrics()
	s.handler = s.routes()
	return s, nil
}

// Handler is the HTTP handler, exposed for tests and for embedding.
func (s *Server) Handler() http.Handler { return s.handler }

// Run serves until ctx ends, campaigning for leadership alongside.
//
// The two are deliberately independent: a replica that is not the leader still serves every
// agent and every user request. Losing the store, or losing an election, does not stop this
// server answering — and it must not, because an agent that gets no answer holds its workers
// where they are (§4.2).
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.listen, err)
	}

	httpServer := &http.Server{
		Handler: s.handler,
		// No read/write timeouts: the assignment long poll holds a request open for as long as
		// MaxLongPollWait, and a write timeout shorter than that would cut it off mid-answer.
		// ReadHeaderTimeout still bounds the one phase a slow client can abuse.
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errs := make(chan error, 1)
	go func() {
		s.logger.Info("serving",
			"address", listener.Addr().String(),
			"tls", s.tlsCert != "",
			"auth", s.token != "",
			"leader_candidate", s.elector.Name())

		var err error
		if s.tlsCert != "" {
			err = httpServer.ServeTLS(listener, s.tlsCert, s.tlsKey)
		} else {
			err = httpServer.Serve(listener)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	elected := make(chan error, 1)
	go func() {
		// The reconciler runs only while this replica holds leadership, and its context is
		// cancelled the moment that ends. Every derived write is a CAS regardless, because a
		// partitioned leader believes it still leads until its lease expires (§4.6).
		elected <- s.elector.Run(ctx, func(ctx context.Context) error {
			// Counted here rather than in the loop because acquiring leadership is the elector's
			// event, and it is the one that matters: repeated increments are the leader churn §4.4
			// warns about, which is otherwise invisible — each individual handover reads like an
			// ordinary startup in the log.
			s.metrics.leaderChanges.Inc()
			return s.loop.Run(ctx)
		})
	}()

	select {
	case <-ctx.Done():
	case err := <-errs:
		if err != nil {
			return err
		}
	case err := <-elected:
		if err != nil {
			s.logger.Error("leader election failed", "error", err)
		}
	}

	// Stop serving before anything else. On a combined-role node the local agent is a client of
	// this server, and shutdown ordering matters: an expired lease is not proof that a node's
	// workers stopped, and nothing may reassign its sessions on that basis (plan §4.7).
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		s.logger.Warn("shutdown did not complete cleanly", "error", err)
	}

	<-errs
	return nil
}

// versions is what this server reports about itself.
func (s *Server) versions() api.Versions {
	return api.Versions{
		Protocol:   api.ProtocolVersion,
		Replicator: version.String(),
	}
}

// checkProtocol gates version skew on the **protocol** version, not the build version (§13.1,
// plan §4.1).
//
// The compatibility promise runs one way: the server is always upgraded first, so it must
// tolerate agents that are behind, and an agent may assume the server is at least as new as it
// is. The one direction the promise does not cover is an agent *newer* than the server, and that
// is refused — but only on a protocol difference. Keying a hard refusal on the build version
// would turn any rolling upgrade of combined-role nodes into a partial outage, because upgrading
// a combined instance upgrades both roles at once and a newer agent reaching an older server is
// then unavoidable.
func (s *Server) checkProtocol(registration api.NodeRegistration) error {
	agent := registration.Capabilities.Versions.Protocol
	if agent > api.ProtocolVersion {
		return fmt.Errorf("agent speaks protocol version %d and this server speaks %d", agent, api.ProtocolVersion)
	}
	return nil
}

// validateRegistration checks the shape of a registration.
//
// Node names are validated for being usable at all, not for uniqueness — uniqueness is enforced
// by the lease being exclusive, which is the only place it can be (§7.1).
func validateRegistration(registration api.NodeRegistration) error {
	if err := validateName("node", registration.Node); err != nil {
		return err
	}
	if registration.Instance == "" {
		return errors.New("instance is required")
	}
	for i, attachment := range registration.Capabilities.Fabrics {
		if attachment.Provider == "" {
			return fmt.Errorf("capabilities.fabrics[%d]: provider is required", i)
		}
		if attachment.Fabric == "" {
			return fmt.Errorf("capabilities.fabrics[%d]: fabric label is required", i)
		}
	}
	for i, domain := range registration.Domains {
		if domain.Name == "" {
			return fmt.Errorf("domains[%d]: name is required", i)
		}
	}
	return nil
}

// maxNameLength bounds node names and request IDs alike: 253, the DNS-subdomain limit
// Kubernetes object names use, which is the shape the adapter on the roadmap will hand us.
const maxNameLength = 253

func validateName(what, name string) error {
	switch {
	case name == "":
		return errors.New(what + " is required")
	case len(name) > maxNameLength:
		return fmt.Errorf("%s is longer than %d characters", what, maxNameLength)
	case strings.TrimSpace(name) != name:
		return fmt.Errorf("%s must not have leading or trailing whitespace", what)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s must not contain control characters", what)
		}
	}
	return nil
}

// canonicaliseSHM rewrites shm attachments' fabric labels to the derivation both sides share
// (§10.1).
//
// The label is what makes shm structurally same-node-only, and it only works if every node
// derives the identical string. Doing it here means an agent that spelled it differently — an
// older build, a hand-written config — still matches itself rather than silently failing to pair
// with its own other domain.
func canonicaliseSHM(registration *api.NodeRegistration) {
	for i := range registration.Capabilities.Fabrics {
		if registration.Capabilities.Fabrics[i].Provider == api.ProviderSHM {
			registration.Capabilities.Fabrics[i].Fabric = api.SHMFabric(registration.Node)
		}
	}
}

// sameCapabilities reports whether a re-registration says anything new.
func sameCapabilities(existing, candidate state.NodeRecord) bool {
	return equalJSON(existing.Capabilities, candidate.Capabilities) &&
		equalJSON(existing.Domains, candidate.Domains)
}

// equalJSON compares two values by their encoded form.
//
// Used to decide whether a re-registration changed anything, which is a compare-before-write:
// the store advances its revision for every write including an identical one, and every revision
// wakes every watcher (§8.3).
func equalJSON(a, b any) bool {
	left, errLeft := json.Marshal(a)
	right, errRight := json.Marshal(b)
	if errLeft != nil || errRight != nil {
		return false
	}
	return bytes.Equal(left, right)
}

// ReplicaName builds a unique name for this process, for leader election and the reconciler
// record.
//
// Hostname plus a random suffix: the hostname alone is not unique (two replicas can share one in
// a container image, and a restarted pod reuses it), and two replicas campaigning under one
// identity is an election that cannot settle.
func ReplicaName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "replica"
	}
	return host + "-" + rand.Text()[:8]
}
