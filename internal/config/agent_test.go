package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/agent/probe"
	"github.com/jonasohland/mxl-replicator/internal/api"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoadAgent(t *testing.T) {
	path := write(t, `
node: edge-01
server:
  - http://ctrl-0:2283
  - http://ctrl-1:2283
domains:
  cameras: /dev/shm/mxl0
search_paths:
  - /dev/shm
output_roots:
  - name: fast
    path: /dev/shm/mxl
  - name: bulk
    path: /mnt/nvme/mxl
fabrics:
  - provider: verbs
    fabric: ib-fabric-a
    interface: ib0
  - provider: efa
    fabric: vpc1-subnet-a
`)

	loaded, err := LoadAgent(path)
	require.NoError(t, err)
	require.NoError(t, loaded.Validate())

	assert.Equal(t, "edge-01", loaded.Node)
	assert.Equal(t, []string{"http://ctrl-0:2283", "http://ctrl-1:2283"}, loaded.Server)
	assert.Equal(t, Domains{"cameras": "/dev/shm/mxl0"}, loaded.Domains)
	assert.Equal(t, []string{"/dev/shm"}, loaded.SearchPaths)
	assert.Equal(t, []OutputRoot{
		{Name: "fast", Path: "/dev/shm/mxl"},
		{Name: "bulk", Path: "/mnt/nvme/mxl"},
	}, loaded.OutputRoots)
	assert.Equal(t, []probe.Attachment{
		{Provider: api.ProviderVerbs, Fabric: "ib-fabric-a", Interface: "ib0"},
		{Provider: api.ProviderEFA, Fabric: "vpc1-subnet-a"},
	}, loaded.Fabrics)
}

// §16: the legacy `domains:` block carries over unchanged, including its `url:` spelling and the
// per-domain fields that no longer mean anything here.
func TestLegacyDomainBlockIsAccepted(t *testing.T) {
	path := write(t, `
domains:
  loopback-in:
    url: mxl:///dev/shm/mxl1
  loopback-out:
    url: mxl:///dev/shm/mxl0
    provider: tcp
    node: 127.0.0.1
    labels:
      studio: a
  archive:
    path: /srv/archive/
  cameras: /dev/shm/mxl2
`)

	loaded, err := LoadAgent(path)
	require.NoError(t, err)
	require.NoError(t, loaded.Validate())

	assert.Equal(t, Domains{
		"loopback-in":  "/dev/shm/mxl1",
		"loopback-out": "/dev/shm/mxl0",
		"archive":      "/srv/archive",
		"cameras":      "/dev/shm/mxl2",
	}, loaded.Domains)
}

func TestDomainBlockRejections(t *testing.T) {
	for _, tc := range []struct{ name, body, wants string }{
		{"relative", "domains:\n  a: dev/shm/mxl0\n", "must be absolute"},
		{"empty", "domains:\n  a: \"\"\n", "empty path"},
		{"both", "domains:\n  a:\n    url: mxl:///x\n    path: /x\n", "not both"},
		{"neither", "domains:\n  a:\n    provider: tcp\n", "neither url nor path"},
		{"wrong scheme", "domains:\n  a:\n    url: file:///x\n", "is not mxl"},
		{"a list", "domains:\n  - a\n", "expected a mapping"},
		{"a domain that is a list", "domains:\n  a: [1, 2]\n", "expected a path or a mapping"},
		{
			// The legacy config used mxl://host/path for *remote* domains. A domain block names
			// directories on this host; a remote flow is addressed by (node, domain) now.
			"remote url",
			"domains:\n  a:\n    url: mxl://other-host/dev/shm/mxl0\n",
			"names a host",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadAgent(write(t, tc.body))
			assert.ErrorContains(t, err, tc.wants)
		})
	}
}

// A mistyped top-level key that silently did nothing would present as a node with no
// connectivity, which reads as missing hardware rather than as a typo.
func TestUnknownTopLevelKeysAreRejected(t *testing.T) {
	_, err := LoadAgent(write(t, "fabircs:\n  - provider: tcp\n"))
	assert.ErrorContains(t, err, "fabircs")
}

func TestEmptyFileIsFine(t *testing.T) {
	loaded, err := LoadAgent(write(t, ""))
	require.NoError(t, err)
	assert.Empty(t, loaded.Node)
	assert.NoError(t, loaded.Validate())
}

func TestMissingFileIsReported(t *testing.T) {
	_, err := LoadAgent(filepath.Join(t.TempDir(), "nope.yaml"))
	assert.ErrorContains(t, err, "nope.yaml")
}

// Maps merge per key, lists replace: a later file adding a domain keeps the earlier ones, but a
// later `fabrics:` block describes this node's connectivity in full.
func TestMergeRules(t *testing.T) {
	first := write(t, `
node: edge-01
server: [http://a:2283]
domains:
  cameras: /dev/shm/mxl0
  ingest: /dev/shm/mxl1
output_roots:
  - name: fast
    path: /dev/shm/mxl
fabrics:
  - provider: tcp
    fabric: dc1
    address: 10.0.0.1
`)
	second := write(t, `
domains:
  ingest: /dev/shm/mxl9
  archive: /dev/shm/mxl2
output_roots:
  - name: bulk
    path: /mnt/nvme/mxl
fabrics:
  - provider: verbs
    fabric: ib-a
    device: mlx5_0
`)

	loaded, err := LoadAgent(first, second)
	require.NoError(t, err)

	assert.Equal(t, "edge-01", loaded.Node, "a key the later file does not mention survives")
	assert.Equal(t, []string{"http://a:2283"}, loaded.Server)
	assert.Equal(t, Domains{
		"cameras": "/dev/shm/mxl0",
		"ingest":  "/dev/shm/mxl9",
		"archive": "/dev/shm/mxl2",
	}, loaded.Domains)
	require.Len(t, loaded.Fabrics, 1)
	assert.Equal(t, api.ProviderVerbs, loaded.Fabrics[0].Provider)

	// Roots replace rather than merge, like fabrics: the list is one description of what this
	// node permits writing into, and half-overriding it is never what anyone means.
	assert.Equal(t, []OutputRoot{{Name: "bulk", Path: "/mnt/nvme/mxl"}}, loaded.OutputRoots)
}

func TestValidateReportsEveryProblem(t *testing.T) {
	agent := &Agent{
		Domains:     Domains{"a": "relative/path"},
		SearchPaths: []string{"also/relative"},
		OutputRoots: []OutputRoot{
			{Name: "fast", Path: "relative/too"},
			{Name: "../escape", Path: "/dev/shm/mxl"},
		},
		Fabrics: []probe.Attachment{
			{Provider: api.ProviderTCP}, // no fabric label
			{Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.0.0.1", Device: "eth0"},
		},
	}

	err := agent.Validate()
	require.Error(t, err)
	assert.ErrorContains(t, err, "must be absolute")
	assert.ErrorContains(t, err, "search path")
	assert.ErrorContains(t, err, "fabrics[0]")
	assert.ErrorContains(t, err, "fabrics[1]")
	// Per-entry only here. Whether roots overlap each other, a mapping or a search path needs the
	// merged file-plus-flags picture, so it is checked once where that exists (§10.6).
	assert.ErrorContains(t, err, `output root "fast"`)
	assert.ErrorContains(t, err, "output_roots[1]")
}

// The flag form exists so that a single-host or development deployment needs no file at all,
// which is what `mxl-replicator run` with no arguments is for.
func TestParseFabric(t *testing.T) {
	attachment, err := ParseFabric("provider=tcp,fabric=dc1-data,interface=eth1")
	require.NoError(t, err)
	assert.Equal(t, probe.Attachment{
		Provider:  api.ProviderTCP,
		Fabric:    "dc1-data",
		Interface: "eth1",
	}, attachment)

	// shm needs no label: it is derived from the node name (§10.1).
	attachment, err = ParseFabric("provider=shm")
	require.NoError(t, err)
	assert.Equal(t, api.ProviderSHM, attachment.Provider)

	attachment, err = ParseFabric(" provider = efa , fabric = vpc1 ")
	require.NoError(t, err)
	assert.Equal(t, probe.Attachment{Provider: api.ProviderEFA, Fabric: "vpc1"}, attachment)

	for _, tc := range []struct{ value, wants string }{
		{"provider=tcp,fabric=dc1,eth1", "is not key=value"},
		{"provider=tcp,fabric=dc1,nic=eth1", "unknown key"},
		{"fabric=dc1", "provider is required"},
		{"provider=sockets,fabric=dc1", "not one this project can negotiate"},
		{"provider=tcp", "fabric label is required"},
		{"provider=tcp,fabric=dc1,address=10.0.0.1,device=eth0", "at most one of"},
	} {
		_, err := ParseFabric(tc.value)
		assert.ErrorContains(t, err, tc.wants, tc.value)
	}
}
