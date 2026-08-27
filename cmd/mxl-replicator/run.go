package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
)

// RunCmd is the only command. It starts the server role, the agent role, or both.
//
// With neither flag both roles run; naming one selects it alone:
//
//	mxl-replicator run                       server + agent
//	mxl-replicator run --server --agent      server + agent, said explicitly
//	mxl-replicator run --agent               agent only  — every ordinary fleet node
//	mxl-replicator run --server              server only — a control-plane-only node
//
// The server role is always a **full server**, never a cut-down local one: it binds the
// configured address, serves the complete user and agent APIs, and other nodes' agents
// register with it exactly as they would with a dedicated one. Deployments in scope:
//
//   - Non-HA fleet: one node runs both roles and hosts the control plane alongside its own
//     agent; every other node runs --agent. This is a production shape, not a toy.
//   - HA fleet: several nodes run both roles against a shared etcd, one of them elected
//     reconciler leader. See the risks recorded in rewrite-plan.md §4 before choosing this.
//   - Dedicated server: one or more nodes run --server, everything else --agent.
//   - Single-host and development: the default, both roles.
//
// When both roles run in one process they still speak HTTP to each other over the loopback
// interface — there is deliberately **no** in-memory short-circuit, so there is exactly one
// code path through the API and the combined form cannot develop behaviour the distributed
// form lacks (§2.2).
type RunCmd struct {
	// Role selection. Naming a role selects it *alone*; naming neither runs both. Note the
	// consequence, which the help text has to make unmissable: --server is not "also enable
	// the server", it is "server only".
	Server bool `help:"Run only the server role. With neither --server nor --agent, both roles run."`
	Agent  bool `help:"Run only the agent role. With neither --server nor --agent, both roles run."`

	ServerOpts ServerOptions `embed:"" prefix:"server-"`
	AgentOpts  AgentOptions  `embed:"" prefix:"agent-"`

	// nodeFromHostname records that --agent-node was defaulted, so Run can say so. In a
	// fleet the node name is operator-assigned and a silently defaulted one is worth a
	// warning, even though the hostname is a sound default.
	nodeFromHostname bool
}

// roles reports which roles this invocation runs. Naming neither runs both, which is what
// makes `run` with no flags the single-host and development case as well as the
// both-roles fleet node.
func (c *RunCmd) roles() (server, agent bool) {
	if !c.Server && !c.Agent {
		return true, true
	}
	return c.Server, c.Agent
}

// resolve fills in the defaults that depend on which roles are enabled, and is where the two
// halves are wired to each other.
func (c *RunCmd) resolve() error {
	server, agent := c.roles()
	if !agent {
		return nil
	}

	if c.AgentOpts.Node == "" {
		// The hostname is unique in practice and is what an operator would have typed. It
		// is still a default for a value that is operator-assigned fleet-wide (§3), so Run
		// warns when it is used.
		hostname, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("--agent-node not given and the hostname could not be determined: %w", err)
		}
		c.AgentOpts.Node = hostname
		c.nodeFromHostname = true
	}

	if server && len(c.AgentOpts.Server) == 0 {
		// The co-located agent dials its own server over loopback: no dependence on
		// external routing, and its server is guaranteed to be its own version, which
		// matters during a rolling upgrade of combined instances (§13.1).
		//
		// The cost is that this agent cannot fail over to another replica. In an HA
		// deployment, pass --agent-server explicitly with every replica (or the
		// load-balancer URL) to buy that back.
		if c.ServerOpts.TLS.Enabled() {
			return fmt.Errorf("--agent-server is required when the server terminates TLS: the co-located agent would dial the derived loopback URL, which a certificate issued for this node's routable name will not cover")
		}

		endpoint, err := loopbackURL(c.ServerOpts.Listen, false)
		if err != nil {
			return err
		}
		c.AgentOpts.Server = []string{endpoint}
	}

	return nil
}

// loopbackURL turns a listen address into a URL the local agent can dial. A wildcard or
// unspecified bind address becomes loopback, because that is the interface the co-located
// agent should use regardless of what else the server is reachable on.
func loopbackURL(listen string, tls bool) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("cannot derive an agent server URL from --server-listen %q: %w", listen, err)
	}
	if port == "" {
		return "", fmt.Errorf("cannot derive an agent server URL from --server-listen %q: no port", listen)
	}

	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}

	scheme := "http"
	if tls {
		scheme = "https"
	}

	return (&url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port)}).String(), nil
}

// Validate is called by kong before Run. The role option structs are embedded flag groups
// rather than command nodes of their own, so their Validate methods are invoked here — and
// only for the roles that are actually enabled, so an agent-only node is not asked to
// produce a valid store configuration.
func (c *RunCmd) Validate() error {
	if err := c.resolve(); err != nil {
		return err
	}

	server, agent := c.roles()

	if server {
		if err := c.ServerOpts.Validate(); err != nil {
			return err
		}
	}
	if agent {
		if err := c.AgentOpts.Validate(); err != nil {
			return err
		}
	}

	// Two listeners in one process. Catch the collision here rather than as a bind error
	// after one of the two halves has already started.
	if server && agent && c.ServerOpts.Listen == c.AgentOpts.Listen {
		return fmt.Errorf("--server-listen and --agent-listen are both %q; the API and the agent metrics endpoint need separate ports", c.ServerOpts.Listen)
	}

	return nil
}

func (c *RunCmd) Run(ctx context.Context, logger *slog.Logger) error {
	if c.nodeFromHostname {
		logger.Warn("node name defaulted to the hostname; node names are operator-assigned and must be unique fleet-wide", "node", c.AgentOpts.Node)
	}

	server, agent := c.roles()

	switch {
	case server && agent:
		logger.With("module", "run").Info("running both roles",
			"listen", c.ServerOpts.Listen,
			"node", c.AgentOpts.Node,
			"agent_server", c.AgentOpts.Server,
		)
		// Deliberately refused rather than half-started. Running the server alone under a
		// command that says it runs both roles would be a node that looks like it is
		// replicating and is not — and a control plane that appears healthy while no media
		// moves is the failure mode this project spends most of its design avoiding.
		return errNotImplemented{role: "agent", milestone: "M5"}
	case server:
		return c.ServerOpts.Run(ctx, logger)
	default:
		return c.AgentOpts.Run(ctx, logger)
	}
}
