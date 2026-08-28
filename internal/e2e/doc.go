// Package e2e is M7: the integration suite (§17).
//
// Every other test in this tree stops at a seam. The agent's tests drive a real agent against a
// hand-written server that answers whatever the test sets; the server's tests drive a real
// server against HTTP calls shaped like an agent's. Both are the right shape for what they
// cover, and neither can catch a disagreement *between* the two — an assignment the server
// derives one way and the agent keys on another, a settling window that lets an empty set
// through, a revision cursor that goes backwards across a restart. Those are the failures with
// the worst blast radius in the design, and they only exist where the halves meet.
//
// So the suite here wires the real thing together: a real [server.Server] on a real listener, a
// real store, and N real [agent.Agent]s, each with a real inventory over a temp directory,
// talking to it over real HTTP. Nothing between an agent and the server is faked.
//
// # What is faked, and why only that
//
// The worker, and nothing else. [fake.Launcher] starts no processes and needs no MXL, no
// libfabric and no RDMA hardware, which is what makes the whole control plane testable in a temp
// directory on any machine (§17). Source and destination flows are built on disk with
// mxl-utils' testutil, so the inventory that drives every decision is reading real flow
// directories rather than a fixture the test invented.
//
// The one test that does not fake the worker is loopback_test.go, which runs the real worker
// binary over shm on one host. It skips when the binaries are not built.
//
// # Reading a failure here
//
// A test in this package failing usually does not mean this package is wrong. It means two
// packages that each pass their own tests do not agree, and the interesting question is which
// of the two moved. Every case below names the invariant it is protecting and where that
// invariant is written down.
package e2e
