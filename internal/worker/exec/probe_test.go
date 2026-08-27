package exec

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// probeBinary re-executes this test binary in worker mode, the same way the launcher tests do.
func probeBinary(t *testing.T, env map[string]string) (string, context.Context) {
	t.Helper()
	t.Setenv(helperEnv, "1")
	for key, value := range env {
		t.Setenv(key, value)
	}
	return os.Args[0], t.Context()
}

func TestProbeVersions(t *testing.T) {
	binary, ctx := probeBinary(t, nil)

	versions, err := ProbeVersions(ctx, binary)
	require.NoError(t, err)
	assert.Equal(t, "0.0.1", versions.Proxy)
	assert.Equal(t, "1.1.0-rc1", versions.MXL)
	assert.Equal(t, "2.6", versions.Libfabric)

	// The probe fills in only what the worker reports; protocol and build version are the
	// agent's to add (§10.2).
	assert.Zero(t, versions.Protocol)
	assert.Empty(t, versions.Replicator)
}

func TestProbeVersionsFailsOnAMissingBinary(t *testing.T) {
	_, err := ProbeVersions(t.Context(), "mxl-fabrics-proxy-worker-that-does-not-exist")
	assert.Error(t, err)
}

func TestProbeInterfaces(t *testing.T) {
	binary, ctx := probeBinary(t, nil)

	interfaces, err := ProbeInterfaces(ctx, binary)
	require.NoError(t, err)
	require.Len(t, interfaces, 2)

	tcp := interfaces[0]
	assert.Equal(t, api.ProviderTCP, tcp.Provider)
	assert.Equal(t, "10.135.0.123", tcp.Address)
	assert.Equal(t, []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive, api.CapBlockingOperations}, tcp.Caps.Flags)
	assert.Equal(t, "eth1", tcp.Device())

	// UINT64_MAX survives, which it would not through a float64 — and a float64 is what
	// decoding into `any` gets you (WRS §2).
	assert.Equal(t, uint64(math.MaxUint64), tcp.Caps.MaxMessageSize)

	// shm reports the hostname as its address and no device_name at all, which is the reason
	// the join in §10.5 has a selectorless case rather than matching on a name.
	shm := interfaces[1]
	assert.Equal(t, api.ProviderSHM, shm.Provider)
	assert.Equal(t, "edge-01", shm.Address)
	assert.Empty(t, shm.Device())

	// There is deliberately no service anywhere in this: the library's is empty for every
	// provider but shm, and the shm value is an artefact of the probing process (§7.4).
	assert.NotContains(t, string(shm.Attr), "service")
}

// The probe puts JSON on stdout and libfabric's diagnostics on stderr, and the worker
// redirects its own stdout away for the duration precisely so the data stream stays parseable
// (WRS §2). Capturing them together would leave the JSON unparseable.
func TestProbeInterfacesIsNotConfusedByDiagnostics(t *testing.T) {
	binary, ctx := probeBinary(t, nil)

	interfaces, err := ProbeInterfaces(ctx, binary)
	require.NoError(t, err)
	assert.NotEmpty(t, interfaces)
}

func TestProbeInterfacesReportsTheFatalLine(t *testing.T) {
	binary, ctx := probeBinary(t, map[string]string{helperProbeFailEnv: "1"})

	_, err := ProbeInterfaces(ctx, binary)
	require.Error(t, err)
	assert.ErrorContains(t, err, "no fabric interfaces", "the reason is on stderr and must reach the operator")
}
