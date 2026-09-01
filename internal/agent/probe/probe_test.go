package probe

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/worker/exec"
)

func iface(provider api.Provider, address, device string, flags ...api.CapFlag) exec.Interface {
	entry := exec.Interface{
		Provider: provider,
		Address:  address,
		Caps:     exec.InterfaceCaps{Flags: flags, MaxMessageSize: 1 << 20},
	}
	if device != "" {
		entry.Attr = json.RawMessage(`{"device_name":"` + device + `"}`)
	}
	return entry
}

func noInterfaces(string) ([]string, error) { return nil, errors.New("no such network interface") }

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		attachment Attachment
		wants      string
	}{
		{"ok, selectorless", Attachment{Provider: api.ProviderEFA, Fabric: "vpc1"}, ""},
		{"ok, address", Attachment{Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.0.0.1"}, ""},
		{"ok, shm needs no fabric", Attachment{Provider: api.ProviderSHM}, ""},
		{"no provider", Attachment{Fabric: "dc1"}, "provider is required"},
		{"unknown provider", Attachment{Provider: "sockets", Fabric: "dc1"}, "not one this project can negotiate"},
		{"no fabric", Attachment{Provider: api.ProviderTCP}, "fabric label is required"},
		{
			"two selectors",
			Attachment{Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.0.0.1", Interface: "eth1"},
			"at most one of address, interface or device",
		},
		{
			// Rejected here rather than dropped later, because the probe has no netdev name for
			// efa and the drop would read as missing hardware.
			"interface on efa",
			Attachment{Provider: api.ProviderEFA, Fabric: "vpc1", Interface: "efa0"},
			"efa attachment is selected by device",
		},
		{
			// The narrowing selectors are not naming selectors, and combining them with one — and
			// with each other — is the case they exist for.
			"ok, a name narrowed twice",
			Attachment{Provider: api.ProviderVerbs, Fabric: "ib-a", Device: "mlx5_0", Network: "10.1.0.0/16", IPVersion: 4},
			"",
		},
		{"ok, network alone", Attachment{Provider: api.ProviderTCP, Fabric: "dc1", Network: "fd00:1::/64"}, ""},
		{"bad ip_version", Attachment{Provider: api.ProviderTCP, Fabric: "dc1", IPVersion: 5}, "want 4 or 6"},
		{"bad network", Attachment{Provider: api.ProviderTCP, Fabric: "dc1", Network: "10.1.0.0"}, "not a CIDR prefix"},
		{
			// Matches nothing on any node, so left to the join it would present as a drop on the
			// whole fleet rather than as the configuration error it is.
			"contradictory network and ip_version",
			Attachment{Provider: api.ProviderTCP, Fabric: "dc1", Network: "10.1.0.0/16", IPVersion: 6},
			"is IPv4 and contradicts ip_version: 6",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.attachment.Validate()
			if tc.wants == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tc.wants)
		})
	}
}

// The fourth row of the M0 table, and the one that actually resolves efa and shm: no selector at
// all, unambiguous because the node has exactly one.
func TestSelectorlessMatchesTheSoleEntry(t *testing.T) {
	result := Join(
		[]Attachment{{Provider: api.ProviderEFA, Fabric: "vpc1-subnet-a"}},
		[]exec.Interface{
			iface(api.ProviderEFA, "fe80::1", "rdmap0s6-rdm", api.CapRemoteWrite, api.CapSendReceive),
			iface(api.ProviderTCP, "10.0.0.1", "eth0", api.CapSendReceive),
		},
		Options{Node: "edge-01", Interfaces: noInterfaces},
	)

	require.Empty(t, result.Dropped)
	require.Len(t, result.Attachments, 1)
	assert.Equal(t, api.FabricAttachment{
		Provider:       api.ProviderEFA,
		Fabric:         "vpc1-subnet-a",
		Address:        "fe80::1",
		CapFlags:       []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive},
		MaxMessageSize: 1 << 20,
		Device:         "rdmap0s6-rdm",
	}, result.Attachments[0])
}

// An ambiguous selectorless match must refuse and hand the operator the exact strings they could
// have written — that is the "typo'd ib0" vs "no verbs here" distinction of §10.5, served better
// by a candidate list than by a failed name match.
func TestAmbiguousSelectorlessMatchListsCandidates(t *testing.T) {
	result := Join(
		[]Attachment{{Provider: api.ProviderTCP, Fabric: "dc1"}},
		[]exec.Interface{
			iface(api.ProviderTCP, "10.0.0.1", "eth0", api.CapSendReceive),
			iface(api.ProviderTCP, "127.0.0.1", "lo", api.CapSendReceive),
		},
		Options{Node: "edge-01", Interfaces: noInterfaces},
	)

	assert.Empty(t, result.Attachments)
	require.Len(t, result.Dropped, 1)
	assert.Contains(t, result.Dropped[0].Reason, "2 tcp interfaces")
	assert.ElementsMatch(t,
		[]string{"10.0.0.1 (device eth0)", "127.0.0.1 (device lo)"},
		result.Dropped[0].Candidates)
}

func TestAddressAndDeviceSelectors(t *testing.T) {
	probed := []exec.Interface{
		iface(api.ProviderVerbs, "10.1.0.7", "mlx5_0", api.CapRemoteWrite),
		iface(api.ProviderVerbs, "10.2.0.7", "mlx5_1", api.CapRemoteWrite),
	}

	result := Join([]Attachment{
		{Provider: api.ProviderVerbs, Fabric: "ib-a", Address: "10.1.0.7"},
		{Provider: api.ProviderVerbs, Fabric: "ib-b", Device: "mlx5_1"},
	}, probed, Options{Node: "edge-01", Interfaces: noInterfaces})

	require.Empty(t, result.Dropped)
	require.Len(t, result.Attachments, 2)
	assert.Equal(t, "ib-a", result.Attachments[0].Fabric)
	assert.Equal(t, "10.1.0.7", result.Attachments[0].Address)
	assert.Equal(t, "ib-b", result.Attachments[1].Fabric)
	assert.Equal(t, "10.2.0.7", result.Attachments[1].Address)
}

// `interface:` is resolved locally, against the kernel's own view, and never reaches the wire.
func TestInterfaceSelectorResolvesLocally(t *testing.T) {
	resolve := func(name string) ([]string, error) {
		if name != "eth1" {
			return nil, errors.New("no such network interface")
		}
		return []string{"10.0.0.1", "fe80::dead:beef"}, nil
	}

	result := Join(
		[]Attachment{{Provider: api.ProviderTCP, Fabric: "dc1", Interface: "eth1"}},
		[]exec.Interface{
			iface(api.ProviderTCP, "127.0.0.1", "lo", api.CapSendReceive),
			iface(api.ProviderTCP, "10.0.0.1", "eth1", api.CapSendReceive),
		},
		Options{Node: "edge-01", Interfaces: resolve},
	)

	require.Empty(t, result.Dropped)
	require.Len(t, result.Attachments, 1)
	assert.Equal(t, "10.0.0.1", result.Attachments[0].Address)
	assert.Equal(t, "eth1", result.Attachments[0].Device)
}

func TestInterfaceSelectorReportsAnUnknownName(t *testing.T) {
	result := Join(
		[]Attachment{{Provider: api.ProviderTCP, Fabric: "dc1", Interface: "ib0"}},
		[]exec.Interface{iface(api.ProviderTCP, "10.0.0.1", "eth0", api.CapSendReceive)},
		Options{Node: "edge-01", Interfaces: noInterfaces},
	)

	assert.Empty(t, result.Attachments)
	require.Len(t, result.Dropped, 1)
	assert.Contains(t, result.Dropped[0].Reason, `interface "ib0"`)
	assert.NotEmpty(t, result.Dropped[0].Candidates, "the operator gets told what this node does have")
}

// "This node has no verbs" reads differently from "someone typo'd ib0": no candidates at all.
func TestNoProviderAtAllIsDistinguishable(t *testing.T) {
	result := Join(
		[]Attachment{{Provider: api.ProviderVerbs, Fabric: "ib-a"}},
		[]exec.Interface{iface(api.ProviderTCP, "10.0.0.1", "eth0", api.CapSendReceive)},
		Options{Node: "edge-01", Interfaces: noInterfaces},
	)

	require.Len(t, result.Dropped, 1)
	assert.Contains(t, result.Dropped[0].Reason, "no verbs interface on this node at all")
	assert.Empty(t, result.Dropped[0].Candidates)
}

// shm's label is derived, never configured, so that same-node-only falls out of ordinary fabric
// matching and both sides derive the identical string (§10.1).
func TestSHMLabelIsDerivedFromTheNodeName(t *testing.T) {
	result := Join(
		[]Attachment{{Provider: api.ProviderSHM, Fabric: "whatever-the-operator-wrote"}},
		[]exec.Interface{iface(api.ProviderSHM, "edge-01.local", "", api.CapSendReceive)},
		Options{Node: "edge-01", Interfaces: noInterfaces},
	)

	require.Empty(t, result.Dropped)
	require.Len(t, result.Attachments, 1)
	assert.Equal(t, api.SHMFabric("edge-01"), result.Attachments[0].Fabric)
	assert.Empty(t, result.Attachments[0].Device, "shm reports no device name")
}

// One typo must not take out a node that also has a working attachment.
func TestOneBadAttachmentDoesNotDropTheGoodOne(t *testing.T) {
	result := Join([]Attachment{
		{Provider: api.ProviderVerbs, Fabric: "ib-a", Address: "10.9.9.9"},
		{Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.0.0.1"},
	}, []exec.Interface{
		iface(api.ProviderVerbs, "10.1.0.7", "mlx5_0", api.CapRemoteWrite),
		iface(api.ProviderTCP, "10.0.0.1", "eth0", api.CapSendReceive),
	}, Options{Node: "edge-01", Interfaces: noInterfaces})

	require.Len(t, result.Attachments, 1)
	assert.Equal(t, api.ProviderTCP, result.Attachments[0].Provider)
	require.Len(t, result.Dropped, 1)
	assert.Equal(t, api.ProviderVerbs, result.Dropped[0].Attachment.Provider)
}

// efa addresses are the link-local, hardware-derived kind, and a zone suffix on one side is a
// spelling difference rather than a different address.
func TestAddressComparisonNormalises(t *testing.T) {
	assert.True(t, sameAddress("fe80::1%eth0", "fe80::1"))
	assert.True(t, sameAddress("fe80:0:0:0:0:0:0:1", "fe80::1"))
	assert.True(t, sameAddress("10.0.0.1", "10.0.0.1"))
	assert.False(t, sameAddress("10.0.0.1", "10.0.0.2"))
	// Not everything is an IP; a device address compares as text.
	assert.True(t, sameAddress("edge-01.local", "edge-01.local"))
	assert.False(t, sameAddress("edge-01.local", "edge-02.local"))
}

// Capability flags travel verbatim in the spelling the probe printed, so the server's
// intersection and the worker's caps_flags key are one vocabulary end to end (§10.3, WRS §3).
func TestCapabilitiesTravelVerbatim(t *testing.T) {
	result := Join(
		[]Attachment{{Provider: api.ProviderTCP, Fabric: "dc1"}},
		[]exec.Interface{{
			Provider: api.ProviderTCP,
			Address:  "10.0.0.1",
			Caps: exec.InterfaceCaps{
				Flags:          []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive, api.CapBlockingOperations},
				MaxMessageSize: 1<<64 - 1,
			},
		}},
		Options{Node: "edge-01", Interfaces: noInterfaces},
	)

	require.Len(t, result.Attachments, 1)
	assert.Equal(t,
		[]api.CapFlag{api.CapRemoteWrite, api.CapSendReceive, api.CapBlockingOperations},
		result.Attachments[0].CapFlags)
	assert.Equal(t, uint64(1<<64-1), result.Attachments[0].MaxMessageSize,
		"UINT64_MAX must survive; a float64 anywhere on this path loses it")
}

func TestNetdevAddressesReportsAnUnknownInterface(t *testing.T) {
	_, err := NetdevAddresses("definitely-not-an-interface")
	assert.ErrorContains(t, err, "no such network interface")
}

// The DaemonSet case §10.1 grew these for: one HCA reporting a v4 address and a link-local v6 one
// is two probe entries under one device name, and `device:` alone cannot say which. Neither can
// `address:`, without a per-node overlay to state a fact that is true of the whole fleet.
func TestIPVersionDisambiguatesADevice(t *testing.T) {
	probed := []exec.Interface{
		iface(api.ProviderVerbs, "10.1.0.7", "mlx5_0", api.CapRemoteWrite),
		iface(api.ProviderVerbs, "fe80::1%ib0", "mlx5_0", api.CapRemoteWrite),
	}

	ambiguous := Join(
		[]Attachment{{Provider: api.ProviderVerbs, Fabric: "ib-a", Device: "mlx5_0"}},
		probed, Options{Node: "edge-01", Interfaces: noInterfaces})
	require.Len(t, ambiguous.Dropped, 1)
	assert.Contains(t, ambiguous.Dropped[0].Reason, "matches 2 verbs interfaces")

	for _, tc := range []struct {
		version int
		wants   string
	}{{4, "10.1.0.7"}, {6, "fe80::1%ib0"}} {
		result := Join(
			[]Attachment{{Provider: api.ProviderVerbs, Fabric: "ib-a", Device: "mlx5_0", IPVersion: tc.version}},
			probed, Options{Node: "edge-01", Interfaces: noInterfaces})

		require.Empty(t, result.Dropped)
		require.Len(t, result.Attachments, 1)
		assert.Equal(t, tc.wants, result.Attachments[0].Address)
	}
}

// `network:` is the selector that is exact without being per-node: the same fleet-wide value picks
// each node's own address on the storage network, naming no device and no interface.
func TestNetworkSelectsWithoutAPerNodeValue(t *testing.T) {
	result := Join(
		[]Attachment{{Provider: api.ProviderTCP, Fabric: "dc1-data", Network: "10.1.0.0/16"}},
		[]exec.Interface{
			iface(api.ProviderTCP, "127.0.0.1", "lo", api.CapSendReceive),
			iface(api.ProviderTCP, "10.9.0.4", "eth0", api.CapSendReceive),
			iface(api.ProviderTCP, "10.1.0.7", "ens5f0", api.CapSendReceive),
		},
		Options{Node: "edge-01", Interfaces: noInterfaces},
	)

	require.Empty(t, result.Dropped)
	require.Len(t, result.Attachments, 1)
	assert.Equal(t, "10.1.0.7", result.Attachments[0].Address)
}

// A prefix written with host bits set is what an operator copies off an `ip addr` line, and it
// means the network it names rather than nothing.
func TestNetworkAcceptsHostBits(t *testing.T) {
	result := Join(
		[]Attachment{{Provider: api.ProviderTCP, Fabric: "dc1-data", Network: "10.1.0.5/16"}},
		[]exec.Interface{
			iface(api.ProviderTCP, "10.9.0.4", "eth0", api.CapSendReceive),
			iface(api.ProviderTCP, "10.1.0.7", "ens5f0", api.CapSendReceive),
		},
		Options{Node: "edge-01", Interfaces: noInterfaces},
	)

	require.Empty(t, result.Dropped)
	require.Len(t, result.Attachments, 1)
	assert.Equal(t, "10.1.0.7", result.Attachments[0].Address)
}

// The narrowing selectors conjoin — with a naming selector and with each other — and the drop
// reason names every one of them, so the operator sees the conjunction they actually wrote.
func TestSelectorsConjoinAndTheReasonNamesThem(t *testing.T) {
	result := Join(
		[]Attachment{{
			Provider: api.ProviderVerbs, Fabric: "ib-a",
			Device: "mlx5_0", Network: "10.1.0.0/16", IPVersion: 4,
		}},
		[]exec.Interface{
			iface(api.ProviderVerbs, "10.1.0.7", "mlx5_1", api.CapRemoteWrite),
			iface(api.ProviderVerbs, "10.2.0.7", "mlx5_0", api.CapRemoteWrite),
		},
		Options{Node: "edge-01", Interfaces: noInterfaces},
	)

	assert.Empty(t, result.Attachments)
	require.Len(t, result.Dropped, 1)
	assert.Equal(t,
		"no verbs interface matches device: mlx5_0 and network: 10.1.0.0/16 and ip_version: 4",
		result.Dropped[0].Reason)
}

// An address that is not an IP at all — shm reports the hostname — cannot be inside a prefix or be
// version 4 or 6. Excluded rather than treated as a parse error, because it is not one.
func TestNarrowingExcludesNonIPAddresses(t *testing.T) {
	result := Join(
		[]Attachment{{Provider: api.ProviderSHM, IPVersion: 4}},
		[]exec.Interface{iface(api.ProviderSHM, "edge-01.local", "", api.CapSendReceive)},
		Options{Node: "edge-01", Interfaces: noInterfaces},
	)

	assert.Empty(t, result.Attachments)
	require.Len(t, result.Dropped, 1)
	assert.Contains(t, result.Dropped[0].Reason, "no shm interface matches ip_version: 4")
}
