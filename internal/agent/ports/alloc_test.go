package ports

import (
	"errors"
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

func newAllocator(t *testing.T, low, high uint16) *Allocator {
	t.Helper()
	a, err := NewAllocator(Range{Low: low, High: high})
	require.NoError(t, err)
	// The default probe-bind would consult the host's real port table, which makes a test's
	// outcome depend on what else is running on the machine.
	a.listen = func(string, string) (net.Listener, error) { return nopListener{}, nil }
	return a
}

func TestNewAllocatorRejectsAnUnsetRange(t *testing.T) {
	_, err := NewAllocator(Range{})
	assert.ErrorContains(t, err, "no range")
}

// §7.4: a worker restarted for a new epoch, after a crash or after its backoff must get the same
// port back. The nonce is what makes the epoch change, so port stability costs nothing.
func TestServiceIsStablePerOwner(t *testing.T) {
	a := newAllocator(t, 24000, 24010)

	first, err := a.Allocate("s1/target", api.ProviderTCP)
	require.NoError(t, err)

	again, err := a.Allocate("s1/target", api.ProviderTCP)
	require.NoError(t, err)
	assert.Equal(t, first, again)

	other, err := a.Allocate("s1/initiator", api.ProviderTCP)
	require.NoError(t, err)
	assert.NotEqual(t, first, other, "two workers of one session bind separately")

	service, ok := a.Service("s1/target")
	assert.True(t, ok)
	assert.Equal(t, first, service)

	_, ok = a.Service("nobody")
	assert.False(t, ok)
}

func TestReleaseReturnsThePortToThePool(t *testing.T) {
	a := newAllocator(t, 24000, 24000) // exactly one port

	first, err := a.Allocate("s1/target", api.ProviderTCP)
	require.NoError(t, err)
	assert.Equal(t, "24000", first)

	_, err = a.Allocate("s2/target", api.ProviderTCP)
	assert.ErrorContains(t, err, "no free port")

	a.Release("s1/target")
	a.Release("s1/target") // idempotent

	second, err := a.Allocate("s2/target", api.ProviderTCP)
	require.NoError(t, err)
	assert.Equal(t, "24000", second)
}

func TestAllocationsAreUniqueAcrossTheWholeRange(t *testing.T) {
	a := newAllocator(t, 24000, 24004)

	seen := map[string]string{}
	for i := range 5 {
		owner := "s" + strconv.Itoa(i)
		service, err := a.Allocate(owner, api.ProviderTCP)
		require.NoError(t, err)
		assert.NotContains(t, seen, service, "port handed out twice")
		seen[service] = owner
	}

	_, err := a.Allocate("s5", api.ProviderTCP)
	assert.ErrorContains(t, err, "5 of 5")
	assert.Len(t, a.Owners(), 5)
}

// shm allocates from the same range with no special case: its service is a host-wide unique
// endpoint *name*, which is what a range allocator produces anyway (M0 plan decision).
func TestSHMSharesOneCollisionDomain(t *testing.T) {
	a := newAllocator(t, 24000, 24001)

	tcp, err := a.Allocate("s1/target", api.ProviderTCP)
	require.NoError(t, err)
	shm, err := a.Allocate("s2/target", api.ProviderSHM)
	require.NoError(t, err)
	assert.NotEqual(t, tcp, shm)
}

// A port some unrelated process on the node holds is skipped rather than handed out, which is the
// collision the legacy random picker discovered by restart-looping.
func TestProbeBindSkipsAPortInUse(t *testing.T) {
	a := newAllocator(t, 24000, 24001)
	a.listen = func(_, address string) (net.Listener, error) {
		if _, port, _ := net.SplitHostPort(address); port == "24000" {
			return nil, errors.New("address already in use")
		}
		return nopListener{}, nil
	}

	service, err := a.Allocate("s1/target", api.ProviderTCP)
	require.NoError(t, err)
	assert.Equal(t, "24001", service)
}

// The probe is only meaningful for tcp: verbs and efa services live in the RDMA CM's own port
// space and shm's is not a port at all, so a TCP probe there would refuse usable ports.
func TestProbeOnlyRunsForTCP(t *testing.T) {
	assert.True(t, shouldProbe(api.ProviderTCP))
	assert.False(t, shouldProbe(api.ProviderVerbs))
	assert.False(t, shouldProbe(api.ProviderEFA))
	assert.False(t, shouldProbe(api.ProviderSHM))

	a := newAllocator(t, 24000, 24001)
	a.listen = func(string, string) (net.Listener, error) {
		t.Fatal("a non-tcp provider must not consult the kernel's TCP port table")
		return nil, nil
	}
	_, err := a.Allocate("s1/target", api.ProviderVerbs)
	require.NoError(t, err)
}

func TestAllocateRejectsAnEmptyOwner(t *testing.T) {
	_, err := newAllocator(t, 24000, 24010).Allocate("", api.ProviderTCP)
	assert.ErrorContains(t, err, "no owner")
}

type nopListener struct{}

func (nopListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (nopListener) Close() error              { return nil }
func (nopListener) Addr() net.Addr            { return nil }
