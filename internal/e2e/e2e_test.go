package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/agent"
	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/epoch"
	"github.com/jonasohland/mxl-replicator/internal/server/reconcile"
	"github.com/jonasohland/mxl-replicator/internal/store"
)

// pair is the fleet most of these tests want: a control plane, a source node with a producing
// flow in "cameras", and a destination node with "ingest" mapped.
type pair struct {
	*fleet

	src, dst *node
	flow     *sourceFlow
}

func newPair(t *testing.T, opts fleetOptions) *pair {
	t.Helper()

	f := newFleet(t, opts)
	p := &pair{fleet: f}
	p.src = f.addNode("studio-a", nodeOptions{domains: []string{"cameras"}})
	p.dst = f.addNode("edge-01", nodeOptions{domains: []string{"ingest"}})

	p.flow = p.src.createFlow("cameras", videoFlowDef("Studio A:Camera 1", "video"))
	p.flow.produce()
	return p
}

// replicate creates the request that replicates the pair's flow by ID.
func (p *pair) replicate(name string) api.Request {
	p.t.Helper()

	return p.request(api.RequestSpec{
		Name:        name,
		Source:      api.Source{Node: "studio-a", Domain: "cameras", Select: api.Selector{Flow: p.flow.ID()}},
		Destination: api.Destination{Node: "edge-01", Domain: "ingest"},
	})
}

// established waits for both ends of the session to be running *and* reported, and returns the
// session ID.
//
// Both halves matter. A worker running locally is what the launcher knows; an endpoint present in
// the fleet view is that fact having travelled agent → status snapshot → store → reconcile, which
// is the round trip every assertion after this point depends on.
func (p *pair) established() string {
	p.t.Helper()

	var sessionID string
	p.eventually("a session with both ends running and reported", func() bool {
		paths := p.paths().Paths
		if len(paths) != 1 || paths[0].Session == nil || paths[0].Session.ID == "" {
			return false
		}
		session := paths[0].Session
		if session.Target == nil || session.Initiator == nil {
			return false
		}
		if p.dst.worker(session.ID, api.RoleTarget) == nil || p.src.worker(session.ID, api.RoleInitiator) == nil {
			return false
		}
		sessionID = session.ID
		return true
	})
	return sessionID
}

// --- 1. the full path ------------------------------------------------------------------------

// M7.1: discovery → inventory → request → path → session → assignment, in one process with
// nothing between the agents and the server faked.
//
// The ordering assertions are the point. §5.3 is a sequence and every step of it is load-bearing:
// the target is assigned alone and first, because openFlow on the destination fails outright if
// the flow does not exist yet; the initiator is not assigned until the target has reported an
// epoch, because connecting to memory registrations that have already died is the failure mode
// the epoch exists to prevent.
func TestFullPathFromDiscoveryToRunningSession(t *testing.T) {
	p := newPair(t, fleetOptions{})

	// Discovery and inventory: the flow the source agent found on disk reaches the fleet view,
	// with the definition the producer wrote and the liveness the agent derived.
	p.eventually("the source flow to reach the fleet inventory", func() bool {
		for _, flow := range p.flows().Flows {
			if flow.ID == p.flow.ID() && flow.Node == "studio-a" && flow.Domain == "cameras" {
				return flow.Producing
			}
		}
		return false
	})

	request := p.replicate("cam1")
	assert.NotEmpty(t, request.ID)

	// Expansion: one selector, one path, refcounted back to the request that created it.
	p.eventually("the request to expand onto a path", func() bool {
		return len(p.paths().Paths) == 1
	})
	path := p.onlyPath()
	assert.Equal(t, api.FlowAddress{Node: "studio-a", Domain: "cameras", Flow: p.flow.ID()}, path.Source)
	assert.Equal(t, api.Destination{Node: "edge-01", Domain: "ingest"}, path.Destination)
	assert.Equal(t, []string{request.ID}, path.Requests)

	sessionID := p.established()

	// The target's end. The blob and the epoch are real: the epoch verifies against the
	// target_info the destination agent reported, which is what the initiator will recompute
	// before it starts (§5.3 step 6).
	path = p.onlyPath()
	require.NotNil(t, path.Session)
	require.NotNil(t, path.Session.Target)
	assert.Equal(t, "edge-01", path.Session.Target.Node)
	assert.Equal(t, api.WorkerReady, path.Session.Target.State)
	assert.NotEmpty(t, path.Session.Epoch)
	assert.Equal(t, "127.0.0.1", path.Session.Target.Address)
	assert.NotEmpty(t, path.Session.Target.Service, "the agent reports what it actually bound (§7.4)")

	// The initiator's end, and the negotiated interface both of them were handed. It is one
	// value written into both assignments, not two independently derived ones (§5.5, §10.3).
	require.NotNil(t, path.Session.Initiator)
	assert.Equal(t, "studio-a", path.Session.Initiator.Node)
	assert.Equal(t, "dc1", path.Session.Fabric)
	assert.Equal(t, api.ProviderTCP, path.Session.Interface.Provider)

	target := p.dst.worker(sessionID, api.RoleTarget).Spec()
	initiator := p.src.worker(sessionID, api.RoleInitiator).Spec()
	assert.Equal(t, p.dst.path("ingest"), target.DomainPath, "the domain name resolved to the destination's own path")
	assert.Equal(t, p.src.path("cameras"), initiator.DomainPath)
	assert.Equal(t, p.flow.ID(), target.FlowID)
	assert.Equal(t, target.Interface, initiator.Interface, "both ends run the negotiated config")

	// The initiator was given the blob and an epoch over it, and it verified before starting.
	require.NotEmpty(t, initiator.TargetInfo)
	info, unknown, err := epoch.Decode(initiator.TargetInfo)
	require.NoError(t, err)
	assert.Empty(t, unknown)
	assert.NoError(t, epoch.Verify(path.Session.Epoch, info))

	// The initiator dials the endpoint the target actually bound — carried inside the blob, not
	// alongside it, which is why the blob has to travel verbatim. The peer endpoint on the
	// assignment is diagnostics and never reaches the worker; it is deliberately absent from
	// [worker.Spec].
	assert.Contains(t, initiator.TargetInfo, path.Session.Target.Service)
	assert.Equal(t, path.Session.Target.Service, target.Service,
		"the service the fleet reports is the one the target was told to bind")

	// Cancelling the intent tears the session down. A request is durable user intent and is
	// never cancelled by the system (§11); this is the only thing that ends it.
	p.cancel(request.ID)
	p.eventually("both workers to be withdrawn", func() bool {
		return p.dst.worker(sessionID, api.RoleTarget) == nil &&
			p.src.worker(sessionID, api.RoleInitiator) == nil
	})
	p.eventually("the path to be gone", func() bool {
		return len(p.paths().Paths) == 0
	})
}

// --- 2. epoch convergence --------------------------------------------------------------------

// M7.2, first half: a target that comes back with a different target_info makes the server issue
// a new epoch, and the initiator is replaced rather than left connected to registrations that no
// longer exist (§5.2).
func TestANewTargetIncarnationReplacesTheInitiator(t *testing.T) {
	p := newPair(t, fleetOptions{})
	p.replicate("cam1")

	sessionID := p.established()
	firstEpoch := p.onlyPath().Session.Epoch
	initiator := p.src.worker(sessionID, api.RoleInitiator)
	require.Equal(t, 1, p.src.starts())

	// The target dies. Its successor produces a different blob, which is what a real restart
	// does: the memory registrations behind the old one died with the process.
	p.dst.worker(sessionID, api.RoleTarget).Die(assert.AnError)

	p.eventually("a new epoch", func() bool {
		path := p.onlyPath()
		return path.Session != nil && path.Session.Epoch != "" && path.Session.Epoch != firstEpoch
	})
	p.eventually("the initiator to be replaced", func() bool {
		return p.src.starts() == 2 && p.src.worker(sessionID, api.RoleInitiator) != initiator
	})

	// The replacement carries the new blob, and an epoch that verifies against it.
	second := p.onlyPath().Session.Epoch
	replaced := p.src.worker(sessionID, api.RoleInitiator)
	require.NotNil(t, replaced)
	info, _, err := epoch.Decode(replaced.Spec().TargetInfo)
	require.NoError(t, err)
	assert.NoError(t, epoch.Verify(second, info))
}

// M7.2, second half, and the degenerate case the incarnation nonce exists for: a target that
// restarts and reports a **byte-identical** target_info must still cause its initiator to
// reconnect (§5.2).
//
// Not hypothetical. Measured against a real tcp target, `fabricAddress` is identical across a
// restart because the agent reuses the port by design, `addr` is "0" in every region because the
// provider reports no mapping address at all, and only the rkey varied. An epoch computed from
// the blob alone would have compared equal, the server would have issued no new assignment, and
// the initiator would have kept writing into registrations that no longer exist — which the NIC
// does not report and no counter in the system reflects.
func TestAnIdenticalBlobStillReconnectsTheInitiator(t *testing.T) {
	p := newPair(t, fleetOptions{})

	// Pin the blob before anything starts, so the two incarnations are byte-identical.
	p.dst.launcher.SetTargetInfo(`{"id":"1","addressFormat":1,"fabricAddress":"127.0.0.1:24100",` +
		`"provider":"tcp","regions":[{"addr":"0","len":"1048576","rkey":"17918262359965949928"}]}`)

	p.replicate("cam1")
	sessionID := p.established()

	before := p.onlyPath().Session
	initiator := p.src.worker(sessionID, api.RoleInitiator)

	p.dst.worker(sessionID, api.RoleTarget).Die(assert.AnError)

	p.eventually("the initiator to be replaced", func() bool {
		return p.src.starts() == 2 && p.src.worker(sessionID, api.RoleInitiator) != initiator
	})

	after := p.onlyPath().Session
	require.NotNil(t, after)
	assert.NotEqual(t, before.Epoch, after.Epoch, "the epoch must change even though the blob did not")

	replaced := p.src.worker(sessionID, api.RoleInitiator)
	assert.Equal(t, initiator.Spec().TargetInfo, replaced.Spec().TargetInfo,
		"the blob really is identical; only the nonce differs")
}

// --- 3. fail-static --------------------------------------------------------------------------

// M7.3, and the invariant with the worst blast radius in the design: with the server unreachable,
// a reconcile is skipped entirely rather than run against an empty set (§4.2, invariant 1).
//
// Worth a dedicated test at this level and not only at the agent's, because the bug is a one-line
// confusion between "no assignments" and "no answer" and it can be introduced on either side of
// the wire — an agent that reconciles on an error, or a server that answers an empty set while it
// does not know. The whole fleet's media stops either way.
func TestFailStaticThroughAControlPlaneOutage(t *testing.T) {
	p := newPair(t, fleetOptions{})
	p.replicate("cam1")

	sessionID := p.established()
	target := p.dst.worker(sessionID, api.RoleTarget)
	initiator := p.src.worker(sessionID, api.RoleInitiator)
	starts := p.src.starts() + p.dst.starts()

	// The entire control plane goes away, for long enough that both agents' leases expire and
	// their polls, heartbeats and reports have all failed repeatedly.
	p.replicas[0].stop()
	time.Sleep(leaseTTL + 500*time.Millisecond)

	assert.True(t, target.Running(), "a target must survive a control-plane outage")
	assert.True(t, initiator.Running(), "and so must its initiator")
	assert.Zero(t, target.Stops())
	assert.Zero(t, initiator.Stops())
	assert.Equal(t, starts, p.src.starts()+p.dst.starts(), "and nothing may be restarted")

	// The control plane comes back on the same address. The agents re-register — their leases
	// are gone — and the workers they never stopped are adopted by session ID rather than
	// replaced (§7.3).
	p.replicas[0].start()
	p.awaitSettled()

	p.eventually("the path to come back", func() bool {
		path := p.paths().Paths
		return len(path) == 1 && path[0].Session != nil && path[0].Session.ID == sessionID
	})
	p.consistently("the workers that were never stopped to keep running", func() bool {
		return target.Running() && initiator.Running() &&
			p.src.starts()+p.dst.starts() == starts
	})
}

// --- 4. restarts that must cost nothing --------------------------------------------------------

// M7.4: a server restart against a fleet already in the desired state produces **zero** worker
// restarts (§7.3).
//
// This is what the settling window and the derived session ID are both for. The server keeps no
// state between processes: it recomputes every session ID from the path identity and the source
// flow definition's hash, and recognises the workers the agents report as the ones it would have
// asked for. Get either half wrong and every upgrade of the control plane glitches every flow in
// the fleet.
func TestAServerRestartRestartsNoWorkers(t *testing.T) {
	p := newPair(t, fleetOptions{
		// The window is what makes the restart safe: the fresh server waits for the fleet to
		// report what it is running before it derives anything.
		settlingHeartbeats: 4,
	})
	p.replicate("cam1")

	sessionID := p.established()
	target := p.dst.worker(sessionID, api.RoleTarget)
	initiator := p.src.worker(sessionID, api.RoleInitiator)
	starts := p.src.starts() + p.dst.starts()
	require.Equal(t, 2, starts)

	for range 3 {
		p.replicas[0].stop()
		p.replicas[0].start()
		p.awaitSettled()

		p.eventually("the path to be served again", func() bool {
			paths := p.paths().Paths
			return len(paths) == 1 && paths[0].Session != nil && paths[0].Session.ID == sessionID
		})
	}

	// The session ID is recomputed, not remembered, so an identical one is the assertion that
	// the derivation is stable across processes.
	assert.Equal(t, sessionID, p.onlyPath().Session.ID)
	assert.Equal(t, starts, p.src.starts()+p.dst.starts(), "no worker may be restarted by a server restart")
	assert.True(t, target.Running())
	assert.True(t, initiator.Running())
	assert.Zero(t, target.Stops())
	assert.Zero(t, initiator.Stops())
}

// The same claim, made against the agents rather than the server: losing a lease says the fleet
// has forgotten this node, not that its media should stop (§4.2, §7.1).
func TestAReRegistrationRestartsNoWorkers(t *testing.T) {
	p := newPair(t, fleetOptions{})
	p.replicate("cam1")

	sessionID := p.established()
	target := p.dst.worker(sessionID, api.RoleTarget)
	starts := p.src.starts() + p.dst.starts()

	// Delete the destination's lease out from under it. The next heartbeat is told to register
	// again, which tears down the agent's server session and everything hanging off it — except
	// the workers, whose context comes from the agent's own lifetime.
	_, err := p.replicas[0].store.Delete(t.Context(), store.LeaseKey("edge-01"))
	require.NoError(t, err)

	p.eventually("the destination to register again", func() bool {
		paths := p.paths().Paths
		return len(paths) == 1 && paths[0].Session != nil && paths[0].Session.Target != nil &&
			paths[0].Session.Target.State == api.WorkerReady
	})
	assert.Equal(t, starts, p.src.starts()+p.dst.starts())
	assert.True(t, target.Running())
	assert.Zero(t, target.Stops())
	assert.Equal(t, sessionID, p.onlyPath().Session.ID)
}

// --- 5. incidental differences -----------------------------------------------------------------

// M7.5: an assignment that differs only in fields the worker's behaviour does not depend on must
// not restart it (§7.3, invariant 5).
//
// This is a bug class that passes every naive test and then flaps in production, because the
// incidental differences only start appearing once there is a second replica or a store round
// trip in the path. So it is driven on the wire between the real server and the real agent: the
// server derives the assignment it would derive, the agent decodes what it would decode, and only
// the bytes in between are perturbed.
//
// The perturbations are the ones §17 names, and each is something that could plausibly change for
// a reason that is not a reason to restart media:
//
//   - **port derivation**: the peer endpoint the initiator is told to dial, plus the user labels.
//     Both are carried for purposes outside the worker — diagnostics and metric labelling — and
//     the worker reads neither.
//   - **JSON field order**: every object's keys re-emitted in sorted order rather than in
//     struct-declaration order, which is what any re-encoding hop would do.
//   - **flow_def serialisation**: whitespace, reintroduced after the API's own compaction.
//
// Key order *inside* flow_def is deliberately not perturbed, and the perturbation is built to
// preserve it: the session identity hashes those bytes, so normalising them here would put this
// test at odds with what a session ID means by "the same flow" (§5.4). It survives every hop by
// construction, which is the property that makes hashing them safe in the first place.
func TestIncidentalDifferencesRestartNothing(t *testing.T) {
	p := newPair(t, fleetOptions{})
	p.replicate("cam1")

	sessionID := p.established()
	target := p.dst.worker(sessionID, api.RoleTarget)
	initiator := p.src.worker(sessionID, api.RoleInitiator)
	starts := p.src.starts() + p.dst.starts()

	perturb := func(body []byte) []byte {
		var set api.AssignmentSet
		if err := json.Unmarshal(body, &set); err != nil {
			return body
		}
		for i := range set.Assignments {
			if peer := set.Assignments[i].Peer; peer != nil {
				peer.Service = "24999"
				peer.Address = "10.255.255.255"
			}
			set.Assignments[i].Labels = map[string]string{"perturbed": "yes"}
		}
		return respaced(reordered(set))
	}
	p.src.rewriter.set(perturb)
	p.dst.rewriter.set(perturb)

	// Nothing has changed on the server, so nothing wakes a poll: wait for the long polls to
	// expire and be answered with the current — now perturbed — set, twice over.
	time.Sleep(2*agentPollWait + 200*time.Millisecond)

	p.consistently("both workers to be left running", func() bool {
		return p.src.starts()+p.dst.starts() == starts &&
			p.dst.worker(sessionID, api.RoleTarget) == target &&
			p.src.worker(sessionID, api.RoleInitiator) == initiator
	})

	// And the control: a *material* difference does restart. The negotiated capability set is
	// written into the worker's config and both ends must agree on it (§10.3), so it is part of
	// the identity key by construction rather than by a list someone maintains.
	p.dst.rewriter.set(func(body []byte) []byte {
		var set api.AssignmentSet
		if err := json.Unmarshal(body, &set); err != nil {
			return body
		}
		for i := range set.Assignments {
			set.Assignments[i].Interface.MaxMessageSize = 4096
		}
		encoded, err := json.Marshal(set)
		if err != nil {
			return body
		}
		return encoded
	})

	p.eventually("a restart for a material change", func() bool {
		return p.dst.starts() == 2
	})
}

// reordered re-encodes an assignment set so that every object's keys come out in sorted order
// rather than in struct-declaration order.
//
// Through maps of [json.RawMessage] rather than of `any`, and that is the whole subtlety: a
// round trip through `any` would decode flow_def into a map too and re-emit *its* keys sorted,
// which is a change to the bytes the session identity hashes and therefore a real difference
// rather than an incidental one. Keeping the definition raw perturbs everything around it and
// leaves it exactly as the producer wrote it.
func reordered(set api.AssignmentSet) []byte {
	encoded, err := json.Marshal(set)
	if err != nil {
		return nil
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &top); err != nil {
		return encoded
	}
	var assignments []map[string]json.RawMessage
	if err := json.Unmarshal(top["assignments"], &assignments); err == nil {
		if remarshalled, err := json.Marshal(assignments); err == nil {
			top["assignments"] = remarshalled
		}
	}

	out, err := json.Marshal(top)
	if err != nil {
		return encoded
	}
	return out
}

// respaced reintroduces insignificant whitespace everywhere, including inside flow_def.
//
// It has to be done on the raw bytes rather than by handing a pretty-printed [json.RawMessage] to
// [json.Marshal], because marshalling compacts a RawMessage on the way out — which is the same
// reason the API's own hop compacts a producer's formatting and changes nothing else. Indenting
// preserves key order everywhere.
func respaced(body []byte) []byte {
	var buf bytes.Buffer
	if err := json.Indent(&buf, body, " ", "   "); err != nil {
		return body
	}
	return buf.Bytes()
}

// --- 6. selector expansion ----------------------------------------------------------------------

// M7.6: a group-hint request gains and loses paths as flows appear and disappear (§9.1).
//
// The property being protected is that a request is durable intent over a *set* that changes,
// rather than a pinned UUID: a producer republishing a flow under a new ID must not need the
// request rewritten, and a request matching nothing right now is WAITING rather than an error.
func TestAGroupHintRequestFollowsTheFlowsThatMatchIt(t *testing.T) {
	f := newFleet(t, fleetOptions{})
	src := f.addNode("studio-a", nodeOptions{domains: []string{"cameras"}})
	f.addNode("edge-01", nodeOptions{domains: []string{"ingest"}})

	request := f.request(api.RequestSpec{
		Name: "camera-1",
		Source: api.Source{Node: "studio-a", Domain: "cameras", Select: api.Selector{
			GroupHint: &api.GroupHintSelector{Name: "Studio A:Camera 1"},
		}},
		Destination: api.Destination{Node: "edge-01", Domain: "ingest"},
	})

	// Matching nothing is WAITING, and costs nothing.
	f.eventually("the request to be waiting on its selector", func() bool {
		resp := f.do(http.MethodGet, api.RequestPath(request.ID), nil)
		if resp.status != http.StatusOK {
			return false
		}
		var got api.Request
		resp.decode(t, &got)
		return got.Status.State == api.StateWaiting && got.Status.ReasonCode == api.ReasonFlowNotFound
	})
	assert.Empty(t, f.paths().Paths)

	// A camera's video and audio share a group name, and a selector without a type takes both —
	// which is how one request replicates a camera whole.
	video := src.createFlow("cameras", videoFlowDef("Studio A:Camera 1", "video"))
	video.produce()

	f.eventually("one path", func() bool { return len(f.paths().Paths) == 1 })

	audio := src.createFlow("cameras", audioFlowDef("Studio A:Camera 1", "audio"))
	audio.produce()

	f.eventually("a second path", func() bool { return len(f.paths().Paths) == 2 })

	// A flow that does not match the hint is not picked up, however close it looks.
	other := src.createFlow("cameras", videoFlowDef("Studio A:Camera 2", "video"))
	other.produce()
	f.consistently("the selector to stay at two paths", func() bool {
		return len(f.paths().Paths) == 2
	})

	// A producer republishing: the flow goes away, and so does its path, without the request
	// being touched.
	audio.destroy()
	f.eventually("the path to be withdrawn with its flow", func() bool {
		return len(f.paths().Paths) == 1
	})

	video.destroy()
	f.eventually("the last path to go", func() bool { return len(f.paths().Paths) == 0 })

	// The request is still there. Failure is made observable; intent is not cancelled by it
	// (§11).
	resp := f.do(http.MethodGet, api.RequestPath(request.ID), nil)
	require.Equal(t, http.StatusOK, resp.status)
}

// --- 7. the state progression -------------------------------------------------------------------

// M7.7: WAITING → ESTABLISHING → PAUSED → ACTIVE, driven by head-index movement on real flow
// directories (§11, §11.1).
//
// Every transition here is driven by the thing that really drives it, which is the only reason
// the test is worth having:
//
//   - **WAITING** is the flow not being visible at all. A pinned flow ID still expands to a path
//     of size one, so the request has somewhere to report from while its flow does not exist.
//   - **PAUSED on an idle source** is admission: a flow that exists and is not being written to
//     starts no workers at all, so requesting a camera that is not live costs nothing. It is
//     PAUSED and not WAITING because the flow *is* there.
//   - **ESTABLISHING** is the pair coming up, held open here by a target that has started and not
//     yet produced its blob — which is a real condition and not a contrivance.
//   - **ACTIVE versus PAUSED** is the **destination** flow's liveness. Not the source's, and
//     never a worker's exit code: a target running and delivering nothing is exactly what an idle
//     source looks like, and it is not a failure (§11, §15.1).
func TestThePathStateFollowsTheFlows(t *testing.T) {
	f := newFleet(t, fleetOptions{})
	src := f.addNode("studio-a", nodeOptions{domains: []string{"cameras"}})
	dst := f.addNode("edge-01", nodeOptions{
		domains: []string{"ingest"},
		// The blob is held below to hold the path in ESTABLISHING; the agent must not give up on
		// the target while the test is looking at it.
		tweak: func(cfg *agent.Config) { cfg.TargetInfoTimeout = time.Minute },
	})

	def := videoFlowDef("Studio A:Camera 1", "video")
	f.request(api.RequestSpec{
		Name:        "cam1",
		Source:      api.Source{Node: "studio-a", Domain: "cameras", Select: api.Selector{Flow: def.ID}},
		Destination: api.Destination{Node: "edge-01", Domain: "ingest"},
	})

	// WAITING: the flow does not exist yet.
	f.eventually("WAITING on a flow that is not there", func() bool {
		paths := f.paths().Paths
		return len(paths) == 1 && paths[0].State == api.StateWaiting && paths[0].ReasonCode == api.ReasonFlowNotFound
	})

	// The flow appears, and is not being written to.
	source := src.createFlow("cameras", def)

	f.eventually("PAUSED on an idle source", func() bool {
		paths := f.paths().Paths
		return len(paths) == 1 && paths[0].State == api.StatePaused && paths[0].ReasonCode == api.ReasonSourceIdle
	})
	assert.Zero(t, src.starts(), "an idle source starts no workers")
	assert.Zero(t, dst.starts())
	assert.Nil(t, f.onlyPath().Session, "and there is no session to run them under")

	// ESTABLISHING: the source starts producing, the target starts, and it has not come up.
	// Nothing may be assigned to the initiator until an epoch exists (§5.3).
	dst.launcher.HoldTargetInfo(true)
	source.produce()

	f.eventually("ESTABLISHING while the target comes up", func() bool {
		paths := f.paths().Paths
		return len(paths) == 1 && paths[0].State == api.StateEstablishing && paths[0].Session != nil
	})
	sessionID := f.onlyPath().Session.ID
	f.eventually("the target to have started", func() bool {
		return dst.worker(sessionID, api.RoleTarget) != nil
	})
	assert.Empty(t, f.onlyPath().Session.Epoch, "no epoch has been reported yet")
	assert.Zero(t, src.starts(), "and so no initiator may be running")

	dst.launcher.HoldTargetInfo(false)

	// PAUSED: both ends up, nothing arriving at the destination. The destination flow is what
	// the target would create; with a fake worker nothing does, which is exactly the shape of a
	// target that is up and receiving nothing.
	f.eventually("both workers to be running", func() bool {
		return dst.worker(sessionID, api.RoleTarget) != nil && src.worker(sessionID, api.RoleInitiator) != nil
	})
	f.eventually("PAUSED while nothing is arriving at the destination", func() bool {
		return f.pathState() == api.StatePaused
	})

	// ACTIVE: the destination flow starts advancing. Same mechanism as a producing source —
	// liveness is head-index movement wherever it is observed, which is what lets one rule serve
	// both admission and the ACTIVE/PAUSED split.
	destination := dst.createFlow("ingest", def)
	destination.produce()

	f.eventually("ACTIVE once the destination flow is being delivered into", func() bool {
		return f.pathState() == api.StateActive
	})

	// And back. A source that stops is PAUSED, never FAILED: it is not an error, and it must not
	// produce a restart loop (§11.1).
	destination.stopProducing()
	source.stopProducing()

	f.eventually("PAUSED when delivery stops", func() bool {
		return f.pathState() == api.StatePaused
	})
	assert.True(t, dst.worker(sessionID, api.RoleTarget).Running(), "PAUSED is a steady state, not a teardown")
	assert.True(t, src.worker(sessionID, api.RoleInitiator).Running())
}

// --- 8. two agents, one node name ----------------------------------------------------------------

// M7.8: a second agent claiming a node name another instance holds is rejected, and the holder is
// undisturbed (§7.1).
//
// The failure this prevents is not cosmetic. Two agents under one name receive the same
// assignments, start the same workers, fight over the same port range, and produce duplicate
// writes into the destination flow. The liveness lease is exclusive so that the second claimant
// loses, loudly, and — the half that is easy to get wrong — starts nothing while it waits.
func TestASecondClaimantIsRejectedAndTheHolderIsUndisturbed(t *testing.T) {
	p := newPair(t, fleetOptions{})
	p.replicate("cam1")

	sessionID := p.established()
	target := p.dst.worker(sessionID, api.RoleTarget)
	starts := p.dst.starts()

	// A second agent for edge-01: a copy-pasted config, or an overlapping rollout.
	impostor := p.addNode("edge-01", nodeOptions{domains: []string{"ingest"}, contested: true})

	p.consistently("the impostor to start nothing", func() bool {
		return impostor.starts() == 0
	})
	p.consistently("the holder to be undisturbed", func() bool {
		return p.dst.starts() == starts && target.Running() && target.Stops() == 0
	})

	// The fleet still sees one edge-01, running the session it was already running.
	path := p.onlyPath()
	require.NotNil(t, path.Session)
	assert.Equal(t, sessionID, path.Session.ID)
	require.NotNil(t, path.Session.Target)
	assert.Equal(t, api.WorkerReady, path.Session.Target.State)

	// The holder goes away. The name is free and the loser takes over — which is why the
	// rejection is a retry rather than an exit.
	p.dst.stop()

	impostor.fleet.eventually("the impostor to take the name over", func() bool {
		return impostor.starts() > 0
	})
}

// --- assignment shape ---------------------------------------------------------------------------

// The matched settings §5.5 calls out: values that must be identical on both ends of a session
// and that no agent may decide for itself, because two nodes disagreeing about one of them is a
// bug rather than a configuration choice.
//
// no_network_latency_measurement is the one whose disagreement is silent — the target reports
// garbage latency with no error at all — which is why it is a server-level setting written into
// both assignments rather than per-side agent config.
func TestMatchedSettingsReachBothWorkersIdentically(t *testing.T) {
	p := newPair(t, fleetOptions{reconcile: reconcile.Config{
		IdleTimeout:                 250 * time.Millisecond,
		ConnectTimeout:              5 * time.Second,
		NoNetworkLatencyMeasurement: true,
	}})
	p.replicate("cam1")

	sessionID := p.established()
	target := p.dst.worker(sessionID, api.RoleTarget).Spec()
	initiator := p.src.worker(sessionID, api.RoleInitiator).Spec()

	assert.Equal(t, 250*time.Millisecond, target.IdleTimeout)
	assert.Equal(t, target.IdleTimeout, initiator.IdleTimeout)
	assert.True(t, target.NoNetworkLatencyMeasurement)
	assert.Equal(t, target.NoNetworkLatencyMeasurement, initiator.NoNetworkLatencyMeasurement)
	assert.Equal(t, target.Interface, initiator.Interface)

	// And the one setting that is deliberately *not* matched: the bind service. Each agent
	// allocates its own from its own range, because the server cannot verify a port it hands out
	// (§7.4).
	assert.NotEqual(t, target.Service, initiator.Service)
}

// --- the invariant that keeps the API from being a write primitive -------------------------------

// Invariant 6, end to end: a destination domain must be a name the destination agent has
// *explicitly mapped*, and a domain merely found by a search path does not satisfy it (§7.2, §13).
//
// This is the single most important invariant in the design, and it holds regardless of what
// authentication is configured — which is exactly why it is worth a test that goes through the
// real API rather than only through the validator. Without it, anyone who can reach `/v1/requests`
// can have a worker create a flow directory anywhere a search path reaches, on every node in the
// fleet.
//
// The other half is that a discovered domain is still a perfectly good *source*, so the check is
// not "discovered domains are ignored".
func TestADiscoveredDomainIsASourceAndNeverADestination(t *testing.T) {
	f := newFleet(t, fleetOptions{})

	root := searchRoot(t)
	src := f.addNode("studio-a", nodeOptions{domains: []string{"cameras"}, searchPaths: []string{root}})
	f.addNode("edge-01", nodeOptions{domains: []string{"ingest"}, searchPaths: []string{root}})

	// A producer creates a domain nobody configured. It is named by its path, which is the one
	// string certain to be unique on the node — and not a path the API can use, because
	// resolution is a lookup in the agent's own mapping table.
	found := filepath.Join(root, "adhoc")
	flow := createFlowAt(t, found, videoFlowDef("Ad Hoc:Camera", "video"))
	flow.produce()

	f.eventually("the discovered domain to be reported as unconfigured", func() bool {
		for _, entry := range f.flows().Flows {
			if entry.ID == flow.ID() && entry.Node == "studio-a" && entry.Domain == found {
				return entry.Producing
			}
		}
		return false
	})

	// As a destination it is refused, and INVALID rather than WAITING: it needs a user to change
	// the node's configuration and will never come good on its own.
	refused := f.do(http.MethodPost, api.PathRequests, api.RequestSpec{
		Name:        "write-anywhere",
		Source:      api.Source{Node: "studio-a", Domain: found, Select: api.Selector{Flow: flow.ID()}},
		Destination: api.Destination{Node: "edge-01", Domain: found},
	})
	require.Equal(t, http.StatusBadRequest, refused.status, "body: %s", refused.body)

	var refusal api.Error
	refused.decode(t, &refusal)
	assert.Equal(t, string(api.ReasonDomainNotMapped), refusal.Details["reason_code"])
	assert.Zero(t, src.starts(), "and nothing may be started for it")

	// The same domain as a *source* is fine, and replicates into a mapped destination.
	f.request(api.RequestSpec{
		Name:        "adhoc-in",
		Source:      api.Source{Node: "studio-a", Domain: found, Select: api.Selector{Flow: flow.ID()}},
		Destination: api.Destination{Node: "edge-01", Domain: "ingest"},
	})
	f.eventually("a session from the discovered domain", func() bool {
		paths := f.paths().Paths
		return len(paths) == 1 && paths[0].Session != nil && paths[0].Session.Epoch != ""
	})
}

// --- negotiation reaching the user ---------------------------------------------------------------

// Two nodes may pair on a provider only if they share a *fabric label* for it (§10.1). Provider
// availability is not reachability: both of these offer tcp, and they are on networks the operator
// has said are different.
//
// The unit tests cover the negotiation table. What this covers is that the refusal reaches the
// caller at request time, as INVALID with the code that says which of the three ways it failed —
// rather than being accepted and left to sit in WAITING looking like it might come good.
func TestNoSharedFabricIsRefusedAtTheRequest(t *testing.T) {
	f := newFleet(t, fleetOptions{})

	src := f.addNode("studio-a", nodeOptions{domains: []string{"cameras"}})
	f.addNode("edge-01", nodeOptions{
		domains: []string{"ingest"},
		capabilities: &api.Capabilities{Fabrics: []api.FabricAttachment{{
			Provider: api.ProviderTCP,
			Fabric:   "somewhere-else",
			Address:  "127.0.0.1",
			CapFlags: []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive},
		}}},
	})

	flow := src.createFlow("cameras", videoFlowDef("Studio A:Camera 1", "video"))
	flow.produce()

	f.eventually("the source flow to be observed", func() bool {
		for _, entry := range f.flows().Flows {
			if entry.ID == flow.ID() {
				return true
			}
		}
		return false
	})

	refused := f.do(http.MethodPost, api.PathRequests, api.RequestSpec{
		Name:        "across-fabrics",
		Source:      api.Source{Node: "studio-a", Domain: "cameras", Select: api.Selector{Flow: flow.ID()}},
		Destination: api.Destination{Node: "edge-01", Domain: "ingest"},
	})
	require.Equal(t, http.StatusBadRequest, refused.status, "body: %s", refused.body)

	var refusal api.Error
	refused.decode(t, &refusal)
	assert.Equal(t, string(api.ReasonNoSharedFabric), refusal.Details["reason_code"])
	assert.Contains(t, refusal.Message, "dc1", "the message must name what each node offers")
	assert.Contains(t, refusal.Message, "somewhere-else")
}

// --- authentication --------------------------------------------------------------------------

// With a token configured, the agent's own client has to carry it on every call — registration,
// heartbeat, both reports and the assignment poll (§13).
//
// The server's tests prove it rejects a request without one. This proves the agent sends one,
// which is the half that cannot be tested from either side alone: an agent that authenticated on
// registration and not on the poll would come up, look registered, and never receive an
// assignment.
func TestTheAgentAuthenticatesOnEveryCall(t *testing.T) {
	p := newPair(t, fleetOptions{token: "e2e-shared-secret"})
	p.replicate("cam1")

	sessionID := p.established()
	assert.NotNil(t, p.dst.worker(sessionID, api.RoleTarget))
	assert.NotNil(t, p.src.worker(sessionID, api.RoleInitiator))
}
