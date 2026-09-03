package e2e

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server"
)

// events reads one object's log through the user API.
func (f *fleet) events(path string) api.EventList {
	f.t.Helper()

	resp := f.do(http.MethodGet, path, nil)
	require.Equal(f.t, http.StatusOK, resp.status, "body: %s", resp.body)

	var out api.EventList
	resp.decode(f.t, &out)
	return out
}

func hasKind(list api.EventList, kind api.EventKind) bool {
	for _, event := range list.Events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

// M12.1: a path's history reaches an operator through the API, written by the leader from what a
// reconcile pass concluded.
//
// The whole reason §12.1 exists is that a status is level-triggered: it says the path is ACTIVE
// and says nothing about how it got there or what it did in between.
func TestAPathRecordsItsOwnHistory(t *testing.T) {
	p := newPair(t, fleetOptions{})
	p.replicate("cam1")
	p.established()

	var log api.EventList
	p.eventually("the path's history to be recorded", func() bool {
		log = p.events(api.PathEventsPath(p.onlyPath().ID))
		return hasKind(log, api.EventSessionEstablished)
	})

	// The negotiated fabric appears nowhere else once the session is replaced, which is why the
	// establishment entry carries it rather than only naming the session.
	for _, event := range log.Events {
		if event.Kind == api.EventSessionEstablished {
			assert.Contains(t, event.Message, string(api.ProviderTCP))
		}
	}

	// Every entry carries a sequence, and the response carries the cursor to resume from. A
	// timestamp cannot do that job: entries are stamped by whoever emitted them (§12.1).
	require.NotEmpty(t, log.Events)
	assert.NotZero(t, log.Events[0].Seq)
	assert.NotZero(t, log.Next)
}

// **A path's log dies with the path** (§12.1): no tombstone, no grace period.
func TestCancellingARequestTakesThePathLogWithIt(t *testing.T) {
	p := newPair(t, fleetOptions{})
	request := p.replicate("cam1")
	p.established()

	pathID := p.onlyPath().ID
	p.eventually("something to be recorded about the path", func() bool {
		return len(p.events(api.PathEventsPath(pathID)).Events) > 0
	})

	p.cancel(request.RequestID())

	p.eventually("the path's log to go with it", func() bool {
		return len(p.events(api.PathEventsPath(pathID)).Events) == 0
	})
}

// M12.2: the sentence explaining a failure travels from the worker's stdout, through the agent,
// to an endpoint an operator can read without shell access to the node.
func TestAFailingWorkersLogReachesTheAPI(t *testing.T) {
	p := newPair(t, fleetOptions{})
	p.replicate("cam1")
	sessionID := p.established()

	pathID := p.onlyPath().ID

	target := p.dst.worker(sessionID, api.RoleTarget)
	require.NotNil(t, target)
	target.SetLogTail(
		"[12:47:22.101] [error] [flow.cpp:244] Failed to create flow : permission denied\n" +
			"[12:47:22.102] [error] fatal: unknown error: failed to create flow writer\n")
	target.Die(errors.New("exited"))

	p.eventually("the exit to be recorded with a log marker", func() bool {
		log := p.events(api.PathEventsPath(pathID))
		for _, event := range log.Events {
			if event.Kind == api.EventWorkerExited && event.HasLog {
				return true
			}
		}
		return false
	})

	// And the tail itself, from its own endpoint — a deliberate second read, so that the ring a UI
	// polls stays small (§12.2).
	resp := p.do(http.MethodGet, api.PathLogsPath(pathID), nil)
	require.Equal(t, http.StatusOK, resp.status, "body: %s", resp.body)

	var tail api.LogTail
	resp.decode(t, &tail)
	assert.Equal(t, "edge-01", tail.Node)
	assert.Equal(t, api.RoleTarget, tail.Role)
	assert.Contains(t, tail.Text, "failed to create flow writer")
}

// M12.1: flows entering and leaving a node's inventory reach that node's log (§12.1).
//
// On by default, and on the **node's** ring rather than any path's: a flow that nothing replicates
// has no path, and a flow that does is not the node's business to report twice.
func TestFlowChurnReachesTheNodeLog(t *testing.T) {
	p := newPair(t, fleetOptions{})
	p.replicate("cam1")
	p.established()

	extra := p.src.createFlow("cameras", videoFlowDef("Studio A:Camera 2", "video"))

	// Waiting for the *log* rather than for the fleet view, and the difference is the whole timing
	// story: `GET /v1/flows` is answered by a read handler running its own Compute, which sees the
	// store the instant the agent writes it. The journal is the leader's, one pass behind. A flow
	// that appeared and vanished between two passes is never recorded at all — level-triggered, like
	// everything else here — so a test that destroyed the flow on the strength of the fleet view
	// would race that and see nothing.
	p.eventually("the appearance to be recorded", func() bool {
		return hasKind(p.events(api.NodeEventsPath("studio-a")), api.EventFlowAppeared)
	})

	extra.destroy()
	p.eventually("the disappearance to be recorded", func() bool {
		return hasKind(p.events(api.NodeEventsPath("studio-a")), api.EventFlowDisappeared)
	})

	log := p.events(api.NodeEventsPath("studio-a"))
	for _, event := range log.Events {
		switch event.Kind {
		case api.EventFlowAppeared:
			assert.Equal(t, api.SeverityInfo, event.Severity)
			assert.Contains(t, event.Message, extra.ID())
		case api.EventFlowDisappeared:
			// Info, like the appearance. A producer stopping is ordinary (§11), and the control
			// plane has no business editorialising about it — the *consequences* carry the severity,
			// on the request whose expansion shrank and the path that went WAITING.
			assert.Equal(t, api.SeverityInfo, event.Severity)
			assert.Contains(t, event.Message, extra.ID())
			// Domain-qualified: the same flow ID can legitimately exist in two domains on one node
			// (§3), so an unqualified ID would report a flow as gone when it left only one of them.
			assert.Contains(t, event.Message, p.src.sourceName("cameras")+"/"+extra.ID())
		}
	}
}

// The switch turns off the entries it governs and nothing else.
func TestInventoryEventsCanBeSwitchedOff(t *testing.T) {
	p := newPair(t, fleetOptions{serverAdjust: func(cfg *server.Config) {
		cfg.NoInventoryEvents = true
	}})
	p.replicate("cam1")
	p.established()

	extra := p.src.createFlow("cameras", videoFlowDef("Studio A:Camera 2", "video"))
	p.eventually("the flow to reach the fleet view", func() bool {
		for _, flow := range p.flows().Flows {
			if flow.ID == extra.ID() {
				return true
			}
		}
		return false
	})

	p.consistently("the node log to stay quiet about it", func() bool {
		return !hasKind(p.events(api.NodeEventsPath("studio-a")), api.EventFlowAppeared)
	})
}

// A healthy fleet that has settled and is doing nothing new writes nothing (§8.3), and the event
// log must not be the writer that breaks that.
//
// The guard is on the *path* ring specifically. The fleet ring legitimately gains a takeover
// marker on the first pass, and a node ring gains a registration, so a whole-prefix assertion
// would be testing the wrong thing.
func TestAQuietFleetRecordsNothingNew(t *testing.T) {
	p := newPair(t, fleetOptions{})
	p.replicate("cam1")
	p.established()

	// Waiting on the *log* rather than on the path's state, and the difference is the point: a read
	// handler runs its own Compute (§7.3), so `GET /v1/paths` can report a settled state before the
	// leader's pass that recorded it has finished. Polling the state and then asserting the log is
	// quiet races that gap and fails intermittently.
	pathID := p.onlyPath().ID
	p.eventually("the path's log to reach a settled state", func() bool {
		events := p.events(api.PathEventsPath(pathID)).Events
		if len(events) == 0 {
			return false
		}
		last := events[len(events)-1].State
		return last == api.StateActive || last == api.StatePaused
	})

	before := p.events(api.PathEventsPath(pathID)).Next
	p.consistently("the path's log to stay put", func() bool {
		return p.events(api.PathEventsPath(pathID)).Next == before
	})
}
