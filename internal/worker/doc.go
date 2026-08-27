// Package worker is the agent's only way to run a media transfer, and the seam that makes the
// rest of this project testable.
//
// One worker moves one flow, in one direction, to one peer, in one role. That is the C++
// binary's model (WRS §1) and it is fine for v1 (§14) — but it is a property of *this*
// implementation, not of the concept, and this package is where the distinction is drawn.
//
// # The rule
//
// Nothing above this interface may assume one process per session, a filesystem work
// directory, or an AF_UNIX metrics socket. Those are how [github.com/jonasohland/mxl-replicator/internal/worker/exec]
// happens to work. A future multi-flow worker is then a substitution rather than a rewrite,
// and `os/exec` appears nowhere in the agent outside the exec implementation (§14, §17).
//
// # Why it exists now rather than later
//
// The future-proofing is the smaller half. The legacy tree execs the worker from deep inside
// its supervisor, which makes the entire control plane untestable without MXL, without
// libfabric and without RDMA hardware — and that is the main reason it has almost no tests.
// The same interface that keeps the worker replaceable is what lets
// [github.com/jonasohland/mxl-replicator/internal/worker/fake] stand in for it, so every
// control-plane test in M4, M5 and M7 runs in a temp directory on any machine. That payoff
// lands immediately; §14's is speculative. The fake is therefore written in the same milestone
// as the interface, not after it.
//
// # What lives here and what does not
//
// [Spec] is everything needed to run one worker *and to recognise it later* — the second half
// matters as much as the first, and [Spec.Key] is where it is spelled out (§7.3). Restart
// policy, backoff, port allocation, domain-name resolution and status classification are all
// the agent's, not this package's: an implementation of [Launcher] starts what it is told to
// start and reports what happened.
package worker
