package worker

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

func targetSpec() Spec {
	return Spec{
		SessionID:   "s-1",
		Role:        api.RoleTarget,
		DomainPath:  "/dev/shm/mxl1",
		FlowID:      "5592a23b-0974-45bb-9388-89ea81c42537",
		FlowDef:     json.RawMessage(`{"id":"5592a23b-0974-45bb-9388-89ea81c42537","format":"urn:x-nmos:format:video"}`),
		BindAddress: "10.0.2.4",
		Service:     "24012",
		Interface: api.InterfaceConfig{
			Provider:       api.ProviderTCP,
			CapFlags:       []api.CapFlag{api.CapRemoteWrite, api.CapBlockingOperations},
			MaxMessageSize: 1048576,
		},
		IdleTimeout: 0,
	}
}

func initiatorSpec() Spec {
	return Spec{
		SessionID:   "s-1",
		Role:        api.RoleInitiator,
		Epoch:       "PQRSTUVWXYZ234567ABCDEFGHI:0f1e2d3c",
		DomainPath:  "/dev/shm/mxl0",
		FlowID:      "5592a23b-0974-45bb-9388-89ea81c42537",
		BindAddress: "10.0.1.7",
		Service:     "24011",
		Interface: api.InterfaceConfig{
			Provider:       api.ProviderTCP,
			CapFlags:       []api.CapFlag{api.CapRemoteWrite, api.CapBlockingOperations},
			MaxMessageSize: 1048576,
		},
		TargetInfo:     `{"id":"1001","fabricAddress":"10.0.2.4:24012"}`,
		ConnectTimeout: 60 * time.Second,
	}
}

func TestValidateAcceptsBothRoles(t *testing.T) {
	t.Parallel()

	require.NoError(t, targetSpec().Validate())
	require.NoError(t, initiatorSpec().Validate())
}

func TestValidateRejects(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*Spec){
		"no session":       func(s *Spec) { s.SessionID = "" },
		"unknown role":     func(s *Spec) { s.Role = "sender" },
		"no domain":        func(s *Spec) { s.DomainPath = "" },
		"relative domain":  func(s *Spec) { s.DomainPath = "shm/mxl1" },
		"no bind address":  func(s *Spec) { s.BindAddress = "" },
		"no service":       func(s *Spec) { s.Service = "" },
		"unknown provider": func(s *Spec) { s.Interface.Provider = "rxm" },
		"provider any":     func(s *Spec) { s.Interface.Provider = "any" },
		"no transfer capability": func(s *Spec) {
			s.Interface.CapFlags = []api.CapFlag{api.CapBlockingOperations}
		},
		"negative idle timeout":    func(s *Spec) { s.IdleTimeout = -time.Second },
		"sub-millisecond idle":     func(s *Spec) { s.IdleTimeout = 500 * time.Microsecond },
		"sub-millisecond connect":  func(s *Spec) { s.ConnectTimeout = 500 * time.Microsecond },
		"target without flow def":  func(s *Spec) { s.FlowDef = nil },
		"target with broken def":   func(s *Spec) { s.FlowDef = json.RawMessage(`{"id":`) },
		"target given target info": func(s *Spec) { s.TargetInfo = `{"id":"1001"}` },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spec := targetSpec()
			mutate(&spec)
			assert.Error(t, spec.Validate())
		})
	}

	for name, mutate := range map[string]func(*Spec){
		"initiator without flow id":     func(s *Spec) { s.FlowID = "" },
		"initiator without target info": func(s *Spec) { s.TargetInfo = "" },
		"initiator without epoch":       func(s *Spec) { s.Epoch = "" },
		"initiator with flow def":       func(s *Spec) { s.FlowDef = json.RawMessage(`{}`) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spec := initiatorSpec()
			mutate(&spec)
			assert.Error(t, spec.Validate())
		})
	}
}

// An initiator with no epoch is invariant 3 of the plan's checklist: there is nothing to
// compare its blob against, and nothing to converge on when the target restarts (§5.3).
func TestValidateNamesTheMissingEpoch(t *testing.T) {
	t.Parallel()

	spec := initiatorSpec()
	spec.Epoch = ""
	assert.ErrorContains(t, spec.Validate(), "epoch")
}

// The reason Key exists. Every perturbation here is one a real deployment produces — a
// re-derived port from this agent's own allocator, a relabelled request, a flow definition that
// took a different route through a JSON encoder — and every one of them would restart a healthy
// worker if the agent diffed the assignment instead (§7.3, invariant 2).
func TestKeyIgnoresIncidentalDifferences(t *testing.T) {
	t.Parallel()

	base := targetSpec()
	for name, mutate := range map[string]func(*Spec){
		"re-derived service": func(s *Spec) { s.Service = "24999" },
		"added label":        func(s *Spec) { s.Labels = map[string]string{"tenant": "studio-a"} },
		// Two names may map to one directory, and only the directory is what the worker runs
		// against. Renaming the domain relabels a metric; it must not restart a flow.
		"renamed domain": func(s *Spec) { s.Domain = "cameras-renamed" },
		"reordered capability flags": func(s *Spec) {
			s.Interface.CapFlags = []api.CapFlag{api.CapBlockingOperations, api.CapRemoteWrite}
		},
		"flow def whitespace": func(s *Spec) {
			s.FlowDef = json.RawMessage("{\n  \"id\": \"5592a23b-0974-45bb-9388-89ea81c42537\",\n  \"format\": \"urn:x-nmos:format:video\"\n}")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			perturbed := base
			mutate(&perturbed)
			assert.Equal(t, base.Key(), perturbed.Key())
		})
	}
}

// Key order in a flow definition is *not* normalised away: it survives every hop by
// construction, and the session identity hashes it too (§5.4), so this package must not hold a
// different opinion about what "the same flow" is.
func TestKeyDistinguishesReorderedFlowDefKeys(t *testing.T) {
	t.Parallel()

	base := targetSpec()
	reordered := base
	reordered.FlowDef = json.RawMessage(`{"format":"urn:x-nmos:format:video","id":"5592a23b-0974-45bb-9388-89ea81c42537"}`)
	assert.NotEqual(t, base.Key(), reordered.Key())
}

func TestKeySeesMaterialDifferences(t *testing.T) {
	t.Parallel()

	prio := 10
	base := initiatorSpec()
	for name, mutate := range map[string]func(*Spec){
		"session":     func(s *Spec) { s.SessionID = "s-2" },
		"role":        func(s *Spec) { s.Role = api.RoleTarget },
		"epoch":       func(s *Spec) { s.Epoch = "ZZZZZZZZZZZZZZZZZZZZZZZZZZ:0f1e2d3c" },
		"domain path": func(s *Spec) { s.DomainPath = "/dev/shm/mxl2" },
		"flow id":     func(s *Spec) { s.FlowID = "0e26b5f6-1a0a-4e5f-9a1b-0b1a2c3d4e5f" },
		"bind address": func(s *Spec) {
			s.BindAddress = "10.0.1.8"
		},
		"provider": func(s *Spec) { s.Interface.Provider = api.ProviderVerbs },
		"capability set": func(s *Spec) {
			s.Interface.CapFlags = []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive, api.CapBlockingOperations}
		},
		"max message size": func(s *Spec) { s.Interface.MaxMessageSize = 65536 },
		"idle timeout":     func(s *Spec) { s.IdleTimeout = time.Minute },
		"connect timeout":  func(s *Spec) { s.ConnectTimeout = 30 * time.Second },
		"latency measurement": func(s *Spec) {
			s.NoNetworkLatencyMeasurement = true
		},
		"sched prio": func(s *Spec) { s.SchedPrio = &prio },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			changed := base
			mutate(&changed)
			assert.NotEqual(t, base.Key(), changed.Key())
		})
	}
}

// The epoch carries the blob's identity, so an initiator whose target restarted converges even
// when the two blobs are byte-identical — and one whose blob was swapped without a new epoch
// does not restart, because Verify already refused to start it in the first place (§5.2).
func TestKeyTracksEpochRatherThanTargetInfo(t *testing.T) {
	t.Parallel()

	base := initiatorSpec()

	sameBlobNewEpoch := base
	sameBlobNewEpoch.Epoch = "ABCDEFGHIJKLMNOPQRSTUVWXYZ:0f1e2d3c"
	assert.NotEqual(t, base.Key(), sameBlobNewEpoch.Key(), "a fresh incarnation must not look already correct")

	newBlobSameEpoch := base
	newBlobSameEpoch.TargetInfo = `{"id":"9999","fabricAddress":"10.0.2.4:24012"}`
	assert.Equal(t, base.Key(), newBlobSameEpoch.Key())
}

// A hash is only useful as an equality test if it is stable within a process, which it would
// not be if any of the map- or slice-valued fields leaked iteration order into it.
func TestKeyIsStable(t *testing.T) {
	t.Parallel()

	spec := initiatorSpec()
	spec.Labels = map[string]string{"a": "1", "b": "2", "c": "3"}
	want := spec.Key()
	for range 32 {
		assert.Equal(t, want, spec.Key())
	}
}
