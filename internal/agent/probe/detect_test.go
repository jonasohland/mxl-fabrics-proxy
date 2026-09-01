package probe

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/worker/exec"
)

// The preference order is the server's own (§10.4), so an detecting node lands where
// negotiation would have taken it if every node offered everything.
func TestDetectPrefersTheFastestProvider(t *testing.T) {
	probed := []exec.Interface{
		iface(api.ProviderTCP, "10.0.0.1", "eth0", api.CapRemoteWrite),
		iface(api.ProviderVerbs, "10.1.0.1", "mlx5_0", api.CapRemoteWrite),
		iface(api.ProviderEFA, "fe80::1", "rdmap0s6-rdm", api.CapRemoteWrite),
		iface(api.ProviderSHM, "host-a", "", api.CapSendReceive),
	}

	detected, skipped := Detect(probed, api.DefaultProviderOrder)
	assert.Equal(t, api.ProviderEFA, detected.Provider)
	assert.Equal(t, "fe80::1", detected.Address)
	assert.Equal(t, DefaultFabric, detected.Fabric)
	assert.Empty(t, skipped, "efa is first in the order and was there")

	// Without efa it falls to verbs, and the skip is recorded rather than left silent: an
	// operator who expected efa needs to see that libfabric reported none.
	detected, skipped = Detect(probed[:2], api.DefaultProviderOrder)
	assert.Equal(t, api.ProviderVerbs, detected.Provider)
	require.Len(t, skipped, 1)
	assert.Contains(t, skipped[0], "efa: libfabric reports none")
}

// The tcp rule, and the reason this function exists at all: the machine running it has half a
// dozen addresses and all but one of them are wrong.
func TestDetectSkipsUnreachableTCPAddresses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		address string
		usable  bool
	}{
		{"routable rfc1918", "10.0.0.1", true},
		{"public", "203.0.113.4", true},
		{"loopback", "127.0.0.1", false},
		{"cgnat, what a CNI hands out", "100.64.3.7", false},
		{"cgnat, top of the range", "100.127.255.254", false},
		{"just below cgnat", "100.63.255.255", true},
		{"just above cgnat", "100.128.0.1", true},
		{"link-local, the failure mode of dhcp", "169.254.10.1", false},
		{"unspecified", "0.0.0.0", false},
		{"ipv6", "2001:db8::1", false},
		{"ipv6 link-local with a zone the peer cannot use", "fe80::1%eth0", false},
		{"not an address at all", "host-a", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detected, skipped := Detect(
				[]exec.Interface{iface(api.ProviderTCP, tc.address, "eth0", api.CapRemoteWrite)},
				[]api.Provider{api.ProviderTCP},
			)
			if tc.usable {
				assert.Equal(t, tc.address, detected.Address)
				return
			}
			assert.Empty(t, detected.Provider)
			require.Len(t, skipped, 1)
			assert.Contains(t, skipped[0], "another node could reach")
			assert.Contains(t, skipped[0], tc.address, "the skip names what it rejected")
		})
	}
}

// First usable in probe order, and the addresses ahead of it do not disqualify the provider.
func TestDetectTakesTheFirstUsableAddress(t *testing.T) {
	detected, skipped := Detect([]exec.Interface{
		iface(api.ProviderTCP, "127.0.0.1", "lo", api.CapRemoteWrite),
		iface(api.ProviderTCP, "100.96.1.4", "cni0", api.CapRemoteWrite),
		iface(api.ProviderTCP, "192.168.1.10", "eth0", api.CapRemoteWrite),
		iface(api.ProviderTCP, "192.168.2.10", "eth1", api.CapRemoteWrite),
	}, api.DefaultProviderOrder)

	assert.Equal(t, "192.168.1.10", detected.Address)
	assert.Len(t, skipped, 2, "efa and verbs, not the tcp addresses passed over")
}

// A tcp node behind a CNI with nothing else falls all the way through to shm rather than
// advertising an address no peer can reach. That node can still replicate between its own
// domains, which is strictly more than a wrong tcp attachment would have given it.
func TestDetectFallsThroughToSHM(t *testing.T) {
	detected, skipped := Detect([]exec.Interface{
		iface(api.ProviderTCP, "100.96.1.4", "cni0", api.CapRemoteWrite),
		iface(api.ProviderSHM, "host-a", "", api.CapSendReceive),
	}, api.DefaultProviderOrder)

	assert.Equal(t, api.ProviderSHM, detected.Provider)
	assert.Equal(t, "host-a", detected.Address, "shm reports the hostname and is not address-filtered")
	assert.Len(t, skipped, 3)

	// Nothing at all is a node that can do nothing, and it says so rather than returning a
	// half-built attachment.
	detected, skipped = Detect(nil, api.DefaultProviderOrder)
	assert.Empty(t, detected.Provider)
	assert.Len(t, skipped, 4)
}

// Detection produces *configuration*: what it returns goes through the ordinary join, which is
// what verifies it, carries the capability flags across and derives shm's label from the node
// name rather than leaving it as "default".
func TestDetectResultSurvivesTheJoin(t *testing.T) {
	probed := []exec.Interface{
		iface(api.ProviderTCP, "127.0.0.1", "lo", api.CapRemoteWrite),
		iface(api.ProviderTCP, "10.0.0.1", "eth0", api.CapRemoteWrite, api.CapSendReceive),
	}

	detected, _ := Detect(probed, api.DefaultProviderOrder)
	result := Join([]Attachment{detected}, probed, Options{Node: "edge-01", Interfaces: noInterfaces})

	require.Empty(t, result.Dropped)
	require.Len(t, result.Attachments, 1)
	assert.Equal(t, api.ProviderTCP, result.Attachments[0].Provider)
	assert.Equal(t, "default", result.Attachments[0].Fabric)
	assert.Equal(t, "10.0.0.1", result.Attachments[0].Address)
	assert.Equal(t, "eth0", result.Attachments[0].Device)
	assert.Equal(t, []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive}, result.Attachments[0].CapFlags)

	// shm keeps its derived label, because the join owns that and detection does not know it.
	detected, _ = Detect([]exec.Interface{iface(api.ProviderSHM, "host-a", "", api.CapSendReceive)}, api.DefaultProviderOrder)
	result = Join([]Attachment{detected}, []exec.Interface{iface(api.ProviderSHM, "host-a", "", api.CapSendReceive)},
		Options{Node: "edge-01", Interfaces: noInterfaces})

	require.Len(t, result.Attachments, 1)
	assert.Equal(t, api.SHMFabric("edge-01"), result.Attachments[0].Fabric)
}

// Whatever it returns must be a legal configured attachment, or the join would drop the thing
// this feature exists to produce.
func TestDetectResultValidates(t *testing.T) {
	for _, provider := range api.DefaultProviderOrder {
		detected, _ := Detect([]exec.Interface{iface(provider, "10.0.0.1", "dev0")}, api.DefaultProviderOrder)
		require.Equal(t, provider, detected.Provider)
		assert.NoError(t, detected.Validate(), "%s", provider)
	}
}
