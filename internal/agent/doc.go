// Package agent is the per-node half of the control plane (§6).
//
// One agent per node. It registers with the server, reports the flows it observes and the workers
// it is running, polls for its assignment set, and makes the processes on this host match it.
// It never receives a "start this" or "stop that" command — only a full desired set, which is
// what makes every operation idempotent and every crash cost nothing but time (§4.1).
//
// # The invariant this package exists to hold
//
// **A failed poll never reconciles** (§4.2, invariant 1). The agent's rule — "make my running
// processes match my assigned set" — is fatal if read literally, because a failed poll and an
// empty assignment set look identical, and an agent that reconciles to zero when it cannot reach
// the server turns a control-plane outage into a fleet-wide media outage. For a system carrying
// live video that is exactly backwards.
//
// So "empty" is a value this agent can only *learn*, never *infer*: [client.Client.Assignments]
// either produces a set or produces an error, [Agent.reconcile] has one call site, and it is on
// the success branch. The consequences are accepted deliberately — a worker that fails during a
// partition stays failed, and a long-partitioned agent reconciles hard when it comes back — which
// is the correct trade: a partition degrades *recovery*, not steady state.
//
// # No persistent state, and no adoption
//
// On restart the agent kills and re-establishes everything on the node (§6.1). It holds nothing
// across a restart, including the epoch: a restart produces a fresh incarnation nonce, which
// produces a new epoch, which is exactly the reconnect that is wanted. In the reference
// deployment it is not even a choice — the agent execs workers as children inside its own
// container, so a container restart tears down the PID namespace and there is nothing to adopt.
//
// The glitch is accepted, so it is made short: inotify for the target's blob rather than a
// backoff poll, long-polled assignments so the *peer* agent learns of a new epoch in well under a
// second, and domain directories pre-created at startup rather than on the establishment path.
//
// # Layout
//
//   - agent.go — lifecycle: registration, heartbeat, reporting, the poll loop.
//   - reconcile.go — an assignment set becomes a set of [worker.Spec]s, and the "already correct"
//     test that decides what actually restarts.
//   - unit.go — one supervised worker: start, wait, classify, back off, restart.
//
// Nothing in this package starts a process. Everything goes through [worker.Launcher], which is
// what keeps the whole control plane testable without MXL, libfabric or RDMA hardware, and what
// would make a future multi-flow worker a substitution rather than a rewrite (§14, §17,
// invariant 11).
package agent
