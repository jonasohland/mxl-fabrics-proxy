package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonasohland/mxl-utils/pkg/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/epoch"
	"github.com/jonasohland/mxl-replicator/internal/worker"
	"github.com/jonasohland/mxl-replicator/internal/worker/fake"
)

func TestNewRejectsAnIncompleteConfig(t *testing.T) {
	_, err := New(Config{})
	assert.ErrorContains(t, err, "no node name")

	_, err = New(Config{Node: "edge-01"})
	assert.ErrorContains(t, err, "no client")
}

// Registration carries what the node has verified, and the agent stamps on what only it knows:
// the protocol and build versions, and its port range (§10.2).
func TestRegistrationAdvertisesVerifiedCapabilities(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.run()

	h.eventually("registration", func() bool {
		registrations, _, _ := h.server.counts()
		return registrations >= 1
	})

	h.server.mu.Lock()
	registration := h.server.registrations[0]
	h.server.mu.Unlock()

	assert.Equal(t, "edge-01", registration.Node)
	assert.Equal(t, "i-1", registration.Instance)
	assert.Equal(t, api.ProtocolVersion, registration.Capabilities.Versions.Protocol)
	assert.NotEmpty(t, registration.Capabilities.Versions.Replicator)
	assert.Equal(t, "24000-24009", registration.Capabilities.PortRange)
	require.Len(t, registration.Capabilities.Fabrics, 1)
	assert.Equal(t, api.ProviderTCP, registration.Capabilities.Fabrics[0].Provider)

	// **Registration carries no domains** (§6): there are none to carry, since a node's domains
	// are discovered and reach the server through inventory, which is where high-churn
	// observations belong.

	// The harness configures one readable area and one writable one, and **both are advertised**
	// (§10.2): a domain's name is `<area>/<elements>`, so a server that does not hold this table
	// cannot render a domain's identity or resolve a name in a request.
	require.Len(t, registration.Capabilities.Areas, 2)
	assert.Equal(t, "fast", registration.Capabilities.Areas[0].Name)
	assert.True(t, registration.Capabilities.Areas[0].Write)
	assert.Equal(t, "media", registration.Capabilities.Areas[1].Name)
	assert.False(t, registration.Capabilities.Areas[1].Write)
}

// Areas are static agent configuration rather than something the worker probe reports, and they
// are advertised at registration for the same reason fabric attachments are: without them the
// server cannot render a domain's identity, resolve a name in a request, or tell an operator
// which area a name landed outside of (§10.2, §10.6).
func TestAreasAreAdvertisedAndWritableOnesPreCreated(t *testing.T) {
	base := t.TempDir()
	fast := filepath.Join(base, "fast")
	bulk := filepath.Join(base, "bulk", "not-yet")
	readOnly := filepath.Join(base, "read-only", "not-yet")

	h := newHarness(t, harnessOptions{areas: []api.Area{
		{Name: "bulk", Path: bulk, Read: true, Write: true},
		{Name: "fast", Path: fast, Read: true, Write: true},
		{Name: "media", Path: readOnly, Read: true},
	}})
	h.run()

	h.eventually("registration", func() bool {
		registrations, _, _ := h.server.counts()
		return registrations >= 1
	})

	h.server.mu.Lock()
	registration := h.server.registrations[0]
	h.server.mu.Unlock()

	// Ordered by name, grants travelling with the entry, and carrying the path for diagnostics
	// only — the server sends an area *name* back to the agent and the agent resolves it.
	assert.Equal(t, []api.Area{
		{Name: "bulk", Path: bulk, Read: true, Write: true},
		{Name: "fast", Path: fast, Read: true, Write: true},
		{Name: "media", Path: readOnly, Read: true},
	}, registration.Capabilities.Areas)

	// **Writable areas** are created at startup, so only the leaf MkdirAll for a domain is ever on
	// the establishment path (§6.1). A read-only area is somewhere this project looks, not
	// somewhere it may create anything.
	for _, path := range []string{fast, bulk} {
		info, err := os.Stat(path)
		require.NoError(t, err, path)
		assert.True(t, info.IsDir())
	}
	_, err := os.Stat(readOnly)
	assert.True(t, os.IsNotExist(err), "a read-only area is not created")
}

// A node name another instance holds is loud and never fatal — the holder may go away — and the
// loser must not start workers while it waits (§7.1).
func TestASecondClaimantStartsNothing(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.server.setClaimed(true)
	h.server.assign("edge-01", targetAssignment("s1"))
	h.run()

	h.eventually("repeated registration attempts", func() bool {
		registrations, _, _ := h.server.counts()
		return registrations >= 2
	})

	_, _, polls := h.server.counts()
	assert.Zero(t, polls, "a rejected agent must not poll for assignments")
	assert.Zero(t, h.launcher.StartCount(), "and must not start workers")

	// The holder goes away.
	h.server.setClaimed(false)
	h.eventually("the target to start once the name is free", func() bool {
		return h.launcher.Find("s1", api.RoleTarget) != nil
	})
}

// §5.3 steps 2–4: the destination agent starts a target, waits for its blob, computes the epoch
// and reports it along with what the worker actually bound.
func TestTargetEstablishmentReportsTheEpoch(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.server.assign("edge-01", targetAssignment("s1"))
	h.run()

	h.eventually("the target to be reported ready", func() bool {
		status, ok := h.server.lastStatus("s1", api.RoleTarget)
		return ok && status.State == api.WorkerReady
	})

	status, _ := h.server.lastStatus("s1", api.RoleTarget)
	assert.NotEmpty(t, status.Epoch)
	assert.NotEmpty(t, status.TargetInfo)
	// The agent has ground truth about what was bound; the server cannot verify a port it hands
	// out, so this is the only place it can come from (§7.4).
	assert.Equal(t, "127.0.0.1", status.Address)
	assert.Contains(t, []string{"24000", "24001"}, status.Service)

	// The epoch is a real epoch over the blob that was reported, not a token.
	info, unknown, err := epoch.Decode(status.TargetInfo)
	require.NoError(t, err)
	assert.Empty(t, unknown)
	assert.NoError(t, epoch.Verify(status.Epoch, info))

	handle := h.launcher.Find("s1", api.RoleTarget)
	require.NotNil(t, handle)
	spec := handle.Spec()
	assert.Equal(t, h.outputDomain, spec.DomainPath,
		"a target's domain name resolved under this node's output root, not through anything it observes")
	assert.Empty(t, spec.TargetInfo, "a target's target_info config key is an output path, chosen by the launcher")
	assert.NotEmpty(t, spec.FlowDef)
}

// §5.3 step 6: the source agent recomputes the epoch from the blob it was given and checks it
// before starting anything.
func TestInitiatorVerifiesTheEpochBeforeStarting(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.run()

	h.eventually("the first poll", func() bool {
		_, _, polls := h.server.counts()
		return polls >= 1
	})

	spec := worker.Spec{SessionID: "s1", Role: api.RoleTarget, BindAddress: "127.0.0.1", Service: "24000"}
	blob := fake.TargetInfo(spec, 1)
	info, _, err := epoch.Decode(blob)
	require.NoError(t, err)
	good := epoch.Compute(epoch.NewNonce(), info)

	h.server.assign("edge-01", initiatorAssignment(h.name, "s1", good, blob))
	h.eventually("the initiator", func() bool {
		return h.launcher.Find("s1", api.RoleInitiator) != nil
	})

	// A blob that does not match its epoch is refused before it reaches a worker. Without this
	// check the worker comes up, connects to nothing, and reports healthy — no error, no data
	// (§5.2).
	other := fake.TargetInfo(spec, 2)
	h.server.assign("edge-01", initiatorAssignment(h.name, "s2", good, other))

	h.eventually("the mismatch to be reported", func() bool {
		status, ok := h.server.lastStatus("s2", api.RoleInitiator)
		return ok && status.State == api.WorkerFailed
	})
	status, _ := h.server.lastStatus("s2", api.RoleInitiator)
	assert.Contains(t, status.Reason, "does not match the assigned epoch")
	assert.Nil(t, h.launcher.Find("s2", api.RoleInitiator), "nothing may be started for a blob that fails verification")
}

// §5.2: the initiator's whole convergence rule is one equality test on the epoch. A target that
// restarts and produces a **byte-identical** blob still gets a fresh nonce and therefore a new
// epoch — and that is exactly the case a key derived from the blob would get wrong, leaving an
// initiator running against rkeys that no longer exist.
func TestInitiatorReconnectsOnANewEpochEvenForAnIdenticalBlob(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.run()

	spec := worker.Spec{SessionID: "s1", Role: api.RoleTarget, BindAddress: "127.0.0.1", Service: "24000"}
	blob := fake.TargetInfo(spec, 1)
	info, _, err := epoch.Decode(blob)
	require.NoError(t, err)

	first := epoch.Compute(epoch.NewNonce(), info)
	h.server.assign("edge-01", initiatorAssignment(h.name, "s1", first, blob))

	h.eventually("the initiator", func() bool {
		return h.launcher.Find("s1", api.RoleInitiator) != nil
	})
	original := h.launcher.Find("s1", api.RoleInitiator)
	require.Equal(t, 1, h.launcher.StartCount())

	// The same assignment again changes nothing: the already-correct test is on material config,
	// and nothing material moved.
	h.server.assign("edge-01", initiatorAssignment(h.name, "s1", first, blob))
	h.consistently("the initiator to be left alone", 150*time.Millisecond, func() bool {
		return h.launcher.StartCount() == 1
	})

	// The target restarted and reported a byte-identical blob under a new incarnation.
	second := epoch.Compute(epoch.NewNonce(), info)
	require.NotEqual(t, first, second)
	h.server.assign("edge-01", initiatorAssignment(h.name, "s1", second, blob))

	h.eventually("the initiator to be replaced", func() bool {
		current := h.launcher.Find("s1", api.RoleInitiator)
		return current != nil && current != original
	})
	assert.False(t, original.Running(), "the old initiator must be stopped, not left against dead rkeys")
	assert.Equal(t, second, h.launcher.Find("s1", api.RoleInitiator).Spec().Epoch)
}

// Invariant 2, and the bug class that passes every single-replica test: a re-derived port, a
// reordered JSON field or a re-serialised flow definition must not restart a healthy worker
// (§7.3, M5g).
func TestIncidentalDifferencesDoNotRestartAWorker(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.server.assign("edge-01", targetAssignment("s1"))
	h.run()

	h.eventually("the target", func() bool {
		return h.launcher.Find("s1", api.RoleTarget) != nil
	})
	original := h.launcher.Find("s1", api.RoleTarget)
	require.Equal(t, 1, h.launcher.StartCount())

	perturbed := targetAssignment("s1")
	// Whitespace in a re-serialised flow definition; a reordered capability set, whose order is
	// an artefact of the server-side intersection rather than part of the negotiated result; user
	// labels, which the worker never reads and the agent applies at scrape time; and a peer
	// endpoint, which is carried for diagnostics.
	//
	// Key *order* inside the definition is deliberately not on this list: it survives every hop
	// by construction and the session identity hashes it too, so normalising it here would put
	// this test at odds with what the session ID means by "the same flow" (§5.4, M2c).
	perturbed.FlowDef = json.RawMessage("{\n  \"id\":  \"5592a23b-0974-45bb-9388-89ea81c42537\" ,\n  \"format\": \"urn:x-nmos:format:video\"\n}")
	perturbed.Interface.CapFlags = []api.CapFlag{api.CapRemoteWrite}
	perturbed.Labels = map[string]string{"studio": "a"}
	perturbed.Peer = &api.PeerEndpoint{Node: "edge-02", Address: "10.0.0.2", Service: "24999"}
	h.server.assign("edge-01", perturbed)

	h.consistently("the worker to be left running", 200*time.Millisecond, func() bool {
		return h.launcher.StartCount() == 1 && h.launcher.Find("s1", api.RoleTarget) == original
	})

	// A material change does restart it: the negotiated capability set is written into the
	// worker's config and both ends must agree on it (§10.3).
	material := targetAssignment("s1")
	material.Interface.CapFlags = []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive}
	h.server.assign("edge-01", material)

	h.eventually("a restart for a material change", func() bool {
		return h.launcher.StartCount() == 2
	})
}

// Invariant 1, and the one with the worst blast radius in the system: with the server
// unreachable, a reconcile must be skipped entirely rather than run against an empty set (§4.2).
func TestFailStaticOnAnUnreachableServer(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.server.assign("edge-01", targetAssignment("s1"))
	h.run()

	h.eventually("the target", func() bool {
		return h.launcher.Find("s1", api.RoleTarget) != nil
	})
	running := h.launcher.Find("s1", api.RoleTarget)

	// The whole control plane goes away.
	h.server.http.Close()

	h.consistently("the worker to keep running through a control-plane outage", 300*time.Millisecond, func() bool {
		return running.Running() && running.Stops() == 0
	})
}

// The same rule against the answer fail-static does *not* protect from: a successful poll that
// returns nothing. The server says not-ready instead, and the agent must treat that exactly like
// a transport failure — a store restored from an empty backup would otherwise tear down every
// worker in the fleet (plan §4.2).
func TestNotReadyIsNotAnEmptyAssignmentSet(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.server.assign("edge-01", targetAssignment("s1"))
	h.run()

	h.eventually("the target", func() bool {
		return h.launcher.Find("s1", api.RoleTarget) != nil
	})
	running := h.launcher.Find("s1", api.RoleTarget)

	h.server.setNotReady(true)
	h.eventually("the server to be polled while it is not ready", func() bool {
		_, _, polls := h.server.counts()
		return polls >= 3
	})

	assert.True(t, running.Running(), "not-ready must change nothing")
	assert.Zero(t, running.Stops())

	// And a genuinely empty set, once the server *is* ready, does stop it — because that answer
	// is a fact rather than an absence.
	h.server.setNotReady(false)
	h.server.assign("edge-01")
	h.eventually("the worker to be withdrawn", func() bool {
		return !running.Running()
	})
}

// Losing a lease says the fleet has forgotten this node, not that its media should stop (§4.2,
// §7.1).
func TestReregistrationKeepsWorkersRunning(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.server.assign("edge-01", targetAssignment("s1"))
	h.run()

	h.eventually("the target", func() bool {
		return h.launcher.Find("s1", api.RoleTarget) != nil
	})
	running := h.launcher.Find("s1", api.RoleTarget)

	// Captured before the lease is taken away: observed state is written under the lease, so a
	// new lease must resend everything rather than suppressing it as already reported — and the
	// resend can land before the test notices the re-registration.
	before := h.server.statusReports()

	h.server.setReregister(true)
	h.eventually("a second registration", func() bool {
		registrations, _, _ := h.server.counts()
		return registrations >= 2
	})
	h.server.setReregister(false)

	assert.True(t, running.Running(), "a lost lease is not a teardown signal")
	assert.Zero(t, running.Stops())

	h.eventually("status to be reported again under the new lease", func() bool {
		return h.server.statusReports() > before
	})
}

// The server writes what it is given without comparing, and every write wakes every watcher in
// the fleet. Compare-before-send is what keeps §8.3's steady state at zero writes.
func TestUnchangedSnapshotsAreNotResent(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.run()

	h.eventually("the first inventory report", func() bool {
		return len(h.server.inventoryReports()) >= 1
	})

	settled := len(h.server.inventoryReports())
	h.consistently("inventory to stay quiet while nothing changes", 300*time.Millisecond, func() bool {
		return len(h.server.inventoryReports()) == settled
	})

	// A *second* flow appearing is a change, and is reported once. The harness's source domain
	// already holds one, because a directory with no flow in it is not discovered at all (§10.7).
	flow, err := testutil.RandomVideoFlow(h.domain)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	h.eventually("the flow to be reported", func() bool {
		reports := h.server.inventoryReports()
		if len(reports) == 0 {
			return false
		}
		last := reports[len(reports)-1]
		return len(last.Domains) == 1 && len(last.Domains[0].Flows) == 2
	})

	reported := len(h.server.inventoryReports())
	h.consistently("inventory to go quiet again", 300*time.Millisecond, func() bool {
		return len(h.server.inventoryReports()) == reported
	})
}

// A worker that dies on its own is restarted with backoff, and the restart count is what the
// server classifies DEGRADED and FAILED from — never an exit code (§11.1 mechanism 3, §15.1).
func TestUnexpectedDeathIsRestartedAndCounted(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.server.assign("edge-01", targetAssignment("s1"))
	h.run()

	h.eventually("the target", func() bool {
		return h.launcher.Find("s1", api.RoleTarget) != nil
	})

	h.launcher.Find("s1", api.RoleTarget).Die(errors.New("timed out waiting for a grain"))

	h.eventually("a replacement worker", func() bool {
		return h.launcher.StartCount() >= 2 && h.launcher.Find("s1", api.RoleTarget) != nil
	})
	// The replacement is a new incarnation, so its epoch differs — which is what makes the peer
	// initiator reconnect rather than keep writing into memory that is gone.
	h.eventually("the restart to be reported, and the replacement to come up", func() bool {
		status, ok := h.server.lastStatus("s1", api.RoleTarget)
		return ok && status.Restarts >= 1 && status.State == api.WorkerReady
	})

	status, _ := h.server.lastStatus("s1", api.RoleTarget)
	assert.NotEmpty(t, status.Epoch)

	// The dead worker's blob is never reported: it describes memory registrations that died with
	// it, so it is worse than no answer.
	for _, reported := range h.server.statusSnapshots() {
		for _, session := range reported.Sessions {
			if session.State == api.WorkerFailed {
				assert.Empty(t, session.TargetInfo, "a failed target must not report a blob")
				assert.Empty(t, session.Epoch)
			}
		}
	}
}

// A destination is a name inside an area the operator granted `write` on. The server validates it
// and is the authority; the agent checks again because it is the invariant that stops the API
// being a remote arbitrary-filesystem-write primitive (§7.2, §13, invariant 6).
func TestAssignmentsAreRefusedRatherThanGuessedAt(t *testing.T) {
	readOnly := t.TempDir()
	found := filepath.Join(readOnly, "discovered")
	require.NoError(t, os.MkdirAll(found, 0o755))
	flow, err := testutil.RandomVideoFlow(found)
	require.NoError(t, err)
	require.NoError(t, flow.Create())

	h := newHarness(t, harnessOptions{extraAreas: []api.Area{{Name: "ro", Path: readOnly, Read: true}}})
	h.run()

	h.eventually("the discovered domain", func() bool {
		_, ok := h.cfg.Inventory.Lookup(api.Domain{Area: "ro", Elements: []string{"discovered"}})
		return ok
	})

	// A raw path where a domain element belongs. It is not interpreted as a path at any point: a
	// separator is simply not something an element may contain (§10.6).
	rawPath := targetAssignment("s1")
	rawPath.Domain.Elements = []string{"/etc"}

	// An area this node advertises but does not grant `write` on. **This is the one line the
	// unification adds**: "this is an output root" used to carry the grant by construction, and
	// now that one table answers both directions the grant is a field that has to be read (§10.6).
	readOnlyArea := targetAssignment("s2")
	readOnlyArea.Domain = api.Domain{Area: "ro", Elements: []string{"discovered"}}

	// An area this node does not advertise at all. The server validates this and is the
	// authority; checking it again is the deliberate duplication that keeps one buggy control
	// plane from writing wherever an area can reach (§13).
	unknownArea := targetAssignment("s3")
	unknownArea.Domain.Area = "bulk"

	// A destination that names no area. There is no "the node's only one" to fall back on any
	// more: the area is the first segment of the domain's name (§10.6).
	noArea := targetAssignment("s4")
	noArea.Domain.Area = ""

	wrongFabric := targetAssignment("s5")
	wrongFabric.Fabric = "ib-somewhere-else"

	h.server.assign("edge-01", rawPath, readOnlyArea, unknownArea, noArea, wrongFabric)

	for _, tc := range []struct{ session, wants string }{
		{"s1", "contains byte 0x2f"},
		{"s2", `area "ro" on this node does not grant writing`},
		{"s3", `no area "bulk" on this node`},
		{"s4", "no area named"},
		{"s5", "advertises no tcp attachment"},
	} {
		h.eventually("session "+tc.session+" to be reported failed", func() bool {
			status, ok := h.server.lastStatus(tc.session, api.RoleTarget)
			return ok && status.State == api.WorkerFailed
		})
		status, _ := h.server.lastStatus(tc.session, api.RoleTarget)
		assert.Contains(t, status.Reason, tc.wants)
	}

	assert.Zero(t, h.launcher.StartCount(), "nothing may be started for an assignment this node cannot honour")
}

// §7.4: a worker replaced for a new epoch keeps its port, and a session that goes away returns
// its port to the pool.
func TestPortsAreStablePerSessionAndReleasedOnWithdrawal(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.server.assign("edge-01", targetAssignment("s1"))
	h.run()

	h.eventually("the target", func() bool {
		return h.launcher.Find("s1", api.RoleTarget) != nil
	})
	first := h.launcher.Find("s1", api.RoleTarget).Spec().Service

	h.launcher.Find("s1", api.RoleTarget).Die(nil)
	h.eventually("a replacement", func() bool {
		handle := h.launcher.Find("s1", api.RoleTarget)
		return handle != nil && handle.Seq() == 2
	})
	assert.Equal(t, first, h.launcher.Find("s1", api.RoleTarget).Spec().Service,
		"a restart is not a reason for a port to move")

	h.server.assign("edge-01")
	h.eventually("the port to be released", func() bool {
		return len(h.cfg.Ports.Owners()) == 0
	})
}

// The agent execs workers as children, so a clean shutdown must not leave them holding ports,
// memory registrations and flows.
func TestShutdownStopsEveryWorker(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.server.assign("edge-01", targetAssignment("s1"), initiatorAssignmentFor(t, h.name, "s2"))
	h.run()

	h.eventually("both workers", func() bool {
		return h.launcher.Find("s1", api.RoleTarget) != nil && h.launcher.Find("s2", api.RoleInitiator) != nil
	})
	running := h.launcher.Running()
	require.Len(t, running, 2)

	h.stop()

	for _, handle := range running {
		assert.False(t, handle.Running(), "%s was left behind", handle.Spec().SessionID)
	}
}

// Worker starts are paced (§6.3). What the gate must *not* do is pace anything else, and this
// covers the two halves: a queued start reports why it is queued rather than sitting silent, and
// the workers that were admitted come up normally beside it.
func TestWorkerStartsArePaced(t *testing.T) {
	h := newHarness(t, harnessOptions{tweak: func(cfg *Config) {
		// One start per twenty seconds, burst of one: anything past the first is still queued when
		// this test ends, which is what makes the assertions below about pacing rather than about
		// timing.
		cfg.StartRate, cfg.StartBurst = 0.05, 1
	}})
	h.server.assign("edge-01", targetAssignment("s1"), targetAssignment("s2"), targetAssignment("s3"))
	h.run()

	h.eventually("the first worker", func() bool { return h.launcher.StartCount() == 1 })
	h.consistently("only one worker started", 300*time.Millisecond, func() bool {
		return h.launcher.StartCount() == 1
	})

	// Reported, not silent: a worker that has not been launched yet is otherwise
	// indistinguishable from one that was launched and is coming up slowly.
	h.eventually("the queued workers to say so", func() bool {
		queued := 0
		for _, session := range []string{"s1", "s2", "s3"} {
			status, ok := h.server.lastStatus(session, api.RoleTarget)
			if ok && status.State == api.WorkerStarting && status.Reason == "waiting for a permit to start" {
				queued++
			}
		}
		return queued == 2
	})

	// The one that was admitted is a normal worker: it produced its blob and its epoch, and
	// nothing about the gate is on that path.
	h.eventually("the admitted worker to be ready", func() bool {
		for _, session := range []string{"s1", "s2", "s3"} {
			status, ok := h.server.lastStatus(session, api.RoleTarget)
			if ok && status.State == api.WorkerReady && status.Epoch != "" {
				return true
			}
		}
		return false
	})

	body := h.expose()
	assert.Contains(t, body, "mxl_repl_worker_starts_waiting 2")
	assert.Contains(t, body, "mxl_repl_worker_starts_delayed_total 2")
}

// **Rate control applies to starts and to nothing else.** Reconcile stops workers synchronously
// (§6), so a queued start that ignored its cancelled context would hold every other stop on the
// node behind a permit that is minutes away — a withdrawal, an agent shutdown and a rolling
// upgrade all wedged by a worker that never ran.
func TestAQueuedStartDoesNotHoldUpAWithdrawal(t *testing.T) {
	h := newHarness(t, harnessOptions{tweak: func(cfg *Config) {
		cfg.StartRate, cfg.StartBurst = 0.05, 1
	}})
	h.server.assign("edge-01", targetAssignment("s1"), targetAssignment("s2"))
	h.run()

	h.eventually("one worker to be queued", func() bool {
		waiting, _, _ := h.starts.stats()
		return waiting == 1 && h.launcher.StartCount() == 1
	})

	// Both withdrawn. The queued one has a permit twenty seconds away and must leave now: the
	// harness's own deadline is five seconds, so waiting it out fails this test.
	h.server.assign("edge-01")
	h.eventually("both sessions to be released", func() bool {
		waiting, _, _ := h.starts.stats()
		return waiting == 0 && len(h.cfg.Ports.Owners()) == 0
	})
}

// A capability probe that fails must not produce a registration claiming this node can do
// nothing: an attachment list is what negotiation runs on, and an empty one silently makes every
// request through this node fail with no_shared_fabric.
func TestAFailedProbeRetriesRatherThanRegisteringEmpty(t *testing.T) {
	h := newHarness(t, harnessOptions{probeErr: errors.New("mxl-fabrics-proxy-worker: no such file")})
	h.run()

	h.consistently("no registration", 200*time.Millisecond, func() bool {
		registrations, _, _ := h.server.counts()
		return registrations == 0
	})
}

// initiatorAssignmentFor builds a self-consistent initiator assignment for a session.
func initiatorAssignmentFor(t *testing.T, domain api.Domain, sessionID string) api.Assignment {
	t.Helper()

	blob := fake.TargetInfo(worker.Spec{
		SessionID:   sessionID,
		Role:        api.RoleTarget,
		BindAddress: "10.0.0.2",
		Service:     "24000",
	}, 1)
	info, _, err := epoch.Decode(blob)
	require.NoError(t, err)

	return initiatorAssignment(domain, sessionID, epoch.Compute(epoch.NewNonce(), info), blob)
}
