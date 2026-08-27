package negotiate

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// attach builds an attachment with both transfer capabilities, which is the ordinary case; the
// capability tests below spell out their own.
func attach(provider api.Provider, fabric, address string) api.FabricAttachment {
	return api.FabricAttachment{
		Provider:       provider,
		Fabric:         fabric,
		Address:        address,
		CapFlags:       []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive},
		MaxMessageSize: 1 << 20,
	}
}

func TestNegotiate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		src, dst []api.FabricAttachment
		pin      api.ProviderPin
		order    []api.Provider

		wantProvider api.Provider
		wantFabric   string
		wantCaps     []api.CapFlag
		wantMaxSize  uint64
		wantCode     api.ReasonCode
	}{
		{
			name:         "single shared pair",
			src:          []api.FabricAttachment{attach(api.ProviderTCP, "dc1-data", "10.1.1.7")},
			dst:          []api.FabricAttachment{attach(api.ProviderTCP, "dc1-data", "10.1.1.8")},
			wantProvider: api.ProviderTCP,
			wantFabric:   "dc1-data",
			wantCaps:     []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive},
			wantMaxSize:  1 << 20,
		},
		{
			name: "preference order picks verbs over tcp",
			src: []api.FabricAttachment{
				attach(api.ProviderTCP, "dc1-data", "10.1.1.7"),
				attach(api.ProviderVerbs, "ib-a", "10.0.1.7"),
			},
			dst: []api.FabricAttachment{
				attach(api.ProviderTCP, "dc1-data", "10.1.1.8"),
				attach(api.ProviderVerbs, "ib-a", "10.0.1.8"),
			},
			wantProvider: api.ProviderVerbs,
			wantFabric:   "ib-a",
		},
		{
			name: "configured order overrides the default",
			src: []api.FabricAttachment{
				attach(api.ProviderTCP, "dc1-data", "10.1.1.7"),
				attach(api.ProviderVerbs, "ib-a", "10.0.1.7"),
			},
			dst: []api.FabricAttachment{
				attach(api.ProviderTCP, "dc1-data", "10.1.1.8"),
				attach(api.ProviderVerbs, "ib-a", "10.0.1.8"),
			},
			order:        []api.Provider{api.ProviderTCP, api.ProviderVerbs},
			wantProvider: api.ProviderTCP,
			wantFabric:   "dc1-data",
		},
		{
			// A provider left out of the operator's order is still negotiable; it just sorts
			// last. Shortening the order is a preference, not a deny-list — that is what the pin
			// is for.
			name:         "a provider absent from the order still negotiates",
			src:          []api.FabricAttachment{attach(api.ProviderSHM, api.SHMFabric("n1"), "n1")},
			dst:          []api.FabricAttachment{attach(api.ProviderSHM, api.SHMFabric("n1"), "n1")},
			order:        []api.Provider{api.ProviderEFA, api.ProviderVerbs},
			wantProvider: api.ProviderSHM,
			wantFabric:   api.SHMFabric("n1"),
		},
		{
			// §10.1: two nodes both offering verbs may simply be on different InfiniBand
			// fabrics. Set intersection on provider names alone would assign a session that
			// cannot connect, and it would fail in the least legible way there is.
			name:     "same provider on different fabrics is not a pair",
			src:      []api.FabricAttachment{attach(api.ProviderVerbs, "ib-a", "10.0.1.7")},
			dst:      []api.FabricAttachment{attach(api.ProviderVerbs, "ib-b", "10.0.2.8")},
			wantCode: api.ReasonNoSharedFabric,
		},
		{
			name:     "shared fabric label but no shared provider on it",
			src:      []api.FabricAttachment{attach(api.ProviderVerbs, "dc1", "10.0.1.7")},
			dst:      []api.FabricAttachment{attach(api.ProviderTCP, "dc1", "10.1.1.8")},
			wantCode: api.ReasonNoSharedProvider,
		},
		{
			name: "shared pair with no transfer capability in common",
			src: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.1.1.7",
				CapFlags: []api.CapFlag{api.CapRemoteWrite, api.CapBlockingOperations},
			}},
			dst: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1", Address: "10.1.1.8",
				CapFlags: []api.CapFlag{api.CapSendReceive, api.CapBlockingOperations},
			}},
			wantCode: api.ReasonNoSharedCapability,
		},
		{
			name:     "no attachments at all on one side",
			src:      []api.FabricAttachment{attach(api.ProviderTCP, "dc1", "10.1.1.7")},
			dst:      nil,
			wantCode: api.ReasonNoSharedFabric,
		},
		{
			name:         "pin honoured",
			src:          []api.FabricAttachment{attach(api.ProviderTCP, "dc1", "10.1.1.7"), attach(api.ProviderVerbs, "ib-a", "10.0.1.7")},
			dst:          []api.FabricAttachment{attach(api.ProviderTCP, "dc1", "10.1.1.8"), attach(api.ProviderVerbs, "ib-a", "10.0.1.8")},
			pin:          api.ProviderPin{api.ProviderTCP},
			wantProvider: api.ProviderTCP,
			wantFabric:   "dc1",
		},
		{
			// §10.4, invariant 7: an explicit provider is honoured or the request fails. Landing
			// on tcp when verbs was asked for is a performance cliff whose dropped grains look
			// like a source problem rather than a routing decision made on the operator's behalf.
			name:     "pin never substituted",
			src:      []api.FabricAttachment{attach(api.ProviderTCP, "dc1", "10.1.1.7")},
			dst:      []api.FabricAttachment{attach(api.ProviderTCP, "dc1", "10.1.1.8")},
			pin:      api.ProviderPin{api.ProviderVerbs},
			wantCode: api.ReasonPinNotViable,
		},
		{
			name:         "pin list is an ordered preference with a fallback",
			src:          []api.FabricAttachment{attach(api.ProviderTCP, "dc1", "10.1.1.7"), attach(api.ProviderEFA, "vpc1", "fe80::1")},
			dst:          []api.FabricAttachment{attach(api.ProviderTCP, "dc1", "10.1.1.8"), attach(api.ProviderEFA, "vpc1", "fe80::2")},
			pin:          api.ProviderPin{api.ProviderTCP, api.ProviderEFA},
			wantProvider: api.ProviderTCP,
			wantFabric:   "dc1",
		},
		{
			name:         "pin falls back when the preferred provider is not shared",
			src:          []api.FabricAttachment{attach(api.ProviderTCP, "dc1", "10.1.1.7")},
			dst:          []api.FabricAttachment{attach(api.ProviderTCP, "dc1", "10.1.1.8")},
			pin:          api.ProviderPin{api.ProviderVerbs, api.ProviderTCP},
			wantProvider: api.ProviderTCP,
		},
		{
			// A pinned provider that *is* shared but cannot transfer is a capability problem,
			// not a pin problem, and the operator has to be told which.
			name: "pinned provider present but incapable reports the capability failure",
			src: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1",
				CapFlags: []api.CapFlag{api.CapRemoteWrite},
			}},
			dst: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1",
				CapFlags: []api.CapFlag{api.CapSendReceive},
			}},
			pin:      api.ProviderPin{api.ProviderTCP},
			wantCode: api.ReasonNoSharedCapability,
		},
		{
			name: "capabilities are intersected",
			src: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1",
				CapFlags: []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive, api.CapBlockingOperations},
			}},
			dst: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1",
				CapFlags: []api.CapFlag{api.CapSendReceive, api.CapBlockingOperations},
			}},
			wantProvider: api.ProviderTCP,
			wantCaps:     []api.CapFlag{api.CapSendReceive, api.CapBlockingOperations},
		},
		{
			name: "max message size is the minimum",
			src: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1",
				CapFlags: []api.CapFlag{api.CapRemoteWrite}, MaxMessageSize: 4096,
			}},
			dst: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1",
				CapFlags: []api.CapFlag{api.CapRemoteWrite}, MaxMessageSize: ^uint64(0),
			}},
			wantProvider: api.ProviderTCP,
			wantMaxSize:  4096,
		},
		{
			// Zero means "the probe reported nothing", not "zero bytes". A node that did not
			// report a size must not cap a peer that did.
			name: "an unreported size does not win the minimum",
			src: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1",
				CapFlags: []api.CapFlag{api.CapRemoteWrite}, MaxMessageSize: 0,
			}},
			dst: []api.FabricAttachment{{
				Provider: api.ProviderTCP, Fabric: "dc1",
				CapFlags: []api.CapFlag{api.CapRemoteWrite}, MaxMessageSize: 8192,
			}},
			wantProvider: api.ProviderTCP,
			wantMaxSize:  8192,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result, err := Negotiate(tc.src, tc.dst, tc.pin, Config{Order: tc.order})

			if tc.wantCode != "" {
				var negErr *Error
				require.ErrorAs(t, err, &negErr, "expected a negotiation error")
				assert.Equal(t, tc.wantCode, negErr.Code)
				assert.NotEmpty(t, negErr.Message, "every failure must explain itself to an operator")
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantProvider, result.Provider())
			if tc.wantFabric != "" {
				assert.Equal(t, tc.wantFabric, result.Fabric)
			}
			if tc.wantCaps != nil {
				assert.Equal(t, tc.wantCaps, result.Interface.CapFlags)
			}
			if tc.wantMaxSize != 0 {
				assert.Equal(t, tc.wantMaxSize, result.Interface.MaxMessageSize)
			}
		})
	}
}

// The three no-viable-pair causes are three different operator problems and the message must
// name what actually failed, not just that something did (§10.3, M4d).
func TestNegotiateFailuresNameTheCause(t *testing.T) {
	t.Parallel()

	_, err := Negotiate(
		[]api.FabricAttachment{attach(api.ProviderVerbs, "ib-a", "10.0.1.7")},
		[]api.FabricAttachment{attach(api.ProviderVerbs, "ib-b", "10.0.2.8")},
		nil, Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verbs/ib-a")
	assert.Contains(t, err.Error(), "verbs/ib-b")

	_, err = Negotiate(
		[]api.FabricAttachment{attach(api.ProviderVerbs, "dc1", "10.0.1.7")},
		[]api.FabricAttachment{attach(api.ProviderTCP, "dc1", "10.1.1.8")},
		nil, Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"dc1"`)

	_, err = Negotiate(
		[]api.FabricAttachment{attach(api.ProviderTCP, "dc1", "10.1.1.7")},
		[]api.FabricAttachment{attach(api.ProviderTCP, "dc1", "10.1.1.8")},
		api.ProviderPin{api.ProviderEFA}, Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"efa"`)
	assert.Contains(t, err.Error(), "tcp/dc1", "the message must show what the nodes do share")
}

// Determinism is not cosmetic: two replicas negotiating the same session must agree, and one
// replica must agree with itself across reconciles, or the assignment flaps and every flap
// restarts a healthy worker (§7.3, plan §4.5).
func TestNegotiateIsDeterministic(t *testing.T) {
	t.Parallel()

	src := []api.FabricAttachment{
		attach(api.ProviderVerbs, "ib-b", "10.0.2.7"),
		attach(api.ProviderVerbs, "ib-a", "10.0.1.7"),
		attach(api.ProviderTCP, "dc1", "10.1.1.7"),
	}
	dst := []api.FabricAttachment{
		attach(api.ProviderTCP, "dc1", "10.1.1.8"),
		attach(api.ProviderVerbs, "ib-a", "10.0.1.8"),
		attach(api.ProviderVerbs, "ib-b", "10.0.2.8"),
	}

	first, err := Negotiate(src, dst, nil, Config{})
	require.NoError(t, err)

	for range 20 {
		again, err := Negotiate(src, dst, nil, Config{})
		require.NoError(t, err)
		assert.Equal(t, first, again)
	}

	// Two verbs fabrics are both viable; the tie is broken by the fabric label so that the
	// answer does not depend on the order the agent happened to advertise them in.
	assert.Equal(t, "ib-a", first.Fabric)
}

// A capability this build does not know about must survive the intersection. The rule
// everywhere in this API is that an unrecognised value is passed through rather than dropped
// (§13.1) — a newer worker pair must not lose a capability both ends offered because the server
// in the middle is older.
func TestUnknownCapabilitiesSurviveTheIntersection(t *testing.T) {
	t.Parallel()

	const future = api.CapFlag("FUTURE_THING")

	result, err := Negotiate(
		[]api.FabricAttachment{{
			Provider: api.ProviderTCP, Fabric: "dc1",
			CapFlags: []api.CapFlag{future, api.CapRemoteWrite},
		}},
		[]api.FabricAttachment{{
			Provider: api.ProviderTCP, Fabric: "dc1",
			CapFlags: []api.CapFlag{api.CapRemoteWrite, future},
		}},
		nil, Config{})
	require.NoError(t, err)

	// Known flags first, in their canonical order, so the comparison the reconciler and the
	// agent both make on this value is stable.
	assert.Equal(t, []api.CapFlag{api.CapRemoteWrite, future}, result.Interface.CapFlags)
}

// shm is structurally same-node-only, and the label derivation is what makes that fall out of
// ordinary fabric matching with no special case in this package (§10.1).
func TestSHMOnlyPairsWithItself(t *testing.T) {
	t.Parallel()

	local := []api.FabricAttachment{attach(api.ProviderSHM, api.SHMFabric("edge-01"), "edge-01")}
	remote := []api.FabricAttachment{attach(api.ProviderSHM, api.SHMFabric("edge-02"), "edge-02")}

	result, err := Negotiate(local, local, nil, Config{})
	require.NoError(t, err)
	assert.Equal(t, api.ProviderSHM, result.Provider())

	_, err = Negotiate(local, remote, nil, Config{})
	var negErr *Error
	require.True(t, errors.As(err, &negErr))
	assert.Equal(t, api.ReasonNoSharedFabric, negErr.Code)
}
