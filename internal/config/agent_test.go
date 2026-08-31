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
areas:
  - {name: media, path: /dev/shm, read: true}
  - {name: fast,  path: /dev/shm/mxl, read: true, write: true}
  - {name: bulk,  path: /mnt/nvme/mxl, write: true}
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
	// **Areas may nest**, and the innermost containing one names a directory (§10.6): `fast` sits
	// inside `media`, which is the ordinary one-MXL-area-per-host layout rather than an exception
	// to anything.
	assert.Equal(t, []api.Area{
		{Name: "media", Path: "/dev/shm", Read: true},
		{Name: "fast", Path: "/dev/shm/mxl", Read: true, Write: true},
		{Name: "bulk", Path: "/mnt/nvme/mxl", Write: true},
	}, loaded.Areas)
	assert.Equal(t, []probe.Attachment{
		{Provider: api.ProviderVerbs, Fabric: "ib-fabric-a", Interface: "ib0"},
		{Provider: api.ProviderEFA, Fabric: "vpc1-subnet-a"},
	}, loaded.Fabrics)
}

// **The `domains:` block is gone** (§6, §16), and a file that still carries one is refused
// outright rather than accepted-and-ignored — which is the ordinary unknown-key rule, applied to a
// key that used to be real. A silently ignored mapping block would be a node whose operator
// believes it has named its domains and which reports them under whatever the discoverer called
// them.
func TestTheDomainsBlockIsNoLongerAccepted(t *testing.T) {
	_, err := LoadAgent(write(t, "domains:\n  cameras: /dev/shm/mxl0\n"))
	assert.ErrorContains(t, err, "domains")
}

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

// Lists replace: a later `fabrics:` block describes this node's connectivity in full, and half
// -overriding one is never what anyone means.
func TestMergeRules(t *testing.T) {
	first := write(t, `
node: edge-01
server: [http://a:2283]
areas:
  - {name: fast, path: /dev/shm/mxl, read: true, write: true}
fabrics:
  - provider: tcp
    fabric: dc1
    address: 10.0.0.1
`)
	second := write(t, `
areas:
  - {name: bulk, path: /mnt/nvme/mxl, read: true, write: true}
fabrics:
  - provider: verbs
    fabric: ib-a
    device: mlx5_0
`)

	loaded, err := LoadAgent(first, second)
	require.NoError(t, err)

	assert.Equal(t, "edge-01", loaded.Node, "a key the later file does not mention survives")
	assert.Equal(t, []string{"http://a:2283"}, loaded.Server)
	require.Len(t, loaded.Fabrics, 1)
	assert.Equal(t, api.ProviderVerbs, loaded.Fabrics[0].Provider)

	// Areas replace rather than merge, like fabrics: the list is one description of what this node
	// permits, and half-overriding it is never what anyone means.
	assert.Equal(t, []api.Area{{Name: "bulk", Path: "/mnt/nvme/mxl", Read: true, Write: true}}, loaded.Areas)
}

func TestValidateReportsEveryProblem(t *testing.T) {
	agent := &Agent{
		Areas: []api.Area{
			{Name: "fast", Path: "relative/too", Read: true},
			{Name: "../escape", Path: "/dev/shm/mxl", Read: true},
			{Name: "inert", Path: "/dev/shm/mxl2"},
		},
		Fabrics: []probe.Attachment{
			{Provider: api.ProviderTCP}, // no fabric label
			{Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.0.0.1", Device: "eth0"},
		},
	}

	err := agent.Validate()
	require.Error(t, err)
	assert.ErrorContains(t, err, "must be absolute")
	assert.ErrorContains(t, err, "fabrics[0]")
	assert.ErrorContains(t, err, "fabrics[1]")
	// Per-entry only here. Whether two areas share a path needs the merged file-plus-flags
	// picture, so it is checked once where that exists (§10.6).
	assert.ErrorContains(t, err, `area "fast"`)
	assert.ErrorContains(t, err, "areas[1]")
	// An area granting neither is a line that does nothing, and an operator who wrote it believes
	// the node has an area there (§10.6).
	assert.ErrorContains(t, err, "grants neither read nor write")
}

// **Areas take grants and there is no default**, because both bits default false in the model: a
// flag that guessed would be granting this project authority over a host's filesystem on the
// strength of an omission (§10.6).
func TestParseArea(t *testing.T) {
	t.Parallel()

	for value, want := range map[string]api.Area{
		"media=/dev/shm/mxl:r":            {Name: "media", Path: "/dev/shm/mxl", Read: true},
		"fast=/dev/shm/mxl/replicated:rw": {Name: "fast", Path: "/dev/shm/mxl/replicated", Read: true, Write: true},
		"bulk=/mnt/nvme/mxl:w":            {Name: "bulk", Path: "/mnt/nvme/mxl", Write: true},
		"odd=/mnt/a:b/c:rw":               {Name: "odd", Path: "/mnt/a:b/c", Read: true, Write: true},
	} {
		area, err := ParseArea(value)
		require.NoError(t, err, value)
		assert.Equal(t, want, area, value)
	}

	for _, value := range []string{
		"media",                  // no path
		"media=/dev/shm/mxl",     // no grants
		"media=/dev/shm/mxl:",    // no grants, spelled
		"media=/dev/shm/mxl:x",   // unknown grant
		"media=/dev/shm/mxl:rwx", // one unknown among the known
	} {
		_, err := ParseArea(value)
		assert.Error(t, err, value)
	}
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
