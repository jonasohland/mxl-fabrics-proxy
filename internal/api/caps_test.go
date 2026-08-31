package api

import (
	"encoding/json"
	"strings"
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

// Where the fail-closed direction lives. Both grants default false, so a registration that names
// no area at all offers no sources and accepts no destinations — which is both the default and
// what an agent whose operator has not opted in reports (§10.6, §13).
func TestCapabilitiesWithoutAreasAreNeitherSourceNorDestination(t *testing.T) {
	t.Parallel()

	var caps Capabilities
	require.NoError(t, json.Unmarshal([]byte(`{"fabrics":[],"sched_prio":true}`), &caps))
	assert.Empty(t, caps.Areas)
	assert.Nil(t, caps.FindArea(""), "the empty area name is not an area")
	assert.Nil(t, caps.FindArea("fast"))

	// And an area decoded with no grants spelled out grants nothing, rather than defaulting to
	// something an operator did not ask for.
	var area Area
	require.NoError(t, json.Unmarshal([]byte(`{"name":"fast","path":"/dev/shm/mxl"}`), &area))
	assert.False(t, area.Read)
	assert.False(t, area.Write)
}

func TestCapabilitiesFindArea(t *testing.T) {
	t.Parallel()

	caps := Capabilities{Areas: []Area{
		{Name: "fast", Path: "/dev/shm/mxl", Read: true, Write: true},
		{Name: "bulk", Path: "/mnt/nvme/mxl", Read: true},
	}}

	require.NotNil(t, caps.FindArea("bulk"))
	assert.Equal(t, "/mnt/nvme/mxl", caps.FindArea("bulk").Path)
	assert.False(t, caps.FindArea("bulk").Write, "the grants travel with the entry")
	assert.Nil(t, caps.FindArea("slow"))

	// A path is advertised for diagnostics but is never required: the server sends an area *name*
	// to an agent and the agent resolves it, exactly as with a domain's elements (§10.2).
	encoded, err := json.Marshal(Area{Name: "fast", Read: true})
	require.NoError(t, err)
	assert.Equal(t, `{"name":"fast","read":true,"write":false}`, string(encoded))
}

// The identity grammar itself: `<area>/<elements>`, injective because neither half can contain the
// separator, and nesting compared within one area (§10.6).
func TestDomainIdentity(t *testing.T) {
	t.Parallel()

	d := Domain{Area: "fast", Elements: []string{"studio-a", "cam1"}}
	assert.Equal(t, "fast/studio-a/cam1", d.String())
	require.NoError(t, d.Valid())

	assert.True(t, d.Equal(Domain{Area: "fast", Elements: []string{"studio-a", "cam1"}}))
	assert.False(t, d.Equal(Domain{Area: "bulk", Elements: []string{"studio-a", "cam1"}}),
		"two areas holding one element list are two domains, which is the whole point")

	// Nesting is an exact slice prefix, so `studio-ab` is not a child of `studio-a` — the trap the
	// string spelling of this question has to work around.
	assert.True(t, d.NestedIn(Domain{Area: "fast", Elements: []string{"studio-a"}}))
	assert.False(t, d.NestedIn(Domain{Area: "fast", Elements: []string{"studio-ab"}}))
	assert.False(t, d.NestedIn(Domain{Area: "bulk", Elements: []string{"studio-a"}}),
		"two areas are two directory trees and cannot nest")
	assert.False(t, d.NestedIn(d), "a domain does not nest in itself")

	// An area's own directory is not a domain, and a domain names an area.
	assert.Error(t, Domain{Area: "fast"}.Valid())
	assert.Error(t, Domain{Elements: []string{"ingest"}}.Valid())
	assert.Error(t, Domain{Area: "fast/inner", Elements: []string{"ingest"}}.Valid())

	// The rendered cap counts the area segment. Measuring only the elements would let it loosen
	// silently the day the area moved into the name (§10.6).
	long := Domain{Area: strings.Repeat("a", MaxDomainNameLen), Elements: []string{}}
	for range MaxDomainElements {
		long.Elements = append(long.Elements, strings.Repeat("b", MaxDomainNameLen))
	}
	assert.Error(t, long.Valid())
}

// The rule every layer defers to, pinned once here because it is shared: the server rejects a
// request naming something else and the agent independently refuses to resolve it, and the
// duplication is only worth its keep if the two cannot disagree (§10.6, §13).
func TestValidDomainName(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"ingest", "ingest-2", "ingest_2", "a.b", "A1", "0",
		strings.Repeat("a", MaxDomainNameLen)} {
		assert.NoError(t, ValidDomainName(name), "%q", name)
	}

	for _, name := range []string{
		"", ".", "..", "../etc", "a/b", "/etc", `a\b`,
		"ingest\x00sh", "ingest sh", "ingest\n",
		".hidden", "-flag",
		"ingést",     // non-ASCII
		"іngest",     // Cyrillic і, a lookalike for a name an operator may legitimately hold
		"ingest⁄etc", // fraction slash, a lookalike for a separator
		strings.Repeat("a", MaxDomainNameLen+1),
	} {
		assert.Error(t, ValidDomainName(name), "%q", name)
	}

	// A refused byte is reported as a byte rather than decoded back into the message. These names
	// are frequently lookalikes for names the rule accepts, and rendering one back to an operator
	// as though it were ASCII makes the error part of the confusion.
	assert.Contains(t, ValidDomainName("іngest").Error(), "0xd1")
}
