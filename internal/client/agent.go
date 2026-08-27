package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// pollSlack is how much longer than the requested wait an assignment poll is given before the
// transport gives up on it.
//
// The server caps the wait it honours and answers on expiry, so a client deadline only slightly
// above the requested wait would race the server's own answer and turn an ordinary empty poll
// into a transport failure — which the agent counts as "no answer" and reports as a fleet
// problem. Wide enough that only a genuinely stuck request hits it.
const pollSlack = 15 * time.Second

// Register performs POST /agent/v1/register (§7.1).
//
// Registration is durable desired state — this node exists, and here is what it can do — and the
// lease it returns is the observed, TTL'd half saying an agent instance currently holds the
// identity. The two are separate concepts and the response carries the cadence for keeping the
// second one alive.
//
// [IsNodeClaimed] is the interesting failure: another instance holds this node name, which is a
// copy-pasted config or an overlapping rollout, and the loser must not start workers.
func (c *Client) Register(ctx context.Context, registration api.NodeRegistration) (*api.RegistrationResponse, error) {
	var out api.RegistrationResponse
	err := c.do(ctx, request{
		method: http.MethodPost,
		path:   api.PathRegister,
		body:   registration,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Heartbeat performs POST /agent/v1/{node}/heartbeat: renew the lease, and nothing else.
//
// The server deliberately writes nothing here. A heartbeat that rewrote its lease record would
// advance the store revision several times a minute per node forever, waking every watcher —
// including every agent's assignment poll, where a spurious wakeup costs a reconcile (§8.3).
func (c *Client) Heartbeat(ctx context.Context, beat api.Heartbeat) (*api.HeartbeatResponse, error) {
	var out api.HeartbeatResponse
	err := c.do(ctx, request{
		method: http.MethodPost,
		path:   api.HeartbeatPath(beat.Node),
		body:   beat,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ReportInventory performs POST /agent/v1/{node}/inventory.
//
// A full snapshot, never a delta (§9.2). The server writes it rather than merging it, and does
// **not** compare it against what is already there — so the caller must not send an unchanged
// snapshot, or every report advances the store revision and wakes every watcher in the fleet for
// nothing.
func (c *Client) ReportInventory(ctx context.Context, snapshot api.InventorySnapshot) error {
	return c.do(ctx, request{
		method: http.MethodPost,
		path:   api.InventoryPath(snapshot.Node),
		body:   snapshot,
	})
}

// ReportStatus performs POST /agent/v1/{node}/status.
//
// Also a full snapshot, and full in a second sense: every session this agent is running, not
// merely the ones it was assigned. That is what lets a restarted server — which has desired state
// and no observed state — recognise a worker it never assigned in this process lifetime and adopt
// it rather than re-establishing media that was fine (§7.3).
//
// It is also the path an epoch travels on: a target reporting a new incarnation lands here, the
// write wakes the reconciler, and the peer's initiator assignment changes (§5.2). The same
// compare-before-send rule as [Client.ReportInventory] applies.
func (c *Client) ReportStatus(ctx context.Context, snapshot api.StatusSnapshot) error {
	return c.do(ctx, request{
		method: http.MethodPost,
		path:   api.StatusPath(snapshot.Node),
		body:   snapshot,
	})
}

// Assignments performs GET /agent/v1/{node}/assignments?rev=&wait=: the long poll that carries
// the complete set of workers this node should be running (§9.2).
//
// A long poll rather than a server push, because a push lands on one replica and state written by
// another has to be watched and fanned out to reach it — which is the sticky-session requirement
// HA exists to avoid (§8.2). It is proxy-transparent, trivially resumable from the cursor, and
// degrades to plain polling behind a proxy that buffers.
//
// # The contract this method exists to hold
//
// **It returns a non-nil set or an error, never both and never neither** (§4.2). A failed poll
// and an empty assignment set are indistinguishable to a reconciler, and an agent that reconciles
// against "empty" when it meant "no answer" tears down every worker on the node — the control
// plane going down stopping all media, which for live video is exactly backwards. So:
//
//   - [api.CodeNotReady] arrives here as an error, checkable with [IsNotReady], not as an empty
//     set. The server sends it while it is settling, while it has no observed state to reconcile
//     against, and when its own view is behind the caller's cursor.
//   - A 200 whose revision has gone *backwards* is refused, even though the server should already
//     have refused it. It costs one comparison and the failure it guards against is an agent
//     oscillating between two assignment versions behind a load balancer, restarting workers on
//     every swing (plan §4.5).
//
// cursor is the revision from the previous answer; zero asks for the current set immediately.
func (c *Client) Assignments(ctx context.Context, node string, cursor int64, wait time.Duration) (*api.AssignmentSet, error) {
	query := url.Values{}
	if cursor > 0 {
		query.Set(api.QueryRevision, strconv.FormatInt(cursor, 10))
	}
	if wait > 0 {
		query.Set(api.QueryWait, strconv.FormatFloat(wait.Seconds(), 'f', -1, 64))
	}

	var out api.AssignmentSet
	err := c.do(ctx, request{
		method:  http.MethodGet,
		path:    api.AssignmentsPath(node),
		query:   query,
		out:     &out,
		timeout: max(wait, 0) + pollSlack,
	})
	if err != nil {
		return nil, err
	}

	if out.Node != "" && out.Node != node {
		return nil, fmt.Errorf("client: assignments for node %q arrived while polling for %q", out.Node, node)
	}
	if out.Revision < cursor {
		return nil, fmt.Errorf("client: assignment set at revision %d is behind the cursor %d already held; not reconciling against a view that moved backwards",
			out.Revision, cursor)
	}

	return &out, nil
}
