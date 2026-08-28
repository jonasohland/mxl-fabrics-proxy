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

// §16: the legacy `-m name=/path` syntax carries over byte-compatible.
func TestAgentDomainMappingSyntaxIsLegacyCompatible(t *testing.T) {
	cli, _ := mustParse(t, agentOnly(
		"-m", "cameras=/dev/shm/mxl0",
		"--agent-map-domain", "ingest=/dev/shm/mxl1",
	)...)

	assert.Equal(t, map[string]string{
		"cameras": "/dev/shm/mxl0",
		"ingest":  "/dev/shm/mxl1",
	}, cli.Run.AgentOpts.Domains)
}

// The write side is opt-in and separate from -m: an input mapping is a directory the node has, an
// output root is a place replication may create directories (§10.6).
func TestAgentOutputRoots(t *testing.T) {
	cli, _ := mustParse(t, agentOnly(
		"--agent-output-root", "fast=/dev/shm/mxl",
		"--agent-output-root", "bulk=/mnt/nvme/mxl",
	)...)

	assert.Equal(t, map[string]string{
		"fast": "/dev/shm/mxl",
		"bulk": "/mnt/nvme/mxl",
	}, cli.Run.AgentOpts.OutputRoots)

	// No default. A node that has not opted in is not a replication destination, which is the
	// correct posture for the one flag that grants the control plane write authority.
	cli, _ = mustParse(t, agentOnly()...)
	assert.Empty(t, cli.Run.AgentOpts.OutputRoots)
}

// One directory must have exactly one name on this node (§10.6). Checked on the merged
// configuration, which is the only place that sees roots, input mappings and search paths at
// once — and checked with the same function the destination resolver is built from, so the rule
// an operator is held to and the rule that decides where a worker writes cannot drift apart.
func TestAgentOutputRootValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "relative path",
			args: []string{"--agent-output-root", "fast=dev/shm/mxl"},
			want: "absolute",
		},
		{
			name: "the filesystem root",
			args: []string{"--agent-output-root", "fast=/"},
			want: "the filesystem root is not an output root",
		},
		{
			name: "name that is not a plain element",
			args: []string{"--agent-output-root", "../fast=/dev/shm/mxl"},
			want: "output root name",
		},
		{
			name: "two roots on one directory",
			args: []string{"--agent-output-root", "fast=/dev/shm/mxl", "--agent-output-root", "bulk=/dev/shm/mxl"},
			want: "an output root twice",
		},
		{
			name: "nested roots",
			args: []string{"--agent-output-root", "fast=/dev/shm/mxl", "--agent-output-root", "bulk=/dev/shm/mxl/inner"},
			want: "overlap",
		},
		{
			// A root that *is* an input domain's directory. Distinct from a root *above* one,
			// which is permitted — the collision that allows is one exact path, and it is refused
			// where it is precise, at resolution time (§10.6).
			name: "root that is an input mapping",
			args: []string{"--agent-output-root", "fast=/dev/shm/mxl0", "-m", "cameras=/dev/shm/mxl0"},
			want: `overlaps domain "cameras"`,
		},
		{
			name: "root under an input mapping",
			args: []string{"--agent-output-root", "fast=/dev/shm/mxl0/inner", "-m", "cameras=/dev/shm/mxl0"},
			want: `overlaps domain "cameras"`,
		},
		{
			// Discovery is pruned at every root, so a search path that *is* one could never find
			// anything. Distinct from a search path *above* a root, which is permitted below.
			name: "root that is a search path",
			args: []string{"--agent-output-root", "fast=/dev/shm/mxl", "--agent-search-path", "/dev/shm/mxl"},
			want: "could never find anything",
		},
		{
			name: "root over a search path",
			args: []string{"--agent-output-root", "fast=/dev/shm/mxl", "--agent-search-path", "/dev/shm/mxl/inner"},
			want: "contains search path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parse(t, agentOnly(tc.args...)...)
			assert.ErrorContains(t, err, tc.want)
		})
	}

	// **A search path may be an ancestor of a root**: one MXL area per host, part of it discovered
	// and part of it written, which is the layout the pruning exists to make safe (§10.6).
	_, _, err := parse(t, agentOnly(
		"--agent-search-path", "/dev/shm/mxl",
		"--agent-output-root", "fast=/dev/shm/mxl/replicated",
		"-m", "cameras=/dev/shm/mxl/cameras",
	)...)
	assert.NoError(t, err)

	// A sibling whose path is a string prefix of a root's is not an overlap. Same boundary the
	// destination resolver is careful about, on the configuration side.
	_, _, err = parse(t, agentOnly(
		"--agent-output-root", "fast=/dev/shm/mxl",
		"-m", "cameras=/dev/shm/mxl-other",
	)...)
	assert.NoError(t, err)
}

func TestAgentValidation(t *testing.T) {
	// Without a co-located server there is nothing to derive a server URL from.
	_, _, err := parse(t, "run", "--agent", "--agent-node", "edge-01")
	assert.ErrorContains(t, err, "--server is required")

	// A relative domain path is ambiguous against the agent's working directory.
	_, _, err = parse(t, agentOnly("-m", "cameras=dev/shm/mxl0")...)
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
	assert.ErrorContains(t, err, "separate ports")

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
	assert.ErrorContains(t, err, "cannot select an efa attachment")
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

// A flag wins over a file for a scalar and adds to it for a collection, so a file describing a
// host's hardware stays extensible on the command line.
func TestAgentConfigFileIsMergedWithFlags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
node: from-file
server:
  - http://from-file:2283
domains:
  cameras: /dev/shm/mxl0
  ingest:
    url: mxl:///dev/shm/mxl1
output_roots:
  - name: fast
    path: /dev/shm/mxl
fabrics:
  - provider: tcp
    fabric: dc1
    address: 10.0.0.1
`), 0o600))

	// Nothing on the command line: the file supplies everything.
	cli, _ := mustParse(t, "run", "--agent", "--agent-config", path)
	assert.Equal(t, "from-file", cli.Run.AgentOpts.Node)
	assert.Equal(t, []string{"http://from-file:2283"}, cli.Run.AgentOpts.Server)
	assert.Equal(t, map[string]string{
		"cameras": "/dev/shm/mxl0",
		"ingest":  "/dev/shm/mxl1",
	}, cli.Run.AgentOpts.Domains)
	assert.Equal(t, map[string]string{"fast": "/dev/shm/mxl"}, cli.Run.AgentOpts.OutputRoots)
	require.Len(t, cli.Run.AgentOpts.fabrics, 1)

	// Flags override the scalars and merge into the collections.
	cli, _ = mustParse(t, "run", "--agent", "--agent-config", path,
		"--agent-node", "edge-01",
		"--agent-server", "http://ctrl:2283",
		"-m", "cameras=/dev/shm/other",
		"-m", "archive=/dev/shm/mxl2",
		"--agent-output-root", "fast=/dev/shm/faster",
		"--agent-output-root", "bulk=/mnt/nvme/mxl",
		"--agent-fabric", "provider=shm")

	assert.Equal(t, "edge-01", cli.Run.AgentOpts.Node)
	assert.Equal(t, []string{"http://ctrl:2283"}, cli.Run.AgentOpts.Server)
	assert.Equal(t, map[string]string{
		"cameras": "/dev/shm/other",
		"ingest":  "/dev/shm/mxl1",
		"archive": "/dev/shm/mxl2",
	}, cli.Run.AgentOpts.Domains)
	// Roots merge by name like domains do: the flag redirects "fast" and adds "bulk".
	assert.Equal(t, map[string]string{
		"fast": "/dev/shm/faster",
		"bulk": "/mnt/nvme/mxl",
	}, cli.Run.AgentOpts.OutputRoots)
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

// A root above an input domain is a legal layout: one directory holding this node's domains, some
// read and some written. The exact path is refused as an output domain at resolution time, which
// is where the collision is precise (§10.6).
func TestARootMayBeTheParentOfAnInputDomain(t *testing.T) {
	t.Parallel()

	_, _, err := parse(t, agentOnly(
		"--agent-output-root", "fast=/dev/shm/mxl",
		"-m", "cameras=/dev/shm/mxl/cameras",
	)...)
	assert.NoError(t, err)
}

// An input domain name shares a namespace with every output domain on this node, so it takes the
// same rule (§10.6). Without it, `-m a/b=…` and `domain: a/b` would be two things with one
// address — and the server's collision check compares exactly those two strings.
func TestAgentDomainNameValidation(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"a/b", "with space", ".hidden", "-flag", ".."} {
		_, _, err := parse(t, agentOnly("-m", name+"=/dev/shm/mxl0")...)
		assert.Error(t, err, "domain name %q should be refused", name)
	}

	// §16 promised `-m name=/path` carries over byte-compatible from the legacy proxy. The syntax
	// does, and so does every name shape a deployment actually uses; what changed is that a name
	// legacy would have accepted but this cannot address is now a startup error rather than one
	// that works until something collides with it.
	for _, name := range []string{"mxl0", "cameras", "loopback-in", "studio_a", "a.b"} {
		_, _, err := parse(t, agentOnly("-m", name+"=/dev/shm/mxl0")...)
		assert.NoError(t, err, "domain name %q should be accepted", name)
	}
}
