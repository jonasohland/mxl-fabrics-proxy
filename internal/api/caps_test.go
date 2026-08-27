package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The capability names are the vocabulary the whole system shares: the worker's --interfaces
// probe prints them, its caps_flags config key accepts them, and the server intersects them
// without translating. A rename here silently breaks the C++ boundary, which no Go test can
// catch — so pin the literals (WRS §2, §3).
func TestCapFlagNamesMatchTheWorker(t *testing.T) {
	t.Parallel()

	assert.Equal(t, CapFlag("REMOTE_WRITE"), CapRemoteWrite)
	assert.Equal(t, CapFlag("SEND_RECEIVE"), CapSendReceive)
	assert.Equal(t, CapFlag("BLOCKING_OPERATIONS"), CapBlockingOperations)

	assert.Equal(t, Provider("tcp"), ProviderTCP)
	assert.Equal(t, Provider("verbs"), ProviderVerbs)
	assert.Equal(t, Provider("efa"), ProviderEFA)
	assert.Equal(t, Provider("shm"), ProviderSHM)
}

// Providers do report UINT64_MAX, and a float64 anywhere in the decoding path loses it — the
// probe's own documentation calls this out (WRS §2). The typed decode must be exact.
func TestMaxMessageSizeSurvivesUint64Max(t *testing.T) {
	t.Parallel()

	const maxUint64 = ^uint64(0)
	const body = `{"provider":"tcp","caps_flags":["REMOTE_WRITE"],"max_message_size":18446744073709551615}`

	var cfg InterfaceConfig
	require.NoError(t, json.Unmarshal([]byte(body), &cfg))
	assert.Equal(t, maxUint64, cfg.MaxMessageSize)

	encoded, err := json.Marshal(cfg)
	require.NoError(t, err)
	// Compared as bytes, not with JSONEq: JSONEq decodes both sides through float64, which is
	// precisely the loss this test exists to catch.
	assert.Contains(t, string(encoded), "18446744073709551615")

	// The hazard itself, recorded so the reason for the typed struct is visible: decoding the
	// same bytes into an untyped map goes through float64 and cannot get back.
	var loose map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &loose))
	reencoded, err := json.Marshal(loose)
	require.NoError(t, err)
	assert.NotContains(t, string(reencoded), "18446744073709551615")
}

func TestInterfaceConfigTransferCapability(t *testing.T) {
	t.Parallel()

	// At least one of REMOTE_WRITE or SEND_RECEIVE must survive the intersection, or the pair
	// is not viable (§10.3).
	assert.True(t, InterfaceConfig{CapFlags: []CapFlag{CapRemoteWrite}}.CanTransfer())
	assert.True(t, InterfaceConfig{CapFlags: []CapFlag{CapSendReceive}}.CanTransfer())
	assert.False(t, InterfaceConfig{CapFlags: []CapFlag{CapBlockingOperations}}.CanTransfer())
	assert.False(t, InterfaceConfig{}.CanTransfer())

	cfg := InterfaceConfig{CapFlags: []CapFlag{CapRemoteWrite, CapBlockingOperations}}
	assert.True(t, cfg.HasCap(CapBlockingOperations))
	assert.False(t, cfg.HasCap(CapSendReceive))
}

// Two nodes may pair on a provider iff they share a fabric label for it: same provider on a
// different fabric is not a match, which is the entire point of §10.1.
func TestCapabilitiesFindFabric(t *testing.T) {
	t.Parallel()

	caps := Capabilities{Fabrics: []FabricAttachment{
		{Provider: ProviderVerbs, Fabric: "ib-fabric-a", Address: "10.0.1.7"},
		{Provider: ProviderVerbs, Fabric: "ib-fabric-b", Address: "10.0.2.7"},
		{Provider: ProviderTCP, Fabric: "dc1-data", Address: "10.1.1.7"},
	}}

	found := caps.FindFabric(ProviderVerbs, "ib-fabric-b")
	require.NotNil(t, found)
	assert.Equal(t, "10.0.2.7", found.Address)

	assert.Nil(t, caps.FindFabric(ProviderVerbs, "dc1-data"))
	assert.Nil(t, caps.FindFabric(ProviderEFA, "ib-fabric-a"))
}

func TestKnownProvider(t *testing.T) {
	t.Parallel()

	for _, provider := range DefaultProviderOrder {
		assert.True(t, KnownProvider(provider))
	}

	// "any" is a worker config value, not a negotiation outcome: the server always resolves a
	// concrete provider before anything is assigned.
	assert.False(t, KnownProvider(Provider("any")))
	assert.False(t, KnownProvider(Provider("")))
}

// A domain the agent merely discovered by a search path must never be usable as a destination,
// and the field that says so must fail closed for an agent that does not set it (§7.2, §13).
func TestDomainMappingConfiguredFailsClosed(t *testing.T) {
	t.Parallel()

	var mapping DomainMapping
	require.NoError(t, json.Unmarshal([]byte(`{"name":"ingest"}`), &mapping))
	assert.False(t, mapping.Configured)
}
