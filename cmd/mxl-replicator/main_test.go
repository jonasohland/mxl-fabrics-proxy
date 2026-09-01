package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

func parse(t *testing.T, args ...string) (*CLI, *kong.Context, error) {
	t.Helper()

	cli := &CLI{}
	parser, err := kong.New(cli,
		kong.Name("mxl-replicator"),
		kong.Vars{"version": "test"},
		kong.Exit(func(int) { t.Fatal("kong tried to exit the test process") }),
	)
	require.NoError(t, err)

	kctx, err := parser.Parse(args)
	return cli, kctx, err
}

func mustParse(t *testing.T, args ...string) (*CLI, *kong.Context) {
	t.Helper()

	cli, kctx, err := parse(t, args...)
	require.NoError(t, err)
	return cli, kctx
}

// agentOnly is the ordinary fleet node: agent role only, pointed at a remote control plane.
func agentOnly(extra ...string) []string {
	return append([]string{"run", "--agent", "--agent-node", "edge-01", "--agent-server", "http://ctrl:2283"}, extra...)
}

// One command; naming a role selects it alone, naming neither runs both. `run` itself is
// optional, since it is kong's default command.
func TestRoleSelection(t *testing.T) {
	cli, kctx := mustParse(t, "run")
	assert.Equal(t, "run", kctx.Command())
	server, agent := cli.Run.roles()
	assert.True(t, server)
	assert.True(t, agent, "neither flag given runs both roles")

	cli, _ = mustParse(t, agentOnly()...)
	server, agent = cli.Run.roles()
	assert.False(t, server, "--agent selects the agent role alone")
	assert.True(t, agent)

	cli, _ = mustParse(t, "run", "--server")
	server, agent = cli.Run.roles()
	assert.True(t, server)
	assert.False(t, agent, "--server selects the server role alone")

	// Both named is the same as neither: the explicit spelling of a both-roles node, and
	// the form §2.2 writes.
	cli, _ = mustParse(t, "run", "--server", "--agent", "--agent-node", "dev")
	server, agent = cli.Run.roles()
	assert.True(t, server)
	assert.True(t, agent)

	// The command word is optional.
	cli, _ = mustParse(t, "--server")
	server, agent = cli.Run.roles()
	assert.True(t, server)
	assert.False(t, agent)
}

func TestServerDefaults(t *testing.T) {
	cli, _ := mustParse(t, "run", "--server")
	server := cli.Run.ServerOpts

	assert.Equal(t, ":2283", server.Listen)
	assert.Equal(t, "sqlite", server.Store.Backend)
	assert.False(t, server.TLS.Enabled())
	// §10.4: EFA > Verbs > TCP > SHM, matching the priority the mxl demo tool uses.
	assert.Equal(t, []string{"efa", "verbs", "tcp", "shm"}, server.ProviderOrder)
	// §7.3: the settling window is a multiple of the heartbeat, not an absolute magic number.
	assert.Equal(t, 3, server.SettlingHeartbeats)
	assert.Equal(t, 3*server.HeartbeatInterval, server.SettlingWindow())
}

func TestServerValidation(t *testing.T) {
	// A lease that expires between heartbeats would expire every agent in the fleet.
	_, _, err := parse(t, "run", "--server", "--server-heartbeat-interval", "10s", "--server-lease-ttl", "5s")
	assert.ErrorContains(t, err, "lease-ttl")

	// etcd without endpoints cannot work; fail at startup, not at the first store call.
	_, _, err = parse(t, "run", "--server", "--server-store-backend", "etcd")
	assert.ErrorContains(t, err, "endpoints")

	_, _, err = parse(t, "run", "--server", "--server-provider-order", "verbs,carrier-pigeon")
	assert.ErrorContains(t, err, "carrier-pigeon")

	// Half-configured TLS is a misconfiguration, not "TLS off".
	dir := t.TempDir()
	cert := filepath.Join(dir, "tls.crt")
	require.NoError(t, os.WriteFile(cert, []byte("x"), 0o600))

	_, _, err = parse(t, "run", "--server", "--server-tls-cert", cert)
	assert.ErrorContains(t, err, "together")
}

// An agent-only node must not be asked to produce a valid server configuration.
func TestDisabledRoleIsNotValidated(t *testing.T) {
	_, _, err := parse(t, agentOnly("--server-store-backend", "etcd")...)
	assert.NoError(t, err, "the server store config is irrelevant under --agent")

	// And the reverse: a server-only node needs no node name.
	_, _, err = parse(t, "run", "--server")
	assert.NoError(t, err)
}

func TestAgentDefaults(t *testing.T) {
	cli, _ := mustParse(t, agentOnly()...)
	agent := cli.Run.AgentOpts

	assert.Equal(t, "edge-01", agent.Node)
	assert.Equal(t, []string{"http://ctrl:2283"}, agent.Server)
	assert.Equal(t, "24000-24999", agent.PortRange.String())
	assert.Equal(t, "mxl-fabrics-proxy-worker", agent.WorkerBinary, "the worker binary keeps its name (§2.2)")
}

func TestServerIdleDefaults(t *testing.T) {
	cli, _ := mustParse(t, "run", "--server")
	idle := cli.Run.ServerOpts.Idle

	// §11.1 mechanism 1: 0 means wait indefinitely, which is what makes PAUSED a real
	// steady state instead of a ~13s restart loop. Both knobs are session-level and therefore
	// the server's, not the agent's (§5.5).
	assert.Zero(t, idle.IdleTimeout)
	assert.NotZero(t, idle.IdleTeardown)
}

// §6, §16: **there is no `-m` flag any more.** A domain is discovered under a search path and
// named through the API, so a flag that still tried to map one is refused as unknown rather than
// silently doing nothing.
func TestTheMapDomainFlagIsGone(t *testing.T) {
	for _, flag := range []string{"-m", "--agent-map-domain"} {
		_, _, err := parse(t, agentOnly(flag, "cameras=/dev/shm/mxl0")...)
		assert.Error(t, err, "flag %q should be unknown", flag)
	}
}

// **One noun with two independent grants** (§10.6). *This supersedes `--search-path` and
// `--output-root` as separate flags*: they were already counterparts and already had to be read as
// a pair.
func TestAgentAreas(t *testing.T) {
	cli, _ := mustParse(t, agentOnly(
		"--agent-area", "media=/dev/shm/mxl:r",
		"--agent-area", "fast=/dev/shm/mxl/replicated:rw",
		"--agent-area", "bulk=/mnt/nvme/mxl:w",
	)...)

	assert.Equal(t, []api.Area{
		{Name: "media", Path: "/dev/shm/mxl", Read: true},
		{Name: "fast", Path: "/dev/shm/mxl/replicated", Read: true, Write: true},
		{Name: "bulk", Path: "/mnt/nvme/mxl", Write: true},
	}, cli.Run.AgentOpts.areas)

	// No default. A node that has declared no area offers no sources and accepts no destinations,
	// which is the correct posture for the one flag that grants this project any authority over a
	// host's filesystem.
	cli, _ = mustParse(t, agentOnly()...)
	assert.Empty(t, cli.Run.AgentOpts.areas)
}

// **The one merged rule left is that no two areas share a path** (§10.6). *This supersedes a table
// of overlap rules — a search path inside a root, a search path equal to a root, a root above an
// input mapping.* Checked with the same function the destination resolver is built from, so the
// rule an operator is held to and the rule that decides where a worker writes cannot drift apart.
func TestAgentAreaValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "relative path",
			args: []string{"--agent-area", "fast=dev/shm/mxl:rw"},
			want: "absolute",
		},
		{
			name: "the filesystem root",
			args: []string{"--agent-area", "fast=/:rw"},
			want: "the filesystem root is not an area",
		},
		{
			name: "name that is not a plain element",
			args: []string{"--agent-area", "../fast=/dev/shm/mxl:rw"},
			want: "area name",
		},
		{
			name: "no grants",
			args: []string{"--agent-area", "fast=/dev/shm/mxl"},
			want: "names no grants",
		},
		{
			name: "same name twice",
			args: []string{"--agent-area", "fast=/dev/shm/mxl:rw", "--agent-area", "fast=/mnt/nvme/mxl:rw"},
			want: "declared twice",
		},
		{
			// The one arrangement the innermost-area rule cannot decide.
			name: "two areas on one directory",
			args: []string{"--agent-area", "fast=/dev/shm/mxl:rw", "--agent-area", "bulk=/dev/shm/mxl:r"},
			want: "an area twice",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parse(t, agentOnly(tc.args...)...)
			assert.ErrorContains(t, err, tc.want)
		})
	}

	// **Nesting is legal**, in either direction, and is the one-MXL-area-per-host layout an
	// operator actually writes: the innermost containing area names a directory (§10.6).
	_, _, err := parse(t, agentOnly(
		"--agent-area", "media=/dev/shm/mxl:r",
		"--agent-area", "fast=/dev/shm/mxl/replicated:rw",
	)...)
	assert.NoError(t, err)

	// A sibling whose path is a string prefix of another's is not a collision. Same boundary the
	// destination resolver is careful about, on the configuration side.
	_, _, err = parse(t, agentOnly(
		"--agent-area", "fast=/dev/shm/mxl:rw",
		"--agent-area", "other=/dev/shm/mxl-other:r",
	)...)
	assert.NoError(t, err)
}

func TestAgentValidation(t *testing.T) {
	// Without a co-located server there is nothing to derive a server URL from.
	_, _, err := parse(t, "run", "--agent", "--agent-node", "edge-01")
	assert.ErrorContains(t, err, "--server is required")

	// A relative area path is ambiguous against the agent's working directory, which is not
	// something the operator controls under a DaemonSet.
	_, _, err = parse(t, agentOnly("--agent-area", "media=dev/shm/mxl0:r")...)
	assert.ErrorContains(t, err, "absolute")

	_, _, err = parse(t, agentOnly("--agent-port-range", "24999-24000")...)
	assert.ErrorContains(t, err, "port range")
}

// With both roles in one process the agent is wired to the co-located server over HTTP.
// There is no in-memory short-circuit, so this URL is load-bearing rather than cosmetic
// (§2.2).
func TestCombinedDerivesAgentServerURL(t *testing.T) {
	cli, _ := mustParse(t, "run", "--agent-node", "dev")
	assert.Equal(t, []string{"http://127.0.0.1:2283"}, cli.Run.AgentOpts.Server)

	cli, _ = mustParse(t, "run", "--agent-node", "dev", "--server-listen", "0.0.0.0:9000")
	assert.Equal(t, []string{"http://127.0.0.1:9000"}, cli.Run.AgentOpts.Server)

	// An explicit URL is never overridden.
	cli, _ = mustParse(t, "run", "--agent-node", "dev", "--agent-server", "http://elsewhere:2283")
	assert.Equal(t, []string{"http://elsewhere:2283"}, cli.Run.AgentOpts.Server)
}

// The derived loopback URL is dialled by the co-located agent, so a server certificate
// issued for this node's routable name would not cover it. Fail at startup rather than at
// the agent's first poll.
func TestCombinedRefusesToDeriveALoopbackURLUnderTLS(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "tls.crt")
	key := filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(cert, []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(key, []byte("x"), 0o600))

	_, _, err := parse(t, "run", "--agent-node", "dev", "--server-tls-cert", cert, "--server-tls-key", key)
	assert.ErrorContains(t, err, "--agent-server is required")

	// Explicit is fine.
	cli, _ := mustParse(t, "run", "--agent-node", "dev",
		"--server-tls-cert", cert, "--server-tls-key", key,
		"--agent-server", "https://ctrl.example:2283")
	assert.Equal(t, []string{"https://ctrl.example:2283"}, cli.Run.AgentOpts.Server)
}

// Two listeners in one process must not collide. Only checked when both roles run.
func TestCombinedRejectsListenCollision(t *testing.T) {
	_, _, err := parse(t, "run", "--agent-node", "dev", "--server-listen", ":9000", "--agent-listen", ":9000")
	assert.ErrorContains(t, err, "--server-listen and --agent-listen are both")

	_, _, err = parse(t, agentOnly("--agent-listen", ":2283")...)
	assert.NoError(t, err, "no collision when the server role is not running")
}

// An HA deployment points the co-located agent at every replica so it can fail over; the
// derivation must not override that.
func TestCombinedAcceptsMultipleServerURLs(t *testing.T) {
	cli, _ := mustParse(t, "run", "--agent-node", "dev",
		"--agent-server", "http://a:2283", "--agent-server", "http://b:2283")
	assert.Equal(t, []string{"http://a:2283", "http://b:2283"}, cli.Run.AgentOpts.Server)
}

func TestDefaultsNodeToHostname(t *testing.T) {
	hostname, err := os.Hostname()
	require.NoError(t, err)

	cli, _ := mustParse(t, "run")
	assert.Equal(t, hostname, cli.Run.AgentOpts.Node)
	assert.True(t, cli.Run.nodeFromHostname, "a defaulted node name is warned about at startup")

	cli, _ = mustParse(t, "run", "--agent-node", "edge-01")
	assert.False(t, cli.Run.nodeFromHostname)
}

func TestLoopbackURL(t *testing.T) {
	for _, tc := range []struct {
		listen string
		tls    bool
		want   string
	}{
		{":2283", false, "http://127.0.0.1:2283"},
		{"0.0.0.0:2283", false, "http://127.0.0.1:2283"},
		{"[::]:2283", false, "http://127.0.0.1:2283"},
		{"127.0.0.1:2283", true, "https://127.0.0.1:2283"},
		{"10.0.0.1:9000", false, "http://10.0.0.1:9000"},
	} {
		got, err := loopbackURL(tc.listen, tc.tls)
		require.NoError(t, err, tc.listen)
		assert.Equal(t, tc.want, got, tc.listen)
	}

	_, err := loopbackURL("2283", false)
	assert.Error(t, err)
}

func TestAuthFlags(t *testing.T) {
	// No auth at all is a supported configuration (§13).
	token, err := AuthFlags{}.Token()
	require.NoError(t, err)
	assert.Empty(t, token)

	token, err = AuthFlags{AuthToken: "secret"}.Token()
	require.NoError(t, err)
	assert.Equal(t, "secret", token)

	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte("  from-file\n"), 0o600))

	token, err = AuthFlags{AuthTokenFile: path}.Token()
	require.NoError(t, err)
	assert.Equal(t, "from-file", token, "surrounding whitespace must be trimmed")

	_, err = AuthFlags{AuthToken: "a", AuthTokenFile: path}.Token()
	assert.ErrorContains(t, err, "mutually exclusive")

	empty := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(empty, []byte("\n"), 0o600))

	_, err = AuthFlags{AuthTokenFile: empty}.Token()
	assert.ErrorContains(t, err, "empty")
}

// A fabric attachment can be declared on the command line, so that a single-host deployment
// needs no configuration file at all (§10.1).
func TestAgentFabricFlag(t *testing.T) {
	cli, _ := mustParse(t, agentOnly(
		"--agent-fabric", "provider=verbs,fabric=ib-a,interface=ib0",
		"--agent-fabric", "provider=efa,fabric=vpc1",
	)...)

	require.Len(t, cli.Run.AgentOpts.fabrics, 2)
	assert.Equal(t, api.ProviderVerbs, cli.Run.AgentOpts.fabrics[0].Provider)
	assert.Equal(t, "ib0", cli.Run.AgentOpts.fabrics[0].Interface)
	assert.Equal(t, api.ProviderEFA, cli.Run.AgentOpts.fabrics[1].Provider)
	assert.False(t, cli.Run.AgentOpts.fabricsDefaulted)

	// The join's failure modes are worth reaching from the command line too.
	_, _, err := parse(t, agentOnly("--agent-fabric", "provider=efa,fabric=vpc1,interface=efa0")...)
	assert.ErrorContains(t, err, "efa attachment is selected by device")
}

// A node with no attachments can do nothing, and refusing to start would break
// `mxl-replicator run` with no arguments — which is the single-host and development case §2.2
// exists to serve. shm is the right assumption there: same-node-only, no address, label derived.
func TestNoFabricsMeansSHM(t *testing.T) {
	cli, _ := mustParse(t, agentOnly()...)

	require.Len(t, cli.Run.AgentOpts.fabrics, 1)
	assert.Equal(t, api.ProviderSHM, cli.Run.AgentOpts.fabrics[0].Provider)
	assert.True(t, cli.Run.AgentOpts.fabricsDefaulted, "the assumption is warned about at startup")
}

// --agent-detect-default-fabric is the other answer to the same question, and it replaces the shm
// assumption rather than joining it: the detection needs the probe, so it happens on the
// registration path and leaves nothing to resolve here (§10.1).
func TestDetectDefaultFabricReplacesTheSHMAssumption(t *testing.T) {
	cli, _ := mustParse(t, agentOnly("--agent-detect-default-fabric")...)

	assert.True(t, cli.Run.AgentOpts.DetectDefaultFabric)
	assert.Empty(t, cli.Run.AgentOpts.fabrics, "detection happens against the probe, not at parse time")
	assert.False(t, cli.Run.AgentOpts.fabricsDefaulted)

	// An attachment the operator wrote is never joined by a detected one — the two would be
	// competing descriptions of the same hardware and the second is the guess. Inert rather than
	// wrong, so it parses and is warned about at startup.
	cli, _ = mustParse(t, agentOnly("--agent-detect-default-fabric", "--agent-fabric", "provider=tcp,fabric=dc1,address=10.0.0.1")...)
	require.Len(t, cli.Run.AgentOpts.fabrics, 1)
	assert.Equal(t, "dc1", cli.Run.AgentOpts.fabrics[0].Fabric)
}

// A flag wins over a file for a scalar and adds to it for a collection, so a file describing a
// host's hardware stays extensible on the command line.
func TestAgentConfigFileIsMergedWithFlags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
node: from-file
server:
  - http://from-file:2283
areas:
  - {name: media, path: /dev/shm/mxl-in, read: true}
  - {name: fast,  path: /dev/shm/mxl, read: true, write: true}
fabrics:
  - provider: tcp
    fabric: dc1
    address: 10.0.0.1
`), 0o600))

	// Nothing on the command line: the file supplies everything.
	cli, _ := mustParse(t, "run", "--agent", "--agent-config", path)
	assert.Equal(t, "from-file", cli.Run.AgentOpts.Node)
	assert.Equal(t, []string{"http://from-file:2283"}, cli.Run.AgentOpts.Server)
	assert.Equal(t, []api.Area{
		{Name: "media", Path: "/dev/shm/mxl-in", Read: true},
		{Name: "fast", Path: "/dev/shm/mxl", Read: true, Write: true},
	}, cli.Run.AgentOpts.areas)
	require.Len(t, cli.Run.AgentOpts.fabrics, 1)

	// Flags override the scalars and merge into the collections.
	cli, _ = mustParse(t, "run", "--agent", "--agent-config", path,
		"--agent-node", "edge-01",
		"--agent-server", "http://ctrl:2283",
		"--agent-area", "bulk=/mnt/nvme/mxl:rw",
		"--agent-fabric", "provider=shm")

	assert.Equal(t, "edge-01", cli.Run.AgentOpts.Node)
	assert.Equal(t, []string{"http://ctrl:2283"}, cli.Run.AgentOpts.Server)
	// Areas accumulate, file first — the same rule fabric attachments follow, so a file describing
	// a host's layout stays extensible on the command line.
	assert.Equal(t, []api.Area{
		{Name: "media", Path: "/dev/shm/mxl-in", Read: true},
		{Name: "fast", Path: "/dev/shm/mxl", Read: true, Write: true},
		{Name: "bulk", Path: "/mnt/nvme/mxl", Read: true, Write: true},
	}, cli.Run.AgentOpts.areas)
	require.Len(t, cli.Run.AgentOpts.fabrics, 2)
}

// A file that will not parse is a startup error, not a node that quietly comes up with no
// connectivity.
func TestAgentConfigFileErrorsAtParseTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte("fabircs:\n  - provider: tcp\n"), 0o600))

	_, _, err := parse(t, agentOnly("--agent-config", path)...)
	assert.ErrorContains(t, err, "fabircs")
}
