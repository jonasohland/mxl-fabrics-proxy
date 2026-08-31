package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
// flow in "cameras", and a destination node whose output root "ingest" is materialised under.
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
	p.dst = f.addNode("edge-01", nodeOptions{})

	p.flow = p.src.createFlow("cameras", videoFlowDef("Studio A:Camera 1", "video"))
	p.flow.produce()
	return p
}

// replicate creates the request that replicates the pair's flow by ID.
func (p *pair) replicate(name string) api.Request {
	p.t.Helper()

	return p.request(api.RequestSpec{
		Name:         name,
		Source:       api.Source{Node: "studio-a", Domain: p.src.source("cameras"), Select: api.Selector{Flow: p.flow.ID()}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
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
			if flow.ID == p.flow.ID() && flow.Node == "studio-a" && flow.Domain == p.src.sourceName("cameras") {
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
	assert.Equal(t, api.FlowAddress{Node: "studio-a", Domain: p.src.sourceName("cameras"), Flow: p.flow.ID()}, path.Source)
	// The *resolved* root, not the one the request spelled: it names none and edge-01 advertises
	// exactly one (§10.6).
	assert.Equal(t, api.Destination{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}, path.Destination)
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
	assert.Equal(t, p.dst.path("ingest"), target.DomainPath,
		"the destination domain resolved under this node's output root, not through anything it observes")
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
	p.cancel(request.RequestID())
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
	f.addNode("edge-01", nodeOptions{})

	request := f.request(api.RequestSpec{
		Name: "camera-1",
		Source: api.Source{Node: "studio-a", Domain: src.source("cameras"), Select: api.Selector{
			GroupHint: &api.GroupHintSelector{Name: "Studio A:Camera 1"},
		}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	})

	// Matching nothing is WAITING, and costs nothing.
	f.eventually("the request to be waiting on its selector", func() bool {
		resp := f.do(http.MethodGet, api.RequestPath(request.RequestID()), nil)
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
	resp := f.do(http.MethodGet, api.RequestPath(request.RequestID()), nil)
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
		// The blob is held below to hold the path in ESTABLISHING; the agent must not give up on
		// the target while the test is looking at it.
		tweak: func(cfg *agent.Config) { cfg.TargetInfoTimeout = time.Minute },
	})

	def := videoFlowDef("Studio A:Camera 1", "video")
	f.request(api.RequestSpec{
		Name:         "cam1",
		Source:       api.Source{Node: "studio-a", Domain: src.source("cameras"), Select: api.Selector{Flow: def.ID}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
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
	//
	// This is also the assertion that catches a missing AddDomain on the materialised output
	// domain (§10.6): the destination domain was created by this request, and if the agent were
	// not observing it the path could never leave ESTABLISHING — which is a much more confusing
	// failure than this one.
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

// --- 7b. output domains: materialisation, refcounting and chains -------------------------------

// An output domain is created by the first path that targets it and forgotten when the last one
// goes, on the refcount that already governs paths — so it needs no lifecycle of its own, no
// create API and no delete API (§10.6).
//
// What is actually asserted is the *watch set*, because that is the step that gets left out:
// §11 derives ACTIVE from the destination flow as reported by the destination agent's own
// inventory, so a domain this project writes into but does not observe can never leave
// ESTABLISHING. A flow in the materialised domain reaching the fleet view is that wiring working.
func TestAnOutputDomainIsRefcountedAcrossRequests(t *testing.T) {
	f := newFleet(t, fleetOptions{})
	src := f.addNode("studio-a", nodeOptions{domains: []string{"cameras"}})
	dst := f.addNode("edge-01", nodeOptions{})

	first := src.createFlow("cameras", videoFlowDef("Studio A:Camera 1", "video"))
	first.produce()
	second := src.createFlow("cameras", videoFlowDef("Studio A:Camera 2", "video"))
	second.produce()

	// Nothing has asked for it yet, so it does not exist. A destination is not provisioned on the
	// node; it is named by a request.
	require.NoDirExists(t, dst.path("ingest"))

	replicate := func(name, flowID string) api.Request {
		return f.request(api.RequestSpec{
			Name:         name,
			Source:       api.Source{Node: "studio-a", Domain: src.source("cameras"), Select: api.Selector{Flow: flowID}},
			Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
		})
	}
	one := replicate("cam1", first.ID())
	two := replicate("cam2", second.ID())

	f.eventually("both paths to have a session", func() bool {
		paths := f.paths().Paths
		return len(paths) == 2 && paths[0].Session != nil && paths[1].Session != nil
	})

	// Two paths, one directory: the same name under the same root is the same domain, and the
	// refcount is simply that two target specs still name it.
	f.eventually("the destination domain to be materialised", func() bool {
		info, err := os.Stat(dst.path("ingest"))
		return err == nil && info.IsDir()
	})

	// A flow in it, which is what the target worker would have created. It reaches the fleet view
	// only because the destination agent added the materialised domain to its own watch set.
	delivered := dst.createFlow("ingest", videoFlowDef("Edge Ingest", "video"))
	delivered.produce()

	observed := func() bool {
		for _, flow := range f.flows().Flows {
			if flow.ID == delivered.ID() && flow.Node == "edge-01" && flow.Domain == "fast/ingest" {
				return true
			}
		}
		return false
	}
	f.eventually("the destination domain to be observed", observed)

	// The first cancellation changes nothing: the other request still names the domain.
	f.cancel(one.RequestID())
	f.eventually("one path to remain", func() bool { return len(f.paths().Paths) == 1 })
	assert.True(t, observed(), "a domain another path still targets must stay observed")

	// The last one releases it. The directory stays — the SDK removes a flow directory when its
	// writer is released, so what is left is empty and invisible to discovery — but this node
	// stops observing it, so it leaves the fleet view.
	f.cancel(two.RequestID())
	f.eventually("the domain to be released once nothing targets it", func() bool { return !observed() })
	assert.DirExists(t, dst.path("ingest"), "releasing stops observation; it does not delete media directories")
}

// A→B→C works with no extra design: the middle node's domain is materialised by the first
// request, which puts it in that agent's watch set, which is what makes it visible as a source for
// the second — and it exists exactly as long as the first hop does, which is the correct
// dependency (§10.6).
func TestAChainUsesAMaterialisedDomainAsItsSource(t *testing.T) {
	f := newFleet(t, fleetOptions{})
	a := f.addNode("studio-a", nodeOptions{domains: []string{"cameras"}})
	b := f.addNode("edge-01", nodeOptions{})
	f.addNode("edge-02", nodeOptions{})

	flow := a.createFlow("cameras", videoFlowDef("Studio A:Camera 1", "video"))
	flow.produce()

	f.request(api.RequestSpec{
		Name:         "a-to-b",
		Source:       api.Source{Node: "studio-a", Domain: a.source("cameras"), Select: api.Selector{Flow: flow.ID()}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"mid"}}}},
	})
	f.eventually("the first hop to establish", func() bool {
		paths := f.paths().Paths
		return len(paths) == 1 && paths[0].Session != nil
	})

	// The middle node's domain exists only because the first request asked for it. The flow in it
	// is what B's target worker would have created from the definition it was assigned.
	f.eventually("the middle node's domain to be materialised", func() bool {
		info, err := os.Stat(b.path("mid"))
		return err == nil && info.IsDir()
	})
	middle := createFlowAt(t, b.path("mid"), videoFlowDef("Studio A:Camera 1", "video"))
	middle.produce()

	f.eventually("the materialised domain to be a visible source", func() bool {
		for _, entry := range f.flows().Flows {
			if entry.ID == middle.ID() && entry.Node == "edge-01" && entry.Domain == "fast/mid" {
				return entry.Producing
			}
		}
		return false
	})

	f.request(api.RequestSpec{
		Name:         "b-to-c",
		Source:       api.Source{Node: "edge-01", Domain: named("fast/mid"), Select: api.Selector{Flow: middle.ID()}},
		Destinations: []api.Destination{{Node: "edge-02", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	})

	f.eventually("the second hop to establish from the materialised domain", func() bool {
		paths := f.paths().Paths
		if len(paths) != 2 {
			return false
		}
		for _, path := range paths {
			if path.Source.Node == "edge-01" && path.Source.Domain == "fast/mid" {
				return path.Session != nil && path.Session.Epoch != ""
			}
		}
		return false
	})
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
	impostor := p.addNode("edge-01", nodeOptions{contested: true})

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
// The other half is that a domain some *other* actor created inside a readable area is a perfectly
// good source, so the check is not "discovered domains are ignored".
func TestADomainInAReadOnlyAreaIsASourceAndNeverADestination(t *testing.T) {
	f := newFleet(t, fleetOptions{})

	area := searchRoot(t)
	src := f.addNode("studio-a", nodeOptions{
		domains:    []string{"cameras"},
		extraAreas: []api.Area{{Name: "ro", Path: area, Read: true}},
	})
	f.addNode("edge-01", nodeOptions{})

	// A producer creates a domain nobody asked for, inside an area that grants only reading. It
	// has a name like any other — `ro/adhoc`, from the innermost containing area (§10.6).
	found := filepath.Join(area, "adhoc")
	flow := createFlowAt(t, found, videoFlowDef("Ad Hoc:Camera", "video"))
	flow.produce()

	f.eventually("the domain to be reported under its area's name", func() bool {
		for _, entry := range f.flows().Flows {
			if entry.ID == flow.ID() && entry.Node == "studio-a" && entry.Domain == "ro/adhoc" {
				return entry.Producing
			}
		}
		return false
	})

	// As a destination it is refused: `ro` grants reading and nothing else, and the **grant is a
	// field on the entry** now rather than something the shape of the configuration carried
	// (§10.6). Refused at the API boundary rather than stored as INVALID, which is the better
	// place for it: nothing is persisted and no reconcile ever sees it.
	refused := f.do(http.MethodPost, api.NamespaceRequestsPath(api.DefaultNamespace), api.RequestSpec{
		Name:   "write-anywhere",
		Source: api.Source{Node: "studio-a", Domain: src.source("cameras"), Select: api.Selector{Flow: flow.ID()}},
		Destinations: []api.Destination{{
			Node: "edge-01", Domain: api.Domain{Area: "ro", Elements: []string{"adhoc"}},
		}},
	})
	require.Equal(t, http.StatusBadRequest, refused.status, "body: %s", refused.body)

	var refusal api.Error
	refused.decode(t, &refusal)
	assert.Equal(t, api.CodeInvalidRequest, refusal.Code)
	assert.Equal(t, string(api.ReasonUnknownArea), refusal.Details["reason_code"],
		"edge-01 has no area called `ro` at all")
	assert.Zero(t, src.starts(), "and nothing may be started for it")

	// And a raw path where a domain's elements belong is refused structurally, before the fleet
	// is consulted at all: a separator is simply not something an element may contain (§10.6).
	raw := f.do(http.MethodPost, api.NamespaceRequestsPath(api.DefaultNamespace), api.RequestSpec{
		Name:   "raw-path",
		Source: api.Source{Node: "studio-a", Domain: src.source("cameras"), Select: api.Selector{Flow: flow.ID()}},
		Destinations: []api.Destination{{
			Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{found}},
		}},
	})
	require.Equal(t, http.StatusBadRequest, raw.status, "body: %s", raw.body)
	raw.decode(t, &refusal)
	assert.Contains(t, refusal.Message, "destinations[0].domain")

	// The same domain as a *source* is fine, and replicates into a writable area.
	f.request(api.RequestSpec{
		Name:         "adhoc-in",
		Source:       api.Source{Node: "studio-a", Domain: named("ro/adhoc"), Select: api.Selector{Flow: flow.ID()}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
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

	refused := f.do(http.MethodPost, api.NamespaceRequestsPath(api.DefaultNamespace), api.RequestSpec{
		Name:         "across-fabrics",
		Source:       api.Source{Node: "studio-a", Domain: src.source("cameras"), Select: api.Selector{Flow: flow.ID()}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
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

// --- 10.7. domain labels and the label selector -------------------------------------------------

// label attaches labels to one (node, domain) through the real API.
func (f *fleet) label(node, domain string, labels map[string]string) api.DomainLabelResult {
	f.t.Helper()

	segments := strings.Split(domain, "/")
	resp := f.do(http.MethodPost, api.NodeDomainsPath(node), api.DomainLabelWrite{
		Domain: api.Domain{Area: segments[0], Elements: segments[1:]},
		Apply:  labels,
	})
	require.Less(f.t, resp.status, 300, "body: %s", resp.body)

	var out api.DomainLabelResult
	resp.decode(f.t, &out)
	return out
}

// **A label applied before its domain is discovered resolves by itself when a producer appears**
// (§10.7, §17). That is what "before or after" means: the operator labels a camera's domain before
// the camera is switched on, and the record is accepted, inert and visible in the meantime.
func TestALabelAppliedBeforeItsDomainResolvesWhenAProducerAppears(t *testing.T) {
	f := newFleet(t, fleetOptions{})
	src := f.addNode("studio-a", nodeOptions{domains: []string{"cameras"}})
	f.addNode("edge-01", nodeOptions{})

	// Nothing is in `media/pending` yet — the directory does not even exist.
	f.label("studio-a", "media/pending", map[string]string{"role": "late"})

	// The record is listed, so the intent is visible rather than lost.
	var list api.DomainList
	f.do(http.MethodGet, api.NodeDomainsPath("studio-a"), nil).decode(t, &list)
	var pending *api.DomainInfo
	for i := range list.Domains {
		if list.Domains[i].Domain.String() == "media/pending" {
			pending = &list.Domains[i]
		}
	}
	require.NotNil(t, pending, "a label on an unobserved domain is a pending record, not an error")
	assert.False(t, pending.Observed)

	// A request selecting it is accepted and WAITING, which §7.2 already files as legitimately not
	// an error.
	request := f.request(api.RequestSpec{
		Name:         "late",
		Source:       api.Source{Node: "studio-a", Domain: api.SelectLabels(map[string]string{"role": "late"}), Select: api.Selector{Flow: "does-not-exist"}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	})
	assert.Equal(t, api.StateWaiting, request.Status.State)
	assert.Empty(t, f.paths().Paths)

	// The producer appears. Nothing was written to make this work: the label record was already
	// there, and the reconcile that notices the domain is the ordinary one (§10.7).
	created := createFlowAt(t, filepath.Join(src.in, "pending"), videoFlowDef("Late:Camera", "video"))
	created.produce()

	f.eventually("the pending label to resolve into a path", func() bool {
		paths := f.paths().Paths
		return len(paths) == 1 && paths[0].Source.Domain == "media/pending"
	})
}

// **A relabel changes a request's expansion without restarting a worker on a path it still
// matches** (§17). That is the property the annotate-don't-rename decision exists for: a label is
// not in path identity, so relabelling is free.
func TestARelabelMovesTheExpansionWithoutRestartingAWorker(t *testing.T) {
	f := newFleet(t, fleetOptions{})
	src := f.addNode("studio-a", nodeOptions{domains: []string{"cameras", "audio"}})
	dst := f.addNode("edge-01", nodeOptions{})

	video := src.createFlow("cameras", videoFlowDef("Studio A:Camera 1", "video"))
	video.produce()
	audio := src.createFlow("audio", videoFlowDef("Studio A:Camera 1", "video"))
	audio.produce()

	f.label("studio-a", "media/cameras", map[string]string{"role": "live"})

	f.request(api.RequestSpec{
		Name: "live",
		Source: api.Source{
			Node:   "studio-a",
			Domain: api.SelectLabels(map[string]string{"role": "live"}),
			Select: api.Selector{GroupHint: &api.GroupHintSelector{Name: "Studio A:Camera 1"}},
		},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	})

	f.eventually("the first path to establish", func() bool {
		paths := f.paths().Paths
		return len(paths) == 1 && paths[0].Session != nil && paths[0].Session.Epoch != ""
	})
	first := f.paths().Paths[0]
	starts := dst.starts()

	// Labelling a *second* domain joins it to the expansion.
	f.label("studio-a", "media/audio", map[string]string{"role": "live"})

	f.eventually("the relabel to widen the expansion", func() bool {
		paths := f.paths().Paths
		if len(paths) != 2 {
			return false
		}
		// Both **established**, not merely planned: "no worker restarted" is otherwise measured
		// against a set that has not finished arriving. An epoch means the target is up and has
		// reported (§5.3 step 4).
		return paths[0].Session != nil && paths[0].Session.Epoch != "" &&
			paths[1].Session != nil && paths[1].Session.Epoch != ""
	})

	// **The path that already matched keeps its identity**, so nothing restarted on it. A worker
	// restart here would be a metadata edit glitching live media, which is exactly what §10.7
	// refuses.
	var still bool
	for _, path := range f.paths().Paths {
		if path.ID == first.ID {
			still = true
			assert.Equal(t, first.Session.ID, path.Session.ID, "same session, same worker")
		}
	}
	assert.True(t, still, "the existing path must not be re-identified")

	// One new target, and no restart of the old one.
	assert.Equal(t, starts+1, dst.starts(), "exactly one worker started, for the new path")
}

// **A label selector declines to match a flow this node is writing, while a sibling flow in the
// same domain — written locally — still matches; and a request naming that domain directly still
// chains** (§17).
//
// One node is both a source and a destination, which is the only shape in which the filter's
// omission is visible at all.
func TestASelectorSkipsThisProjectsOutputWhileNamingItStillChains(t *testing.T) {
	f := newFleet(t, fleetOptions{})
	src := f.addNode("studio-a", nodeOptions{domains: []string{"cameras"}})
	mid := f.addNode("edge-01", nodeOptions{domains: []string{"local"}})
	f.addNode("edge-02", nodeOptions{})

	original := src.createFlow("cameras", videoFlowDef("Studio A:Camera 1", "video"))
	original.produce()

	// Hop one puts a flow into edge-01's writable area, and edge-01's own target worker is the
	// thing writing it — which is what `replicated` reports (§6).
	f.request(api.RequestSpec{
		Name:         "a-to-b",
		Source:       api.Source{Node: "studio-a", Domain: src.source("cameras"), Select: api.Selector{Flow: original.ID()}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	})
	f.eventually("the first hop to establish", func() bool {
		paths := f.paths().Paths
		return len(paths) == 1 && paths[0].Session != nil
	})

	// The fake worker does not actually create the flow, so the test does — this is the flow the
	// target would have made from the definition it was assigned. **Same ID as the original**,
	// which is what replication means: the flow ID is unique to the media, not to a location
	// (§3), and it is what the agent's provenance keys on.
	replicatedDef := videoFlowDef("Studio A:Camera 1", "video")
	replicatedDef.ID = original.ID()
	replicated := createFlowAt(t, mid.path("ingest"), replicatedDef)
	replicated.produce()

	// And a flow a *local* media function produced beside it, in the same domain.
	sibling := createFlowAt(t, mid.path("ingest"), videoFlowDef("Studio A:Camera 1", "video"))
	sibling.produce()

	f.eventually("edge-01 to report both flows, one of them as replicated", func() bool {
		var seen, provenance int
		for _, flow := range f.flows().Flows {
			if flow.Node != "edge-01" || flow.Domain != "fast/ingest" {
				continue
			}
			seen++
			if flow.Replicated {
				provenance++
			}
		}
		return seen == 2 && provenance == 1
	})

	f.label("edge-01", "fast/ingest", map[string]string{"role": "onward"})

	// A **label selector** takes the sibling and not the replicated one.
	f.request(api.RequestSpec{
		Name: "matched",
		Source: api.Source{
			Node:   "edge-01",
			Domain: api.SelectLabels(map[string]string{"role": "onward"}),
			Select: api.Selector{GroupHint: &api.GroupHintSelector{Name: "Studio A:Camera 1"}},
		},
		Destinations: []api.Destination{{Node: "edge-02", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	})

	f.eventually("the selector to take exactly the flow this node did not write", func() bool {
		var matched []string
		for _, path := range f.paths().Paths {
			if path.Source.Node == "edge-01" {
				matched = append(matched, path.Source.Flow)
			}
		}
		return len(matched) == 1 && matched[0] == sibling.ID()
	})

	// And it says so: the skipped flow is named in the request's status, where a flow that simply
	// did not match the labels is not listed at all (§9.1).
	var requests api.RequestList
	f.do(http.MethodGet, api.PathRequests, nil).decode(t, &requests)
	var excluded []api.Exclusion
	for _, request := range requests.Requests {
		if request.Name == "matched" {
			excluded = request.Status.Excluded
		}
	}
	require.Len(t, excluded, 1)
	assert.Equal(t, replicated.ID(), excluded[0].Flow)
	assert.Equal(t, api.ExclusionSelfOutput, excluded[0].Reason)

	// **Naming the domain directly still reaches it**, which is what keeps chaining possible:
	// explicit chaining is intent, matched chaining is emergence (§10.7).
	f.request(api.RequestSpec{
		Name:         "named",
		Source:       api.Source{Node: "edge-01", Domain: named("fast/ingest"), Select: api.Selector{Flow: replicated.ID()}},
		Destinations: []api.Destination{{Node: "edge-02", Domain: api.Domain{Area: "fast", Elements: []string{"onward"}}}},
	})
	f.eventually("the named source to reach the replicated flow", func() bool {
		for _, path := range f.paths().Paths {
			if path.Source.Flow == replicated.ID() && path.Destination.Node == "edge-02" {
				return true
			}
		}
		return false
	})
}
